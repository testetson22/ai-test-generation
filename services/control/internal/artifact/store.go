package artifact

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

var (
	ErrInvalidDigest = errors.New("invalid artifact digest")
	ErrTooLarge      = errors.New("artifact exceeds configured size limit")
	ErrCorrupt       = errors.New("artifact content does not match its digest")
)

type Ref struct {
	Digest    string
	SizeBytes int64
}

// Store is the content-addressed boundary shared by development and future
// S3-compatible adapters.
type Store interface {
	Put(context.Context, []byte) (Ref, error)
	Get(context.Context, string, int64) ([]byte, error)
}

// FileStore is a durable local development adapter. It is not a production
// substitute for the selected S3-compatible artifact service.
type FileStore struct {
	mu       sync.Mutex
	root     *os.Root
	maxBytes int64
	syncDir  func(*os.Root) error
}

func NewFileStore(rootPath string, maxBytes int64) (*FileStore, error) {
	if strings.TrimSpace(rootPath) == "" || maxBytes <= 0 {
		return nil, fmt.Errorf("artifact root and positive size limit are required")
	}
	if err := os.MkdirAll(rootPath, 0o700); err != nil {
		return nil, fmt.Errorf("create artifact root: %w", err)
	}
	absolute, err := filepath.Abs(rootPath)
	if err != nil {
		return nil, fmt.Errorf("resolve artifact root: %w", err)
	}
	pathInfo, err := os.Lstat(absolute)
	if err != nil {
		return nil, fmt.Errorf("inspect artifact root: %w", err)
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 || !pathInfo.IsDir() {
		return nil, fmt.Errorf("artifact root must be a real directory")
	}
	root, err := os.OpenRoot(absolute)
	if err != nil {
		return nil, fmt.Errorf("open artifact root: %w", err)
	}
	openedInfo, err := root.Stat(".")
	if err != nil || !os.SameFile(pathInfo, openedInfo) {
		_ = root.Close()
		return nil, fmt.Errorf("artifact root changed while opening")
	}
	// MkdirAll may have created multiple ancestors. Repeat these barriers on
	// reopen as well, since an earlier initialization could have failed to sync.
	for parent := filepath.Dir(absolute); ; parent = filepath.Dir(parent) {
		directory, err := os.OpenRoot(parent)
		if err == nil {
			err = syncDirectory(directory)
			_ = directory.Close()
		}
		if err != nil {
			_ = root.Close()
			return nil, fmt.Errorf("sync artifact root ancestor: %w", err)
		}
		if parent == filepath.Dir(parent) {
			break
		}
	}
	return &FileStore{root: root, maxBytes: maxBytes, syncDir: syncDirectory}, nil
}

func (s *FileStore) Put(ctx context.Context, content []byte) (Ref, error) {
	if err := ctx.Err(); err != nil {
		return Ref{}, err
	}
	if int64(len(content)) > s.maxBytes {
		return Ref{}, ErrTooLarge
	}
	digest := Digest(content)

	s.mu.Lock()
	defer s.mu.Unlock()
	directory, err := s.openDirectory(true, "sha256", digest[7:9])
	if err != nil {
		return Ref{}, err
	}
	defer directory.Close()
	name := digest[9:]
	if existing, err := readObject(directory, name, s.maxBytes); err == nil {
		if Digest(existing) != digest || !equal(existing, content) {
			return Ref{}, ErrCorrupt
		}
		if err := s.syncDir(directory); err != nil {
			return Ref{}, err
		}
		return Ref{Digest: digest, SizeBytes: int64(len(existing))}, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return Ref{}, err
	}

	temporary, err := randomTemporaryName()
	if err != nil {
		return Ref{}, err
	}
	file, err := directory.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return Ref{}, fmt.Errorf("create temporary artifact: %w", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = directory.Remove(temporary)
		}
	}()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		return Ref{}, fmt.Errorf("temporary artifact is not a regular file")
	}
	if err := writeAll(file, content); err != nil {
		_ = file.Close()
		return Ref{}, fmt.Errorf("write artifact: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return Ref{}, fmt.Errorf("sync artifact: %w", err)
	}
	if err := file.Close(); err != nil {
		return Ref{}, fmt.Errorf("close artifact: %w", err)
	}
	if err := directory.Rename(temporary, name); err != nil {
		return Ref{}, fmt.Errorf("publish artifact: %w", err)
	}
	cleanup = false
	if err := s.syncDir(directory); err != nil {
		return Ref{}, err
	}
	return Ref{Digest: digest, SizeBytes: int64(len(content))}, nil
}

func (s *FileStore) Get(ctx context.Context, digest string, maxBytes int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !validDigest(digest) {
		return nil, ErrInvalidDigest
	}
	if maxBytes <= 0 || maxBytes > s.maxBytes {
		maxBytes = s.maxBytes
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	directory, err := s.openDirectory(false, "sha256", digest[7:9])
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	content, err := readObject(directory, digest[9:], maxBytes)
	if err != nil {
		return nil, err
	}
	if Digest(content) != digest {
		return nil, ErrCorrupt
	}
	return content, nil
}

func (s *FileStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.root.Close()
}

func Digest(content []byte) string {
	digest := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func (s *FileStore) openDirectory(syncExisting bool, parts ...string) (*os.Root, error) {
	current := s.root
	owned := false
	for _, part := range parts {
		before, err := current.Lstat(part)
		created := false
		if errors.Is(err, os.ErrNotExist) {
			mkdirErr := current.Mkdir(part, 0o700)
			if mkdirErr != nil && !errors.Is(mkdirErr, os.ErrExist) {
				if owned {
					_ = current.Close()
				}
				return nil, fmt.Errorf("create artifact directory: %w", mkdirErr)
			}
			created = mkdirErr == nil
			before, err = current.Lstat(part)
		}
		if err != nil || before.Mode()&os.ModeSymlink != 0 || !before.IsDir() {
			if owned {
				_ = current.Close()
			}
			return nil, fmt.Errorf("artifact path contains a symlink or non-directory")
		}
		// A previous call may have created this directory but failed its sync.
		if syncExisting || created {
			if err := s.syncDir(current); err != nil {
				if owned {
					_ = current.Close()
				}
				return nil, err
			}
		}
		next, err := current.OpenRoot(part)
		if err != nil {
			if owned {
				_ = current.Close()
			}
			return nil, fmt.Errorf("open artifact directory: %w", err)
		}
		opened, openedErr := next.Stat(".")
		after, afterErr := current.Lstat(part)
		if openedErr != nil || afterErr != nil || after.Mode()&os.ModeSymlink != 0 || !os.SameFile(before, opened) || !os.SameFile(after, opened) {
			_ = next.Close()
			if owned {
				_ = current.Close()
			}
			return nil, fmt.Errorf("artifact directory changed while opening")
		}
		if owned {
			_ = current.Close()
		}
		current, owned = next, true
	}
	return current, nil
}

func readObject(root *os.Root, name string, limit int64) ([]byte, error) {
	file, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("artifact object is not a regular file")
	}
	if info.Size() > limit {
		return nil, ErrTooLarge
	}
	content, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(content)) > limit {
		return nil, ErrTooLarge
	}
	return content, nil
}

func randomTemporaryName() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("generate temporary artifact identity: %w", err)
	}
	return ".atg-" + hex.EncodeToString(random[:]) + ".tmp", nil
}

func writeAll(file *os.File, content []byte) error {
	for len(content) > 0 {
		written, err := file.Write(content)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		content = content[written:]
	}
	return nil
}

func syncDirectory(root *os.Root) error {
	file, err := root.Open(".")
	if err != nil {
		return fmt.Errorf("open artifact directory for sync: %w", err)
	}
	defer file.Close()
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync artifact directory: %w", err)
	}
	return nil
}

func validDigest(digest string) bool {
	if len(digest) != len("sha256:")+64 || !strings.HasPrefix(digest, "sha256:") {
		return false
	}
	_, err := hex.DecodeString(digest[len("sha256:"):])
	return err == nil && digest == strings.ToLower(digest)
}

func equal(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
