package testkit

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/project-lightpath/ai-test-generation/services/control/internal/provider"
)

func TestSubmissionAckLossCanBeReconciledWithoutDuplicateLaunch(t *testing.T) {
	clock := NewManualClock(time.Date(2026, time.November, 17, 19, 0, 0, 0, time.UTC))
	jobs := NewStatefulProvider(clock)
	request := provider.SubmitRequest{SubmissionToken: "token-1", TaskID: "task-1", TaskSpecDigest: "sha256:spec"}
	jobs.LoseNextSubmitAcknowledgement()

	if _, err := jobs.Submit(context.Background(), request); !errors.Is(err, provider.ErrSubmissionAckLost) {
		t.Fatalf("submit error = %v, want acknowledgement loss", err)
	}
	observed, err := jobs.Observe(context.Background(), request.SubmissionToken)
	if err != nil {
		t.Fatalf("reconcile accepted job: %v", err)
	}
	if observed.Status != provider.StatusPending || observed.Job.ID == "" {
		t.Fatalf("observation = %#v, want accepted pending job", observed)
	}
	job, err := jobs.Submit(context.Background(), request)
	if err != nil {
		t.Fatalf("idempotent resubmit: %v", err)
	}
	if job != observed.Job || jobs.LaunchCount() != 1 {
		t.Fatalf("resubmission job = %#v, launches = %d; want same job and one launch", job, jobs.LaunchCount())
	}

	request.TaskSpecDigest = "sha256:different"
	if _, err := jobs.Submit(context.Background(), request); !errors.Is(err, provider.ErrAttemptConflict) {
		t.Fatalf("conflicting token error = %v, want conflict", err)
	}
}

func TestCancellationAndCleanupHaveSeparateAcknowledgements(t *testing.T) {
	jobs := NewStatefulProvider(NewManualClock(time.Date(2026, time.November, 17, 19, 0, 0, 0, time.UTC)))
	request := provider.SubmitRequest{SubmissionToken: "token-2", TaskID: "task-2", TaskSpecDigest: "sha256:spec"}
	if _, err := jobs.Submit(context.Background(), request); err != nil {
		t.Fatalf("submit: %v", err)
	}

	jobs.DeferNextTermination()
	observation, err := jobs.Cancel(context.Background(), request.SubmissionToken)
	if err != nil {
		t.Fatalf("request cancellation: %v", err)
	}
	if observation.Status != provider.StatusCancelRequested || observation.TerminationConfirmed {
		t.Fatalf("cancel observation = %#v, want request without termination proof", observation)
	}
	observation, err = jobs.ConfirmTermination(request.SubmissionToken)
	if err != nil {
		t.Fatalf("confirm termination: %v", err)
	}
	if observation.Status != provider.StatusCancelled || !observation.TerminationConfirmed {
		t.Fatalf("termination observation = %#v, want confirmed cancellation", observation)
	}

	jobs.FailNextCleanup()
	if err := jobs.Cleanup(context.Background(), request.SubmissionToken); !errors.Is(err, ErrCleanupInjected) {
		t.Fatalf("first cleanup error = %v, want injected failure", err)
	}
	if jobs.WasCleaned(request.SubmissionToken) {
		t.Fatal("failed cleanup was recorded as complete")
	}
	if err := jobs.Cleanup(context.Background(), request.SubmissionToken); err != nil {
		t.Fatalf("retry cleanup: %v", err)
	}
	if !jobs.WasCleaned(request.SubmissionToken) {
		t.Fatal("successful cleanup was not recorded")
	}
	observation, err = jobs.Observe(context.Background(), request.SubmissionToken)
	if err != nil || observation.Status != provider.StatusCancelled {
		t.Fatalf("job evidence after cleanup = %#v, %v; want retained cancellation observation", observation, err)
	}
}

func TestProviderStateCanOutliveControllerReconstruction(t *testing.T) {
	jobs := NewStatefulProvider(NewManualClock(time.Date(2026, time.November, 17, 19, 0, 0, 0, time.UTC)))
	request := provider.SubmitRequest{SubmissionToken: "token-3", TaskID: "task-3", TaskSpecDigest: "sha256:spec"}
	if _, err := jobs.Submit(context.Background(), request); err != nil {
		t.Fatalf("submit: %v", err)
	}

	// A controller restart reuses the independent provider simulator instance.
	restartedControllerView := jobs
	observation, err := restartedControllerView.Observe(context.Background(), request.SubmissionToken)
	if err != nil || observation.Status != provider.StatusPending || restartedControllerView.LaunchCount() != 1 {
		t.Fatalf("reconciled observation = %#v, err = %v, launches = %d", observation, err, restartedControllerView.LaunchCount())
	}
}
