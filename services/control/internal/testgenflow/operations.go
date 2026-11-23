package testgenflow

import (
	"encoding/json"
	"errors"
	"time"
)

var (
	ErrFlowClaimLost     = errors.New("test-generation flow claim lost")
	ErrOperationConflict = errors.New("test-generation operation conflict")
)

type FlowClaim struct {
	RunID      string    `json:"run_id"`
	Holder     string    `json:"holder"`
	Fence      int64     `json:"fence"`
	LeaseUntil time.Time `json:"lease_until"`
}

// A replayed prepared operation is reconciliation-only, not authorization to
// repeat an external call whose outcome may have been lost.
type Operation struct {
	Key         string          `json:"key"`
	RunID       string          `json:"run_id"`
	Kind        string          `json:"kind"`
	InputDigest string          `json:"input_digest"`
	State       string          `json:"state"`
	Result      json.RawMessage `json:"result,omitempty"`
	Diagnostic  string          `json:"diagnostic,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
}
