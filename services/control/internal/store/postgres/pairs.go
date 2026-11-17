package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/project-lightpath/ai-test-generation/services/control/internal/execution"
)

func (s *Store) CreatePair(ctx context.Context, input execution.PairInput) (execution.PairRecord, bool, error) {
	if !executionValidID(input.IdempotencyKey) || strings.TrimSpace(input.IdempotencyKey) != input.IdempotencyKey || input.CreatedAt.IsZero() {
		return execution.PairRecord{}, false, fmt.Errorf("pair idempotency key and creation time are required")
	}
	base, err := execution.DecodeTaskSpec(input.BaseSpec)
	if err != nil {
		return execution.PairRecord{}, false, err
	}
	candidate, err := execution.DecodeTaskSpec(input.CandidateSpec)
	if err != nil {
		return execution.PairRecord{}, false, err
	}
	if err := execution.ValidatePair(base, candidate); err != nil {
		return execution.PairRecord{}, false, err
	}

	digest := input.RequestDigest()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return execution.PairRecord{}, false, fmt.Errorf("begin create execution pair: %w", err)
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx, `
		INSERT INTO execution_pairs(pair_id, run_id, generation_id, comparison_mode, idempotency_key, request_digest, base_task_id, candidate_task_id, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (run_id, idempotency_key) DO NOTHING`,
		base.PairID, base.RunID, base.GenerationID, base.ComparisonMode, input.IdempotencyKey, digest,
		base.TaskSpecID, candidate.TaskSpecID, input.CreatedAt.UTC(),
	)
	if err != nil {
		return execution.PairRecord{}, false, fmt.Errorf("insert execution pair: %w", err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return execution.PairRecord{}, false, fmt.Errorf("inspect pair insertion: %w", err)
	}
	if inserted == 0 {
		pair, err := scanPair(tx.QueryRowContext(ctx, `
			SELECT pair_id, run_id, generation_id, base_task_id, candidate_task_id, request_digest, created_at
			FROM execution_pairs WHERE run_id = $1 AND idempotency_key = $2 FOR UPDATE`, base.RunID, input.IdempotencyKey))
		if err != nil {
			return execution.PairRecord{}, false, fmt.Errorf("load idempotent execution pair: %w", err)
		}
		if pair.RequestDigest != digest {
			return execution.PairRecord{}, false, execution.ErrIdempotencyConflict
		}
		if err := tx.Commit(); err != nil {
			return execution.PairRecord{}, false, fmt.Errorf("commit idempotent execution pair read: %w", err)
		}
		return pair, false, nil
	}

	for _, spec := range []struct {
		document []byte
		parsed   execution.TaskSpec
	}{{input.BaseSpec, base}, {input.CandidateSpec, candidate}} {
		taskDigest := execution.DigestTaskSpec(spec.document)
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO execution_tasks(task_id, pair_id, run_id, subject_role, specification_bytes, specification_digest, status, revision, attempt_count, next_action_at, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, 'queued', 1, 0, $7, $7, $7)`,
			spec.parsed.TaskSpecID, base.PairID, base.RunID, spec.parsed.SubjectRole, spec.document, taskDigest, input.CreatedAt.UTC(),
		); err != nil {
			return execution.PairRecord{}, false, fmt.Errorf("insert %s execution task: %w", spec.parsed.SubjectRole, err)
		}
		payload, err := json.Marshal(struct {
			TaskID string `json:"task_id"`
			Status string `json:"status"`
		}{TaskID: spec.parsed.TaskSpecID, Status: "queued"})
		if err != nil {
			return execution.PairRecord{}, false, fmt.Errorf("encode task-created event: %w", err)
		}
		if err := appendTaskEvent(ctx, tx, spec.parsed.TaskSpecID, 0, 1, "task_created", payload, input.CreatedAt.UTC()); err != nil {
			return execution.PairRecord{}, false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return execution.PairRecord{}, false, fmt.Errorf("commit execution pair: %w", err)
	}
	return execution.PairRecord{
		PairID: base.PairID, RunID: base.RunID, GenerationID: base.GenerationID,
		BaseTaskID: base.TaskSpecID, CandidateTaskID: candidate.TaskSpecID,
		RequestDigest: digest, CreatedAt: input.CreatedAt.UTC(),
	}, true, nil
}

func (s *Store) GetPair(ctx context.Context, pairID string) (execution.PairRecord, error) {
	pair, err := scanPair(s.db.QueryRowContext(ctx, `
		SELECT pair_id, run_id, generation_id, base_task_id, candidate_task_id, request_digest, created_at
		FROM execution_pairs WHERE pair_id = $1`, pairID))
	if errors.Is(err, sql.ErrNoRows) {
		return execution.PairRecord{}, execution.ErrPairNotFound
	}
	if err != nil {
		return execution.PairRecord{}, fmt.Errorf("load execution pair: %w", err)
	}
	return pair, nil
}

func (s *Store) GetTask(ctx context.Context, taskID string) (execution.TaskRecord, error) {
	task, err := scanTask(s.db.QueryRowContext(ctx, taskSelect+` WHERE task_id = $1`, taskID))
	if errors.Is(err, sql.ErrNoRows) {
		return execution.TaskRecord{}, execution.ErrTaskNotFound
	}
	if err != nil {
		return execution.TaskRecord{}, fmt.Errorf("load execution task: %w", err)
	}
	return task, nil
}

const taskSelect = `
	SELECT task_id, run_id, pair_id, subject_role, specification_bytes, specification_digest,
	       status, revision, attempt_count, COALESCE(active_attempt_id, ''), next_action_at,
	       COALESCE(terminal_reason, ''), created_at, updated_at
	FROM execution_tasks`

type rowScanner interface {
	Scan(...any) error
}

func scanPair(row rowScanner) (execution.PairRecord, error) {
	var pair execution.PairRecord
	err := row.Scan(&pair.PairID, &pair.RunID, &pair.GenerationID, &pair.BaseTaskID, &pair.CandidateTaskID, &pair.RequestDigest, &pair.CreatedAt)
	return pair, err
}

func scanTask(row rowScanner) (execution.TaskRecord, error) {
	var task execution.TaskRecord
	err := row.Scan(
		&task.TaskID, &task.RunID, &task.PairID, &task.SubjectRole, &task.Specification,
		&task.SpecificationDigest, &task.Status, &task.Revision, &task.AttemptCount,
		&task.ActiveAttemptID, &task.NextActionAt, &task.TerminalReason, &task.CreatedAt, &task.UpdatedAt,
	)
	if err == nil && execution.DigestTaskSpec(task.Specification) != task.SpecificationDigest {
		return execution.TaskRecord{}, execution.ErrStoredTaskCorrupt
	}
	return task, err
}

func appendTaskEvent(ctx context.Context, tx *sql.Tx, taskID string, expectedRevision, revision int64, eventType string, payload []byte, occurredAt time.Time) error {
	eventID, err := newID("event")
	if err != nil {
		return err
	}
	payloadDigest := execution.DigestTaskSpec(payload)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO execution_task_events(event_id, task_id, expected_revision, revision, event_type, payload_digest, payload_bytes, occurred_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, eventID, taskID, expectedRevision, revision, eventType, payloadDigest, payload, occurredAt); err != nil {
		return fmt.Errorf("append %s event for task %s: %w", eventType, taskID, err)
	}
	return nil
}

func executionValidID(value string) bool {
	return value != "" && len(value) <= 256
}
