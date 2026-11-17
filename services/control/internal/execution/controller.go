package execution

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/project-lightpath/ai-test-generation/services/control/internal/provider"
)

type Admission interface {
	Check(context.Context, TaskSpec, provider.Identity) error
}

type AdmissionFunc func(context.Context, TaskSpec, provider.Identity) error

func (f AdmissionFunc) Check(ctx context.Context, spec TaskSpec, identity provider.Identity) error {
	return f(ctx, spec, identity)
}

type AdmissionError struct {
	Category string
	Reason   string
}

func (e AdmissionError) Error() string {
	return e.Reason
}

type ControllerConfig struct {
	ClaimLease   time.Duration
	PollInterval time.Duration
}

type Controller struct {
	repository Repository
	provider   provider.Provider
	providerID provider.Identity
	clock      Clock
	admission  Admission
	config     ControllerConfig
}

func NewController(repository Repository, executionProvider provider.Provider, providerID provider.Identity, clock Clock, admission Admission, config ControllerConfig) (*Controller, error) {
	if repository == nil || executionProvider == nil || providerID.Kind == "" || providerID.ID == "" || admission == nil {
		return nil, fmt.Errorf("repository, provider identity, provider adapter, and admission checker are required")
	}
	if config.ClaimLease <= 0 || config.PollInterval <= 0 {
		return nil, fmt.Errorf("claim lease and provider poll interval must be positive")
	}
	if clock == nil {
		clock = SystemClock()
	}
	return &Controller{repository: repository, provider: executionProvider, providerID: providerID, clock: clock, admission: admission, config: config}, nil
}

// ProcessNext claims and performs at most one provider operation. The claim and
// attempt have already committed before any external side effect occurs.
func (c *Controller) ProcessNext(ctx context.Context, holder string) (bool, error) {
	now := c.clock.Now().UTC()
	if err := c.repository.ScheduleRetryableFailures(ctx, now); err != nil {
		return false, err
	}
	claim, err := c.repository.ClaimNext(ctx, holder, c.providerID, now, c.config.ClaimLease, c.config.PollInterval)
	if err != nil {
		return false, err
	}
	if claim == nil {
		return false, nil
	}
	switch claim.Action {
	case ActionSubmit:
		return true, c.submit(ctx, *claim, now)
	case ActionObserve:
		observation, err := c.provider.Observe(ctx, claim.SubmissionToken)
		if err != nil {
			return true, c.repository.ScheduleReconciliation(ctx, *claim, now, c.config.PollInterval)
		}
		return true, c.recordObservation(ctx, *claim, observation, now)
	case ActionCancel:
		observation, err := c.provider.Cancel(ctx, claim.SubmissionToken)
		if err != nil {
			return true, c.repository.ScheduleReconciliation(ctx, *claim, now, c.config.PollInterval)
		}
		return true, c.recordObservation(ctx, *claim, observation, now)
	default:
		return true, fmt.Errorf("unsupported execution claim action %q", claim.Action)
	}
}

func (c *Controller) submit(ctx context.Context, claim Claim, now time.Time) error {
	spec, err := DecodeTaskSpec(claim.TaskSpec)
	if err != nil {
		return c.repository.BlockClaim(ctx, claim, "invalid_input", "stored task specification is invalid", now)
	}
	if err := c.admission.Check(ctx, spec, c.providerID); err != nil {
		var admissionErr AdmissionError
		if errors.As(err, &admissionErr) && validAdmissionFailure(admissionErr.Category) {
			return c.repository.BlockClaim(ctx, claim, admissionErr.Category, "dispatch admission denied: "+admissionErr.Category, now)
		}
		return c.repository.BlockClaim(ctx, claim, "capability_unavailable", "dispatch prerequisites are not satisfied", now)
	}
	job, err := c.provider.Submit(ctx, provider.SubmitRequest{
		SubmissionToken: claim.SubmissionToken,
		TaskID:          claim.TaskID,
		TaskSpecDigest:  claim.TaskSpecDigest,
	})
	if err != nil {
		if errors.Is(err, provider.ErrAttemptConflict) {
			return c.repository.RecordSubmitFailure(ctx, claim, "invalid_input", now)
		}
		var submitErr *provider.SubmissionError
		if errors.As(err, &submitErr) && !submitErr.OutcomeUnknown && validSubmissionFailure(submitErr.Category) {
			if storeErr := c.repository.RecordSubmitFailure(ctx, claim, submitErr.Category, now); storeErr != nil {
				return storeErr
			}
			return c.authorizeRetryIfEligible(ctx, claim.TaskID, now)
		}
		return c.repository.RecordSubmissionUncertain(ctx, claim, now, c.config.PollInterval)
	}
	if job.ID == "" {
		return c.repository.RecordSubmissionUncertain(ctx, claim, now, c.config.PollInterval)
	}
	return c.repository.RecordSubmitted(ctx, claim, job, now, c.config.PollInterval)
}

func (c *Controller) recordObservation(ctx context.Context, claim Claim, observation provider.Observation, receivedAt time.Time) error {
	if err := c.repository.RecordObservation(ctx, claim, observation, receivedAt, c.config.PollInterval); err != nil {
		return err
	}
	if observation.Status == provider.StatusFailed {
		return c.authorizeRetryIfEligible(ctx, claim.TaskID, receivedAt)
	}
	return nil
}

func (c *Controller) authorizeRetryIfEligible(ctx context.Context, taskID string, now time.Time) error {
	decisionID, err := newDecisionID()
	if err != nil {
		return err
	}
	_, err = c.repository.AuthorizeRetry(ctx, taskID, decisionID, now)
	if errors.Is(err, ErrRetryNotAuthorized) || errors.Is(err, ErrDeadlineExceeded) {
		return nil
	}
	return err
}

func newDecisionID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate retry decision identity: %w", err)
	}
	return "retry-" + hex.EncodeToString(raw[:]), nil
}

func validAdmissionFailure(category string) bool {
	switch category {
	case "unauthorized", "profile_unapproved", "capability_unavailable":
		return true
	default:
		return false
	}
}

func validSubmissionFailure(category string) bool {
	switch category {
	case "invalid_input", "unauthorized", "profile_unapproved", "capability_unavailable", "provider_unavailable":
		return true
	default:
		return false
	}
}
