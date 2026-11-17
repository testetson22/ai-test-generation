package postgres

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

func newID(prefix string) (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate %s identity: %w", prefix, err)
	}
	return prefix + "-" + hex.EncodeToString(raw[:]), nil
}
