package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/project-lightpath/ai-test-generation/services/control/internal/testgenflow"
)

func TestGenerationClaimValidationRequiresModelProfileAndPolicyBeforeStoreAccess(t *testing.T) {
	var store *Store
	digest := "sha256:" + strings.Repeat("a", 64)
	for _, input := range []struct {
		holder, profile, policy string
		lease                   time.Duration
	}{
		{"holder", "", digest, time.Minute},
		{"holder", digest, "", time.Minute},
		{"holder", "mutable-profile", digest, time.Minute},
		{"holder", digest, "mutable-policy", time.Minute},
		{"", digest, digest, time.Minute},
		{"holder", digest, digest, 0},
		{"holder", digest, digest, time.Nanosecond},
	} {
		claim, err := store.ClaimNextModelGenerationRun(context.Background(), input.holder, input.profile, input.policy, input.lease)
		if err == nil || claim != nil || strings.Contains(err.Error(), "store is unavailable") {
			t.Fatalf("invalid model scope reached store: %#v, %v", input, err)
		}
	}
	if claim, err := store.ClaimNextModelGenerationRun(context.Background(), "holder", digest, digest, time.Minute); claim != nil || err == nil || !strings.Contains(err.Error(), "store is unavailable") {
		t.Fatalf("valid model claim did not reach store guard: %#v, %v", claim, err)
	}
	if claim, err := store.ClaimNextTestGenerationRun(context.Background(), "holder", "", time.Minute); claim != nil || err == nil || !strings.Contains(err.Error(), "store is unavailable") {
		t.Fatalf("legacy artifact claim lost optional-profile semantics: %#v, %v", claim, err)
	}
}

// These optional PostgreSQL scenarios are not part of the offline unit run.
func TestPostgresGenerationClaimsSeparateModesProfilesPoliciesAndPauseReviews(t *testing.T) {
	ctx := context.Background()
	store := integrationStore(t)
	artifact := createClaimableTestGenerationRun(t, store, "")
	request := artifact.Request
	request.InputMode, request.InputArtifactDigest = "", ""
	createTestGenerationRun(t, store, request, time.Now().Add(-time.Second))
	request.InputMode = testgenflow.InputModeModelGeneration
	request.Generation = &testgenflow.GenerationRequest{Objective: "Assert the specified behavior", ContextPaths: []string{"src/main/java"}, PolicyDigest: testGenerationStateDigest([]byte(testID(t, "policy")))}
	target := createTestGenerationRun(t, store, request, time.Now().Add(-time.Second))
	otherPolicy := *request.Generation
	otherPolicy.PolicyDigest = testGenerationStateDigest([]byte(testID(t, "other-policy")))
	request.Generation = &otherPolicy
	createTestGenerationRun(t, store, request, time.Now().Add(-time.Second))
	request = target.Request
	request.ProfileDigest = testGenerationStateDigest([]byte(testID(t, "other-profile")))
	createTestGenerationRun(t, store, request, time.Now().Add(-time.Second))
	artifactClaim, err := store.ClaimNextTestGenerationRun(ctx, "artifact-server", artifact.Request.ProfileDigest, time.Minute)
	if err != nil || artifactClaim == nil || artifactClaim.RunID != artifact.RunID {
		t.Fatalf("artifact server claimed model or legacy run: %#v, %v", artifactClaim, err)
	}
	claim, err := store.ClaimNextModelGenerationRun(ctx, "model-server", target.Request.ProfileDigest, target.Request.Generation.PolicyDigest, time.Minute)
	if err != nil || claim == nil || claim.RunID != target.RunID {
		t.Fatalf("model server crossed mode/profile/policy scope: %#v, %v", claim, err)
	}
	if err := store.ReleaseTestGenerationClaim(ctx, *claim); err != nil {
		t.Fatal(err)
	}
	target = postgresWaitingGenerationReview(t, store, target)
	if claim, err := store.ClaimNextModelGenerationRun(ctx, "paused-model-server", target.Request.ProfileDigest, target.Request.Generation.PolicyDigest, time.Minute); err != nil || claim != nil {
		t.Fatalf("paused generation review was claimed: %#v, %v", claim, err)
	}
	target, err = store.ApplyEvent(ctx, target.RunID, testgenflow.Event{ID: testID(t, "cancel-review"), ExpectedRevision: target.Revision, Type: testgenflow.EventCancelRequested, OccurredAt: target.UpdatedAt})
	if err != nil {
		t.Fatal(err)
	}
	claim, err = store.ClaimNextModelGenerationRun(ctx, "cancel-model-server", target.Request.ProfileDigest, target.Request.Generation.PolicyDigest, time.Minute)
	if err != nil || claim == nil || claim.RunID != target.RunID || target.Status != testgenflow.StatusCancelRequested || target.Checkpoint != nil {
		t.Fatalf("review cancellation did not reactivate claimable work: %#v run=%#v err=%v", claim, target, err)
	}
}

func TestPostgresGenerationReviewSurvivesRestartAndBindsReplayedPayload(t *testing.T) {
	ctx := context.Background()
	store := integrationStore(t)
	request := createClaimableTestGenerationRun(t, store, "").Request
	request.InputMode, request.InputArtifactDigest = testgenflow.InputModeModelGeneration, ""
	request.Generation = &testgenflow.GenerationRequest{Objective: "Review the exact plan", ContextPaths: []string{"src/main/java"}, PolicyDigest: testGenerationStateDigest([]byte(testID(t, "policy")))}
	run := postgresWaitingGenerationReview(t, store, createTestGenerationRun(t, store, request, time.Now().Add(-time.Second)))
	checkpoint := run.Checkpoint
	review := testgenflow.GenerationReview{DecisionID: testID(t, "decision"), CheckpointID: checkpoint.ID, Stage: checkpoint.Stage, ArtifactRef: checkpoint.ArtifactRef, Attempt: checkpoint.Attempt, Action: "approve", ReviewerID: "human-reviewer", ReviewerRole: "reviewer", AuthorizationRef: "auth://local/review", Rationale: "Reviewed immutable plan", DecidedAt: run.UpdatedAt}
	event := testgenflow.Event{ID: testID(t, "review-event"), ExpectedRevision: run.Revision, Type: testgenflow.EventGenerationReviewDecided, Review: &review, OccurredAt: review.DecidedAt}
	approved, err := store.ApplyEvent(ctx, run.RunID, event)
	if err != nil || approved.Status != testgenflow.StatusRunning || approved.Checkpoint != nil || len(approved.Reviews) != 1 || !reflect.DeepEqual(approved.Reviews[0], review) {
		t.Fatalf("durable review = %#v, %v", approved, err)
	}
	restarted := integrationStore(t)
	if replayed, err := restarted.ApplyEvent(ctx, run.RunID, event); err != nil || !reflect.DeepEqual(replayed, approved) {
		t.Fatalf("restarted review replay changed state: %#v, %v", replayed, err)
	}
	changed := review
	changed.AuthorizationRef = "auth://different"
	event.Review = &changed
	if _, err := restarted.ApplyEvent(ctx, run.RunID, event); !errors.Is(err, testgenflow.ErrEventConflict) {
		t.Fatalf("changed review reused event identity: %v", err)
	}
	claim, err := restarted.ClaimNextModelGenerationRun(ctx, "resume-model", run.Request.ProfileDigest, run.Request.Generation.PolicyDigest, time.Minute)
	if err != nil || claim == nil || claim.RunID != run.RunID {
		t.Fatalf("approval did not resume model work: %#v, %v", claim, err)
	}
}

func TestPostgresGenerationReviewsMigrationPreservesHistoricalBytes(t *testing.T) {
	ctx := context.Background()
	store := integrationStore(t)
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	schema := strings.ReplaceAll(testID(t, "generation_migration"), "-", "_")
	if _, err := tx.ExecContext(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `SET LOCAL search_path TO `+schema); err != nil {
		t.Fatal(err)
	}
	for _, migration := range []string{"0003_test_generation_development_runs.sql", "0004_local_test_generation.sql"} {
		raw, err := migrationFiles.ReadFile("migrations/" + migration)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(ctx, string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	request := testgenflow.Request{RepositoryURL: "https://git.example/project.git", Revision: strings.Repeat("a", 40), BaseBranch: "main", Subdirectory: ".", ProfileID: "java17", ProfileVersion: "1.0.0", ProfileDigest: "sha256:" + strings.Repeat("b", 64)}
	run, err := testgenflow.NewRun("historical-run", "historical-key", request, 3, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	digest := testGenerationStateDigest(payload)
	if _, err := tx.ExecContext(ctx, `INSERT INTO development_test_generation_runs(run_id,idempotency_key,request_digest,state_digest,state_bytes,status,stage,revision,next_action_at,created_at,updated_at) VALUES ($1,$2,$3,$4,$5,'queued','source',1,$6,$6,$6)`, run.RunID, run.IdempotencyKey, run.RequestDigest, digest, payload, run.CreatedAt); err != nil {
		t.Fatal(err)
	}
	upgrade, err := migrationFiles.ReadFile("migrations/0005_generation_reviews.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, string(upgrade)); err != nil {
		t.Fatal(err)
	}
	var persisted []byte
	var persistedDigest string
	if err := tx.QueryRowContext(ctx, `SELECT state_bytes,state_digest FROM development_test_generation_runs WHERE run_id=$1`, run.RunID).Scan(&persisted, &persistedDigest); err != nil || string(persisted) != string(payload) || persistedDigest != digest {
		t.Fatalf("status migration rewrote historical payload: %s, %s, %v", persisted, persistedDigest, err)
	}
	for _, status := range []string{"waiting_review", "rejected", "blocked"} {
		if _, err := tx.ExecContext(ctx, `UPDATE development_test_generation_runs SET status=$2 WHERE run_id=$1`, run.RunID, status); err != nil {
			t.Fatalf("migration rejected status %s: %v", status, err)
		}
	}
}

func postgresWaitingGenerationReview(t *testing.T, store *Store, run testgenflow.Run) testgenflow.Run {
	t.Helper()
	for _, event := range []testgenflow.Event{
		{Type: testgenflow.EventSourceResolved, ResolvedCommit: run.Request.Revision},
		{Type: testgenflow.EventBaselineCompleted, Succeeded: true},
		{Type: testgenflow.EventGenerationStarted},
	} {
		event.ID, event.ExpectedRevision, event.OccurredAt = testID(t, "generation-event"), run.Revision, run.UpdatedAt
		var err error
		run, err = store.ApplyEvent(context.Background(), run.RunID, event)
		if err != nil {
			t.Fatal(err)
		}
	}
	ref := testGenerationStateDigest([]byte("immutable plan"))
	checkpoint := testgenflow.GenerationCheckpoint{ID: testgenflow.GenerationCheckpointID(run.RunID, run.RequestDigest, 1, "plan", ref), Stage: "plan", ArtifactRef: ref, Attempt: 1, ExpiresAt: run.UpdatedAt.Add(time.Hour)}
	waiting, err := store.ApplyEvent(context.Background(), run.RunID, testgenflow.Event{ID: testID(t, "checkpoint"), ExpectedRevision: run.Revision, Type: testgenflow.EventGenerationCheckpointRequested, Checkpoint: &checkpoint, OccurredAt: run.UpdatedAt})
	if err != nil {
		t.Fatal(err)
	}
	return waiting
}
