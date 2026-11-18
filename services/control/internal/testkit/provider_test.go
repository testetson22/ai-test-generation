package testkit

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/project-lightpath/ai-test-generation/services/control/internal/provider"
)

func TestSubmissionAckLossCanBeReconciledWithoutDuplicateLaunch(t *testing.T) {
	clock := NewManualClock(time.Date(2026, time.November, 17, 19, 0, 0, 0, time.UTC))
	jobs := NewStatefulProvider(clock)
	request := validSubmitRequest("token-1", "task-1", "attempt-1")
	jobs.LoseNextSubmitAcknowledgement()

	if _, err := jobs.Submit(context.Background(), request); !errors.Is(err, provider.ErrSubmissionAckLost) {
		t.Fatalf("submit error = %v, want acknowledgement loss", err)
	}
	observed, err := jobs.Observe(context.Background(), request)
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

	request.TaskSpecDigest = testDigest("b")
	if _, err := jobs.Submit(context.Background(), request); !errors.Is(err, provider.ErrAttemptConflict) {
		t.Fatalf("conflicting token error = %v, want conflict", err)
	}
}

func TestCancellationAndCleanupHaveSeparateAcknowledgements(t *testing.T) {
	jobs := NewStatefulProvider(NewManualClock(time.Date(2026, time.November, 17, 19, 0, 0, 0, time.UTC)))
	request := validSubmitRequest("token-2", "task-2", "attempt-2")
	if _, err := jobs.Submit(context.Background(), request); err != nil {
		t.Fatalf("submit: %v", err)
	}

	jobs.DeferNextTermination()
	observation, err := jobs.Cancel(context.Background(), request)
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
	if err := jobs.Cleanup(context.Background(), request); !errors.Is(err, ErrCleanupInjected) {
		t.Fatalf("first cleanup error = %v, want injected failure", err)
	}
	if jobs.WasCleaned(request.SubmissionToken) {
		t.Fatal("failed cleanup was recorded as complete")
	}
	if err := jobs.Cleanup(context.Background(), request); err != nil {
		t.Fatalf("retry cleanup: %v", err)
	}
	if !jobs.WasCleaned(request.SubmissionToken) {
		t.Fatal("successful cleanup was not recorded")
	}
	observation, err = jobs.Observe(context.Background(), request)
	if err != nil || observation.Status != provider.StatusCancelled {
		t.Fatalf("job evidence after cleanup = %#v, %v; want retained cancellation observation", observation, err)
	}
}

func TestProviderStateCanOutliveControllerReconstruction(t *testing.T) {
	jobs := NewStatefulProvider(NewManualClock(time.Date(2026, time.November, 17, 19, 0, 0, 0, time.UTC)))
	request := validSubmitRequest("token-3", "task-3", "attempt-3")
	if _, err := jobs.Submit(context.Background(), request); err != nil {
		t.Fatalf("submit: %v", err)
	}

	// A controller restart reuses the independent provider simulator instance.
	restartedControllerView := jobs
	observation, err := restartedControllerView.Observe(context.Background(), request)
	if err != nil || observation.Status != provider.StatusPending || restartedControllerView.LaunchCount() != 1 {
		t.Fatalf("reconciled observation = %#v, err = %v, launches = %d", observation, err, restartedControllerView.LaunchCount())
	}
}

func validSubmitRequest(token, taskID, attemptID string) provider.SubmitRequest {
	return provider.SubmitRequest{
		SubmissionToken: token, TaskID: taskID, AttemptID: attemptID,
		TaskSpecDigest: testDigest("a"), ProfileID: "profile-1", ProfileVersion: "1.0.0", ProfileDigest: testDigest("c"),
	}
}

func testDigest(char string) string { return "sha256:" + strings.Repeat(char, 64) }
