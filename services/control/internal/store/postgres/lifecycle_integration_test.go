package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/project-lightpath/ai-test-generation/services/control/internal/execution"
	"github.com/project-lightpath/ai-test-generation/services/control/internal/provider"
)

func TestPostgresCancellationFailureCannotAuthorizeRetryAfterRestart(t *testing.T) {
	store, pair, claim, now := lifecycleAttempt(t)
	ctx := context.Background()
	task, err := store.GetTask(ctx, pair.BaseTaskID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.RequestCancel(ctx, task.TaskID, task.Revision, now); err != nil {
		t.Fatal(err)
	}
	observation := testObservation(t, claim, uint64(claim.FencingToken), provider.StatusFailed, claim.ProviderJob, now)
	observation.FailureCategory = "worker_lost"
	if err := store.RecordObservation(ctx, claim, observation, now, time.Second); err != nil {
		t.Fatal(err)
	}
	restarted := integrationStore(t)
	task, err = restarted.GetTask(ctx, task.TaskID)
	if err != nil || task.Status != "cancel_requested" || task.CancelRequestedAt == nil {
		t.Fatalf("cancellation after failure = %#v, %v", task, err)
	}
	result := lifecycleResult(t, task, claim, execution.ResultFailed, now)
	if _, err := restarted.AuthorizeRetry(ctx, task.TaskID, testID(t, "forbidden-retry"), now); !errors.Is(err, execution.ErrRetryNotAuthorized) {
		t.Fatalf("cancelled retry authorization = %v", err)
	}
	if err := restarted.ScheduleRetryableFailures(ctx, now); err != nil {
		t.Fatal(err)
	}
	cancel, err := restarted.ClaimNext(ctx, "restarted-controller", claim.Provider, now.Add(2*time.Second), time.Minute, time.Second)
	if err != nil || cancel == nil || cancel.Action != execution.ActionCancel || cancel.AttemptID != claim.AttemptID {
		t.Fatalf("termination reconciliation = %#v, %v", cancel, err)
	}
	observation = testObservation(t, *cancel, uint64(cancel.FencingToken), provider.StatusCancelled, claim.ProviderJob, now.Add(2*time.Second))
	observation.TerminationConfirmed = true
	if err := restarted.RecordObservation(ctx, *cancel, observation, now.Add(2*time.Second), time.Second); err != nil {
		t.Fatal(err)
	}
	task, err = restarted.GetTask(ctx, task.TaskID)
	if err != nil || task.Status != "cancelled" || task.AttemptCount != 1 {
		t.Fatalf("settled cancellation = %#v, %v", task, err)
	}
	provisional := lifecycleReference(t, result)
	if err := restarted.RecordProvisional(ctx, result, provisional); err != nil {
		t.Fatalf("retain failed evidence after confirmed cancellation: %v", err)
	}
	final, finalRef := lifecycleFinal(t, result, now.Add(3*time.Second))
	if err := restarted.RecordFinalized(ctx, final, provisional, finalRef); err != nil {
		t.Fatal(err)
	}
	var attempts, decisions int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM execution_attempts WHERE task_id = $1`, task.TaskID).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM execution_retry_decisions WHERE task_id = $1`, task.TaskID).Scan(&decisions); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 || decisions != 0 {
		t.Fatalf("cancelled work created attempts=%d retry decisions=%d", attempts, decisions)
	}
}

func TestPostgresCancellationCompletionRetainsEvidence(t *testing.T) {
	for _, when := range []string{"before_completion", "after_completion", "after_provisional"} {
		for _, confirmed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/termination_%t", when, confirmed), func(t *testing.T) {
				store, pair, claim, now := lifecycleAttempt(t)
				ctx := context.Background()
				task, err := store.GetTask(ctx, pair.BaseTaskID)
				if err != nil {
					t.Fatal(err)
				}
				var cancelRevision int64
				cancel := func() {
					t.Helper()
					cancelRevision = task.Revision
					var err error
					task, err = store.RequestCancel(ctx, task.TaskID, cancelRevision, now)
					if err != nil || task.CancelRequestedAt == nil {
						t.Fatalf("cancel collecting attempt = %#v, %v", task, err)
					}
				}
				if when == "before_completion" {
					cancel()
				}
				observation := testObservation(t, claim, uint64(claim.FencingToken), provider.StatusCompleted, claim.ProviderJob, now)
				observation.TerminationConfirmed = confirmed
				if err := store.RecordObservation(ctx, claim, observation, now, time.Second); err != nil {
					t.Fatal(err)
				}
				task, err = store.GetTask(ctx, task.TaskID)
				if err != nil {
					t.Fatal(err)
				}
				result := lifecycleResult(t, task, claim, execution.ResultCompleted, now)
				if when == "after_completion" {
					cancel()
				}
				wrongRevision := result
				wrongRevision.ExpectedTaskRevision = 1
				if err := store.RecordProvisional(ctx, wrongRevision, lifecycleReference(t, wrongRevision)); !errors.Is(err, execution.ErrResultNotReady) {
					t.Fatalf("stale pre-attempt result revision = %v", err)
				}
				provisional := lifecycleReference(t, result)
				if err := store.RecordProvisional(ctx, result, provisional); err != nil {
					t.Fatalf("retain completed evidence despite cancellation: %v", err)
				}
				task, err = store.GetTask(ctx, task.TaskID)
				if err != nil {
					t.Fatal(err)
				}
				if when == "after_provisional" {
					cancel()
				}
				restarted := integrationStore(t)
				if !confirmed {
					cancellation, err := restarted.ClaimNext(ctx, "cancel-recovery", claim.Provider, now.Add(2*time.Second), time.Minute, time.Second)
					if err != nil || cancellation == nil || cancellation.Action != execution.ActionCancel || cancellation.AttemptID != claim.AttemptID {
						t.Fatalf("completed attempt reconciliation = %#v, %v", cancellation, err)
					}
					proof := testObservation(t, *cancellation, uint64(cancellation.FencingToken), provider.StatusCancelled, claim.ProviderJob, now.Add(2*time.Second))
					proof.TerminationConfirmed = true
					if err := restarted.RecordObservation(ctx, *cancellation, proof, now.Add(2*time.Second), time.Second); err != nil {
						t.Fatal(err)
					}
				}
				final, finalRef := lifecycleFinal(t, result, now.Add(3*time.Second))
				if err := restarted.RecordFinalized(ctx, final, provisional, finalRef); err != nil {
					t.Fatalf("finalize evidence after cancellation: %v", err)
				}
				task, err = restarted.GetTask(ctx, task.TaskID)
				if err != nil || task.Status != "cancelled" || task.CancelRequestedAt == nil || task.ExecutionResult == nil || *task.ExecutionResult != finalRef {
					t.Fatalf("cancelled task evidence = %#v, %v", task, err)
				}
				if err := restarted.RecordProvisional(ctx, result, provisional); err != nil {
					t.Fatalf("replayed provisional delivery: %v", err)
				}
				if err := restarted.RecordFinalized(ctx, final, provisional, finalRef); err != nil {
					t.Fatalf("replayed final delivery: %v", err)
				}
				repeated, err := restarted.RequestCancel(ctx, task.TaskID, cancelRevision, now.Add(4*time.Second))
				if err != nil || repeated.Revision != task.Revision {
					t.Fatalf("replayed cancellation changed finalized state: %#v, %v", repeated, err)
				}
				if _, err := restarted.AuthorizeRetry(ctx, task.TaskID, testID(t, "no-retry"), now); !errors.Is(err, execution.ErrRetryNotAuthorized) {
					t.Fatalf("retry cancelled completion: %v", err)
				}
				var attemptStatus string
				var finishedAt time.Time
				if err := store.db.QueryRowContext(ctx, `SELECT status, finished_at FROM execution_attempts WHERE attempt_id = $1`, claim.AttemptID).Scan(&attemptStatus, &finishedAt); err != nil || attemptStatus != "completed" || !finishedAt.Equal(now) {
					t.Fatalf("cancellation overwrote workload outcome: %q finished=%s, %v", attemptStatus, finishedAt, err)
				}
			})
		}
	}
}

func TestPostgresRetryRetainsEachAttemptsEvidenceInEitherDeliveryOrder(t *testing.T) {
	for _, beforeRetry := range []bool{false, true} {
		t.Run(fmt.Sprintf("failed_evidence_before_retry_%t", beforeRetry), func(t *testing.T) {
			store, pair, first, now := lifecycleAttempt(t)
			ctx := context.Background()
			failed := testObservation(t, first, uint64(first.FencingToken), provider.StatusFailed, first.ProviderJob, now)
			failed.FailureCategory = "worker_lost"
			if err := store.RecordObservation(ctx, first, failed, now, time.Second); err != nil {
				t.Fatal(err)
			}
			task, err := store.GetTask(ctx, pair.BaseTaskID)
			if err != nil {
				t.Fatal(err)
			}
			firstResult := lifecycleResult(t, task, first, execution.ResultFailed, now)
			firstRef := lifecycleReference(t, firstResult)
			prematureFinal, prematureRef := lifecycleFinal(t, firstResult, now.Add(time.Second))
			if err := store.RecordFinalized(ctx, prematureFinal, firstRef, prematureRef); !errors.Is(err, execution.ErrResultConflict) {
				t.Fatalf("cleanup finalization without durable provisional delivery = %v", err)
			}
			if beforeRetry {
				if err := store.RecordProvisional(ctx, firstResult, firstRef); err != nil {
					t.Fatal(err)
				}
			}
			decision, err := store.AuthorizeRetry(ctx, task.TaskID, testID(t, "retry"), now)
			if err != nil {
				t.Fatal(err)
			}
			second, err := store.ClaimNext(ctx, "retry-controller", first.Provider, decision.NextAttemptAt, time.Minute, time.Second)
			if err != nil || second == nil || second.AttemptID == first.AttemptID || second.AttemptNumber != 2 {
				t.Fatalf("retry claim = %#v, %v", second, err)
			}
			second.ProviderJob = provider.JobRef{ID: testID(t, "retry-job")}
			if err := store.RecordSubmitted(ctx, *second, second.ProviderJob, decision.NextAttemptAt, time.Second); err != nil {
				t.Fatal(err)
			}
			pollAt := decision.NextAttemptAt.Add(2 * time.Second)
			second, err = store.ClaimNext(ctx, "retry-controller", first.Provider, pollAt, time.Minute, time.Second)
			if err != nil || second == nil {
				t.Fatalf("retry completion claim = %#v, %v", second, err)
			}
			completed := testObservation(t, *second, uint64(second.FencingToken), provider.StatusCompleted, second.ProviderJob, pollAt)
			completed.TerminationConfirmed = true
			if err := store.RecordObservation(ctx, *second, completed, pollAt, time.Second); err != nil {
				t.Fatal(err)
			}
			task, err = store.GetTask(ctx, task.TaskID)
			if err != nil || task.ExecutionResult != nil {
				t.Fatalf("retry retained previous current result: %#v, %v", task, err)
			}
			secondResult := lifecycleResult(t, task, *second, execution.ResultCompleted, pollAt)
			secondRef := lifecycleReference(t, secondResult)
			if err := store.RecordProvisional(ctx, secondResult, secondRef); err != nil {
				t.Fatalf("successful retry provisional delivery: %v", err)
			}
			secondFinal, secondFinalRef := lifecycleFinal(t, secondResult, pollAt.Add(time.Second))
			if err := store.RecordFinalized(ctx, secondFinal, secondRef, secondFinalRef); err != nil {
				t.Fatalf("successful retry final delivery: %v", err)
			}
			current, err := store.GetTask(ctx, task.TaskID)
			if err != nil || current.Status != "completed" {
				t.Fatalf("successful retry = %#v, %v", current, err)
			}
			restarted := integrationStore(t)
			if !beforeRetry {
				stale := firstResult
				stale.ExpectedTaskRevision = 1
				if err := restarted.RecordProvisional(ctx, stale, lifecycleReference(t, stale)); !errors.Is(err, execution.ErrResultNotReady) {
					t.Fatalf("unattributable historical revision = %v", err)
				}
			}
			if err := restarted.RecordProvisional(ctx, firstResult, firstRef); err != nil {
				t.Fatalf("retained failed-attempt delivery after retry: %v", err)
			}
			firstFinal, firstFinalRef := lifecycleFinal(t, firstResult, pollAt.Add(2*time.Second))
			if err := restarted.RecordFinalized(ctx, firstFinal, firstRef, firstFinalRef); err != nil {
				t.Fatalf("retained failed-attempt cleanup after retry: %v", err)
			}
			if err := restarted.RecordFinalized(ctx, firstFinal, firstRef, firstFinalRef); err != nil {
				t.Fatalf("idempotent historical finalization: %v", err)
			}
			conflict := firstResult
			conflict.Worker.ID = "different-worker"
			if err := restarted.RecordProvisional(ctx, conflict, lifecycleReference(t, conflict)); !errors.Is(err, execution.ErrResultConflict) {
				t.Fatalf("conflicting historical delivery = %v", err)
			}
			after, err := restarted.GetTask(ctx, current.TaskID)
			if err != nil || after.Revision != current.Revision || after.Status != "completed" || after.ExecutionResult == nil || *after.ExecutionResult != secondFinalRef {
				t.Fatalf("historical evidence changed current retry: %#v, %v", after, err)
			}
			for _, retained := range []struct {
				attemptID              string
				provisional, finalized execution.Artifact
			}{{first.AttemptID, firstRef, firstFinalRef}, {second.AttemptID, secondRef, secondFinalRef}} {
				var provisionalBytes, finalizedBytes []byte
				if err := restarted.db.QueryRowContext(ctx, `SELECT provisional_reference_bytes, finalized_reference_bytes FROM execution_attempt_results WHERE attempt_id = $1`, retained.attemptID).Scan(&provisionalBytes, &finalizedBytes); err != nil {
					t.Fatal(err)
				}
				var provisional, finalized execution.Artifact
				if err := json.Unmarshal(provisionalBytes, &provisional); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(finalizedBytes, &finalized); err != nil {
					t.Fatal(err)
				}
				if provisional != retained.provisional || finalized != retained.finalized {
					t.Fatalf("attempt %s lost evidence: provisional=%#v finalized=%#v", retained.attemptID, provisional, finalized)
				}
			}
		})
	}
}

func TestPostgresConcurrentRetryAndEvidenceRetainFailedDelivery(t *testing.T) {
	store, pair, claim, now := lifecycleAttempt(t)
	ctx := context.Background()
	failed := testObservation(t, claim, uint64(claim.FencingToken), provider.StatusFailed, claim.ProviderJob, now)
	failed.FailureCategory = "worker_lost"
	if err := store.RecordObservation(ctx, claim, failed, now, time.Second); err != nil {
		t.Fatal(err)
	}
	task, err := store.GetTask(ctx, pair.BaseTaskID)
	if err != nil {
		t.Fatal(err)
	}
	result := lifecycleResult(t, task, claim, execution.ResultFailed, now)
	reference := lifecycleReference(t, result)
	start, results := make(chan struct{}), make(chan error, 2)
	go func() {
		<-start
		_, err := store.AuthorizeRetry(ctx, task.TaskID, "concurrent-retry-"+claim.AttemptID, now)
		results <- err
	}()
	go func() {
		<-start
		results <- store.RecordProvisional(ctx, result, reference)
	}()
	close(start)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	task, err = store.GetTask(ctx, task.TaskID)
	if err != nil || task.Status != "eligible" || task.ActiveAttemptID != "" || task.ExecutionResult != nil {
		t.Fatalf("concurrent retry projection = %#v, %v", task, err)
	}
	final, finalRef := lifecycleFinal(t, result, now.Add(time.Second))
	if err := store.RecordFinalized(ctx, final, reference, finalRef); err != nil {
		t.Fatalf("concurrent delivery did not preserve cleanup authorization: %v", err)
	}
}

func TestPostgresCancellationWithUnknownCleanupKeepsReconcilingTermination(t *testing.T) {
	store, pair, claim, now := lifecycleAttempt(t)
	ctx := context.Background()
	completed := testObservation(t, claim, uint64(claim.FencingToken), provider.StatusCompleted, claim.ProviderJob, now)
	if err := store.RecordObservation(ctx, claim, completed, now, time.Second); err != nil {
		t.Fatal(err)
	}
	task, err := store.GetTask(ctx, pair.BaseTaskID)
	if err != nil {
		t.Fatal(err)
	}
	task, err = store.RequestCancel(ctx, task.TaskID, task.Revision, now)
	if err != nil {
		t.Fatal(err)
	}
	result := lifecycleResult(t, task, claim, execution.ResultCompleted, now)
	provisional := lifecycleReference(t, result)
	if err := store.RecordProvisional(ctx, result, provisional); err != nil {
		t.Fatal(err)
	}
	final := result
	final.Status = execution.ResultOutcomeUnknown
	final.UpdatedAt, final.FinalizedAt = now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)
	final.Cleanup = execution.CleanupEvidence{Status: "unknown"}
	finalRef := lifecycleReference(t, final)
	if err := store.RecordFinalized(ctx, final, provisional, finalRef); err != nil {
		t.Fatal(err)
	}
	restarted := integrationStore(t)
	for i, status := range []provider.Status{provider.StatusCancelRequested, provider.StatusCancelled} {
		at := now.Add(time.Duration(i+1) * 2 * time.Second)
		cancellation, err := restarted.ClaimNext(ctx, "unknown-cleanup-controller", claim.Provider, at, time.Minute, time.Second)
		if err != nil || cancellation == nil || cancellation.Action != execution.ActionCancel || cancellation.AttemptID != claim.AttemptID {
			t.Fatalf("unknown-cleanup cancellation claim = %#v, %v", cancellation, err)
		}
		observation := testObservation(t, *cancellation, uint64(cancellation.FencingToken), status, claim.ProviderJob, at)
		observation.TerminationConfirmed = status == provider.StatusCancelled
		if err := restarted.RecordObservation(ctx, *cancellation, observation, at, time.Second); err != nil {
			t.Fatal(err)
		}
		task, err = restarted.GetTask(ctx, task.TaskID)
		want := "cancel_requested"
		if observation.TerminationConfirmed {
			want = "cancelled"
		}
		if err != nil || task.Status != want || task.ExecutionResult == nil || *task.ExecutionResult != finalRef {
			t.Fatalf("termination %s lost cancellation/evidence = %#v, %v", status, task, err)
		}
	}
}

func TestPostgresAttemptResultsMigrationPreservesRecordedHistory(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	schema := strings.ReplaceAll(testID(t, "migration"), "-", "_")
	if _, err := tx.ExecContext(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `SET LOCAL search_path TO `+schema); err != nil {
		t.Fatal(err)
	}
	initial, err := migrationFiles.ReadFile("migrations/0001_execution.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, string(initial)); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `INSERT INTO execution_pairs VALUES ('pair', 'run', 'generation', 'same_subject_control', 'key', 'digest', 'base', 'candidate', $1)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO execution_tasks(task_id, pair_id, run_id, subject_role, specification_bytes, specification_digest, status, revision, next_action_at, created_at, updated_at)
		VALUES ('base', 'pair', 'run', 'base', ''::bytea, 'digest', 'completed', 6, $1, $1, $1)`, now); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 2; i++ {
		attemptID := fmt.Sprintf("attempt-%d", i)
		if _, err := tx.ExecContext(ctx, `INSERT INTO execution_attempts(attempt_id, task_id, attempt_number, task_spec_digest, submission_token, status, provider_id, provider_identity_bytes, fencing_token, next_action_at, created_at, updated_at)
			VALUES ($1, 'base', $2, 'digest', $1, 'completed', 'provider', '{}'::bytea, 1, $3, $3, $3)`, attemptID, i, now); err != nil {
			t.Fatal(err)
		}
		provisional := execution.Artifact{ArtifactID: "result-" + attemptID, MediaType: execution.ExecutionResultMediaType, Digest: "sha256:" + strings.Repeat(fmt.Sprint(i), 64)}
		final := provisional
		final.Digest = "sha256:" + strings.Repeat(fmt.Sprint(i+2), 64)
		for j, eventType := range []string{"result_provisional", "execution_result_finalized"} {
			payload := map[string]any{"attempt_id": attemptID, "result": provisional}
			if j == 1 {
				payload["result"], payload["provisional_result"] = final, provisional
				payload["cleanup"] = execution.CleanupEvidence{Status: "succeeded", TerminationConfirmed: true}
			}
			encoded, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			revision := int64(i*2 + j + 1)
			if err := appendTaskEvent(ctx, tx, "base", revision-1, revision, eventType, encoded, now); err != nil {
				t.Fatal(err)
			}
		}
	}
	upgrade, err := migrationFiles.ReadFile("migrations/0002_attempt_results.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, string(upgrade)); err != nil {
		t.Fatalf("upgrade populated 0001 schema: %v", err)
	}
	for i := 1; i <= 2; i++ {
		attemptID := fmt.Sprintf("attempt-%d", i)
		delivery, err := loadAttemptResult(ctx, tx, attemptID)
		if err != nil || delivery.expectedRevision != int64(i*2) || delivery.finalized == nil || delivery.provisional.Digest != "sha256:"+strings.Repeat(fmt.Sprint(i), 64) || delivery.finalized.Digest != "sha256:"+strings.Repeat(fmt.Sprint(i+2), 64) {
			t.Fatalf("migrated attempt %s delivery = %#v, %v", attemptID, delivery, err)
		}
		var terminated bool
		if err := tx.QueryRowContext(ctx, `SELECT termination_confirmed FROM execution_attempts WHERE attempt_id = $1`, attemptID).Scan(&terminated); err != nil || !terminated {
			t.Fatalf("migrated cleanup proof for %s = %t, %v", attemptID, terminated, err)
		}
	}
}

func lifecycleAttempt(t *testing.T) (*Store, execution.PairRecord, execution.Claim, time.Time) {
	t.Helper()
	store := integrationStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	pair, _, err := store.CreatePair(ctx, pairInput(t, now, "same_subject_control", []string{"worker_lost"}))
	if err != nil {
		t.Fatal(err)
	}
	cleanupPair(t, store, pair.PairID)
	if _, err := store.RequestCancel(ctx, pair.CandidateTaskID, 1, now); err != nil {
		t.Fatal(err)
	}
	identity := provider.Identity{Kind: "test-provider", ID: testID(t, "lifecycle-provider"), Version: "1"}
	claim, err := store.ClaimNext(ctx, "lifecycle-controller", identity, now, time.Minute, time.Second)
	if err != nil || claim == nil {
		t.Fatalf("initial claim = %#v, %v", claim, err)
	}
	job := provider.JobRef{ID: testID(t, "lifecycle-job")}
	if err := store.RecordSubmitted(ctx, *claim, job, now, time.Second); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Second)
	claim, err = store.ClaimNext(ctx, "lifecycle-controller", identity, now, time.Minute, time.Second)
	if err != nil || claim == nil || claim.Action != execution.ActionObserve {
		t.Fatalf("initial observation claim = %#v, %v", claim, err)
	}
	return store, pair, *claim, now
}

func lifecycleResult(t *testing.T, task execution.TaskRecord, claim execution.Claim, outcome execution.ResultStatus, now time.Time) execution.ExecutionResult {
	t.Helper()
	spec, err := execution.DecodeTaskSpec(task.Specification)
	if err != nil {
		t.Fatal(err)
	}
	stamp := now.UTC().Format(time.RFC3339Nano)
	result := execution.ExecutionResult{
		Contract: "atg.execution_result", Version: "1.0", ResultID: testID(t, "lifecycle-result"),
		TaskID: task.TaskID, RunID: task.RunID, PairID: task.PairID, AttemptID: claim.AttemptID,
		AttemptNumber: claim.AttemptNumber, ExpectedTaskRevision: task.Revision, TaskSpecDigest: task.SpecificationDigest,
		SubjectRole: task.SubjectRole, Source: spec.Subject.Source.Identity, Subject: spec.Subject.Identity,
		Profile:  execution.ResultProfile{ProfileID: spec.Profile.ProfileID, ProfileVersion: spec.Profile.ProfileVersion, Digest: spec.Profile.Artifact.Digest},
		Provider: execution.Identity(claim.Provider), ProviderJob: execution.Identity{Kind: "job", ID: claim.ProviderJob.ID},
		Worker:      execution.Identity{Kind: "worker", ID: "lifecycle-worker", Digest: "sha256:" + strings.Repeat("8", 64)},
		Environment: execution.Identity{Kind: "environment", ID: "lifecycle-environment", Digest: "sha256:" + strings.Repeat("9", 64)},
		Status:      execution.ResultProvisional, ObservedOutcome: outcome,
		Phases: []execution.ExecutionPhase{{Phase: "test", CommandID: "run-tests", Status: "succeeded", StartedAt: stamp, FinishedAt: stamp}},
		Tests: execution.TestExecutionEvidence{
			ExpectedTestIDs: spec.ExpectedTestIDs, DiscoveredTestIDs: spec.ExpectedTestIDs, SelectedTestIDs: spec.ExpectedTestIDs,
			ExecutedTestIDs: spec.ExpectedTestIDs, SkippedTestIDs: []string{}, FailedTestIDs: []string{},
		},
		Artifacts: []execution.Artifact{}, Cleanup: execution.CleanupEvidence{Status: "pending"}, StartedAt: stamp, UpdatedAt: stamp,
	}
	if outcome == execution.ResultFailed {
		result.FailureCategory = "worker_lost"
		result.Phases[0].Status, result.Phases[0].FailureCategory = "failed", "worker_lost"
		result.Tests.ExecutedTestIDs, result.Tests.SkippedTestIDs = []string{}, spec.ExpectedTestIDs
	}
	if err := result.Validate(); err != nil {
		t.Fatalf("result fixture: %v", err)
	}
	return result
}

func lifecycleReference(t *testing.T, result execution.ExecutionResult) execution.Artifact {
	t.Helper()
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	return execution.Artifact{ArtifactID: result.ResultID, MediaType: execution.ExecutionResultMediaType, Digest: execution.DigestTaskSpec(encoded), SizeBytes: int64(len(encoded))}
}

func lifecycleFinal(t *testing.T, provisional execution.ExecutionResult, now time.Time) (execution.ExecutionResult, execution.Artifact) {
	t.Helper()
	final := provisional
	final.Status = final.ObservedOutcome
	final.UpdatedAt = now.UTC().Format(time.RFC3339Nano)
	final.FinalizedAt = final.UpdatedAt
	final.Cleanup = execution.CleanupEvidence{Status: "succeeded", TerminationConfirmed: true, FinishedAt: final.UpdatedAt}
	if err := final.Validate(); err != nil {
		t.Fatal(err)
	}
	return final, lifecycleReference(t, final)
}
