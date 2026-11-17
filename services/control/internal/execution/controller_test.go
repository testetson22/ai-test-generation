package execution

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/project-lightpath/ai-test-generation/services/control/internal/provider"
)

func TestControllerBlocksSubmissionWhenAdmissionIsDenied(t *testing.T) {
	controller, repository, executionProvider, now := newControllerHarness(t, ActionSubmit)
	controller.admission = AdmissionFunc(func(context.Context, TaskSpec, provider.Identity) error {
		return AdmissionError{Category: "profile_unapproved", Reason: "untrusted reason"}
	})

	worked, err := controller.ProcessNext(context.Background(), "controller-1")
	if !worked || err != nil {
		t.Fatalf("ProcessNext() = (%t, %v), want (true, nil)", worked, err)
	}
	if executionProvider.submitCalls != 0 {
		t.Fatalf("provider submit calls = %d, want 0", executionProvider.submitCalls)
	}
	if repository.blockedCategory != "profile_unapproved" || repository.blockedReason != "dispatch admission denied: profile_unapproved" || !repository.blockedAt.Equal(now) {
		t.Fatalf("blocked claim = (%q, %q, %s), want sanitized admission decision at %s", repository.blockedCategory, repository.blockedReason, repository.blockedAt, now)
	}
}

func TestControllerReconcilesAmbiguousSubmissionWithoutRetry(t *testing.T) {
	controller, repository, executionProvider, now := newControllerHarness(t, ActionSubmit)
	executionProvider.submitErr = &provider.SubmissionError{
		Category: "provider_unavailable", OutcomeUnknown: true, Err: errors.New("connection closed after send"),
	}

	worked, err := controller.ProcessNext(context.Background(), "controller-1")
	if !worked || err != nil {
		t.Fatalf("ProcessNext() = (%t, %v), want (true, nil)", worked, err)
	}
	if executionProvider.submitCalls != 1 || repository.uncertainCalls != 1 {
		t.Fatalf("submit calls = %d, uncertain records = %d, want 1 each", executionProvider.submitCalls, repository.uncertainCalls)
	}
	if repository.retryCalls != 0 || repository.submittedCalls != 0 {
		t.Fatalf("retry calls = %d, submitted records = %d, want 0", repository.retryCalls, repository.submittedCalls)
	}
	if !repository.uncertainAt.Equal(now) || repository.uncertainInterval != controller.config.PollInterval {
		t.Fatalf("uncertain submission schedule = (%s, %s), want (%s, %s)", repository.uncertainAt, repository.uncertainInterval, now, controller.config.PollInterval)
	}
}

func TestControllerAuthorizesRetryAfterFailedProviderObservation(t *testing.T) {
	controller, repository, executionProvider, now := newControllerHarness(t, ActionObserve)
	executionProvider.observation = provider.Observation{
		SubmissionToken: "submission-1", Job: provider.JobRef{ID: "job-1"}, Sequence: 1,
		Status: provider.StatusFailed, FailureCategory: "worker_lost", ObservedAt: now,
	}

	worked, err := controller.ProcessNext(context.Background(), "controller-1")
	if !worked || err != nil {
		t.Fatalf("ProcessNext() = (%t, %v), want (true, nil)", worked, err)
	}
	if executionProvider.observeCalls != 1 || repository.observationCalls != 1 {
		t.Fatalf("observe calls = %d, observation records = %d, want 1 each", executionProvider.observeCalls, repository.observationCalls)
	}
	if repository.retryCalls != 1 || repository.retryTaskID != "task-1" || repository.retryDecisionID == "" || !repository.retryAt.Equal(now) {
		t.Fatalf("retry authorization = (%d, %q, %q, %s), want one task-1 decision at %s", repository.retryCalls, repository.retryTaskID, repository.retryDecisionID, repository.retryAt, now)
	}
}

func TestControllerSchedulesReconciliationWhenObservationFails(t *testing.T) {
	controller, repository, executionProvider, now := newControllerHarness(t, ActionObserve)
	executionProvider.observeErr = errors.New("provider unavailable")

	worked, err := controller.ProcessNext(context.Background(), "controller-1")
	if !worked || err != nil {
		t.Fatalf("ProcessNext() = (%t, %v), want (true, nil)", worked, err)
	}
	if repository.reconciliationCalls != 1 || repository.observationCalls != 0 || repository.retryCalls != 0 {
		t.Fatalf("reconciliation = %d, observations = %d, retries = %d, want (1, 0, 0)", repository.reconciliationCalls, repository.observationCalls, repository.retryCalls)
	}
	if !repository.reconciliationAt.Equal(now) || repository.reconciliationInterval != controller.config.PollInterval {
		t.Fatalf("reconciliation schedule = (%s, %s), want (%s, %s)", repository.reconciliationAt, repository.reconciliationInterval, now, controller.config.PollInterval)
	}
}

func newControllerHarness(t *testing.T, action ClaimAction) (*Controller, *controllerTestRepository, *controllerTestProvider, time.Time) {
	t.Helper()
	now := time.Date(2026, time.November, 17, 19, 0, 0, 0, time.UTC)
	specBytes, err := json.Marshal(validTaskSpec())
	if err != nil {
		t.Fatalf("marshal task spec: %v", err)
	}
	repository := &controllerTestRepository{claim: &Claim{
		TaskID: "task-1", PairID: "pair-1", RunID: "run-1", AttemptID: "attempt-1",
		TaskSpec: specBytes, TaskSpecDigest: DigestTaskSpec(specBytes), SubmissionToken: "submission-1",
		Action: action,
	}}
	executionProvider := &controllerTestProvider{}
	providerID := provider.Identity{Kind: "test-provider", ID: "provider-1", Version: "1"}
	controller, err := NewController(repository, executionProvider, providerID, fixedControllerClock{now}, AdmissionFunc(func(context.Context, TaskSpec, provider.Identity) error {
		return nil
	}), ControllerConfig{ClaimLease: 10 * time.Second, PollInterval: 5 * time.Second})
	if err != nil {
		t.Fatalf("create controller: %v", err)
	}
	return controller, repository, executionProvider, now
}

type fixedControllerClock struct{ now time.Time }

func (c fixedControllerClock) Now() time.Time { return c.now }

type controllerTestRepository struct {
	claim                  *Claim
	blockedCategory        string
	blockedReason          string
	blockedAt              time.Time
	submittedCalls         int
	uncertainCalls         int
	uncertainAt            time.Time
	uncertainInterval      time.Duration
	observationCalls       int
	reconciliationCalls    int
	reconciliationAt       time.Time
	reconciliationInterval time.Duration
	retryCalls             int
	retryTaskID            string
	retryDecisionID        string
	retryAt                time.Time
}

func (r *controllerTestRepository) CreatePair(context.Context, PairInput) (PairRecord, bool, error) {
	return PairRecord{}, false, errors.New("unexpected CreatePair call")
}

func (r *controllerTestRepository) GetPair(context.Context, string) (PairRecord, error) {
	return PairRecord{}, errors.New("unexpected GetPair call")
}

func (r *controllerTestRepository) GetTask(context.Context, string) (TaskRecord, error) {
	return TaskRecord{}, errors.New("unexpected GetTask call")
}

func (r *controllerTestRepository) ScheduleRetryableFailures(context.Context, time.Time) error {
	return nil
}

func (r *controllerTestRepository) ClaimNext(context.Context, string, provider.Identity, time.Time, time.Duration, time.Duration) (*Claim, error) {
	return r.claim, nil
}

func (r *controllerTestRepository) RecordSubmitted(_ context.Context, _ Claim, _ provider.JobRef, _ time.Time, _ time.Duration) error {
	r.submittedCalls++
	return nil
}

func (r *controllerTestRepository) RecordSubmissionUncertain(_ context.Context, _ Claim, now time.Time, interval time.Duration) error {
	r.uncertainCalls++
	r.uncertainAt, r.uncertainInterval = now, interval
	return nil
}

func (r *controllerTestRepository) RecordSubmitFailure(context.Context, Claim, string, time.Time) error {
	return errors.New("unexpected RecordSubmitFailure call")
}

func (r *controllerTestRepository) ScheduleReconciliation(_ context.Context, _ Claim, now time.Time, interval time.Duration) error {
	r.reconciliationCalls++
	r.reconciliationAt, r.reconciliationInterval = now, interval
	return nil
}

func (r *controllerTestRepository) BlockClaim(_ context.Context, _ Claim, category, reason string, now time.Time) error {
	r.blockedCategory, r.blockedReason, r.blockedAt = category, reason, now
	return nil
}

func (r *controllerTestRepository) RecordObservation(context.Context, Claim, provider.Observation, time.Time, time.Duration) error {
	r.observationCalls++
	return nil
}

func (r *controllerTestRepository) AuthorizeRetry(_ context.Context, taskID, decisionID string, now time.Time) (RetryDecision, error) {
	r.retryCalls++
	r.retryTaskID, r.retryDecisionID, r.retryAt = taskID, decisionID, now
	return RetryDecision{}, nil
}

func (r *controllerTestRepository) RequestCancel(context.Context, string, int64, time.Time) (TaskRecord, error) {
	return TaskRecord{}, errors.New("unexpected RequestCancel call")
}

type controllerTestProvider struct {
	submitCalls  int
	observeCalls int
	submitErr    error
	observeErr   error
	observation  provider.Observation
}

func (p *controllerTestProvider) Submit(context.Context, provider.SubmitRequest) (provider.JobRef, error) {
	p.submitCalls++
	if p.submitErr != nil {
		return provider.JobRef{}, p.submitErr
	}
	return provider.JobRef{ID: "job-1"}, nil
}

func (p *controllerTestProvider) Observe(context.Context, string) (provider.Observation, error) {
	p.observeCalls++
	return p.observation, p.observeErr
}

func (p *controllerTestProvider) Cancel(context.Context, string) (provider.Observation, error) {
	return provider.Observation{}, errors.New("unexpected Cancel call")
}

func (p *controllerTestProvider) Cleanup(context.Context, string) error { return nil }
