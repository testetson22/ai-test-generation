package testkit

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
)

var (
	ErrArtifactTooLarge    = errors.New("artifact exceeds configured size limit")
	ErrArtifactMissing     = errors.New("artifact not found")
	ErrArtifactCorrupt     = errors.New("stored artifact digest mismatch")
	ErrArtifactAckLost     = errors.New("artifact persisted but acknowledgement was lost")
	ErrArtifactUnavailable = errors.New("artifact store unavailable")
)

// MemoryArtifactStore models content-addressed external storage. Keep it alive
// across controller reconstruction to exercise restart behavior; it is not a
// production artifact adapter.
type MemoryArtifactStore struct {
	mu             sync.Mutex
	objects        map[string][]byte
	maxBytes       int
	failAfterWrite bool
	unavailable    bool
}

func NewMemoryArtifactStore(maxBytes int) *MemoryArtifactStore {
	return &MemoryArtifactStore{objects: make(map[string][]byte), maxBytes: maxBytes}
}

func (s *MemoryArtifactStore) FailNextAcknowledgementAfterWrite() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failAfterWrite = true
}

func (s *MemoryArtifactStore) SetUnavailable(unavailable bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unavailable = unavailable
}

func (s *MemoryArtifactStore) Put(data []byte) (string, error) {
	if s.maxBytes > 0 && len(data) > s.maxBytes {
		return "", ErrArtifactTooLarge
	}
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(data))

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.unavailable {
		return "", ErrArtifactUnavailable
	}
	if existing, ok := s.objects[digest]; ok {
		if !equalBytes(existing, data) {
			return "", ErrArtifactCorrupt
		}
		return digest, nil
	}
	s.objects[digest] = append([]byte(nil), data...)
	if s.failAfterWrite {
		s.failAfterWrite = false
		return "", ErrArtifactAckLost
	}
	return digest, nil
}

func (s *MemoryArtifactStore) Get(digest string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.unavailable {
		return nil, ErrArtifactUnavailable
	}
	data, ok := s.objects[digest]
	if !ok {
		return nil, ErrArtifactMissing
	}
	if got := fmt.Sprintf("sha256:%x", sha256.Sum256(data)); got != digest {
		return nil, ErrArtifactCorrupt
	}
	return append([]byte(nil), data...), nil
}

func (s *MemoryArtifactStore) CorruptForTest(digest string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.objects[digest]
	if !ok {
		return ErrArtifactMissing
	}
	if len(data) == 0 {
		data = []byte{0}
	} else {
		data[0] ^= 0xff
	}
	s.objects[digest] = data
	return nil
}

func equalBytes(left, right []byte) bool {
	return bytes.Equal(left, right)
}
