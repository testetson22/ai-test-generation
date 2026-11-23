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

func TestPostgresTestGenerationConcurrentClaimsAndRenewalPreserveProjection(t *testing.T) {
	ctx := context.Background()
	store := integrationStore(t)
	run := createClaimableTestGenerationRun(t, store, "")
	type outcome struct {
		claim *testgenflow.FlowClaim
		err   error
	}
	start := make(chan struct{})
	results := make(chan outcome, 2)
	for _, holder := range []string{"holder-a", "holder-b"} {
		go func() {
			<-start
			claim, err := store.ClaimNextTestGenerationRun(ctx, holder, run.Request.ProfileDigest, time.Minute)
			results <- outcome{claim, err}
		}()
	}
	close(start)
	var claim *testgenflow.FlowClaim
	for range 2 {
		result := <-results
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.claim != nil {
			if claim != nil {
				t.Fatalf("two holders claimed one run: %#v and %#v", claim, result.claim)
			}
			claim = result.claim
		}
	}
	if claim == nil || claim.RunID != run.RunID || claim.Fence != 1 || claim.LeaseUntil.IsZero() {
		t.Fatalf("claim = %#v, want exactly one first-fence claim", claim)
	}
	var databaseNow time.Time
	if err := store.db.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&databaseNow); err != nil || !claim.LeaseUntil.After(databaseNow) || claim.LeaseUntil.After(databaseNow.Add(time.Minute)) {
		t.Fatalf("database-clock lease = %s, database now = %s, err = %v", claim.LeaseUntil, databaseNow, err)
	}
	if err := store.RenewTestGenerationClaim(ctx, *claim, 2*time.Minute); err != nil {
		t.Fatal(err)
	}
	var renewed time.Time
	if err := store.db.QueryRowContext(ctx, `SELECT claim_lease_until FROM development_test_generation_runs WHERE run_id = $1`, run.RunID).Scan(&renewed); err != nil || !renewed.After(claim.LeaseUntil) {
		t.Fatalf("renewed lease = %s, original = %s, err = %v", renewed, claim.LeaseUntil, err)
	}
	if err := store.ReleaseTestGenerationClaim(ctx, *claim); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := store.ClaimNextTestGenerationRun(ctx, claim.Holder, run.Request.ProfileDigest, time.Minute)
	if err != nil || reclaimed == nil || reclaimed.RunID != run.RunID || reclaimed.Fence != claim.Fence+1 {
		t.Fatalf("reclaim with same holder = %#v, err = %v", reclaimed, err)
	}
	if err := store.ReleaseTestGenerationClaim(ctx, *claim); !errors.Is(err, testgenflow.ErrFlowClaimLost) {
		t.Fatalf("release old same-holder fence = %v", err)
	}
	persisted, err := store.GetRun(ctx, run.RunID)
	if err != nil || !reflect.DeepEqual(persisted, run) {
		t.Fatalf("claim/renew/release changed lifecycle projection: %#v, err = %v", persisted, err)
	}
}

func TestPostgresTestGenerationClaimsSkipLockedAndIneligibleRuns(t *testing.T) {
	ctx := context.Background()
	store := integrationStore(t)
	run := createClaimableTestGenerationRun(t, store, "")
	second := createClaimableTestGenerationRun(t, store, run.Request.ProfileDigest)
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT run_id FROM development_test_generation_runs WHERE run_id = $1 FOR UPDATE`, run.RunID); err != nil {
		t.Fatal(err)
	}
	claimCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	claim, err := store.ClaimNextTestGenerationRun(claimCtx, "skip-locked", run.Request.ProfileDigest, time.Minute)
	if err != nil || claim == nil || claim.RunID != second.RunID {
		t.Fatalf("claim beside locked run = %#v, err = %v", claim, err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	first, err := store.ClaimNextTestGenerationRun(ctx, "unlocked", run.Request.ProfileDigest, time.Minute)
	if err != nil || first == nil || first.RunID != run.RunID {
		t.Fatalf("claim unlocked run = %#v, err = %v", first, err)
	}

	legacy := run.Request
	legacy.InputMode, legacy.InputArtifactDigest, legacy.CompletionMode = "", "", ""
	createTestGenerationRun(t, store, legacy, time.Now().Add(-time.Hour))
	future := createTestGenerationRun(t, store, run.Request, time.Now().Add(time.Hour))
	terminal := createTestGenerationRun(t, store, run.Request, time.Now())
	if _, err := store.ApplyEvent(ctx, terminal.RunID, testgenflow.Event{ID: testID(t, "failed"), ExpectedRevision: 1, Type: testgenflow.EventStageFailed, OccurredAt: terminal.UpdatedAt, Diagnostic: "test prerequisite unavailable"}); err != nil {
		t.Fatal(err)
	}
	other := createClaimableTestGenerationRun(t, store, "")
	if claim, err := store.ClaimNextTestGenerationRun(ctx, "filtered", run.Request.ProfileDigest, time.Minute); err != nil || claim != nil {
		t.Fatalf("claimed held/legacy/future/terminal/wrong-profile run = %#v, err = %v (future %s)", claim, err, future.RunID)
	}
	claim, err = store.ClaimNextTestGenerationRun(ctx, "any-profile", "", time.Minute)
	if err != nil || claim == nil || claim.RunID != other.RunID {
		t.Fatalf("unfiltered profile claim = %#v, err = %v; want %s", claim, err, other.RunID)
	}
}

func TestPostgresTestGenerationExpiredAndStaleClaimsFenceEveryWrite(t *testing.T) {
	ctx := context.Background()
	store := integrationStore(t)
	run := createClaimableTestGenerationRun(t, store, "")
	claim := claimTestGenerationRun(t, store, run, "holder-before-expiry")
	key := "source:1"
	if _, created, err := store.BeginTestGenerationOperation(ctx, claim, key, "source", run.RequestDigest); err != nil || !created {
		t.Fatalf("prepare operation created=%t, err=%v", created, err)
	}
	event := testgenflow.Event{ID: testID(t, "source-event"), ExpectedRevision: 1, Type: testgenflow.EventSourceResolved, OccurredAt: run.UpdatedAt, ResolvedCommit: run.Request.Revision}
	if _, err := store.ApplyClaimedTestGenerationEvent(ctx, claim, event); err != nil {
		t.Fatal(err)
	}
	// Expire the authoritative database lease without relying on wall-clock sleeps.
	if _, err := store.db.ExecContext(ctx, `UPDATE development_test_generation_runs SET claim_lease_until = clock_timestamp() - interval '1 second' WHERE run_id = $1`, run.RunID); err != nil {
		t.Fatal(err)
	}
	claim.LeaseUntil = time.Now().Add(24 * time.Hour)
	assertLost := func(label string, stale testgenflow.FlowClaim) {
		t.Helper()
		for name, write := range map[string]func() error{
			"renew":   func() error { return store.RenewTestGenerationClaim(ctx, stale, time.Minute) },
			"release": func() error { return store.ReleaseTestGenerationClaim(ctx, stale) },
			"begin": func() error {
				_, _, err := store.BeginTestGenerationOperation(ctx, stale, "another", "source", run.RequestDigest)
				return err
			},
			"replay intent": func() error {
				_, _, err := store.BeginTestGenerationOperation(ctx, stale, key, "source", run.RequestDigest)
				return err
			},
			"finish": func() error {
				return store.FinishTestGenerationOperation(ctx, stale, key, "completed", json.RawMessage(`{"commit":"pinned"}`), "")
			},
			"duplicate event": func() error {
				_, err := store.ApplyClaimedTestGenerationEvent(ctx, stale, event)
				return err
			},
			"new event": func() error {
				_, err := store.ApplyClaimedTestGenerationEvent(ctx, stale, testgenflow.Event{ID: testID(t, "baseline"), ExpectedRevision: 2, Type: testgenflow.EventBaselineCompleted, OccurredAt: run.UpdatedAt, Succeeded: true})
				return err
			},
		} {
			if err := write(); !errors.Is(err, testgenflow.ErrFlowClaimLost) {
				t.Fatalf("%s %s = %v, want claim lost", label, name, err)
			}
		}
	}
	assertLost("expired", claim)
	reclaimed := claimTestGenerationRun(t, store, run, "holder-after-expiry")
	if reclaimed.Fence != claim.Fence+1 {
		t.Fatalf("reclaimed fence = %d, want %d", reclaimed.Fence, claim.Fence+1)
	}
	assertLost("stale fence", claim)
	forged := reclaimed
	forged.Holder = claim.Holder
	assertLost("wrong holder", forged)
	if op, created, err := store.BeginTestGenerationOperation(ctx, reclaimed, key, "source", run.RequestDigest); err != nil || created || op.State != "prepared" || op.Key != key {
		t.Fatalf("reclaimed prepared intent = %#v, created=%t, err=%v; must reconcile, not reissue", op, created, err)
	}
	if err := store.FinishTestGenerationOperation(ctx, reclaimed, key, "completed", json.RawMessage(`{"commit":"pinned"}`), ""); err != nil {
		t.Fatal(err)
	}
	ops, err := store.ListTestGenerationOperations(ctx, run.RunID)
	if err != nil || len(ops) != 1 || ops[0].State != "completed" {
		t.Fatalf("fenced writes changed operations = %#v, err = %v", ops, err)
	}
	persisted, err := store.GetRun(ctx, run.RunID)
	if err != nil || persisted.Revision != 2 || persisted.Stage != testgenflow.StageBaseline {
		t.Fatalf("fenced writes changed run = %#v, err = %v", persisted, err)
	}
}

func TestPostgresTestGenerationOperationCrashRecoveryAndEventReplay(t *testing.T) {
	ctx := context.Background()
	store := integrationStore(t)
	run := createClaimableTestGenerationRun(t, store, "")
	claim := claimTestGenerationRun(t, store, run, "before-crash")
	key := "source:1"
	prepared, created, err := store.BeginTestGenerationOperation(ctx, claim, key, "source", run.RequestDigest)
	if err != nil || !created || prepared.Key != key || prepared.RunID != run.RunID || prepared.Kind != "source" || prepared.InputDigest != run.RequestDigest || prepared.State != "prepared" || prepared.Result != nil || prepared.Diagnostic != "" || prepared.CreatedAt.IsZero() || !prepared.CreatedAt.Equal(prepared.UpdatedAt) {
		t.Fatalf("prepared operation = %#v, created=%t, err=%v", prepared, created, err)
	}
	restarted := integrationStore(t)
	replayed, created, err := restarted.BeginTestGenerationOperation(ctx, claim, key, "source", run.RequestDigest)
	if err != nil || created || !reflect.DeepEqual(replayed, prepared) {
		t.Fatalf("prepared crash replay = %#v, created=%t, err=%v", replayed, created, err)
	}
	result := json.RawMessage(`{"commit":"` + run.Request.Revision + `"}`)
	if err := restarted.FinishTestGenerationOperation(ctx, claim, key, "completed", result, ""); err != nil {
		t.Fatal(err)
	}
	// The external result is durable even if the process crashes before applying
	// its corresponding lifecycle event.
	afterSecondCrash := integrationStore(t)
	persisted, err := afterSecondCrash.GetRun(ctx, run.RunID)
	if err != nil || persisted.Revision != 1 || persisted.Stage != testgenflow.StageSource {
		t.Fatalf("operation prematurely projected event = %#v, err=%v", persisted, err)
	}
	completed, created, err := afterSecondCrash.BeginTestGenerationOperation(ctx, claim, key, "source", run.RequestDigest)
	if err != nil || created || completed.State != "completed" || string(completed.Result) != string(result) || completed.InputDigest != prepared.InputDigest {
		t.Fatalf("completed crash replay = %#v, created=%t, err=%v", completed, created, err)
	}
	if err := afterSecondCrash.FinishTestGenerationOperation(ctx, claim, key, "completed", result, ""); err != nil {
		t.Fatal(err)
	}
	ops, err := afterSecondCrash.ListTestGenerationOperations(ctx, run.RunID)
	if err != nil || len(ops) != 1 || !reflect.DeepEqual(ops[0], completed) {
		t.Fatalf("completed replay mutated outcome = %#v, err=%v", ops, err)
	}
	event := testgenflow.Event{ID: testID(t, "source-event"), ExpectedRevision: 1, Type: testgenflow.EventSourceResolved, OccurredAt: run.UpdatedAt, ResolvedCommit: run.Request.Revision}
	persisted, err = afterSecondCrash.ApplyClaimedTestGenerationEvent(ctx, claim, event)
	if err != nil || persisted.Revision != 2 || persisted.Stage != testgenflow.StageBaseline {
		t.Fatalf("project recovered result = %#v, err=%v", persisted, err)
	}
	if repeated, err := afterSecondCrash.ApplyClaimedTestGenerationEvent(ctx, claim, event); err != nil || !reflect.DeepEqual(repeated, persisted) {
		t.Fatalf("claimed event replay = %#v, err=%v", repeated, err)
	}
	conflict := event
	conflict.Diagnostic = "changed payload"
	if _, err := afterSecondCrash.ApplyClaimedTestGenerationEvent(ctx, claim, conflict); !errors.Is(err, testgenflow.ErrEventConflict) {
		t.Fatalf("same claimed event ID with changed payload = %v", err)
	}
	cancelEvent := testgenflow.Event{ID: testID(t, "cancel"), ExpectedRevision: 2, Type: testgenflow.EventCancelRequested, OccurredAt: run.UpdatedAt}
	persisted, err = afterSecondCrash.ApplyEvent(ctx, run.RunID, cancelEvent)
	if err != nil || persisted.Revision != 3 || persisted.Status != testgenflow.StatusCancelRequested {
		t.Fatalf("external cancellation while claimed = %#v, err=%v", persisted, err)
	}
	if _, err := afterSecondCrash.ApplyClaimedTestGenerationEvent(ctx, claim, testgenflow.Event{ID: testID(t, "baseline"), ExpectedRevision: 2, Type: testgenflow.EventBaselineCompleted, OccurredAt: run.UpdatedAt, Succeeded: true}); !errors.Is(err, testgenflow.ErrRevisionConflict) {
		t.Fatalf("claimed event bypassed external cancellation revision = %v", err)
	}
	if _, err := afterSecondCrash.ApplyClaimedTestGenerationEvent(ctx, claim, testgenflow.Event{ID: testID(t, "cancelled"), ExpectedRevision: 3, Type: testgenflow.EventCancelled, OccurredAt: run.UpdatedAt, TerminationConfirmed: true}); err != nil {
		t.Fatal(err)
	}
	if err := afterSecondCrash.ReleaseTestGenerationClaim(ctx, claim); err != nil {
		t.Fatal(err)
	}
	if claim, err := afterSecondCrash.ClaimNextTestGenerationRun(ctx, "after-cancellation", run.Request.ProfileDigest, time.Minute); err != nil || claim != nil {
		t.Fatalf("reclaimed terminal cancelled run = %#v, err=%v", claim, err)
	}
}

func TestPostgresTestGenerationConcurrentOperationIntentsAreCreatedOnce(t *testing.T) {
	ctx := context.Background()
	store := integrationStore(t)
	run := createClaimableTestGenerationRun(t, store, "")
	claim := claimTestGenerationRun(t, store, run, "concurrent-intent-holder")
	type outcome struct {
		op      testgenflow.Operation
		created bool
		err     error
	}
	start := make(chan struct{})
	results := make(chan outcome, 2)
	for range 2 {
		go func() {
			<-start
			op, created, err := store.BeginTestGenerationOperation(ctx, claim, "source:1", "source", run.RequestDigest)
			results <- outcome{op, created, err}
		}()
	}
	close(start)
	createdCount := 0
	for range 2 {
		result := <-results
		if result.err != nil || result.op.Key != "source:1" || result.op.State != "prepared" || result.op.InputDigest != run.RequestDigest {
			t.Fatalf("concurrent intent = %#v", result)
		}
		if result.created {
			createdCount++
		}
	}
	if createdCount != 1 {
		t.Fatalf("newly created operation intents = %d, want exactly one", createdCount)
	}
	ops, err := store.ListTestGenerationOperations(ctx, run.RunID)
	if err != nil || len(ops) != 1 || ops[0].Key != "source:1" || ops[0].State != "prepared" {
		t.Fatalf("persisted concurrent intents = %#v, err=%v", ops, err)
	}
}

func TestPostgresTestGenerationOperationBindingsAndImmutableOutcomes(t *testing.T) {
	ctx := context.Background()
	store := integrationStore(t)
	run := createClaimableTestGenerationRun(t, store, "")
	claim := claimTestGenerationRun(t, store, run, "journal-holder")
	for _, terminal := range []string{"completed", "failed"} {
		t.Run(terminal, func(t *testing.T) {
			key := "operation:" + terminal
			if _, created, err := store.BeginTestGenerationOperation(ctx, claim, key, "maven", run.RequestDigest); err != nil || !created {
				t.Fatalf("prepare created=%t, err=%v", created, err)
			}
			for _, binding := range []struct{ kind, digest string }{{"other", run.RequestDigest}, {"maven", testGenerationStateDigest([]byte("different input"))}} {
				if _, _, err := store.BeginTestGenerationOperation(ctx, claim, key, binding.kind, binding.digest); !errors.Is(err, testgenflow.ErrOperationConflict) {
					t.Fatalf("changed operation binding = %v", err)
				}
			}
			if err := store.FinishTestGenerationOperation(ctx, claim, key, terminal, json.RawMessage(`{"exit_code":1}`), "bounded category"); err != nil {
				t.Fatal(err)
			}
			for _, mutation := range []struct{ state, result, diagnostic string }{
				{"unknown", `{"exit_code":1}`, "bounded category"},
				{terminal, `{"exit_code":0}`, "bounded category"},
				{terminal, `{"exit_code":1}`, "changed category"},
			} {
				if err := store.FinishTestGenerationOperation(ctx, claim, key, mutation.state, json.RawMessage(mutation.result), mutation.diagnostic); !errors.Is(err, testgenflow.ErrOperationConflict) {
					t.Fatalf("mutated terminal operation = %v", err)
				}
			}
			op, created, err := store.BeginTestGenerationOperation(ctx, claim, key, "maven", run.RequestDigest)
			if err != nil || created || op.State != terminal || string(op.Result) != `{"exit_code":1}` || op.Diagnostic != "bounded category" {
				t.Fatalf("immutable operation = %#v, created=%t, err=%v", op, created, err)
			}
		})
	}
	if err := store.FinishTestGenerationOperation(ctx, claim, "missing", "completed", nil, ""); !errors.Is(err, testgenflow.ErrOperationConflict) {
		t.Fatalf("finish without durable intent = %v", err)
	}
	if _, _, err := store.BeginTestGenerationOperation(ctx, claim, "unknown", "maven", run.RequestDigest); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishTestGenerationOperation(ctx, claim, "unknown", "unknown", nil, "worker outcome unknown"); err != nil {
		t.Fatal(err)
	}
	if op, created, err := store.BeginTestGenerationOperation(ctx, claim, "unknown", "maven", run.RequestDigest); err != nil || created || op.State != "unknown" || string(op.Result) != "null" {
		t.Fatalf("unknown replay = %#v, created=%t, err=%v", op, created, err)
	}
	if err := store.FinishTestGenerationOperation(ctx, claim, "unknown", "completed", json.RawMessage(`{"exit_code":0}`), ""); err != nil {
		t.Fatalf("resolve unknown outcome: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `DELETE FROM development_test_generation_runs WHERE run_id = $1`, run.RunID); err != nil {
		t.Fatal(err)
	}
	if ops, err := store.ListTestGenerationOperations(ctx, run.RunID); err != nil || len(ops) != 0 {
		t.Fatalf("operation cascade = %#v, err=%v", ops, err)
	}
}

func TestPostgresTestGenerationOperationValidationRejectsUnsafePayloads(t *testing.T) {
	ctx := context.Background()
	store := integrationStore(t)
	run := createClaimableTestGenerationRun(t, store, "")
	claim := claimTestGenerationRun(t, store, run, "validation-holder")
	for _, input := range []struct{ key, kind, digest string }{
		{"", "source", run.RequestDigest},
		{"key\n", "source", run.RequestDigest},
		{strings.Repeat("k", 257), "source", run.RequestDigest},
		{"key", "bad kind", run.RequestDigest},
		{"key", "source", "sha256:bad"},
	} {
		if _, _, err := store.BeginTestGenerationOperation(ctx, claim, input.key, input.kind, input.digest); err == nil {
			t.Fatal("invalid operation binding accepted")
		}
	}
	if _, _, err := store.BeginTestGenerationOperation(ctx, claim, "valid", "source", strings.ToUpper(run.RequestDigest[7:])); err == nil {
		t.Fatal("digest without algorithm prefix accepted")
	}
	if _, created, err := store.BeginTestGenerationOperation(ctx, claim, "valid", "source", "sha256:"+strings.ToUpper(run.RequestDigest[7:])); err != nil || !created {
		t.Fatalf("uppercase digest created=%t, err=%v", created, err)
	}
	for _, input := range []struct{ state, result, diagnostic string }{
		{"prepared", `{}`, ""},
		{"completed", `{`, ""},
		{"completed", `{}` + `{}`, ""},
		{"completed", `"` + strings.Repeat("x", 65536) + `"`, ""},
		{"completed", `{}`, strings.Repeat("x", 1025)},
		{"completed", `{}`, "untrusted\nlog"},
		{"failed", `{}`, "password=do-not-persist"},
		{"failed", `{"token":"do-not-persist"}`, ""},
		{"failed", `{"authorization":"Bearer do-not-persist"}`, ""},
		{"failed", `{"url":"https://user:credential@example.invalid/repo"}`, ""},
		{"failed", `{"url":"https://credential@example.invalid/repo"}`, ""},
	} {
		if err := store.FinishTestGenerationOperation(ctx, claim, "valid", input.state, json.RawMessage(input.result), input.diagnostic); err == nil {
			t.Fatal("unsafe operation outcome accepted")
		}
	}
	ops, err := store.ListTestGenerationOperations(ctx, run.RunID)
	if err != nil || len(ops) != 1 || ops[0].Key != "valid" || ops[0].State != "prepared" || ops[0].Result != nil || ops[0].Diagnostic != "" || ops[0].InputDigest != run.RequestDigest {
		t.Fatalf("rejected input leaked into journal = %#v, err=%v", ops, err)
	}
}

func TestPostgresLocalTestGenerationMigrationPreservesHistoricalPayload(t *testing.T) {
	ctx := context.Background()
	store := integrationStore(t)
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	schema := strings.ReplaceAll(testID(t, "flow_migration"), "-", "_")
	if _, err := tx.ExecContext(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `SET LOCAL search_path TO `+schema); err != nil {
		t.Fatal(err)
	}
	initial, err := migrationFiles.ReadFile("migrations/0003_test_generation_development_runs.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, string(initial)); err != nil {
		t.Fatal(err)
	}
	legacyRequest := testgenflow.Request{RepositoryURL: "https://git.example/group/repository.git", Revision: strings.Repeat("a", 40), BaseBranch: "main", Subdirectory: ".", ProfileID: "spring-java17", ProfileVersion: "1.0.0", ProfileDigest: "sha256:" + strings.Repeat("b", 64)}
	run, err := testgenflow.NewRun("historical-run", "historical-idempotency", legacyRequest, 6, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), "input_mode") || strings.Contains(string(payload), "completion_mode") {
		t.Fatal("historical payload unexpectedly includes new fields")
	}
	digest := testGenerationStateDigest(payload)
	if _, err := tx.ExecContext(ctx, `INSERT INTO development_test_generation_runs(run_id,idempotency_key,request_digest,state_digest,state_bytes,status,stage,revision,next_action_at,created_at,updated_at) VALUES ($1,$2,$3,$4,$5,'queued','source',1,$6,$6,$6)`, run.RunID, run.IdempotencyKey, run.RequestDigest, digest, payload, run.CreatedAt); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO development_test_generation_run_events(run_id,event_id,expected_revision,revision,event_type,payload_digest,payload_bytes,occurred_at) VALUES ($1,'created',0,1,'run_created',$2,$3,$4)`, run.RunID, digest, payload, run.CreatedAt); err != nil {
		t.Fatal(err)
	}
	upgrade, err := migrationFiles.ReadFile("migrations/0004_local_test_generation.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, string(upgrade)); err != nil {
		t.Fatalf("upgrade populated 0003 schema: %v", err)
	}
	persisted, err := scanTestGenerationRun(tx.QueryRowContext(ctx, testGenerationRunSelect+` WHERE run_id = $1`, run.RunID))
	if err != nil || !reflect.DeepEqual(persisted, run) {
		t.Fatalf("migrated historical run = %#v, err=%v", persisted, err)
	}
	var eventPayload []byte
	var eventDigest string
	if err := tx.QueryRowContext(ctx, `SELECT payload_bytes,payload_digest FROM development_test_generation_run_events WHERE run_id = $1`, run.RunID).Scan(&eventPayload, &eventDigest); err != nil || string(eventPayload) != string(payload) || eventDigest != digest {
		t.Fatalf("migration changed event bytes/digest: digest=%s err=%v", eventDigest, err)
	}
	for _, stage := range []string{"artifact_input", "publish_patch"} {
		if _, err := tx.ExecContext(ctx, `UPDATE development_test_generation_runs SET stage = $2 WHERE run_id = $1`, run.RunID, stage); err != nil {
			t.Fatalf("new stage %s rejected by migration: %v", stage, err)
		}
	}
}

func createClaimableTestGenerationRun(t *testing.T, store *Store, profileDigest string) testgenflow.Run {
	t.Helper()
	if profileDigest == "" {
		profileDigest = testGenerationStateDigest([]byte(testID(t, "unique-profile")))
	}
	request := testgenflow.Request{
		RepositoryURL: "https://git.example/group/spring-demo.git", Revision: strings.Repeat("a", 40),
		BaseBranch: "main", Subdirectory: ".", ProfileID: "spring-java17-maven", ProfileVersion: "1.0.0", ProfileDigest: profileDigest,
		InputMode: testgenflow.InputModeArtifactInput, InputArtifactDigest: "sha256:" + strings.Repeat("b", 64), CompletionMode: testgenflow.CompletionModePatchOnly,
	}
	return createTestGenerationRun(t, store, request, time.Now().Add(-time.Second))
}

func createTestGenerationRun(t *testing.T, store *Store, request testgenflow.Request, now time.Time) testgenflow.Run {
	t.Helper()
	run, err := testgenflow.NewRun(testID(t, "claim-run"), testID(t, "claim-idempotency"), request, 6, now)
	if err != nil {
		t.Fatal(err)
	}
	created, inserted, err := store.CreateRun(context.Background(), run)
	if err != nil || !inserted {
		t.Fatalf("create claimable run = %#v, inserted=%t, err=%v", created, inserted, err)
	}
	cleanupTestGenerationFlowRun(t, store, run.RunID)
	return created
}

func claimTestGenerationRun(t *testing.T, store *Store, run testgenflow.Run, holder string) testgenflow.FlowClaim {
	t.Helper()
	claim, err := store.ClaimNextTestGenerationRun(context.Background(), holder, run.Request.ProfileDigest, time.Minute)
	if err != nil || claim == nil || claim.RunID != run.RunID {
		t.Fatalf("claim = %#v, err=%v; want run %s", claim, err, run.RunID)
	}
	return *claim
}
