package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/project-lightpath/ai-test-generation/services/control/internal/testgenflow"
)

func TestPostgresTestGenerationFlowIdempotencyRevisionFencingAndRestart(t *testing.T) {
	ctx := context.Background()
	store := integrationStore(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	request := testgenflow.Request{
		RepositoryURL:  "https://gitlab.example/group/spring-demo.git",
		Revision:       "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		BaseBranch:     "main",
		IssueIID:       "42",
		Subdirectory:   "source",
		ProfileID:      "spring-java17-maven",
		ProfileVersion: "0.1.0",
		ProfileDigest:  "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
	}
	runID := testID(t, "testgen-run")
	idempotencyKey := testID(t, "test-generation-idempotency")
	run, err := testgenflow.NewRun(runID, idempotencyKey, request, 6, now)
	if err != nil {
		t.Fatal(err)
	}
	created, inserted, err := store.CreateRun(ctx, run)
	if err != nil || !inserted || created.Revision != 1 {
		t.Fatalf("create test-generation run = %#v inserted=%t err=%v", created, inserted, err)
	}
	cleanupTestGenerationFlowRun(t, store, runID)

	duplicateRun, err := testgenflow.NewRun(testID(t, "other-run"), idempotencyKey, request, 6, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	existing, inserted, err := store.CreateRun(ctx, duplicateRun)
	if err != nil || inserted || existing.RunID != runID {
		t.Fatalf("idempotent test-generation run = %#v inserted=%t err=%v", existing, inserted, err)
	}

	changedRequest := request
	changedRequest.BaseBranch = "release"
	changedRun, err := testgenflow.NewRun(testID(t, "changed-run"), idempotencyKey, changedRequest, 6, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, inserted, err := store.CreateRun(ctx, changedRun); !errors.Is(err, testgenflow.ErrIdempotencyConflict) || inserted {
		t.Fatalf("changed idempotent run inserted=%t err=%v, want idempotency conflict", inserted, err)
	}

	resolved := testgenflow.Event{
		ID:               testID(t, "testgen-event"),
		ExpectedRevision: 1,
		Type:             testgenflow.EventSourceResolved,
		OccurredAt:       now.Add(time.Second),
		ResolvedCommit:   request.Revision,
	}
	run, err = store.ApplyEvent(ctx, runID, resolved)
	if err != nil || run.Revision != 2 || run.Stage != testgenflow.StageBaseline {
		t.Fatalf("apply source event = %#v err=%v", run, err)
	}
	duplicate, err := store.ApplyEvent(ctx, runID, resolved)
	if err != nil || duplicate.Revision != 2 {
		t.Fatalf("duplicate source event = %#v err=%v", duplicate, err)
	}
	conflicting := resolved
	conflicting.ResolvedCommit = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if _, err := store.ApplyEvent(ctx, runID, conflicting); !errors.Is(err, testgenflow.ErrEventConflict) {
		t.Fatalf("same event ID with new payload error = %v, want event conflict", err)
	}
	stale := resolved
	stale.ID = testID(t, "test-generation-stale-event")
	if _, err := store.ApplyEvent(ctx, runID, stale); !errors.Is(err, testgenflow.ErrRevisionConflict) {
		t.Fatalf("stale event error = %v, want revision conflict", err)
	}

	restarted := integrationStore(t)
	persisted, err := restarted.GetRun(ctx, runID)
	if err != nil || persisted.Revision != 2 || persisted.ResolvedCommit != request.Revision || persisted.Stage != testgenflow.StageBaseline {
		t.Fatalf("restarted test-generation run = %#v err=%v", persisted, err)
	}
}

func cleanupTestGenerationFlowRun(t *testing.T, store *Store, runID string) {
	t.Helper()
	t.Cleanup(func() {
		if _, err := store.db.ExecContext(context.Background(), `DELETE FROM development_test_generation_runs WHERE run_id = $1`, runID); err != nil {
			t.Errorf("delete test-generation development run: %v", err)
		}
	})
}
