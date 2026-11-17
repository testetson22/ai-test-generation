package postgres

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/project-lightpath/ai-test-generation/services/control/internal/execution"
	"github.com/project-lightpath/ai-test-generation/services/control/internal/provider"
)

type lockedAttempt struct {
	status                string
	fencingToken          int64
	taskSpecDigest        string
	providerID            string
	providerJobID         string
	submissionToken       string
	providerIdentityBytes []byte
	provider              provider.Identity
	claimHolder           string
	leaseExpiresAt        sql.NullTime
	failureCategory       string
}

func (s *Store) RecordSubmitted(ctx context.Context, claim execution.Claim, job provider.JobRef, now time.Time, pollInterval time.Duration) error {
	if job.ID == "" || pollInterval <= 0 {
		return fmt.Errorf("provider job identity and positive poll interval are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin submission acknowledgement: %w", err)
	}
	defer tx.Rollback()
	task, attempt, err := lockClaim(ctx, tx, claim)
	if err != nil {
		return err
	}
	if attempt.status == "submitted" && attempt.providerJobID == job.ID {
		return tx.Commit()
	}
	if err := assertActiveClaim(task, attempt, claim, now); err != nil {
		return err
	}
	if attempt.status != "prepared" && attempt.status != "submission_unknown" {
		return fmt.Errorf("attempt %s cannot accept submission in state %s", claim.AttemptID, attempt.status)
	}
	nextAction := now.Add(pollInterval)
	if task.Status == "cancel_requested" {
		nextAction = now
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE execution_attempts SET status = 'submitted', provider_job_id = $2,
		       claim_holder = NULL, lease_expires_at = NULL, next_action_at = $3, updated_at = $4
		WHERE attempt_id = $1`, claim.AttemptID, job.ID, nextAction.UTC(), now.UTC()); err != nil {
		return fmt.Errorf("record provider submission: %w", err)
	}
	if task.Status != "cancel_requested" && !terminalTask(task.Status) {
		if err := transitionTask(ctx, tx, task, "submitted", "", now, nextAction, "attempt_submitted", map[string]any{"attempt_id": claim.AttemptID, "provider_job_id": job.ID}); err != nil {
			return err
		}
	} else if _, err := tx.ExecContext(ctx, `UPDATE execution_tasks SET updated_at = $2 WHERE task_id = $1`, task.TaskID, now.UTC()); err != nil {
		return fmt.Errorf("update task submission timestamp: %w", err)
	}
	return tx.Commit()
}

func (s *Store) RecordSubmissionUncertain(ctx context.Context, claim execution.Claim, now time.Time, reconcileAfter time.Duration) error {
	if reconcileAfter <= 0 {
		return fmt.Errorf("reconciliation interval must be positive")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin uncertain submission update: %w", err)
	}
	defer tx.Rollback()
	task, attempt, err := lockClaim(ctx, tx, claim)
	if err != nil {
		return err
	}
	if attempt.status == "submission_unknown" || attempt.status == "submitted" || attempt.status == "running" {
		return tx.Commit()
	}
	if err := assertActiveClaim(task, attempt, claim, now); err != nil {
		return err
	}
	if attempt.status != "prepared" && attempt.status != "submission_unknown" {
		return fmt.Errorf("attempt %s cannot become submission_unknown from %s", claim.AttemptID, attempt.status)
	}
	next := now.Add(reconcileAfter).UTC()
	if task.Status == "cancel_requested" {
		next = now.UTC()
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE execution_attempts SET status = 'submission_unknown', claim_holder = NULL,
		       lease_expires_at = NULL, next_action_at = $2, updated_at = $3
		WHERE attempt_id = $1`, claim.AttemptID, next, now.UTC()); err != nil {
		return fmt.Errorf("persist uncertain submission: %w", err)
	}
	if task.Status != "cancel_requested" && !terminalTask(task.Status) {
		if err := transitionTask(ctx, tx, task, "outcome_unknown", "provider submission acknowledgement is uncertain", now, next, "submission_uncertain", map[string]any{"attempt_id": claim.AttemptID, "submission_token": claim.SubmissionToken}); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) RecordSubmitFailure(ctx context.Context, claim execution.Claim, failureCategory string, now time.Time) error {
	switch failureCategory {
	case "invalid_input", "unauthorized", "profile_unapproved", "capability_unavailable", "provider_unavailable":
	default:
		return fmt.Errorf("unsupported provider submission failure category %q", failureCategory)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin failed submission update: %w", err)
	}
	defer tx.Rollback()
	task, attempt, err := lockClaim(ctx, tx, claim)
	if err != nil {
		return err
	}
	if attempt.status == "failed" && attempt.failureCategory == failureCategory {
		return tx.Commit()
	}
	if err := assertActiveClaim(task, attempt, claim, now); err != nil {
		return err
	}
	if attempt.status != "prepared" {
		return fmt.Errorf("attempt %s cannot fail submission from %s", claim.AttemptID, attempt.status)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE execution_attempts SET status = 'failed', failure_category = $2, finished_at = $3,
		       claim_holder = NULL, lease_expires_at = NULL, updated_at = $3
		WHERE attempt_id = $1`, claim.AttemptID, failureCategory, now.UTC()); err != nil {
		return fmt.Errorf("persist failed submission: %w", err)
	}
	status := "failed"
	if task.Status == "cancel_requested" {
		status = "not_reached"
	}
	if err := transitionTask(ctx, tx, task, status, failureCategory, now, now, "attempt_failed", map[string]any{"attempt_id": claim.AttemptID, "failure_category": failureCategory}); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ScheduleReconciliation(ctx context.Context, claim execution.Claim, now time.Time, pollInterval time.Duration) error {
	if pollInterval <= 0 {
		return fmt.Errorf("poll interval must be positive")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin provider reconciliation schedule: %w", err)
	}
	defer tx.Rollback()
	task, attempt, err := lockClaim(ctx, tx, claim)
	if err != nil {
		return err
	}
	if err := assertActiveClaim(task, attempt, claim, now); err != nil {
		return err
	}
	next := now.Add(pollInterval).UTC()
	if task.Status == "cancel_requested" {
		next = now.UTC()
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE execution_attempts SET claim_holder = NULL, lease_expires_at = NULL,
		       next_action_at = $2, updated_at = $3 WHERE attempt_id = $1`, claim.AttemptID, next, now.UTC()); err != nil {
		return fmt.Errorf("schedule provider reconciliation: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE execution_tasks SET next_action_at = $2, updated_at = $3 WHERE task_id = $1`, task.TaskID, next, now.UTC()); err != nil {
		return fmt.Errorf("schedule task reconciliation: %w", err)
	}
	return tx.Commit()
}

func (s *Store) BlockClaim(ctx context.Context, claim execution.Claim, category, reason string, now time.Time) error {
	if category == "" || reason == "" {
		return fmt.Errorf("blocked claim requires failure category and reason")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin blocked execution claim: %w", err)
	}
	defer tx.Rollback()
	task, attempt, err := lockClaim(ctx, tx, claim)
	if err != nil {
		return err
	}
	if attempt.status == "blocked" && attempt.failureCategory == category {
		return tx.Commit()
	}
	if err := assertActiveClaim(task, attempt, claim, now); err != nil {
		return err
	}
	if attempt.status != "prepared" {
		return fmt.Errorf("only an unsubmitted attempt can be blocked by admission")
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE execution_attempts SET status = 'blocked', failure_category = $2,
		       finished_at = $3, claim_holder = NULL, lease_expires_at = NULL, updated_at = $3
		WHERE attempt_id = $1`, claim.AttemptID, category, now.UTC()); err != nil {
		return fmt.Errorf("persist blocked attempt: %w", err)
	}
	if err := transitionTask(ctx, tx, task, "blocked", reason, now, now, "dispatch_blocked", map[string]any{
		"attempt_id": claim.AttemptID, "failure_category": category,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RecordObservation(ctx context.Context, claim execution.Claim, observation provider.Observation, receivedAt time.Time, pollInterval time.Duration) error {
	if observation.SubmissionToken != claim.SubmissionToken || observation.Sequence == 0 || observation.ObservedAt.IsZero() || pollInterval <= 0 {
		return fmt.Errorf("provider observation identity, sequence, timestamp, and poll interval are required")
	}
	if observation.Sequence > uint64(^uint64(0)>>1) {
		return fmt.Errorf("provider observation sequence exceeds PostgreSQL BIGINT range")
	}
	if !validObservationStatus(observation.Status) {
		return fmt.Errorf("unsupported provider observation status %q", observation.Status)
	}
	if observation.Status == provider.StatusFailed && !validObservationFailure(observation.FailureCategory) {
		return fmt.Errorf("failed provider observation requires a failure category")
	}
	if observation.Status == provider.StatusCancelled && !observation.TerminationConfirmed {
		return fmt.Errorf("cancelled provider observation requires confirmed termination")
	}
	payload, err := json.Marshal(observation)
	if err != nil {
		return fmt.Errorf("encode provider observation: %w", err)
	}
	payloadHash := sha256.Sum256(payload)
	digest := "sha256:" + hex.EncodeToString(payloadHash[:])
	observationID := observationID(claim.AttemptID, observation.Sequence)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin provider observation: %w", err)
	}
	defer tx.Rollback()
	task, attempt, err := lockClaim(ctx, tx, claim)
	if err != nil {
		return err
	}
	if attempt.provider != claim.Provider {
		return execution.ErrProviderChanged
	}
	if attempt.providerJobID != "" && observation.Job.ID != "" && attempt.providerJobID != observation.Job.ID {
		return fmt.Errorf("provider observation job identity does not match attempt")
	}
	inserted, err := tx.ExecContext(ctx, `
		INSERT INTO execution_provider_observations(observation_id, attempt_id, sequence, observation_digest, payload_bytes, received_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (attempt_id, sequence) DO NOTHING`, observationID, claim.AttemptID, observation.Sequence, digest, payload, receivedAt.UTC())
	if err != nil {
		return fmt.Errorf("persist provider observation: %w", err)
	}
	insertCount, err := inserted.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect provider observation insert: %w", err)
	}
	if insertCount == 0 {
		var previous string
		if err := tx.QueryRowContext(ctx, `SELECT observation_digest FROM execution_provider_observations WHERE attempt_id = $1 AND sequence = $2`, claim.AttemptID, observation.Sequence).Scan(&previous); err != nil {
			return fmt.Errorf("load duplicate provider observation: %w", err)
		}
		if previous != digest {
			return provider.ErrObservationConflict
		}
		if err := rescheduleClaimAfterObservation(ctx, tx, task, attempt, claim, receivedAt, pollInterval); err != nil {
			return err
		}
		return tx.Commit()
	}
	var latestSequence int64
	if err := tx.QueryRowContext(ctx, `SELECT MAX(sequence) FROM execution_provider_observations WHERE attempt_id = $1`, claim.AttemptID).Scan(&latestSequence); err != nil {
		return fmt.Errorf("load latest provider observation sequence: %w", err)
	}
	if uint64(latestSequence) != observation.Sequence || assertActiveClaim(task, attempt, claim, receivedAt) != nil || terminalTask(task.Status) {
		if uint64(latestSequence) != observation.Sequence {
			if err := rescheduleClaimAfterObservation(ctx, tx, task, attempt, claim, receivedAt, pollInterval); err != nil {
				return err
			}
		}
		return tx.Commit()
	}
	if attempt.status == "completed" || attempt.status == "failed" || attempt.status == "cancelled" || attempt.status == "superseded" {
		return tx.Commit()
	}
	if err := s.applyObservation(ctx, tx, task, claim, observation, receivedAt, pollInterval); err != nil {
		return err
	}
	return tx.Commit()
}

func rescheduleClaimAfterObservation(ctx context.Context, tx *sql.Tx, task execution.TaskRecord, attempt lockedAttempt, claim execution.Claim, receivedAt time.Time, pollInterval time.Duration) error {
	if err := assertActiveClaim(task, attempt, claim, receivedAt); err != nil {
		return nil
	}
	next := receivedAt.Add(pollInterval).UTC()
	if _, err := tx.ExecContext(ctx, `
		UPDATE execution_attempts SET claim_holder = NULL, lease_expires_at = NULL,
		       next_action_at = $2, updated_at = $3 WHERE attempt_id = $1`, claim.AttemptID, next, receivedAt.UTC()); err != nil {
		return fmt.Errorf("release duplicate observation claim: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE execution_tasks SET next_action_at = $2, updated_at = $3 WHERE task_id = $1`, task.TaskID, next, receivedAt.UTC()); err != nil {
		return fmt.Errorf("reschedule duplicate observation task: %w", err)
	}
	return nil
}

func (s *Store) applyObservation(ctx context.Context, tx *sql.Tx, task execution.TaskRecord, claim execution.Claim, observation provider.Observation, receivedAt time.Time, pollInterval time.Duration) error {
	next := receivedAt.Add(pollInterval).UTC()
	var attemptStatus string
	taskStatus := task.Status
	failure := observation.FailureCategory
	switch observation.Status {
	case provider.StatusPending:
		attemptStatus, taskStatus = "submitted", "submitted"
	case provider.StatusRunning:
		attemptStatus, taskStatus = "running", "running"
	case provider.StatusCompleted:
		attemptStatus, taskStatus = "completed", "collecting"
	case provider.StatusFailed:
		attemptStatus, taskStatus = "failed", "failed"
	case provider.StatusCancelRequested:
		attemptStatus, taskStatus = "cancel_requested", "cancel_requested"
	case provider.StatusCancelled:
		if observation.TerminationConfirmed {
			attemptStatus, taskStatus = "cancelled", "cancelled"
		} else {
			attemptStatus, taskStatus = "cancel_requested", "cancel_requested"
		}
	case provider.StatusNotFound:
		attemptStatus, taskStatus = "submission_unknown", "outcome_unknown"
	case provider.StatusOutcomeUnknown:
		attemptStatus, taskStatus = "outcome_unknown", "outcome_unknown"
	default:
		return fmt.Errorf("unsupported provider observation status %q", observation.Status)
	}
	if task.Status == "cancel_requested" && taskStatus != "cancelled" && taskStatus != "failed" {
		taskStatus = "cancel_requested"
	}
	finishedAt := any(nil)
	if attemptStatus == "completed" || attemptStatus == "failed" || attemptStatus == "cancelled" {
		finishedAt = receivedAt.UTC()
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE execution_attempts SET status = $2, failure_category = NULLIF($3, ''),
		       provider_job_id = COALESCE(NULLIF($7, ''), provider_job_id),
	       started_at = COALESCE(started_at, CASE WHEN $2 IN ('running', 'completed', 'failed') THEN $4::timestamptz ELSE NULL END),
	       finished_at = COALESCE($5::timestamptz, finished_at), claim_holder = NULL, lease_expires_at = NULL,
	       next_action_at = $6, updated_at = $4
		WHERE attempt_id = $1`, claim.AttemptID, attemptStatus, failure, receivedAt.UTC(), finishedAt, next, observation.Job.ID); err != nil {
		return fmt.Errorf("update execution attempt from provider observation: %w", err)
	}
	if taskStatus == task.Status {
		if _, err := tx.ExecContext(ctx, `UPDATE execution_tasks SET next_action_at = $2, updated_at = $3 WHERE task_id = $1`, task.TaskID, next, receivedAt.UTC()); err != nil {
			return fmt.Errorf("schedule next provider observation: %w", err)
		}
		return nil
	}
	reason := ""
	if taskStatus == "failed" {
		reason = failure
	}
	return transitionTask(ctx, tx, task, taskStatus, reason, receivedAt, next, "provider_observed", map[string]any{
		"attempt_id": claim.AttemptID, "sequence": observation.Sequence,
		"provider_status": observation.Status, "failure_category": failure,
	})
}

func (s *Store) AuthorizeRetry(ctx context.Context, taskID, decisionID string, now time.Time) (execution.RetryDecision, error) {
	if !executionValidID(taskID) || !executionValidID(decisionID) {
		return execution.RetryDecision{}, fmt.Errorf("task and retry decision identities are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return execution.RetryDecision{}, fmt.Errorf("begin retry authorization: %w", err)
	}
	defer tx.Rollback()
	task, err := scanTask(tx.QueryRowContext(ctx, taskSelect+` WHERE task_id = $1 FOR UPDATE`, taskID))
	if errors.Is(err, sql.ErrNoRows) {
		return execution.RetryDecision{}, execution.ErrTaskNotFound
	}
	if err != nil {
		return execution.RetryDecision{}, fmt.Errorf("lock retry task: %w", err)
	}
	if task.Status == "eligible" {
		prior, err := latestRetryDecision(ctx, tx, taskID)
		if err == nil {
			if err := tx.Commit(); err != nil {
				return execution.RetryDecision{}, fmt.Errorf("commit idempotent retry lookup: %w", err)
			}
			return prior, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return execution.RetryDecision{}, fmt.Errorf("load existing retry authorization: %w", err)
		}
	}
	if task.Status != "failed" || task.ActiveAttemptID == "" {
		return execution.RetryDecision{}, execution.ErrRetryNotAuthorized
	}
	var attemptNumber int
	var failureCategory string
	if err := tx.QueryRowContext(ctx, `SELECT attempt_number, COALESCE(failure_category, '') FROM execution_attempts WHERE attempt_id = $1 FOR UPDATE`, task.ActiveAttemptID).Scan(&attemptNumber, &failureCategory); err != nil {
		return execution.RetryDecision{}, fmt.Errorf("load failed attempt for retry: %w", err)
	}
	var prior execution.RetryDecision
	err = tx.QueryRowContext(ctx, `
		SELECT decision_id, task_id, predecessor_attempt_id, failure_category, policy_id, authorized_at, next_attempt_at
		FROM execution_retry_decisions WHERE task_id = $1 AND predecessor_attempt_id = $2`, taskID, task.ActiveAttemptID).Scan(
		&prior.DecisionID, &prior.TaskID, &prior.PredecessorID, &prior.FailureCategory,
		&prior.PolicyID, &prior.AuthorizedAt, &prior.NextAttemptAt,
	)
	if err == nil {
		if prior.DecisionID != decisionID {
			return execution.RetryDecision{}, execution.ErrRetryNotAuthorized
		}
		if err := tx.Commit(); err != nil {
			return execution.RetryDecision{}, fmt.Errorf("commit idempotent retry decision: %w", err)
		}
		return prior, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return execution.RetryDecision{}, fmt.Errorf("check prior retry decision: %w", err)
	}
	spec, err := execution.DecodeTaskSpec(task.Specification)
	if err != nil {
		return execution.RetryDecision{}, err
	}
	if attemptNumber >= spec.RetryPolicy.MaxAttempts || !contains(spec.RetryPolicy.RetryableFailures, failureCategory) {
		return execution.RetryDecision{}, execution.ErrRetryNotAuthorized
	}
	deadline, err := time.Parse(time.RFC3339, spec.RetryPolicy.DeadlineAt)
	if err != nil {
		return execution.RetryDecision{}, fmt.Errorf("parse task retry deadline: %w", err)
	}
	if !now.Before(deadline) {
		return execution.RetryDecision{}, execution.ErrDeadlineExceeded
	}
	backoff := retryBackoff(spec.RetryPolicy, attemptNumber)
	nextAttempt := now.Add(backoff)
	if nextAttempt.After(deadline) {
		return execution.RetryDecision{}, execution.ErrDeadlineExceeded
	}
	authorization := execution.RetryAuthorization{
		DecisionID: decisionID, PolicyID: spec.RetryPolicy.PolicyID,
		FailureCategory: failureCategory, AuthorizedAt: now.UTC(),
	}
	payload, err := json.Marshal(authorization)
	if err != nil {
		return execution.RetryDecision{}, fmt.Errorf("encode retry authorization: %w", err)
	}
	decision := execution.RetryDecision{
		DecisionID: decisionID, TaskID: taskID, PredecessorID: task.ActiveAttemptID,
		FailureCategory: failureCategory, PolicyID: spec.RetryPolicy.PolicyID,
		AuthorizedAt: now.UTC(), NextAttemptAt: nextAttempt.UTC(), AttemptNumber: attemptNumber + 1,
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO execution_retry_decisions(decision_id, task_id, predecessor_attempt_id, policy_id, failure_category, authorized_at, next_attempt_at, payload_bytes)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, decision.DecisionID, decision.TaskID, decision.PredecessorID,
		decision.PolicyID, decision.FailureCategory, decision.AuthorizedAt, decision.NextAttemptAt, payload); err != nil {
		return execution.RetryDecision{}, fmt.Errorf("persist retry authorization: %w", err)
	}
	if err := transitionTask(ctx, tx, task, "eligible", "", now, nextAttempt, "retry_authorized", decision); err != nil {
		return execution.RetryDecision{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE execution_tasks SET active_attempt_id = NULL, next_action_at = $2 WHERE task_id = $1`, taskID, nextAttempt.UTC()); err != nil {
		return execution.RetryDecision{}, fmt.Errorf("schedule authorized retry: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return execution.RetryDecision{}, fmt.Errorf("commit retry authorization: %w", err)
	}
	return decision, nil
}

func (s *Store) ScheduleRetryableFailures(ctx context.Context, now time.Time) error {
	rows, err := s.db.QueryContext(ctx, `
		SELECT t.task_id, a.attempt_id
		FROM execution_tasks t
		JOIN execution_attempts a ON a.attempt_id = t.active_attempt_id
		WHERE t.status = 'failed'
		  AND NOT EXISTS (SELECT 1 FROM execution_retry_decisions d WHERE d.task_id = t.task_id AND d.predecessor_attempt_id = a.attempt_id)
		ORDER BY t.next_action_at, t.created_at, t.task_id`)
	if err != nil {
		return fmt.Errorf("find failed tasks needing retry decisions: %w", err)
	}
	type pendingFailure struct {
		taskID    string
		attemptID string
	}
	var pending []pendingFailure
	for rows.Next() {
		var item pendingFailure
		if err := rows.Scan(&item.taskID, &item.attemptID); err != nil {
			rows.Close()
			return fmt.Errorf("read retry candidate: %w", err)
		}
		pending = append(pending, item)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close retry candidate query: %w", err)
	}
	for _, item := range pending {
		decisionID := deterministicRetryDecisionID(item.taskID, item.attemptID)
		if _, err := s.AuthorizeRetry(ctx, item.taskID, decisionID, now); err != nil &&
			!errors.Is(err, execution.ErrRetryNotAuthorized) && !errors.Is(err, execution.ErrDeadlineExceeded) {
			return err
		}
	}
	return nil
}

func latestRetryDecision(ctx context.Context, tx *sql.Tx, taskID string) (execution.RetryDecision, error) {
	var decision execution.RetryDecision
	err := tx.QueryRowContext(ctx, `
		SELECT decision_id, task_id, predecessor_attempt_id, failure_category, policy_id, authorized_at, next_attempt_at
		FROM execution_retry_decisions WHERE task_id = $1 ORDER BY authorized_at DESC LIMIT 1`, taskID).Scan(
		&decision.DecisionID, &decision.TaskID, &decision.PredecessorID, &decision.FailureCategory,
		&decision.PolicyID, &decision.AuthorizedAt, &decision.NextAttemptAt,
	)
	return decision, err
}

func deterministicRetryDecisionID(taskID, attemptID string) string {
	digest := sha256.Sum256([]byte(taskID + "\x00" + attemptID))
	return "retry-reconcile-" + hex.EncodeToString(digest[:16])
}

func (s *Store) RequestCancel(ctx context.Context, taskID string, expectedRevision int64, now time.Time) (execution.TaskRecord, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return execution.TaskRecord{}, fmt.Errorf("begin task cancellation: %w", err)
	}
	defer tx.Rollback()
	task, err := scanTask(tx.QueryRowContext(ctx, taskSelect+` WHERE task_id = $1 FOR UPDATE`, taskID))
	if errors.Is(err, sql.ErrNoRows) {
		return execution.TaskRecord{}, execution.ErrTaskNotFound
	}
	if err != nil {
		return execution.TaskRecord{}, fmt.Errorf("lock task for cancellation: %w", err)
	}
	if task.Revision != expectedRevision {
		return execution.TaskRecord{}, execution.ErrStaleRevision
	}
	if terminalTask(task.Status) {
		return task, tx.Commit()
	}
	status := "cancel_requested"
	next := now.UTC()
	reason := "cancellation requested"
	if task.ActiveAttemptID == "" {
		status = "not_reached"
		reason = "cancelled before any worker attempt was launched"
	}
	if task.ActiveAttemptID != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE execution_attempts SET next_action_at = $2, updated_at = $2 WHERE attempt_id = $1`, task.ActiveAttemptID, now.UTC()); err != nil {
			return execution.TaskRecord{}, fmt.Errorf("schedule provider cancellation: %w", err)
		}
	}
	if err := transitionTask(ctx, tx, task, status, reason, now, next, "cancellation_requested", map[string]any{"active_attempt_id": task.ActiveAttemptID}); err != nil {
		return execution.TaskRecord{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE execution_tasks SET cancel_requested_at = $2 WHERE task_id = $1`, taskID, now.UTC()); err != nil {
		return execution.TaskRecord{}, fmt.Errorf("record cancellation timestamp: %w", err)
	}
	updated, err := scanTask(tx.QueryRowContext(ctx, taskSelect+` WHERE task_id = $1`, taskID))
	if err != nil {
		return execution.TaskRecord{}, fmt.Errorf("read cancelled task: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return execution.TaskRecord{}, fmt.Errorf("commit task cancellation: %w", err)
	}
	return updated, nil
}

func lockClaim(ctx context.Context, tx *sql.Tx, claim execution.Claim) (execution.TaskRecord, lockedAttempt, error) {
	task, err := scanTask(tx.QueryRowContext(ctx, taskSelect+` WHERE task_id = $1 FOR UPDATE`, claim.TaskID))
	if errors.Is(err, sql.ErrNoRows) {
		return execution.TaskRecord{}, lockedAttempt{}, execution.ErrTaskNotFound
	}
	if err != nil {
		return execution.TaskRecord{}, lockedAttempt{}, fmt.Errorf("lock claimed execution task: %w", err)
	}
	var attempt lockedAttempt
	err = tx.QueryRowContext(ctx, `
		SELECT status, fencing_token, task_spec_digest, provider_id, COALESCE(provider_job_id, ''), submission_token, provider_identity_bytes,
		       COALESCE(claim_holder, ''), lease_expires_at, COALESCE(failure_category, '')
		FROM execution_attempts WHERE attempt_id = $1 AND task_id = $2 FOR UPDATE`, claim.AttemptID, claim.TaskID).Scan(
		&attempt.status, &attempt.fencingToken, &attempt.taskSpecDigest, &attempt.providerID, &attempt.providerJobID, &attempt.submissionToken, &attempt.providerIdentityBytes,
		&attempt.claimHolder, &attempt.leaseExpiresAt, &attempt.failureCategory,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return execution.TaskRecord{}, lockedAttempt{}, execution.ErrTaskNotFound
	}
	if err != nil {
		return execution.TaskRecord{}, lockedAttempt{}, fmt.Errorf("lock claimed execution attempt: %w", err)
	}
	if attempt.submissionToken != claim.SubmissionToken {
		return execution.TaskRecord{}, lockedAttempt{}, execution.ErrStaleClaim
	}
	if task.SpecificationDigest != attempt.taskSpecDigest {
		return execution.TaskRecord{}, lockedAttempt{}, execution.ErrStoredTaskCorrupt
	}
	var identity provider.Identity
	if err := json.Unmarshal(attempt.providerIdentityBytes, &identity); err != nil {
		return execution.TaskRecord{}, lockedAttempt{}, fmt.Errorf("decode attempt provider identity: %w", err)
	}
	attempt.provider = identity
	if attempt.providerID != identity.ID || identity != claim.Provider {
		return execution.TaskRecord{}, lockedAttempt{}, execution.ErrProviderChanged
	}
	return task, attempt, nil
}

func assertActiveClaim(task execution.TaskRecord, attempt lockedAttempt, claim execution.Claim, now time.Time) error {
	if task.ActiveAttemptID != claim.AttemptID || attempt.fencingToken != claim.FencingToken ||
		claim.Holder == "" || attempt.claimHolder != claim.Holder || !attempt.leaseExpiresAt.Valid ||
		!attempt.leaseExpiresAt.Time.After(now) {
		return execution.ErrStaleClaim
	}
	if attempt.provider != claim.Provider {
		return execution.ErrProviderChanged
	}
	return nil
}

func transitionTask(ctx context.Context, tx *sql.Tx, task execution.TaskRecord, status, reason string, occurredAt, nextAction time.Time, eventType string, payload any) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode task event %s: %w", eventType, err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE execution_tasks SET status = $2, revision = revision + 1,
		       terminal_reason = NULLIF($3, ''), next_action_at = $4, updated_at = $5
		WHERE task_id = $1`, task.TaskID, status, reason, nextAction.UTC(), occurredAt.UTC()); err != nil {
		return fmt.Errorf("transition execution task to %s: %w", status, err)
	}
	if err := appendTaskEvent(ctx, tx, task.TaskID, task.Revision, task.Revision+1, eventType, encoded, occurredAt.UTC()); err != nil {
		return err
	}
	return nil
}

func terminalTask(status string) bool {
	switch status {
	case "completed", "failed", "blocked", "not_reached", "cancelled", "superseded":
		return true
	default:
		return false
	}
}

func retryFailureCategory(category string) bool {
	switch category {
	case "provider_unavailable", "worker_lost", "worker_resource_exhausted", "artifact_transport_failed":
		return true
	default:
		return false
	}
}

func validObservationStatus(status provider.Status) bool {
	switch status {
	case provider.StatusPending, provider.StatusRunning, provider.StatusCompleted, provider.StatusFailed,
		provider.StatusCancelRequested, provider.StatusCancelled, provider.StatusNotFound, provider.StatusOutcomeUnknown:
		return true
	default:
		return false
	}
}

func validObservationFailure(category string) bool {
	switch category {
	case "provider_unavailable", "worker_setup_failed", "source_mismatch", "materialization_failed",
		"build_failed", "test_collection_failed", "test_assertion_failed", "report_invalid",
		"timed_out", "worker_resource_exhausted", "worker_lost", "artifact_transport_failed",
		"cleanup_failed", "unknown":
		return true
	default:
		return false
	}
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func retryBackoff(policy execution.RetryPolicy, failedAttempt int) time.Duration {
	seconds := policy.InitialBackoffSeconds
	for i := 1; i < failedAttempt && seconds < policy.MaxBackoffSeconds; i++ {
		if seconds > policy.MaxBackoffSeconds/2 {
			seconds = policy.MaxBackoffSeconds
			break
		}
		seconds *= 2
	}
	if seconds > policy.MaxBackoffSeconds {
		seconds = policy.MaxBackoffSeconds
	}
	return time.Duration(seconds) * time.Second
}

func observationID(attemptID string, sequence uint64) string {
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", attemptID, sequence)))
	return "observation-" + hex.EncodeToString(digest[:16])
}
