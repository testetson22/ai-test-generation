package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/project-lightpath/ai-test-generation/services/control/internal/execution"
	"github.com/project-lightpath/ai-test-generation/services/control/internal/provider"
)

type dueAttempt struct {
	task               execution.TaskRecord
	attemptID          string
	attemptNumber      int
	attemptSpecDigest  string
	submissionToken    string
	attemptStatus      string
	providerID         string
	providerIdentity   []byte
	providerJobID      string
	fencingToken       int64
	latestObservation  string
	retryAuthorization []byte
}

func (s *Store) ClaimNext(ctx context.Context, holder string, providerID provider.Identity, now time.Time, leaseDuration, pollInterval time.Duration) (*execution.Claim, error) {
	if holder == "" || !validProviderIdentity(providerID) || leaseDuration <= 0 || pollInterval <= 0 {
		return nil, fmt.Errorf("claim holder, provider identity, lease, and poll interval are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin execution claim: %w", err)
	}
	defer tx.Rollback()

	task, err := selectDueTask(ctx, tx, now)
	if err == nil {
		claim, err := s.createFirstOrRetryAttempt(ctx, tx, task, holder, providerID, now, leaseDuration, pollInterval)
		if err != nil {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("commit execution claim: %w", err)
		}
		return claim, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("select queued execution task: %w", err)
	}

	active, err := selectDueAttempt(ctx, tx, now)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("select due execution attempt: %w", err)
	}
	if active.attemptSpecDigest != active.task.SpecificationDigest {
		return nil, execution.ErrStoredTaskCorrupt
	}
	if err := expireActiveTask(ctx, tx, &active.task, now); err != nil {
		return nil, err
	}
	var persistedProvider provider.Identity
	if err := json.Unmarshal(active.providerIdentity, &persistedProvider); err != nil {
		return nil, fmt.Errorf("decode persisted provider identity: %w", err)
	}
	if active.providerID != persistedProvider.ID {
		return nil, execution.ErrProviderChanged
	}
	if persistedProvider != providerID {
		return nil, execution.ErrProviderChanged
	}
	leaseExpiry := now.Add(leaseDuration)
	newFence := active.fencingToken + 1
	if _, err := tx.ExecContext(ctx, `
		UPDATE execution_attempts
		SET claim_holder = $2, lease_expires_at = $3, fencing_token = $4, updated_at = $5
		WHERE attempt_id = $1`, active.attemptID, holder, leaseExpiry, newFence, now.UTC()); err != nil {
		return nil, fmt.Errorf("fence execution attempt claim: %w", err)
	}
	claim := &execution.Claim{
		TaskID: active.task.TaskID, PairID: active.task.PairID, RunID: active.task.RunID,
		AttemptID: active.attemptID, AttemptNumber: active.attemptNumber,
		TaskSpecDigest: active.task.SpecificationDigest, TaskSpec: active.task.Specification,
		SubmissionToken: active.submissionToken, Holder: holder, FencingToken: newFence,
		LeaseExpiresAt: leaseExpiry, Provider: persistedProvider,
		RetryAuthorization: decodeRetryAuthorization(active.retryAuthorization),
	}
	if active.providerJobID != "" {
		claim.ProviderJob = provider.JobRef{ID: active.providerJobID}
	}
	switch {
	case active.task.Status == "cancel_requested" || active.attemptStatus == "cancel_requested":
		claim.Action = execution.ActionCancel
	case active.attemptStatus == "prepared":
		claim.Action = execution.ActionSubmit
	case active.attemptStatus == "submission_unknown", active.attemptStatus == "submitted", active.attemptStatus == "running", active.attemptStatus == "outcome_unknown":
		claim.Action = execution.ActionObserve
	default:
		return nil, fmt.Errorf("attempt %s with state %q is not dispatchable", active.attemptID, active.attemptStatus)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit fenced execution claim: %w", err)
	}
	return claim, nil
}

func expireActiveTask(ctx context.Context, tx *sql.Tx, task *execution.TaskRecord, now time.Time) error {
	spec, err := execution.DecodeTaskSpec(task.Specification)
	if err != nil {
		return fmt.Errorf("decode active task specification: %w", err)
	}
	deadline, err := time.Parse(time.RFC3339, spec.RetryPolicy.DeadlineAt)
	if err != nil {
		return fmt.Errorf("parse active task deadline: %w", err)
	}
	if now.Before(deadline) || task.Status == "cancel_requested" {
		return nil
	}
	if err := transitionTask(ctx, tx, *task, "cancel_requested", "execution deadline elapsed", now, now, "execution_deadline_elapsed", map[string]string{
		"attempt_id": task.ActiveAttemptID,
	}); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE execution_tasks SET cancel_requested_at = $2 WHERE task_id = $1`, task.TaskID, now.UTC()); err != nil {
		return fmt.Errorf("persist deadline cancellation: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE execution_attempts SET next_action_at = $2 WHERE attempt_id = $1`, task.ActiveAttemptID, now.UTC()); err != nil {
		return fmt.Errorf("schedule deadline cancellation: %w", err)
	}
	task.Status = "cancel_requested"
	task.Revision++
	task.TerminalReason = "execution deadline elapsed"
	return nil
}

func selectDueTask(ctx context.Context, tx *sql.Tx, now time.Time) (execution.TaskRecord, error) {
	return scanTask(tx.QueryRowContext(ctx, taskSelect+`
		WHERE status IN ('queued', 'eligible') AND active_attempt_id IS NULL AND next_action_at <= $1
		ORDER BY next_action_at, created_at, CASE subject_role WHEN 'base' THEN 0 ELSE 1 END, task_id
		LIMIT 1 FOR UPDATE SKIP LOCKED`, now.UTC()))
}

func selectDueAttempt(ctx context.Context, tx *sql.Tx, now time.Time) (dueAttempt, error) {
	query := `
		SELECT t.task_id, t.run_id, t.pair_id, t.subject_role, t.specification_bytes,
		       t.specification_digest, t.status, t.revision, t.attempt_count,
		       COALESCE(t.active_attempt_id, ''), t.next_action_at, COALESCE(t.terminal_reason, ''),
		       t.created_at, t.updated_at,
		       a.attempt_id, a.attempt_number, a.task_spec_digest, a.submission_token, a.status,
		       a.provider_id, a.provider_identity_bytes, COALESCE(a.provider_job_id, ''), a.fencing_token,
		       COALESCE(a.retry_authorization_bytes, ''::bytea),
	       COALESCE(o.status, '')
		FROM execution_tasks t
		JOIN execution_attempts a ON a.attempt_id = t.active_attempt_id
		LEFT JOIN LATERAL (
			SELECT (convert_from(payload_bytes, 'UTF8')::jsonb ->> 'status') AS status, sequence
			FROM execution_provider_observations WHERE attempt_id = a.attempt_id
			ORDER BY sequence DESC LIMIT 1
		) o ON TRUE
		WHERE t.status NOT IN ('completed', 'failed', 'blocked', 'not_reached', 'cancelled', 'superseded')
		  AND a.status IN ('prepared', 'submission_unknown', 'submitted', 'running', 'outcome_unknown', 'cancel_requested')
		  AND a.next_action_at <= $1
		  AND (a.lease_expires_at IS NULL OR a.lease_expires_at <= $1)
		ORDER BY a.next_action_at, a.created_at
		LIMIT 1 FOR UPDATE OF t SKIP LOCKED`
	var active dueAttempt
	err := tx.QueryRowContext(ctx, query, now.UTC()).Scan(
		&active.task.TaskID, &active.task.RunID, &active.task.PairID, &active.task.SubjectRole,
		&active.task.Specification, &active.task.SpecificationDigest, &active.task.Status,
		&active.task.Revision, &active.task.AttemptCount, &active.task.ActiveAttemptID,
		&active.task.NextActionAt, &active.task.TerminalReason, &active.task.CreatedAt, &active.task.UpdatedAt,
		&active.attemptID, &active.attemptNumber, &active.attemptSpecDigest, &active.submissionToken, &active.attemptStatus,
		&active.providerID, &active.providerIdentity, &active.providerJobID, &active.fencingToken,
		&active.retryAuthorization, &active.latestObservation,
	)
	return active, err
}

func (s *Store) createFirstOrRetryAttempt(ctx context.Context, tx *sql.Tx, task execution.TaskRecord, holder string, providerID provider.Identity, now time.Time, leaseDuration, pollInterval time.Duration) (*execution.Claim, error) {
	var retryAuthorization *execution.RetryAuthorization
	attemptNumber := task.AttemptCount + 1
	if task.AttemptCount > 0 {
		var decision execution.RetryDecision
		var payload []byte
		err := tx.QueryRowContext(ctx, `
			SELECT decision_id, predecessor_attempt_id, failure_category, policy_id, authorized_at, next_attempt_at, payload_bytes
			FROM execution_retry_decisions WHERE task_id = $1
			ORDER BY authorized_at DESC LIMIT 1 FOR UPDATE`, task.TaskID).Scan(
			&decision.DecisionID, &decision.PredecessorID, &decision.FailureCategory,
			&decision.PolicyID, &decision.AuthorizedAt, &decision.NextAttemptAt, &payload,
		)
		if err != nil {
			return nil, fmt.Errorf("load retry authorization for task %s: %w", task.TaskID, err)
		}
		var authorization execution.RetryAuthorization
		if err := json.Unmarshal(payload, &authorization); err != nil {
			return nil, fmt.Errorf("decode retry authorization: %w", err)
		}
		var latestAttemptID string
		if err := tx.QueryRowContext(ctx, `SELECT attempt_id FROM execution_attempts WHERE task_id = $1 ORDER BY attempt_number DESC LIMIT 1`, task.TaskID).Scan(&latestAttemptID); err != nil {
			return nil, fmt.Errorf("load retry predecessor for task %s: %w", task.TaskID, err)
		}
		if decision.PredecessorID != latestAttemptID {
			return nil, execution.ErrRetryNotAuthorized
		}
		retryAuthorization = &authorization
	}
	spec, err := execution.DecodeTaskSpec(task.Specification)
	if err != nil {
		return nil, fmt.Errorf("decode queued task specification: %w", err)
	}
	deadline, err := time.Parse(time.RFC3339, spec.RetryPolicy.DeadlineAt)
	if err != nil {
		return nil, fmt.Errorf("parse execution deadline: %w", err)
	}
	if !now.Before(deadline) {
		if err := transitionTask(ctx, tx, task, "blocked", "execution deadline elapsed before dispatch", now, now, "task_blocked", map[string]string{"reason": "deadline_elapsed"}); err != nil {
			return nil, err
		}
		return nil, nil
	}
	attemptID, err := newID("attempt")
	if err != nil {
		return nil, err
	}
	submissionToken, err := newID("submit")
	if err != nil {
		return nil, err
	}
	providerBytes, err := json.Marshal(providerID)
	if err != nil {
		return nil, fmt.Errorf("encode provider identity: %w", err)
	}
	var retryBytes []byte
	if retryAuthorization != nil {
		retryBytes, err = json.Marshal(retryAuthorization)
		if err != nil {
			return nil, fmt.Errorf("encode retry authorization: %w", err)
		}
	}
	leaseExpiry := now.Add(leaseDuration)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO execution_attempts(attempt_id, task_id, attempt_number, task_spec_digest, submission_token, status, provider_id, provider_identity_bytes, claim_holder, lease_expires_at, fencing_token, next_action_at, retry_authorization_bytes, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, 'prepared', $6, $7, $8, $9, 1, $10, $11, $12, $12)`,
		attemptID, task.TaskID, attemptNumber, task.SpecificationDigest, submissionToken, providerID.ID,
		providerBytes, holder, leaseExpiry, now.UTC(), retryBytes, now.UTC(),
	); err != nil {
		return nil, fmt.Errorf("persist execution attempt before provider submission: %w", err)
	}
	newStatus := "dispatching"
	if _, err := tx.ExecContext(ctx, `
		UPDATE execution_tasks SET status = $2, revision = revision + 1, attempt_count = $3,
		       active_attempt_id = $4, next_action_at = $5, updated_at = $6
		WHERE task_id = $1`, task.TaskID, newStatus, attemptNumber, attemptID, now.Add(pollInterval), now.UTC()); err != nil {
		return nil, fmt.Errorf("activate execution attempt: %w", err)
	}
	payload, _ := json.Marshal(map[string]any{"attempt_id": attemptID, "attempt_number": attemptNumber, "submission_token": submissionToken, "status": newStatus})
	if err := appendTaskEvent(ctx, tx, task.TaskID, task.Revision, task.Revision+1, "attempt_prepared", payload, now.UTC()); err != nil {
		return nil, err
	}
	return &execution.Claim{
		TaskID: task.TaskID, PairID: task.PairID, RunID: task.RunID, AttemptID: attemptID,
		AttemptNumber: attemptNumber, TaskSpecDigest: task.SpecificationDigest,
		TaskSpec: append([]byte(nil), task.Specification...), SubmissionToken: submissionToken, Holder: holder,
		Action: execution.ActionSubmit, FencingToken: 1, LeaseExpiresAt: leaseExpiry,
		RetryAuthorization: retryAuthorization, Provider: providerID,
	}, nil
}

func validProviderIdentity(identity provider.Identity) bool {
	return identity.Kind != "" && identity.ID != "" && len(identity.ID) <= 256
}

func decodeRetryAuthorization(payload []byte) *execution.RetryAuthorization {
	if len(payload) == 0 {
		return nil
	}
	var authorization execution.RetryAuthorization
	if json.Unmarshal(payload, &authorization) != nil {
		return nil
	}
	return &authorization
}
