package provider

import (
	"context"
	"errors"
	"time"
)

var (
	ErrSubmissionAckLost   = errors.New("provider accepted submission but acknowledgement was lost")
	ErrAttemptConflict     = errors.New("submission token is bound to a different attempt specification")
	ErrAttemptNotFound     = errors.New("provider attempt not found")
	ErrObservationConflict = errors.New("provider observation sequence is bound to different content")
)

type Status string

const (
	StatusPending         Status = "pending"
	StatusRunning         Status = "running"
	StatusCompleted       Status = "completed"
	StatusFailed          Status = "failed"
	StatusCancelRequested Status = "cancel_requested"
	StatusCancelled       Status = "cancelled"
	StatusNotFound        Status = "not_found"
	StatusOutcomeUnknown  Status = "outcome_unknown"
)

type SubmitRequest struct {
	SubmissionToken string `json:"submission_token"`
	TaskID          string `json:"task_id"`
	TaskSpecDigest  string `json:"task_spec_digest"`
}

type JobRef struct {
	ID string `json:"id"`
}

type Identity struct {
	Kind    string `json:"kind"`
	ID      string `json:"id"`
	Version string `json:"version,omitempty"`
	Digest  string `json:"digest,omitempty"`
}

type SubmissionError struct {
	Category       string
	OutcomeUnknown bool
	Err            error
}

func (e *SubmissionError) Error() string {
	if e.Err != nil {
		return e.Err.Error()
	}
	return e.Category
}

func (e *SubmissionError) Unwrap() error {
	return e.Err
}

type Observation struct {
	SubmissionToken      string    `json:"submission_token"`
	Job                  JobRef    `json:"job"`
	Sequence             uint64    `json:"sequence"`
	Status               Status    `json:"status"`
	FailureCategory      string    `json:"failure_category,omitempty"`
	TerminationConfirmed bool      `json:"termination_confirmed"`
	ObservedAt           time.Time `json:"observed_at"`
}

type Provider interface {
	// Submit must be idempotent for a submission token and task specification.
	// A token reused with different task/specification identity must fail. This
	// permits safe same-attempt redelivery; it does not authorize a new attempt.
	Submit(context.Context, SubmitRequest) (JobRef, error)
	// Observe reconciles the stable token. NotFound alone does not authorize a
	// new token or prove that a previously accepted external job never ran.
	Observe(context.Context, string) (Observation, error)
	Cancel(context.Context, string) (Observation, error)
	Cleanup(context.Context, string) error
}
