package testkit

import (
	"errors"
	"testing"
)

func TestArtifactStoreLostAcknowledgementIsIdempotent(t *testing.T) {
	store := NewMemoryArtifactStore(32)
	store.FailNextAcknowledgementAfterWrite()
	if _, err := store.Put([]byte("evidence")); !errors.Is(err, ErrArtifactAckLost) {
		t.Fatalf("first put error = %v, want acknowledgement loss", err)
	}
	digest, err := store.Put([]byte("evidence"))
	if err != nil {
		t.Fatalf("idempotent put: %v", err)
	}
	content, err := store.Get(digest)
	if err != nil || string(content) != "evidence" {
		t.Fatalf("read content = %q, error = %v", content, err)
	}
}

func TestArtifactStoreBoundsAvailabilityAndVerifiesContent(t *testing.T) {
	store := NewMemoryArtifactStore(4)
	if _, err := store.Put([]byte("oversized")); !errors.Is(err, ErrArtifactTooLarge) {
		t.Fatalf("oversized put error = %v, want size error", err)
	}
	digest, err := store.Put([]byte("data"))
	if err != nil {
		t.Fatalf("put artifact: %v", err)
	}
	store.SetUnavailable(true)
	if _, err := store.Get(digest); !errors.Is(err, ErrArtifactUnavailable) {
		t.Fatalf("unavailable get error = %v, want unavailable", err)
	}
	store.SetUnavailable(false)
	if err := store.CorruptForTest(digest); err != nil {
		t.Fatalf("corrupt artifact: %v", err)
	}
	if _, err := store.Get(digest); !errors.Is(err, ErrArtifactCorrupt) {
		t.Fatalf("corrupt get error = %v, want digest mismatch", err)
	}
}
