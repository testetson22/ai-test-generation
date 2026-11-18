package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/project-lightpath/ai-test-generation/services/control/internal/testgenflow"
)

func (s *Store) CreateRun(ctx context.Context, run testgenflow.Run) (testgenflow.Run, bool, error) {
	if s == nil || s.db == nil {
		return testgenflow.Run{}, false, fmt.Errorf("test-generation run store is unavailable")
	}
	if err := run.Validate(); err != nil {
		return testgenflow.Run{}, false, err
	}
	stateBytes, err := json.Marshal(run)
	if err != nil {
		return testgenflow.Run{}, false, fmt.Errorf("encode test-generation run state: %w", err)
	}
	stateDigest := testGenerationStateDigest(stateBytes)
	eventID, err := newID("testgen-event")
	if err != nil {
		return testgenflow.Run{}, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return testgenflow.Run{}, false, fmt.Errorf("begin create test-generation run: %w", err)
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx, `
		INSERT INTO development_test_generation_runs(
			run_id, idempotency_key, request_digest, state_digest, state_bytes,
			status, stage, revision, next_action_at, cancel_requested_at, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		ON CONFLICT (idempotency_key) DO NOTHING`,
		run.RunID, run.IdempotencyKey, run.RequestDigest, stateDigest, stateBytes,
		string(run.Status), string(run.Stage), run.Revision, run.CreatedAt.UTC(), nullableTime(run.CancelRequestedAt), run.CreatedAt.UTC(), run.UpdatedAt.UTC(),
	)
	if err != nil {
		return testgenflow.Run{}, false, fmt.Errorf("insert test-generation run: %w", err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return testgenflow.Run{}, false, fmt.Errorf("inspect test-generation run insertion: %w", err)
	}
	if inserted == 0 {
		existing, err := scanTestGenerationRun(tx.QueryRowContext(ctx, testGenerationRunSelect+` WHERE idempotency_key = $1 FOR UPDATE`, run.IdempotencyKey))
		if err != nil {
			return testgenflow.Run{}, false, fmt.Errorf("load idempotent test-generation run: %w", err)
		}
		if existing.RequestDigest != run.RequestDigest {
			return testgenflow.Run{}, false, testgenflow.ErrIdempotencyConflict
		}
		if err := tx.Commit(); err != nil {
			return testgenflow.Run{}, false, fmt.Errorf("commit idempotent test-generation run read: %w", err)
		}
		return existing, false, nil
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO development_test_generation_run_events(run_id, event_id, expected_revision, revision, event_type, payload_digest, payload_bytes, occurred_at)
		VALUES ($1, $2, 0, $3, 'run_created', $4, $5, $6)`,
		run.RunID, eventID, run.Revision, stateDigest, stateBytes, run.CreatedAt.UTC(),
	); err != nil {
		return testgenflow.Run{}, false, fmt.Errorf("record test-generation run creation event: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return testgenflow.Run{}, false, fmt.Errorf("commit test-generation run creation: %w", err)
	}
	return run, true, nil
}

func (s *Store) GetRun(ctx context.Context, runID string) (testgenflow.Run, error) {
	if s == nil || s.db == nil {
		return testgenflow.Run{}, fmt.Errorf("test-generation run store is unavailable")
	}
	run, err := scanTestGenerationRun(s.db.QueryRowContext(ctx, testGenerationRunSelect+` WHERE run_id = $1`, runID))
	if errors.Is(err, sql.ErrNoRows) {
		return testgenflow.Run{}, testgenflow.ErrRunNotFound
	}
	if err != nil {
		return testgenflow.Run{}, fmt.Errorf("load test-generation run: %w", err)
	}
	return run, nil
}

func (s *Store) ApplyEvent(ctx context.Context, runID string, event testgenflow.Event) (testgenflow.Run, error) {
	if s == nil || s.db == nil {
		return testgenflow.Run{}, fmt.Errorf("test-generation run store is unavailable")
	}
	eventBytes, err := json.Marshal(event)
	if err != nil {
		return testgenflow.Run{}, fmt.Errorf("encode test-generation run event: %w", err)
	}
	eventDigest := testGenerationStateDigest(eventBytes)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return testgenflow.Run{}, fmt.Errorf("begin test-generation run transition: %w", err)
	}
	defer tx.Rollback()

	run, err := scanTestGenerationRun(tx.QueryRowContext(ctx, testGenerationRunSelect+` WHERE run_id = $1 FOR UPDATE`, runID))
	if errors.Is(err, sql.ErrNoRows) {
		return testgenflow.Run{}, testgenflow.ErrRunNotFound
	}
	if err != nil {
		return testgenflow.Run{}, fmt.Errorf("load test-generation run for transition: %w", err)
	}
	var priorDigest string
	err = tx.QueryRowContext(ctx, `SELECT payload_digest FROM development_test_generation_run_events WHERE run_id = $1 AND event_id = $2`, runID, event.ID).Scan(&priorDigest)
	if err == nil {
		if priorDigest != eventDigest {
			return testgenflow.Run{}, testgenflow.ErrEventConflict
		}
		if err := tx.Commit(); err != nil {
			return testgenflow.Run{}, fmt.Errorf("commit duplicate test-generation run event: %w", err)
		}
		return run, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return testgenflow.Run{}, fmt.Errorf("check prior test-generation run event: %w", err)
	}
	if event.ExpectedRevision != run.Revision {
		return testgenflow.Run{}, testgenflow.ErrRevisionConflict
	}
	next, err := run.Apply(event)
	if err != nil {
		return testgenflow.Run{}, err
	}
	stateBytes, err := json.Marshal(next)
	if err != nil {
		return testgenflow.Run{}, fmt.Errorf("encode next test-generation run state: %w", err)
	}
	stateDigest := testGenerationStateDigest(stateBytes)
	nextActionAt := any(nil)
	if next.Status == testgenflow.StatusQueued || next.Status == testgenflow.StatusRunning || next.Status == testgenflow.StatusCancelRequested {
		nextActionAt = next.UpdatedAt.UTC()
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE development_test_generation_runs SET
			state_digest = $2, state_bytes = $3, status = $4, stage = $5, revision = $6,
			next_action_at = $7, cancel_requested_at = $8, updated_at = $9
		WHERE run_id = $1 AND revision = $10`,
		runID, stateDigest, stateBytes, string(next.Status), string(next.Stage), next.Revision,
		nextActionAt, nullableTime(next.CancelRequestedAt), next.UpdatedAt.UTC(), run.Revision,
	)
	if err != nil {
		return testgenflow.Run{}, fmt.Errorf("update test-generation run state: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return testgenflow.Run{}, fmt.Errorf("inspect test-generation run transition: %w", err)
	}
	if updated != 1 {
		return testgenflow.Run{}, testgenflow.ErrRevisionConflict
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO development_test_generation_run_events(run_id, event_id, expected_revision, revision, event_type, payload_digest, payload_bytes, occurred_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		runID, event.ID, event.ExpectedRevision, next.Revision, string(event.Type), eventDigest, eventBytes, event.OccurredAt.UTC(),
	); err != nil {
		return testgenflow.Run{}, fmt.Errorf("append test-generation run event: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return testgenflow.Run{}, fmt.Errorf("commit test-generation run transition: %w", err)
	}
	return next, nil
}

const testGenerationRunSelect = `
	SELECT run_id, idempotency_key, request_digest, state_digest, state_bytes,
	       status, stage, revision, next_action_at, cancel_requested_at, created_at, updated_at
	FROM development_test_generation_runs`

func scanTestGenerationRun(row rowScanner) (testgenflow.Run, error) {
	var runID, idempotencyKey, requestDigest, stateDigest, status, stage string
	var stateBytes []byte
	var revision int64
	var nextActionAt, cancelRequestedAt sql.NullTime
	var createdAt, updatedAt time.Time
	if err := row.Scan(
		&runID, &idempotencyKey, &requestDigest, &stateDigest, &stateBytes,
		&status, &stage, &revision, &nextActionAt, &cancelRequestedAt, &createdAt, &updatedAt,
	); err != nil {
		return testgenflow.Run{}, err
	}
	if testGenerationStateDigest(stateBytes) != stateDigest {
		return testgenflow.Run{}, testgenflow.ErrRunCorrupt
	}
	decoder := json.NewDecoder(bytes.NewReader(stateBytes))
	decoder.DisallowUnknownFields()
	var run testgenflow.Run
	if err := decoder.Decode(&run); err != nil {
		return testgenflow.Run{}, testgenflow.ErrRunCorrupt
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return testgenflow.Run{}, testgenflow.ErrRunCorrupt
	}
	if err := run.Validate(); err != nil || run.RunID != runID || run.IdempotencyKey != idempotencyKey ||
		run.RequestDigest != requestDigest || string(run.Status) != status || string(run.Stage) != stage ||
		run.Revision != revision || !run.CreatedAt.Equal(createdAt) || !run.UpdatedAt.Equal(updatedAt) {
		return testgenflow.Run{}, testgenflow.ErrRunCorrupt
	}
	if cancelRequestedAt.Valid != (run.CancelRequestedAt != nil) || cancelRequestedAt.Valid && !run.CancelRequestedAt.Equal(cancelRequestedAt.Time) {
		return testgenflow.Run{}, testgenflow.ErrRunCorrupt
	}
	if nextActionAt.Valid != (run.Status == testgenflow.StatusQueued || run.Status == testgenflow.StatusRunning || run.Status == testgenflow.StatusCancelRequested) {
		return testgenflow.Run{}, testgenflow.ErrRunCorrupt
	}
	if nextActionAt.Valid {
		expectedNextActionAt := run.UpdatedAt
		if run.Status == testgenflow.StatusQueued {
			expectedNextActionAt = run.CreatedAt
		}
		if !nextActionAt.Time.Equal(expectedNextActionAt) {
			return testgenflow.Run{}, testgenflow.ErrRunCorrupt
		}
	}
	return run, nil
}

func nullableTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return value.UTC()
}

func testGenerationStateDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}
