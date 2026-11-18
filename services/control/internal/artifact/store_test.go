package artifact

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestFileStorePersistsContentAddressedObjectsAcrossReopen(t *testing.T) {
	root := filepath.Join(t.TempDir(), "objects")
	store, err := NewFileStore(root, 32)
	if err != nil {
		t.Fatalf("open file store: %v", err)
	}
	first, err := store.Put(context.Background(), []byte("durable evidence"))
	if err != nil {
		t.Fatalf("put artifact: %v", err)
	}
	if first.Digest != Digest([]byte("durable evidence")) || first.SizeBytes != int64(len("durable evidence")) {
		t.Fatalf("artifact reference = %#v", first)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close first store: %v", err)
	}

	reopened, err := NewFileStore(root, 32)
	if err != nil {
		t.Fatalf("reopen file store: %v", err)
	}
	defer reopened.Close()
	duplicate, err := reopened.Put(context.Background(), []byte("durable evidence"))
	if err != nil || duplicate != first {
		t.Fatalf("duplicate put = %#v, %v; want %#v, nil", duplicate, err, first)
	}
	content, err := reopened.Get(context.Background(), first.Digest, 32)
	if err != nil || string(content) != "durable evidence" {
		t.Fatalf("get artifact = %q, %v", content, err)
	}
	if _, err := reopened.Get(context.Background(), "sha256:invalid", 32); !errors.Is(err, ErrInvalidDigest) {
		t.Fatalf("invalid digest error = %v, want ErrInvalidDigest", err)
	}
}

func TestFileStoreRejectsOversizeCorruptionAndSymlinkedBuckets(t *testing.T) {
	root := filepath.Join(t.TempDir(), "objects")
	store, err := NewFileStore(root, 4)
	if err != nil {
		t.Fatalf("open file store: %v", err)
	}
	if _, err := store.Put(context.Background(), []byte("oversized")); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversize error = %v, want ErrTooLarge", err)
	}
	ref, err := store.Put(context.Background(), []byte("data"))
	if err != nil {
		t.Fatalf("put test object: %v", err)
	}
	objectPath := filepath.Join(root, "sha256", ref.Digest[7:9], ref.Digest[9:])
	if err := os.WriteFile(objectPath, []byte("evil"), 0o600); err != nil {
		t.Fatalf("corrupt object: %v", err)
	}
	if _, err := store.Get(context.Background(), ref.Digest, 4); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt object error = %v, want ErrCorrupt", err)
	}
	if _, err := store.Put(context.Background(), []byte("data")); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt duplicate put error = %v, want ErrCorrupt", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close file store: %v", err)
	}

	linkRoot := filepath.Join(t.TempDir(), "objects")
	if err := os.MkdirAll(linkRoot, 0o700); err != nil {
		t.Fatalf("create link root: %v", err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(linkRoot, "sha256")); err != nil {
		t.Fatalf("create bucket symlink: %v", err)
	}
	linkedStore, err := NewFileStore(linkRoot, 16)
	if err != nil {
		t.Fatalf("open symlink test store: %v", err)
	}
	defer linkedStore.Close()
	if _, err := linkedStore.Put(context.Background(), []byte("data")); err == nil {
		t.Fatal("put followed a symlinked artifact bucket")
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatalf("outside bucket entries = %v, error = %v; want empty", entries, err)
	}
}

func TestFileStoreRetriesFailedDirectoryDurabilityBarriers(t *testing.T) {
	content := []byte("durable evidence")
	digest := Digest(content)
	directories := []string{".", "sha256", filepath.Join("sha256", digest[7:9])}
	for barrier, name := range []string{"root ancestor", "digest ancestor", "post rename"} {
		for _, restart := range []struct {
			name   string
			reopen bool
		}{{name: "same store"}, {name: "reopened store", reopen: true}} {
			t.Run(name+"/"+restart.name, func(t *testing.T) {
				root := filepath.Join(t.TempDir(), "objects")
				store, err := NewFileStore(root, 1024)
				if err != nil {
					t.Fatalf("open store: %v", err)
				}
				defer func() {
					if store != nil {
						_ = store.Close()
					}
				}()
				injected := errors.New("injected directory fsync failure")
				remainingFailures := 2
				var syncCalls []string
				syncDir := func(directory *os.Root) error {
					info, err := directory.Stat(".")
					if err != nil {
						t.Fatal(err)
					}
					for _, relative := range directories {
						expected, err := os.Stat(filepath.Join(root, relative))
						if errors.Is(err, os.ErrNotExist) {
							continue
						}
						if err != nil {
							t.Fatal(err)
						}
						if os.SameFile(info, expected) {
							syncCalls = append(syncCalls, relative)
							if relative == directories[barrier] && remainingFailures > 0 {
								remainingFailures--
								return injected
							}
							return syncDirectory(directory)
						}
					}
					return errors.New("sync requested for an unexpected directory")
				}
				store.syncDir = syncDir
				for attempt := 0; attempt < 2; attempt++ {
					syncCalls = nil
					ref, err := store.Put(context.Background(), content)
					if !errors.Is(err, injected) || ref != (Ref{}) {
						t.Fatalf("attempt %d acknowledged unsynced evidence: %#v, %v", attempt, ref, err)
					}
					if !slices.Equal(syncCalls, directories[:barrier+1]) {
						t.Fatalf("attempt %d sync calls = %v, want %v", attempt, syncCalls, directories[:barrier+1])
					}
					objectPath := filepath.Join(root, directories[2], digest[9:])
					actual, readErr := os.ReadFile(objectPath)
					if barrier == 2 {
						if readErr != nil || string(actual) != string(content) {
							t.Fatalf("post-rename object was not published before sync failure: %q, %v", actual, readErr)
						}
					} else if !errors.Is(readErr, os.ErrNotExist) {
						t.Fatalf("object published despite failed ancestor sync: %q, %v", actual, readErr)
					}
					if restart.reopen && attempt == 0 {
						if err := store.Close(); err != nil {
							t.Fatal(err)
						}
						store, err = NewFileStore(root, 1024)
						if err != nil {
							t.Fatalf("reopen after failed sync: %v", err)
						}
						store.syncDir = syncDir
					}
				}
				for attempt := 0; attempt < 2; attempt++ {
					syncCalls = nil
					ref, err := store.Put(context.Background(), content)
					if err != nil || ref != (Ref{Digest: digest, SizeBytes: int64(len(content))}) {
						t.Fatalf("durable retry/deduplication = %#v, %v", ref, err)
					}
					if !slices.Equal(syncCalls, directories) {
						t.Fatalf("successful write skipped durability barriers: %v, want %v", syncCalls, directories)
					}
				}
				store.syncDir = func(*os.Root) error { return injected }
				actual, err := store.Get(context.Background(), digest, 1024)
				if err != nil || string(actual) != string(content) {
					t.Fatalf("reading durable bytes required another sync: %q, %v", actual, err)
				}
			})
		}
	}
}
