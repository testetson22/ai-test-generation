package postgres

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/project-lightpath/ai-test-generation/services/control/internal/execution"
	"github.com/project-lightpath/ai-test-generation/services/control/internal/provider"
	"github.com/project-lightpath/ai-test-generation/services/control/internal/testkit"
)

func TestPostgresPairCreationClaimFencingAndObservationOrdering(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	input := pairInput(t, now, "same_subject_control", []string{"worker_lost"})

	pair, created, err := store.CreatePair(ctx, input)
	if err != nil || !created {
		t.Fatalf("create pair = %#v, created %t, err %v", pair, created, err)
	}
	cleanupPair(t, store, pair.PairID)
	if _, err := store.RequestCancel(ctx, pair.CandidateTaskID, 1, now); err != nil {
		t.Fatalf("defer unused candidate task: %v", err)
	}
	duplicate, created, err := store.CreatePair(ctx, input)
	if err != nil || created || duplicate.PairID != pair.PairID {
		t.Fatalf("idempotent create = %#v, created %t, err %v", duplicate, created, err)
	}

	changed := input
	base, candidate := decodeInputSpecs(t, changed)
	base.ExpectedTestIDs = []string{"fixture.SmokeTest#different"}
	candidate.ExpectedTestIDs = []string{"fixture.SmokeTest#different"}
	changed.BaseSpec, changed.CandidateSpec = marshalSpecs(t, base, candidate)
	if _, _, err := store.CreatePair(ctx, changed); !errors.Is(err, execution.ErrIdempotencyConflict) {
		t.Fatalf("changed idempotent request error = %v, want conflict", err)
	}

	baseTask, err := store.GetTask(ctx, pair.BaseTaskID)
	if err != nil || baseTask.Status != "queued" || baseTask.Revision != 1 {
		t.Fatalf("new task = %#v, err %v; want revision-1 queued task", baseTask, err)
	}

	providerID := provider.Identity{Kind: "test-provider", ID: "provider-1", Version: "1"}
	first, err := store.ClaimNext(ctx, "controller-a", providerID, now, 10*time.Second, 5*time.Second)
	if err != nil || first == nil || first.Action != execution.ActionSubmit {
		t.Fatalf("first claim = %#v, err %v", first, err)
	}
	if _, err := store.ClaimNext(ctx, "controller-b", providerID, now.Add(time.Second), 10*time.Second, 5*time.Second); err != nil {
		t.Fatalf("second controller before lease: %v", err)
	}
	reclaimed, err := store.ClaimNext(ctx, "controller-b", providerID, now.Add(11*time.Second), 10*time.Second, 5*time.Second)
	if err != nil || reclaimed == nil || reclaimed.AttemptID != first.AttemptID || reclaimed.SubmissionToken != first.SubmissionToken || reclaimed.FencingToken != first.FencingToken+1 {
		t.Fatalf("reclaimed claim = %#v, err %v", reclaimed, err)
	}
	if err := store.RecordSubmitted(ctx, *first, provider.JobRef{ID: "stale-job"}, now.Add(11*time.Second), 5*time.Second); !errors.Is(err, execution.ErrStaleClaim) {
		t.Fatalf("stale claim acknowledgement error = %v, want fenced rejection", err)
	}
	if err := store.RecordSubmitted(ctx, *reclaimed, provider.JobRef{ID: "job-1"}, now.Add(11*time.Second), 5*time.Second); err != nil {
		t.Fatalf("record accepted submission: %v", err)
	}
	if err := store.RecordSubmitted(ctx, *reclaimed, provider.JobRef{ID: "job-1"}, now.Add(12*time.Second), 5*time.Second); err != nil {
		t.Fatalf("idempotent duplicate submission acknowledgement: %v", err)
	}

	firstPoll, err := store.ClaimNext(ctx, "controller-a", providerID, now.Add(17*time.Second), 10*time.Second, 5*time.Second)
	if err != nil || firstPoll == nil || firstPoll.Action != execution.ActionObserve {
		t.Fatalf("poll claim = %#v, err %v", firstPoll, err)
	}
	pendingObservation := provider.Observation{
		SubmissionToken: firstPoll.SubmissionToken, Job: provider.JobRef{ID: "job-1"},
		Sequence: 1, Status: provider.StatusPending, ObservedAt: now.Add(17 * time.Second),
	}
	if err := store.RecordObservation(ctx, *firstPoll, pendingObservation, now.Add(17*time.Second), 5*time.Second); err != nil {
		t.Fatalf("record pending observation: %v", err)
	}

	runningPoll, err := store.ClaimNext(ctx, "controller-a", providerID, now.Add(23*time.Second), 10*time.Second, 5*time.Second)
	if err != nil || runningPoll == nil {
		t.Fatalf("running poll claim = %#v, err %v", runningPoll, err)
	}
	runningObservation := provider.Observation{
		SubmissionToken: runningPoll.SubmissionToken, Job: provider.JobRef{ID: "job-1"},
		Sequence: 2, Status: provider.StatusRunning, ObservedAt: now.Add(23 * time.Second),
	}
	if err := store.RecordObservation(ctx, *runningPoll, runningObservation, now.Add(23*time.Second), 5*time.Second); err != nil {
		t.Fatalf("record running observation: %v", err)
	}
	if err := store.RecordObservation(ctx, *runningPoll, runningObservation, now.Add(24*time.Second), 5*time.Second); err != nil {
		t.Fatalf("idempotent duplicate observation: %v", err)
	}
	lateCompletion := runningObservation
	lateCompletion.Sequence = 3
	lateCompletion.Status = provider.StatusCompleted
	if err := store.RecordObservation(ctx, *runningPoll, lateCompletion, now.Add(24*time.Second), 5*time.Second); err != nil {
		t.Fatalf("persist late completion observation: %v", err)
	}
	baseTask, err = store.GetTask(ctx, pair.BaseTaskID)
	if err != nil || baseTask.Status != "running" {
		t.Fatalf("late completion advanced task = %#v, err %v; want running", baseTask, err)
	}

	completedPoll, err := store.ClaimNext(ctx, "controller-a", providerID, now.Add(29*time.Second), 10*time.Second, 5*time.Second)
	if err != nil || completedPoll == nil {
		t.Fatalf("completion poll claim = %#v, err %v", completedPoll, err)
	}
	completed := provider.Observation{
		SubmissionToken: completedPoll.SubmissionToken, Job: provider.JobRef{ID: "job-1"},
		Sequence: 4, Status: provider.StatusCompleted, ObservedAt: now.Add(29 * time.Second),
	}
	if err := store.RecordObservation(ctx, *completedPoll, completed, now.Add(29*time.Second), 5*time.Second); err != nil {
		t.Fatalf("record completed observation: %v", err)
	}
	late := pendingObservation
	if err := store.RecordObservation(ctx, *completedPoll, late, now.Add(30*time.Second), 5*time.Second); err != nil {
		t.Fatalf("retain late observation: %v", err)
	}
	conflicting := completed
	conflicting.Status = provider.StatusFailed
	conflicting.FailureCategory = "worker_lost"
	if err := store.RecordObservation(ctx, *completedPoll, conflicting, now.Add(31*time.Second), 5*time.Second); !errors.Is(err, provider.ErrObservationConflict) {
		t.Fatalf("conflicting sequence error = %v, want conflict", err)
	}
	baseTask, err = store.GetTask(ctx, pair.BaseTaskID)
	if err != nil || baseTask.Status != "collecting" {
		t.Fatalf("completed operational task = %#v, err %v; want collecting, not passing", baseTask, err)
	}
}

func TestPostgresControllerReconcilesLostSubmissionAndAuthorizesOnlyPolicyRetries(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	input := pairInput(t, now, "same_subject_control", []string{"worker_lost"})
	pair, _, err := store.CreatePair(ctx, input)
	if err != nil {
		t.Fatalf("create execution pair: %v", err)
	}
	cleanupPair(t, store, pair.PairID)
	if _, err := store.RequestCancel(ctx, pair.CandidateTaskID, 1, now); err != nil {
		t.Fatalf("defer unused candidate task: %v", err)
	}
	clock := testkit.NewManualClock(now)
	worker := testkit.NewStatefulProvider(clock)
	worker.LoseNextSubmitAcknowledgement()
	providerID := provider.Identity{Kind: "test-provider", ID: "controller-recovery-test", Version: "1"}
	controller, err := execution.NewController(store, worker, providerID, clock, execution.AdmissionFunc(func(context.Context, execution.TaskSpec, provider.Identity) error { return nil }), execution.ControllerConfig{ClaimLease: 10 * time.Second, PollInterval: 5 * time.Second})
	if err != nil {
		t.Fatalf("create controller: %v", err)
	}
	worked, err := controller.ProcessNext(ctx, "controller-1")
	if err != nil || !worked || worker.LaunchCount() != 1 {
		t.Fatalf("process initial submit worked=%t err=%v launches=%d", worked, err, worker.LaunchCount())
	}
	task, err := store.GetTask(ctx, pair.BaseTaskID)
	if err != nil || task.Status != "outcome_unknown" {
		t.Fatalf("lost-ack task = %#v, err %v; want outcome_unknown", task, err)
	}
	if err := clock.Advance(6 * time.Second); err != nil {
		t.Fatal(err)
	}
	worked, err = controller.ProcessNext(ctx, "controller-1-restarted")
	if err != nil || !worked || worker.LaunchCount() != 1 {
		t.Fatalf("reconcile after restart worked=%t err=%v launches=%d", worked, err, worker.LaunchCount())
	}
	task, err = store.GetTask(ctx, pair.BaseTaskID)
	if err != nil || task.Status != "submitted" || task.AttemptCount != 1 {
		t.Fatalf("reconciled task = %#v, err %v", task, err)
	}

	var submissionToken string
	if err := store.db.QueryRowContext(ctx, `SELECT submission_token FROM execution_attempts WHERE attempt_id = $1`, task.ActiveAttemptID).Scan(&submissionToken); err != nil {
		t.Fatalf("load active attempt token: %v", err)
	}
	if _, err := worker.SetStatus(submissionToken, provider.StatusFailed, "worker_lost"); err != nil {
		t.Fatalf("set provider failure: %v", err)
	}
	if err := clock.Advance(6 * time.Second); err != nil {
		t.Fatal(err)
	}
	worked, err = controller.ProcessNext(ctx, "controller-1")
	if err != nil || !worked {
		t.Fatalf("process retryable failure worked=%t err=%v", worked, err)
	}
	task, err = store.GetTask(ctx, pair.BaseTaskID)
	if err != nil || task.Status != "eligible" || task.AttemptCount != 1 {
		t.Fatalf("retry-authorized task = %#v, err %v", task, err)
	}
	if err := clock.Advance(11 * time.Second); err != nil {
		t.Fatal(err)
	}
	worked, err = controller.ProcessNext(ctx, "controller-1")
	if err != nil || !worked || worker.LaunchCount() != 2 {
		t.Fatalf("process authorized attempt worked=%t err=%v launches=%d", worked, err, worker.LaunchCount())
	}
	task, err = store.GetTask(ctx, pair.BaseTaskID)
	if err != nil || task.AttemptCount != 2 || task.Status != "submitted" {
		t.Fatalf("second attempt task = %#v, err %v", task, err)
	}
	var retryBytes []byte
	if err := store.db.QueryRowContext(ctx, `SELECT retry_authorization_bytes FROM execution_attempts WHERE task_id = $1 AND attempt_number = 2`, task.TaskID).Scan(&retryBytes); err != nil || len(retryBytes) == 0 {
		t.Fatalf("second attempt retry authorization bytes=%q err=%v", retryBytes, err)
	}
}

func TestPostgresAdmissionRevocationBlocksBeforeProviderSubmission(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	input := pairInput(t, now, "same_subject_control", []string{"worker_lost"})
	pair, _, err := store.CreatePair(ctx, input)
	if err != nil {
		t.Fatalf("create execution pair: %v", err)
	}
	cleanupPair(t, store, pair.PairID)
	if _, err := store.RequestCancel(ctx, pair.CandidateTaskID, 1, now); err != nil {
		t.Fatalf("defer unused candidate task: %v", err)
	}
	clock := testkit.NewManualClock(now)
	worker := testkit.NewStatefulProvider(clock)
	providerID := provider.Identity{Kind: "test-provider", ID: "admission-test", Version: "1"}
	controller, err := execution.NewController(store, worker, providerID, clock, execution.AdmissionFunc(func(context.Context, execution.TaskSpec, provider.Identity) error {
		return execution.AdmissionError{Category: "profile_unapproved", Reason: "should not be persisted verbatim"}
	}), execution.ControllerConfig{ClaimLease: 10 * time.Second, PollInterval: 5 * time.Second})
	if err != nil {
		t.Fatalf("create controller: %v", err)
	}
	worked, err := controller.ProcessNext(ctx, "controller-admission")
	if err != nil || !worked || worker.LaunchCount() != 0 {
		t.Fatalf("blocked admission worked=%t err=%v provider launches=%d", worked, err, worker.LaunchCount())
	}
	task, err := store.GetTask(ctx, pair.BaseTaskID)
	if err != nil || task.Status != "blocked" || task.TerminalReason != "dispatch admission denied: profile_unapproved" {
		t.Fatalf("admission-blocked task = %#v, err %v", task, err)
	}
}

func TestPostgresConcurrentClaimsDoNotCreateDuplicateAttempts(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	input := pairInput(t, now, "same_subject_control", []string{"worker_lost"})
	pair, _, err := store.CreatePair(ctx, input)
	if err != nil {
		t.Fatalf("create execution pair: %v", err)
	}
	cleanupPair(t, store, pair.PairID)
	if _, err := store.RequestCancel(ctx, pair.CandidateTaskID, 1, now); err != nil {
		t.Fatalf("defer unused candidate task: %v", err)
	}
	providerID := provider.Identity{Kind: "test-provider", ID: "concurrency-test", Version: "1"}
	type claimResult struct {
		claim *execution.Claim
		err   error
	}
	results := make(chan claimResult, 8)
	for i := 0; i < cap(results); i++ {
		go func(i int) {
			claim, err := store.ClaimNext(ctx, "controller-"+strings.Repeat("x", i+1), providerID, now, time.Minute, time.Second)
			results <- claimResult{claim: claim, err: err}
		}(i)
	}
	claims := 0
	for i := 0; i < cap(results); i++ {
		result := <-results
		if result.err != nil {
			t.Fatalf("concurrent claim: %v", result.err)
		}
		if result.claim != nil {
			claims++
		}
	}
	if claims != 1 {
		t.Fatalf("concurrent claims = %d, want exactly one claim for the queued task", claims)
	}
}

func TestPostgresNotFoundObservationDoesNotAuthorizeAnotherAttempt(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	input := pairInput(t, now, "same_subject_control", []string{"worker_lost"})
	pair, _, err := store.CreatePair(ctx, input)
	if err != nil {
		t.Fatalf("create execution pair: %v", err)
	}
	cleanupPair(t, store, pair.PairID)
	if _, err := store.RequestCancel(ctx, pair.CandidateTaskID, 1, now); err != nil {
		t.Fatalf("defer unused candidate task: %v", err)
	}
	providerID := provider.Identity{Kind: "test-provider", ID: "not-found-test", Version: "1"}
	claim, err := store.ClaimNext(ctx, "controller-1", providerID, now, 10*time.Second, 5*time.Second)
	if err != nil || claim == nil || claim.Action != execution.ActionSubmit {
		t.Fatalf("initial claim = %#v, err %v", claim, err)
	}
	if err := store.RecordSubmissionUncertain(ctx, *claim, now, 5*time.Second); err != nil {
		t.Fatalf("record uncertain submission: %v", err)
	}
	reconcile, err := store.ClaimNext(ctx, "controller-2", providerID, now.Add(6*time.Second), 10*time.Second, 5*time.Second)
	if err != nil || reconcile == nil || reconcile.Action != execution.ActionObserve {
		t.Fatalf("reconciliation claim = %#v, err %v", reconcile, err)
	}
	if err := store.RecordObservation(ctx, *reconcile, provider.Observation{
		SubmissionToken: reconcile.SubmissionToken,
		Sequence:        1, Status: provider.StatusNotFound, ObservedAt: now.Add(6 * time.Second),
	}, now.Add(6*time.Second), 5*time.Second); err != nil {
		t.Fatalf("record provider not-found observation: %v", err)
	}
	secondReconcile, err := store.ClaimNext(ctx, "controller-3", providerID, now.Add(12*time.Second), 10*time.Second, 5*time.Second)
	if err != nil || secondReconcile == nil || secondReconcile.Action != execution.ActionObserve || secondReconcile.AttemptNumber != 1 || secondReconcile.SubmissionToken != claim.SubmissionToken {
		t.Fatalf("not-found recovery action = %#v, err %v; want same-attempt reconciliation", secondReconcile, err)
	}
}

func TestPostgresRetrySchedulerRecoversFailureAfterControllerRestart(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	input := pairInput(t, now, "same_subject_control", []string{"provider_unavailable"})
	pair, _, err := store.CreatePair(ctx, input)
	if err != nil {
		t.Fatalf("create execution pair: %v", err)
	}
	cleanupPair(t, store, pair.PairID)
	if _, err := store.RequestCancel(ctx, pair.CandidateTaskID, 1, now); err != nil {
		t.Fatalf("defer unused candidate task: %v", err)
	}
	providerID := provider.Identity{Kind: "test-provider", ID: "retry-restart-test", Version: "1"}
	claim, err := store.ClaimNext(ctx, "controller-before-crash", providerID, now, time.Minute, time.Second)
	if err != nil || claim == nil {
		t.Fatalf("claim before failure: %#v, %v", claim, err)
	}
	if err := store.RecordSubmitFailure(ctx, *claim, "provider_unavailable", now); err != nil {
		t.Fatalf("record provider failure before controller crash: %v", err)
	}

	// A new store/controller process recovers the persisted failure without
	// relying on the previous process's in-memory callback continuation.
	restarted := integrationStore(t)
	if err := restarted.ScheduleRetryableFailures(ctx, now); err != nil {
		t.Fatalf("schedule retry after restart: %v", err)
	}
	task, err := restarted.GetTask(ctx, pair.BaseTaskID)
	if err != nil || task.Status != "eligible" || task.AttemptCount != 1 {
		t.Fatalf("recovered retry state = %#v, err %v", task, err)
	}
	if err := restarted.ScheduleRetryableFailures(ctx, now); err != nil {
		t.Fatalf("repeat retry reconciliation: %v", err)
	}
	var decisions int
	if err := restarted.db.QueryRowContext(ctx, `SELECT count(*) FROM execution_retry_decisions WHERE task_id = $1`, pair.BaseTaskID).Scan(&decisions); err != nil || decisions != 1 {
		t.Fatalf("retry decisions = %d, err %v; want one durable authorization", decisions, err)
	}
}

func integrationStore(t *testing.T) *Store {
	t.Helper()
	databaseURL := os.Getenv("ATG_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("ATG_TEST_DATABASE_URL is not configured; PostgreSQL integration test skipped")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	store, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open integration PostgreSQL store: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close integration store: %v", err)
		}
	})
	return store
}

func cleanupPair(t *testing.T, store *Store, pairID string) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		_, err := store.db.ExecContext(ctx, `
			DELETE FROM execution_provider_observations
			WHERE attempt_id IN (SELECT attempt_id FROM execution_attempts WHERE task_id IN (SELECT task_id FROM execution_tasks WHERE pair_id = $1))`, pairID)
		if err != nil {
			t.Errorf("delete pair observations: %v", err)
		}
		_, err = store.db.ExecContext(ctx, `
			DELETE FROM execution_retry_decisions
			WHERE task_id IN (SELECT task_id FROM execution_tasks WHERE pair_id = $1)`, pairID)
		if err != nil {
			t.Errorf("delete pair retry decisions: %v", err)
		}
		_, err = store.db.ExecContext(ctx, `
			DELETE FROM execution_task_events
			WHERE task_id IN (SELECT task_id FROM execution_tasks WHERE pair_id = $1)`, pairID)
		if err != nil {
			t.Errorf("delete pair task events: %v", err)
		}
		_, err = store.db.ExecContext(ctx, `
			DELETE FROM execution_attempts
			WHERE task_id IN (SELECT task_id FROM execution_tasks WHERE pair_id = $1)`, pairID)
		if err != nil {
			t.Errorf("delete pair attempts: %v", err)
		}
		_, err = store.db.ExecContext(ctx, `DELETE FROM execution_tasks WHERE pair_id = $1`, pairID)
		if err != nil {
			t.Errorf("delete pair tasks: %v", err)
		}
		if _, err := store.db.ExecContext(ctx, `DELETE FROM execution_pairs WHERE pair_id = $1`, pairID); err != nil {
			t.Errorf("delete pair: %v", err)
		}
	})
}

func pairInput(t *testing.T, now time.Time, comparisonMode string, retryFailures []string) execution.PairInput {
	t.Helper()
	pairID := testID(t, "pair")
	runID := testID(t, "run")
	generationID := testID(t, "generation")
	reviewer := execution.Reviewer{
		Identity: execution.Identity{Kind: "person", ID: testID(t, "reviewer")},
		Role:     "execution-reviewer", AuthorizationRef: testID(t, "authorization"),
	}
	base := execution.TaskSpec{
		Contract: "atg.execution_task_spec", Version: "1.0",
		TaskSpecID: testID(t, "task-base"), RunID: runID, ExpectedRunRevision: 1, PairID: pairID, GenerationID: generationID,
		SubjectRole: "base", ComparisonMode: comparisonMode,
		Subject: execution.Subject{
			Source: execution.Source{
				Identity: execution.Identity{Kind: "git-commit", ID: testID(t, "commit-base"), Digest: "sha256:" + strings.Repeat("1", 64)},
				Locator:  "https://example.invalid/fixture.git",
			},
			Identity: execution.Identity{Kind: "source-tree", ID: testID(t, "tree-base"), Digest: "sha256:" + strings.Repeat("2", 64)},
		},
		Generation: execution.GenerationRefs{
			Manifest: execution.Artifact{ArtifactID: testID(t, "manifest"), MediaType: "application/vnd.atg.generation+json", Digest: "sha256:" + strings.Repeat("3", 64)},
			Patch:    execution.Artifact{ArtifactID: testID(t, "patch"), MediaType: "text/x-diff", Digest: "sha256:" + strings.Repeat("4", 64)},
			Tests:    execution.Artifact{ArtifactID: testID(t, "tests"), MediaType: "application/java-archive", Digest: "sha256:" + strings.Repeat("5", 64)},
		},
		CodeApproval: execution.CodeApproval{
			CheckpointID: testID(t, "checkpoint"), DecisionID: testID(t, "decision"), Reviewer: reviewer,
			ReviewedGenerationDigest: "sha256:" + strings.Repeat("3", 64),
		},
		Profile: execution.ProfileRef{
			ProfileID: testID(t, "profile"), ProfileVersion: "1.0.0",
			Artifact: execution.Artifact{ArtifactID: testID(t, "profile-artifact"), MediaType: "application/vnd.atg.execution-profile+json", Digest: "sha256:" + strings.Repeat("6", 64)},
		},
		ProfileApproval: execution.ProfileApproval{
			DecisionID: testID(t, "profile-decision"), Reviewer: reviewer,
			ApprovedProfileDigest: "sha256:" + strings.Repeat("6", 64),
		},
		ExpectedTestIDs: []string{"fixture.SmokeTest#works"},
		RetryPolicy: execution.RetryPolicy{
			PolicyID: testID(t, "retry-policy"), MaxAttempts: 2,
			DeadlineAt:            now.Add(24 * time.Hour).UTC().Format(time.RFC3339),
			InitialBackoffSeconds: 10, MaxBackoffSeconds: 60,
			RetryableFailures: append([]string(nil), retryFailures...),
		},
		CreatedAt: now.UTC().Format(time.RFC3339),
	}
	candidate := base
	candidate.TaskSpecID = testID(t, "task-candidate")
	candidate.SubjectRole = "candidate"
	if comparisonMode == "differential" {
		candidate.Subject.Source.Identity.ID = testID(t, "commit-candidate")
		candidate.Subject.Source.Identity.Digest = "sha256:" + strings.Repeat("a", 64)
		candidate.Subject.Identity.ID = testID(t, "tree-candidate")
		candidate.Subject.Identity.Digest = "sha256:" + strings.Repeat("b", 64)
	}
	baseBytes, candidateBytes := marshalSpecs(t, base, candidate)
	key := testID(t, "idempotency")
	return execution.PairInput{IdempotencyKey: key, BaseSpec: baseBytes, CandidateSpec: candidateBytes, CreatedAt: now.UTC()}
}

func decodeInputSpecs(t *testing.T, input execution.PairInput) (execution.TaskSpec, execution.TaskSpec) {
	t.Helper()
	base, err := execution.DecodeTaskSpec(input.BaseSpec)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := execution.DecodeTaskSpec(input.CandidateSpec)
	if err != nil {
		t.Fatal(err)
	}
	return base, candidate
}

func marshalSpecs(t *testing.T, base, candidate execution.TaskSpec) ([]byte, []byte) {
	t.Helper()
	baseBytes, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	candidateBytes, err := json.Marshal(candidate)
	if err != nil {
		t.Fatal(err)
	}
	return baseBytes, candidateBytes
}

func testID(t *testing.T, prefix string) string {
	t.Helper()
	var value [8]byte
	if _, err := rand.Read(value[:]); err != nil {
		t.Fatal(err)
	}
	return prefix + "-" + hex.EncodeToString(value[:])
}
