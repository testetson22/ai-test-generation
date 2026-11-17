package artifact

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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
