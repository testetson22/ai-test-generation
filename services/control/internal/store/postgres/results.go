package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/project-lightpath/ai-test-generation/services/control/internal/execution"
	"github.com/project-lightpath/ai-test-generation/services/control/internal/provider"
)

func (s *Store) RecordProvisional(ctx context.Context, result execution.ExecutionResult, reference execution.Artifact) error {
	if result.Status != execution.ResultProvisional {
		return fmt.Errorf("provisional result delivery requires provisional status")
	}
	if err := validateResultReference(result, reference); err != nil {
		return err
	}
	encodedRef, err := json.Marshal(reference)
	if err != nil {
		return fmt.Errorf("encode provisional result reference: %w", err)
	}
	updatedAt, err := time.Parse(time.RFC3339Nano, result.UpdatedAt)
	if err != nil {
		return fmt.Errorf("parse provisional result time: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin provisional result delivery: %w", err)
	}
	defer tx.Rollback()
	task, attempt, spec, err := lockExecutionResult(ctx, tx, result)
	if err != nil {
		return err
	}
	if err := matchResultToSpec(result, task, attempt, spec); err != nil {
		return err
	}
	delivery, err := loadAttemptResult(ctx, tx, result.AttemptID)
	if err == nil {
		if delivery.provisional == reference && delivery.expectedRevision == result.ExpectedTaskRevision {
			return tx.Commit()
		}
		return execution.ErrResultConflict
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if !eligibleResultState(attempt, result) {
		return execution.ErrResultNotReady
	}
	validRevision, err := resultRevisionMatches(ctx, tx, task, result)
	if err != nil {
		return err
	}
	if !validRevision {
		return execution.ErrResultNotReady
	}
	var projectedRevision any
	if resultAppliesToTask(task, result) {
		if task.ExecutionResult != nil {
			return execution.ErrResultConflict
		}
		payload := map[string]any{"attempt_id": result.AttemptID, "result": reference, "result_status": result.Status}
		if err := transitionTask(ctx, tx, task, task.Status, task.TerminalReason, updatedAt, updatedAt, "result_provisional", payload); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE execution_tasks SET execution_result_bytes = $2 WHERE task_id = $1`, task.TaskID, encodedRef); err != nil {
			return fmt.Errorf("persist provisional execution result reference: %w", err)
		}
		projectedRevision = task.Revision + 1
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO execution_attempt_results(attempt_id, expected_task_revision, provisional_reference_bytes, projected_revision, provisional_at)
		VALUES ($1, $2, $3, $4, $5)`, result.AttemptID, result.ExpectedTaskRevision, encodedRef, projectedRevision, updatedAt); err != nil {
		return fmt.Errorf("retain provisional attempt result: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit provisional result delivery: %w", err)
	}
	return nil
}

type attemptResult struct {
	expectedRevision  int64
	projectedRevision sql.NullInt64
	provisional       execution.Artifact
	finalized         *execution.Artifact
}

func loadAttemptResult(ctx context.Context, tx *sql.Tx, attemptID string) (attemptResult, error) {
	var delivery attemptResult
	var provisional, finalized []byte
	err := tx.QueryRowContext(ctx, `
		SELECT expected_task_revision, projected_revision, provisional_reference_bytes, finalized_reference_bytes
		FROM execution_attempt_results WHERE attempt_id = $1 FOR UPDATE`, attemptID).Scan(
		&delivery.expectedRevision, &delivery.projectedRevision, &provisional, &finalized,
	)
	if err != nil {
		return delivery, err
	}
	if json.Unmarshal(provisional, &delivery.provisional) != nil || delivery.provisional.Validate() != nil {
		return delivery, execution.ErrStoredTaskCorrupt
	}
	if len(finalized) > 0 {
		var reference execution.Artifact
		if json.Unmarshal(finalized, &reference) != nil || reference.Validate() != nil {
			return delivery, execution.ErrStoredTaskCorrupt
		}
		delivery.finalized = &reference
	}
	return delivery, nil
}

func resultAppliesToTask(task execution.TaskRecord, result execution.ExecutionResult) bool {
	return task.ActiveAttemptID == result.AttemptID && (task.Status == "collecting" || task.Status == "failed" || task.Status == "cancel_requested" || task.Status == "cancelled" || task.Status == "outcome_unknown")
}

func resultRevisionMatches(ctx context.Context, tx *sql.Tx, task execution.TaskRecord, result execution.ExecutionResult) (bool, error) {
	if task.ActiveAttemptID == result.AttemptID && task.Revision == result.ExpectedTaskRevision {
		return true, nil
	}
	// A retry or cancellation may have advanced the task while evidence was in
	// transit. Require an exact observation/cancellation revision bound to that
	// attempt; retaining old evidence must not advance the current task.
	var matches bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM execution_task_events
		WHERE task_id = $1 AND revision = $2 AND (
		  (event_type = 'provider_observed'
		   AND convert_from(payload_bytes, 'UTF8')::jsonb ->> 'attempt_id' = $3
		   AND convert_from(payload_bytes, 'UTF8')::jsonb ->> 'provider_status' IN ('completed', 'failed', 'cancelled', 'outcome_unknown'))
		  OR (event_type = 'cancellation_requested'
		      AND convert_from(payload_bytes, 'UTF8')::jsonb ->> 'active_attempt_id' = $3)
		  OR (event_type = 'execution_deadline_elapsed'
		      AND convert_from(payload_bytes, 'UTF8')::jsonb ->> 'attempt_id' = $3)
		)
	)`, task.TaskID, result.ExpectedTaskRevision, result.AttemptID).Scan(&matches)
	if err != nil {
		return false, fmt.Errorf("validate retained result revision: %w", err)
	}
	return matches, nil
}

func (s *Store) RecordFinalized(ctx context.Context, result execution.ExecutionResult, provisional, finalized execution.Artifact) error {
	if result.Status == execution.ResultProvisional {
		return fmt.Errorf("final result delivery requires finalized status")
	}
	if err := validateResultReference(result, finalized); err != nil {
		return err
	}
	if provisional.ArtifactID != result.ResultID || provisional.MediaType != execution.ExecutionResultMediaType || provisional.Validate() != nil {
		return fmt.Errorf("provisional result reference is invalid")
	}
	encodedRef, err := json.Marshal(finalized)
	if err != nil {
		return fmt.Errorf("encode finalized result reference: %w", err)
	}
	updatedAt, err := time.Parse(time.RFC3339Nano, result.UpdatedAt)
	if err != nil {
		return fmt.Errorf("parse finalized result time: %w", err)
	}
	status, reason, err := finalTaskStatus(result)
	if err != nil {
		return err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin finalized result delivery: %w", err)
	}
	defer tx.Rollback()
	task, attempt, spec, err := lockExecutionResult(ctx, tx, result)
	if err != nil {
		return err
	}
	if err := matchResultToSpec(result, task, attempt, spec); err != nil {
		return err
	}
	delivery, err := loadAttemptResult(ctx, tx, result.AttemptID)
	if errors.Is(err, sql.ErrNoRows) {
		return execution.ErrResultConflict
	}
	if err != nil {
		return err
	}
	if delivery.expectedRevision != result.ExpectedTaskRevision || delivery.provisional != provisional {
		return execution.ErrResultConflict
	}
	if delivery.finalized != nil {
		if *delivery.finalized == finalized {
			return tx.Commit()
		}
		return execution.ErrResultConflict
	}
	if !eligibleResultState(attempt, result) {
		return execution.ErrResultConflict
	}
	if resultAppliesToTask(task, result) {
		if task.ExecutionResult == nil || *task.ExecutionResult != provisional || !delivery.projectedRevision.Valid ||
			(task.Revision != delivery.projectedRevision.Int64 && task.CancelRequestedAt == nil) {
			return execution.ErrResultConflict
		}
		if task.CancelRequestedAt != nil {
			status, reason = "cancel_requested", "cancellation requested"
			if attempt.terminationConfirmed || result.Cleanup.TerminationConfirmed {
				status = "cancelled"
			}
		} else if task.Status != "collecting" {
			status, reason = task.Status, task.TerminalReason
		}
		payload := map[string]any{
			"attempt_id": result.AttemptID, "provisional_result": provisional,
			"result": finalized, "result_status": result.Status, "cleanup": result.Cleanup,
		}
		if err := transitionTask(ctx, tx, task, status, reason, updatedAt, updatedAt, "execution_result_finalized", payload); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE execution_tasks SET execution_result_bytes = $2 WHERE task_id = $1`, task.TaskID, encodedRef); err != nil {
			return fmt.Errorf("persist finalized execution result reference: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE execution_attempt_results
		SET finalized_reference_bytes = $2, finalized_at = $3 WHERE attempt_id = $1`, result.AttemptID, encodedRef, updatedAt); err != nil {
		return fmt.Errorf("retain finalized attempt result: %w", err)
	}
	if result.Cleanup.TerminationConfirmed {
		if _, err := tx.ExecContext(ctx, `UPDATE execution_attempts SET termination_confirmed = TRUE WHERE attempt_id = $1`, result.AttemptID); err != nil {
			return fmt.Errorf("retain attempt termination proof: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit finalized result delivery: %w", err)
	}
	return nil
}

func eligibleResultState(attempt resultAttempt, result execution.ExecutionResult) bool {
	switch attempt.status {
	case "completed":
		return true
	case "failed":
		return result.ObservedOutcome == execution.ResultFailed || result.ObservedOutcome == execution.ResultOutcomeUnknown
	case "cancelled":
		return result.ObservedOutcome == execution.ResultCancelled || result.ObservedOutcome == execution.ResultOutcomeUnknown
	case "outcome_unknown":
		return result.ObservedOutcome == execution.ResultOutcomeUnknown
	default:
		return false
	}
}

type resultAttempt struct {
	status               string
	attemptNumber        int
	taskSpecDigest       string
	providerIdentity     provider.Identity
	providerJobID        string
	terminationConfirmed bool
}

func lockExecutionResult(ctx context.Context, tx *sql.Tx, result execution.ExecutionResult) (execution.TaskRecord, resultAttempt, execution.TaskSpec, error) {
	task, err := scanTask(tx.QueryRowContext(ctx, taskSelect+` WHERE task_id = $1 FOR UPDATE`, result.TaskID))
	if errors.Is(err, sql.ErrNoRows) {
		return execution.TaskRecord{}, resultAttempt{}, execution.TaskSpec{}, execution.ErrTaskNotFound
	}
	if err != nil {
		return execution.TaskRecord{}, resultAttempt{}, execution.TaskSpec{}, fmt.Errorf("lock execution task for result: %w", err)
	}
	var attempt resultAttempt
	var providerBytes []byte
	if err := tx.QueryRowContext(ctx, `
		SELECT status, attempt_number, task_spec_digest, provider_identity_bytes, COALESCE(provider_job_id, ''), termination_confirmed
		FROM execution_attempts WHERE attempt_id = $1 AND task_id = $2 FOR UPDATE`, result.AttemptID, result.TaskID).Scan(
		&attempt.status, &attempt.attemptNumber, &attempt.taskSpecDigest, &providerBytes, &attempt.providerJobID, &attempt.terminationConfirmed,
	); errors.Is(err, sql.ErrNoRows) {
		return execution.TaskRecord{}, resultAttempt{}, execution.TaskSpec{}, execution.ErrResultNotReady
	} else if err != nil {
		return execution.TaskRecord{}, resultAttempt{}, execution.TaskSpec{}, fmt.Errorf("lock execution attempt for result: %w", err)
	}
	if err := json.Unmarshal(providerBytes, &attempt.providerIdentity); err != nil {
		return execution.TaskRecord{}, resultAttempt{}, execution.TaskSpec{}, execution.ErrStoredTaskCorrupt
	}
	spec, err := execution.DecodeTaskSpec(task.Specification)
	if err != nil {
		return execution.TaskRecord{}, resultAttempt{}, execution.TaskSpec{}, err
	}
	return task, attempt, spec, nil
}

func matchResultToSpec(result execution.ExecutionResult, task execution.TaskRecord, attempt resultAttempt, spec execution.TaskSpec) error {
	if result.TaskID != task.TaskID || result.RunID != task.RunID || result.PairID != task.PairID || result.SubjectRole != task.SubjectRole ||
		result.AttemptNumber != attempt.attemptNumber || result.TaskSpecDigest != task.SpecificationDigest || result.TaskSpecDigest != attempt.taskSpecDigest ||
		result.Source != spec.Subject.Source.Identity || result.Subject != spec.Subject.Identity ||
		result.Profile.ProfileID != spec.Profile.ProfileID || result.Profile.ProfileVersion != spec.Profile.ProfileVersion || result.Profile.Digest != spec.Profile.Artifact.Digest ||
		result.Provider != execution.Identity(attempt.providerIdentity) || result.ProviderJob.ID != attempt.providerJobID ||
		!slices.Equal(result.Tests.ExpectedTestIDs, spec.ExpectedTestIDs) {
		return execution.ErrResultConflict
	}
	return nil
}

func validateResultReference(result execution.ExecutionResult, reference execution.Artifact) error {
	if err := result.Validate(); err != nil {
		return err
	}
	if reference.ArtifactID != result.ResultID || reference.MediaType != execution.ExecutionResultMediaType || reference.Validate() != nil {
		return fmt.Errorf("execution result reference identity or media type is invalid")
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("encode execution result for digest validation: %w", err)
	}
	if reference.Digest != execution.DigestTaskSpec(encoded) || reference.SizeBytes != int64(len(encoded)) {
		return fmt.Errorf("execution result reference digest or size differs from payload")
	}
	return nil
}

func finalTaskStatus(result execution.ExecutionResult) (string, string, error) {
	switch result.Status {
	case execution.ResultCompleted:
		return "completed", "", nil
	case execution.ResultFailed:
		return "failed", result.FailureCategory, nil
	case execution.ResultBlocked:
		return "blocked", "execution blocked", nil
	case execution.ResultNotReached:
		return "not_reached", "execution not reached", nil
	case execution.ResultCancelled:
		return "cancelled", "execution cancelled", nil
	case execution.ResultOutcomeUnknown:
		return "outcome_unknown", "execution outcome or termination is unknown", nil
	default:
		return "", "", fmt.Errorf("unsupported finalized execution result status %q", result.Status)
	}
}
