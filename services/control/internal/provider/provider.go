package provider

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"
)

var (
	ErrSubmissionAckLost     = errors.New("provider accepted submission but acknowledgement was lost")
	ErrAttemptConflict       = errors.New("submission token is bound to a different attempt specification")
	ErrAttemptNotFound       = errors.New("provider attempt not found")
	ErrObservationConflict   = errors.New("provider observation sequence is bound to different content")
	ErrInvalidSubmission     = errors.New("invalid provider submission request")
	submissionIDPattern      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,255}$`)
	submissionDigestPattern  = regexp.MustCompile(`^sha256:[A-Fa-f0-9]{64}$`)
	submissionVersionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
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
	// These fields bind one attempt to the immutable task and approved profile;
	// no commands, credentials, or result evidence are carried here.
	SubmissionToken string `json:"submission_token"`
	TaskID          string `json:"task_id"`
	AttemptID       string `json:"attempt_id"`
	TaskSpecDigest  string `json:"task_spec_digest"`
	ProfileID       string `json:"profile_id"`
	ProfileVersion  string `json:"profile_version"`
	ProfileDigest   string `json:"profile_digest"`
	// KnownJob is persisted reconciliation context, not immutable task identity.
	// It may be absent while recovering an ambiguous initial submission.
	KnownJob JobRef `json:"known_job,omitempty"`
}

// SameAttempt excludes reconciliation context from immutable request identity.
func (r SubmitRequest) SameAttempt(other SubmitRequest) bool {
	r.KnownJob, other.KnownJob = JobRef{}, JobRef{}
	return r == other
}

func (r SubmitRequest) Validate() error {
	if !submissionIDPattern.MatchString(r.SubmissionToken) || !submissionIDPattern.MatchString(r.TaskID) ||
		!submissionIDPattern.MatchString(r.AttemptID) || !submissionIDPattern.MatchString(r.ProfileID) ||
		!submissionVersionPattern.MatchString(r.ProfileVersion) || !submissionDigestPattern.MatchString(r.TaskSpecDigest) ||
		!submissionDigestPattern.MatchString(r.ProfileDigest) {
		return fmt.Errorf("%w: attempt, task, profile, token, or digest identity is invalid", ErrInvalidSubmission)
	}
	return nil
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
	SubmissionToken string `json:"submission_token"`
	TaskID          string `json:"task_id"`
	AttemptID       string `json:"attempt_id"`
	TaskSpecDigest  string `json:"task_spec_digest"`
	ProfileID       string `json:"profile_id"`
	ProfileVersion  string `json:"profile_version"`
	ProfileDigest   string `json:"profile_digest"`
	Job             JobRef `json:"job"`
	Sequence        uint64 `json:"sequence"`
	// ProviderRevision is an opaque native version; Sequence is controller-fenced.
	ProviderRevision     string    `json:"provider_revision,omitempty"`
	Status               Status    `json:"status"`
	FailureCategory      string    `json:"failure_category,omitempty"`
	TerminationConfirmed bool      `json:"termination_confirmed"`
	ObservedAt           time.Time `json:"observed_at"`
}

func (o Observation) ValidateFor(request SubmitRequest) error {
	if err := request.Validate(); err != nil {
		return err
	}
	if o.SubmissionToken != request.SubmissionToken || o.TaskID != request.TaskID || o.AttemptID != request.AttemptID ||
		o.TaskSpecDigest != request.TaskSpecDigest || o.ProfileID != request.ProfileID ||
		o.ProfileVersion != request.ProfileVersion || o.ProfileDigest != request.ProfileDigest ||
		request.KnownJob.ID != "" && o.Job != request.KnownJob {
		return ErrObservationConflict
	}
	return nil
}

type Provider interface {
	// Submit must be idempotent for a submission token and task specification.
	// A token reused with different task/specification identity must fail. This
	// permits safe same-attempt redelivery; it does not authorize a new attempt.
	Submit(context.Context, SubmitRequest) (JobRef, error)
	// Observe reconciles the full attempt identity. NotFound alone does not
	// authorize a new token or prove that an accepted external job never ran.
	Observe(context.Context, SubmitRequest) (Observation, error)
	// KnownJob must be checked before cancellation or deletion side effects.
	Cancel(context.Context, SubmitRequest) (Observation, error)
	Cleanup(context.Context, SubmitRequest) error
}
