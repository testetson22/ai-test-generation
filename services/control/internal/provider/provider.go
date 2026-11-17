package provider

import (
	"context"
	"errors"
	"time"
)

var (
	ErrSubmissionAckLost = errors.New("provider accepted submission but acknowledgement was lost")
	ErrAttemptConflict   = errors.New("submission token is bound to a different attempt specification")
	ErrAttemptNotFound   = errors.New("provider attempt not found")
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
	SubmissionToken string
	TaskID          string
	TaskSpecDigest  string
}

type JobRef struct {
	ID string
}

type Observation struct {
	SubmissionToken      string
	Job                  JobRef
	Sequence             uint64
	Status               Status
	FailureCategory      string
	TerminationConfirmed bool
	ObservedAt           time.Time
}

type Provider interface {
	Submit(context.Context, SubmitRequest) (JobRef, error)
	Observe(context.Context, string) (Observation, error)
	Cancel(context.Context, string) (Observation, error)
	Cleanup(context.Context, string) error
}
