package postgres

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/project-lightpath/ai-test-generation/services/control/internal/testgenflow"
)

var (
	flowOperationIDPattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,255}$`)
	flowOperationDigestPattern = regexp.MustCompile(`^sha256:[A-Fa-f0-9]{64}$`)
	flowCredentialPattern      = regexp.MustCompile(`(?i)(?:\b(?:authorization|password|passwd|api[_-]?key|(?:access[_-]?)?token|(?:client[_-]?)?secret|private[_-]?key)\b["\s]*[:=]["\s]*[^"\s,}]+|\bbearer\s+\S+|https?://[^\s/"@]+@|-----BEGIN [A-Z ]*PRIVATE KEY-----)`)
)

func (s *Store) ClaimNextTestGenerationRun(ctx context.Context, holder, profileDigest string, lease time.Duration) (*testgenflow.FlowClaim, error) {
	return s.claimNextGenerationRun(ctx, holder, profileDigest, "", testgenflow.InputModeArtifactInput, lease)
}

func (s *Store) ClaimNextModelGenerationRun(ctx context.Context, holder, profileDigest, policyDigest string, lease time.Duration) (*testgenflow.FlowClaim, error) {
	if !flowOperationDigestPattern.MatchString(profileDigest) || !flowOperationDigestPattern.MatchString(policyDigest) {
		return nil, fmt.Errorf("model generation claims require immutable profile and policy digests")
	}
	return s.claimNextGenerationRun(ctx, holder, profileDigest, policyDigest, testgenflow.InputModeModelGeneration, lease)
}

func (s *Store) ClaimUnknownModelGenerationRun(ctx context.Context, runID, holder, profileDigest, policyDigest string, expectedRevision int64, lease time.Duration) (*testgenflow.FlowClaim, error) {
	if !flowOperationIDPattern.MatchString(runID) || !flowOperationIDPattern.MatchString(holder) || !flowOperationDigestPattern.MatchString(profileDigest) || !flowOperationDigestPattern.MatchString(policyDigest) || expectedRevision < 1 || lease < time.Microsecond {
		return nil, fmt.Errorf("invalid model-generation recovery claim")
	}
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("test-generation run store is unavailable")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin model-generation recovery claim: %w", err)
	}
	defer tx.Rollback()
	var found string
	err = tx.QueryRowContext(ctx, `
		SELECT run_id FROM development_test_generation_runs
		WHERE run_id = $1 AND status = 'outcome_unknown' AND revision = $2
		  AND (claim_lease_until IS NULL OR claim_lease_until <= clock_timestamp())
		  AND convert_from(state_bytes, 'UTF8')::jsonb -> 'request' ->> 'input_mode' = $3
		  AND convert_from(state_bytes, 'UTF8')::jsonb -> 'request' ->> 'profile_digest' = $4
		  AND convert_from(state_bytes, 'UTF8')::jsonb -> 'request' -> 'generation' ->> 'policy_digest' = $5
		FOR UPDATE`, runID, expectedRevision, testgenflow.InputModeModelGeneration, profileDigest, policyDigest).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("select model-generation run for recovery: %w", err)
	}
	claim := &testgenflow.FlowClaim{RunID: found, Holder: holder}
	if err := tx.QueryRowContext(ctx, `
		UPDATE development_test_generation_runs
		SET claim_holder = $2, claim_fence = claim_fence + 1,
		    claim_lease_until = clock_timestamp() + $3 * interval '1 microsecond'
		WHERE run_id = $1 RETURNING claim_fence, claim_lease_until`, found, holder, lease.Microseconds()).Scan(&claim.Fence, &claim.LeaseUntil); err != nil {
		return nil, fmt.Errorf("claim model-generation run for recovery: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit model-generation recovery claim: %w", err)
	}
	return claim, nil
}

func (s *Store) claimNextGenerationRun(ctx context.Context, holder, profileDigest, policyDigest, inputMode string, lease time.Duration) (*testgenflow.FlowClaim, error) {
	if !flowOperationIDPattern.MatchString(holder) || lease < time.Microsecond || profileDigest != "" && !flowOperationDigestPattern.MatchString(profileDigest) {
		return nil, fmt.Errorf("invalid test-generation claim holder, profile digest, or lease")
	}
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("test-generation run store is unavailable")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin test-generation claim: %w", err)
	}
	defer tx.Rollback()
	run, err := scanTestGenerationRun(tx.QueryRowContext(ctx, testGenerationRunSelect+`
		WHERE status IN ('queued', 'running', 'cancel_requested')
		  AND next_action_at <= clock_timestamp()
		  AND (claim_lease_until IS NULL OR claim_lease_until <= clock_timestamp())
		  AND convert_from(state_bytes, 'UTF8')::jsonb -> 'request' ->> 'input_mode' = $2
		  AND ($1 = '' OR convert_from(state_bytes, 'UTF8')::jsonb -> 'request' ->> 'profile_digest' = $1)
		  AND ($3 = '' OR convert_from(state_bytes, 'UTF8')::jsonb -> 'request' -> 'generation' ->> 'policy_digest' = $3)
		ORDER BY next_action_at, created_at, run_id
		LIMIT 1 FOR UPDATE SKIP LOCKED`, profileDigest, inputMode, policyDigest))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("select test-generation run to claim: %w", err)
	}
	claim := &testgenflow.FlowClaim{RunID: run.RunID, Holder: holder}
	if err := tx.QueryRowContext(ctx, `
		UPDATE development_test_generation_runs
		SET claim_holder = $2, claim_fence = claim_fence + 1,
		    claim_lease_until = clock_timestamp() + $3 * interval '1 microsecond'
		WHERE run_id = $1 RETURNING claim_fence, claim_lease_until`, run.RunID, holder, lease.Microseconds()).Scan(&claim.Fence, &claim.LeaseUntil); err != nil {
		return nil, fmt.Errorf("claim test-generation run: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit test-generation claim: %w", err)
	}
	return claim, nil
}

func (s *Store) RenewTestGenerationClaim(ctx context.Context, claim testgenflow.FlowClaim, lease time.Duration) error {
	if lease < time.Microsecond {
		return fmt.Errorf("test-generation claim lease must be positive")
	}
	tx, err := s.beginTestGenerationClaim(ctx, claim)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE development_test_generation_runs SET claim_lease_until = clock_timestamp() + $2 * interval '1 microsecond' WHERE run_id = $1 AND claim_lease_until > clock_timestamp()`, claim.RunID, lease.Microseconds())
	if err != nil {
		return fmt.Errorf("renew test-generation claim: %w", err)
	}
	if updated, err := result.RowsAffected(); err != nil || updated != 1 {
		return testgenflow.ErrFlowClaimLost
	}
	return commitTestGenerationClaim(ctx, tx, claim)
}

func (s *Store) ReleaseTestGenerationClaim(ctx context.Context, claim testgenflow.FlowClaim) error {
	tx, err := s.beginTestGenerationClaim(ctx, claim)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE development_test_generation_runs SET claim_holder = NULL, claim_lease_until = NULL WHERE run_id = $1 AND claim_lease_until > clock_timestamp()`, claim.RunID)
	if err != nil {
		return fmt.Errorf("release test-generation claim: %w", err)
	}
	if updated, err := result.RowsAffected(); err != nil || updated != 1 {
		return testgenflow.ErrFlowClaimLost
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit test-generation claim release: %w", err)
	}
	return nil
}

func (s *Store) ApplyClaimedTestGenerationEvent(ctx context.Context, claim testgenflow.FlowClaim, event testgenflow.Event) (testgenflow.Run, error) {
	tx, err := s.beginTestGenerationClaim(ctx, claim)
	if err != nil {
		return testgenflow.Run{}, err
	}
	defer tx.Rollback()
	run, err := applyTestGenerationEvent(ctx, tx, claim.RunID, event)
	if err != nil {
		return testgenflow.Run{}, err
	}
	if err := commitTestGenerationClaim(ctx, tx, claim); err != nil {
		return testgenflow.Run{}, err
	}
	return run, nil
}

func (s *Store) BeginTestGenerationOperation(ctx context.Context, claim testgenflow.FlowClaim, key, kind, inputDigest string) (testgenflow.Operation, bool, error) {
	if !flowOperationIDPattern.MatchString(key) || !flowOperationIDPattern.MatchString(kind) || !flowOperationDigestPattern.MatchString(inputDigest) {
		return testgenflow.Operation{}, false, fmt.Errorf("invalid test-generation operation identity or input digest")
	}
	inputDigest = strings.ToLower(inputDigest)
	tx, err := s.beginTestGenerationClaim(ctx, claim)
	if err != nil {
		return testgenflow.Operation{}, false, err
	}
	defer tx.Rollback()
	op, err := scanTestGenerationOperation(tx.QueryRowContext(ctx, testGenerationOperationSelect+` WHERE run_id = $1 AND operation_key = $2`, claim.RunID, key))
	created := errors.Is(err, sql.ErrNoRows)
	if err != nil && !created {
		return testgenflow.Operation{}, false, fmt.Errorf("read test-generation operation: %w", err)
	}
	if created {
		op, err = scanTestGenerationOperation(tx.QueryRowContext(ctx, `
			INSERT INTO development_test_generation_operations(run_id, operation_key, kind, input_digest, state, created_at, updated_at)
			VALUES ($1, $2, $3, $4, 'prepared', statement_timestamp(), statement_timestamp())
			RETURNING operation_key, run_id, kind, input_digest, state, result_bytes, diagnostic, created_at, updated_at`, claim.RunID, key, kind, inputDigest))
		if err != nil {
			return testgenflow.Operation{}, false, fmt.Errorf("prepare test-generation operation: %w", err)
		}
	} else if op.Kind != kind || op.InputDigest != inputDigest {
		return testgenflow.Operation{}, false, testgenflow.ErrOperationConflict
	}
	if err := commitTestGenerationClaim(ctx, tx, claim); err != nil {
		return testgenflow.Operation{}, false, err
	}
	return op, created, nil
}

func (s *Store) FinishTestGenerationOperation(ctx context.Context, claim testgenflow.FlowClaim, key, state string, result json.RawMessage, diagnostic string) error {
	if !flowOperationIDPattern.MatchString(key) || state != "completed" && state != "failed" && state != "unknown" {
		return fmt.Errorf("invalid test-generation operation identity or outcome")
	}
	if len(result) == 0 {
		result = json.RawMessage(`null`)
	}
	// Callers must supply structured, nonsecret outcomes, never raw worker logs.
	// Reject common credential markers as defense in depth, not as a log scrubber.
	if len(result) > 65536 || !json.Valid(result) || !utf8.Valid(result) || len(diagnostic) > 1024 || !utf8.ValidString(diagnostic) || strings.IndexFunc(diagnostic, unicode.IsControl) >= 0 || flowCredentialPattern.Match(result) || flowCredentialPattern.MatchString(diagnostic) {
		return fmt.Errorf("test-generation operation result or diagnostic is invalid, oversized, or contains credentials")
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, result); err != nil {
		return fmt.Errorf("invalid test-generation operation result")
	}
	result = compact.Bytes()
	tx, err := s.beginTestGenerationClaim(ctx, claim)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	op, err := scanTestGenerationOperation(tx.QueryRowContext(ctx, testGenerationOperationSelect+` WHERE run_id = $1 AND operation_key = $2`, claim.RunID, key))
	if errors.Is(err, sql.ErrNoRows) {
		return testgenflow.ErrOperationConflict
	}
	if err != nil {
		return fmt.Errorf("read test-generation operation outcome: %w", err)
	}
	if op.State == "completed" || op.State == "failed" || op.State == "unknown" && state == "unknown" {
		if op.State != state || !bytes.Equal(op.Result, result) || op.Diagnostic != diagnostic {
			return testgenflow.ErrOperationConflict
		}
	} else if _, err := tx.ExecContext(ctx, `
		UPDATE development_test_generation_operations
		SET state = $3, result_bytes = $4, diagnostic = $5, updated_at = clock_timestamp()
		WHERE run_id = $1 AND operation_key = $2`, claim.RunID, key, state, []byte(result), diagnostic); err != nil {
		return fmt.Errorf("finish test-generation operation: %w", err)
	}
	return commitTestGenerationClaim(ctx, tx, claim)
}

func (s *Store) ListTestGenerationOperations(ctx context.Context, runID string) ([]testgenflow.Operation, error) {
	if !flowOperationIDPattern.MatchString(runID) {
		return nil, fmt.Errorf("invalid test-generation run identity")
	}
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("test-generation run store is unavailable")
	}
	rows, err := s.db.QueryContext(ctx, testGenerationOperationSelect+` WHERE run_id = $1 ORDER BY created_at, operation_key`, runID)
	if err != nil {
		return nil, fmt.Errorf("list test-generation operations: %w", err)
	}
	defer rows.Close()
	ops := []testgenflow.Operation{}
	for rows.Next() {
		op, err := scanTestGenerationOperation(rows)
		if err != nil {
			return nil, fmt.Errorf("read test-generation operation: %w", err)
		}
		ops = append(ops, op)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate test-generation operations: %w", err)
	}
	return ops, nil
}

func (s *Store) beginTestGenerationClaim(ctx context.Context, claim testgenflow.FlowClaim) (*sql.Tx, error) {
	if !flowOperationIDPattern.MatchString(claim.RunID) || !flowOperationIDPattern.MatchString(claim.Holder) || claim.Fence < 1 {
		return nil, testgenflow.ErrFlowClaimLost
	}
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("test-generation run store is unavailable")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin claimed test-generation write: %w", err)
	}
	var runID string
	err = tx.QueryRowContext(ctx, `SELECT run_id FROM development_test_generation_runs WHERE run_id = $1 FOR UPDATE`, claim.RunID).Scan(&runID)
	if errors.Is(err, sql.ErrNoRows) {
		tx.Rollback()
		return nil, testgenflow.ErrFlowClaimLost
	}
	if err != nil {
		tx.Rollback()
		return nil, fmt.Errorf("lock claimed test-generation run: %w", err)
	}
	// Check the database clock after acquiring the lock, not before a lock wait.
	if err := checkTestGenerationClaim(ctx, tx, claim); err != nil {
		tx.Rollback()
		return nil, err
	}
	return tx, nil
}

func checkTestGenerationClaim(ctx context.Context, tx *sql.Tx, claim testgenflow.FlowClaim) error {
	var valid bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM development_test_generation_runs
		WHERE run_id = $1 AND claim_holder = $2 AND claim_fence = $3 AND claim_lease_until > clock_timestamp())`, claim.RunID, claim.Holder, claim.Fence).Scan(&valid); err != nil {
		return fmt.Errorf("check test-generation claim: %w", err)
	}
	if !valid {
		return testgenflow.ErrFlowClaimLost
	}
	return nil
}

func commitTestGenerationClaim(ctx context.Context, tx *sql.Tx, claim testgenflow.FlowClaim) error {
	if err := checkTestGenerationClaim(ctx, tx, claim); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit claimed test-generation write: %w", err)
	}
	return nil
}

const testGenerationOperationSelect = `
	SELECT operation_key, run_id, kind, input_digest, state, result_bytes, diagnostic, created_at, updated_at
	FROM development_test_generation_operations`

func scanTestGenerationOperation(row rowScanner) (testgenflow.Operation, error) {
	var op testgenflow.Operation
	var result []byte
	err := row.Scan(&op.Key, &op.RunID, &op.Kind, &op.InputDigest, &op.State, &result, &op.Diagnostic, &op.CreatedAt, &op.UpdatedAt)
	op.Result = json.RawMessage(result)
	return op, err
}
