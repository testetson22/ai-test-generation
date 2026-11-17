CREATE TABLE IF NOT EXISTS execution_pairs (
    pair_id TEXT PRIMARY KEY,
    run_id TEXT NOT NULL,
    generation_id TEXT NOT NULL,
    comparison_mode TEXT NOT NULL CHECK (comparison_mode IN ('same_subject_control', 'differential')),
    idempotency_key TEXT NOT NULL,
    request_digest TEXT NOT NULL,
    base_task_id TEXT NOT NULL,
    candidate_task_id TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    UNIQUE (run_id, idempotency_key)
);

CREATE TABLE IF NOT EXISTS execution_tasks (
    task_id TEXT PRIMARY KEY,
    pair_id TEXT NOT NULL REFERENCES execution_pairs(pair_id),
    run_id TEXT NOT NULL,
    subject_role TEXT NOT NULL CHECK (subject_role IN ('base', 'candidate')),
    specification_bytes BYTEA NOT NULL,
    specification_digest TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('queued', 'eligible', 'dispatching', 'submitted', 'running', 'collecting', 'completed', 'failed', 'blocked', 'not_reached', 'cancel_requested', 'cancelled', 'outcome_unknown', 'superseded')),
    revision BIGINT NOT NULL CHECK (revision >= 1),
    attempt_count INTEGER NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    active_attempt_id TEXT,
    next_action_at TIMESTAMPTZ NOT NULL,
    terminal_reason TEXT,
    cancel_requested_at TIMESTAMPTZ,
    execution_result_bytes BYTEA,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    UNIQUE (pair_id, subject_role)
);

CREATE TABLE IF NOT EXISTS execution_attempts (
    attempt_id TEXT PRIMARY KEY,
    task_id TEXT NOT NULL REFERENCES execution_tasks(task_id),
    attempt_number INTEGER NOT NULL CHECK (attempt_number >= 1),
    task_spec_digest TEXT NOT NULL,
    submission_token TEXT NOT NULL UNIQUE,
    status TEXT NOT NULL CHECK (status IN ('prepared', 'submission_unknown', 'submitted', 'running', 'collecting', 'completed', 'failed', 'blocked', 'not_reached', 'cancel_requested', 'cancelled', 'outcome_unknown', 'superseded')),
    provider_id TEXT NOT NULL,
    provider_identity_bytes BYTEA NOT NULL,
    provider_job_id TEXT,
    claim_holder TEXT,
    lease_expires_at TIMESTAMPTZ,
    fencing_token BIGINT NOT NULL CHECK (fencing_token >= 1),
    next_action_at TIMESTAMPTZ NOT NULL,
    retry_authorization_bytes BYTEA,
    failure_category TEXT,
    started_at TIMESTAMPTZ,
    finished_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    UNIQUE (task_id, attempt_number)
);

CREATE TABLE IF NOT EXISTS execution_retry_decisions (
    decision_id TEXT PRIMARY KEY,
    task_id TEXT NOT NULL REFERENCES execution_tasks(task_id),
    predecessor_attempt_id TEXT NOT NULL REFERENCES execution_attempts(attempt_id),
    policy_id TEXT NOT NULL,
    failure_category TEXT NOT NULL,
    authorized_at TIMESTAMPTZ NOT NULL,
    next_attempt_at TIMESTAMPTZ NOT NULL,
    payload_bytes BYTEA NOT NULL,
    UNIQUE (task_id, predecessor_attempt_id)
);

CREATE TABLE IF NOT EXISTS execution_provider_observations (
    observation_id TEXT PRIMARY KEY,
    attempt_id TEXT NOT NULL REFERENCES execution_attempts(attempt_id),
    sequence BIGINT NOT NULL CHECK (sequence >= 1),
    observation_digest TEXT NOT NULL,
    payload_bytes BYTEA NOT NULL,
    received_at TIMESTAMPTZ NOT NULL,
    UNIQUE (attempt_id, sequence)
);

CREATE TABLE IF NOT EXISTS execution_task_events (
    event_id TEXT PRIMARY KEY,
    task_id TEXT NOT NULL REFERENCES execution_tasks(task_id),
    expected_revision BIGINT NOT NULL CHECK (expected_revision >= 0),
    revision BIGINT NOT NULL CHECK (revision > expected_revision),
    event_type TEXT NOT NULL,
    payload_digest TEXT NOT NULL,
    payload_bytes BYTEA NOT NULL,
    occurred_at TIMESTAMPTZ NOT NULL,
    UNIQUE (task_id, revision)
);

CREATE INDEX IF NOT EXISTS execution_tasks_due_idx
    ON execution_tasks (next_action_at, created_at)
    WHERE status IN ('queued', 'eligible');

CREATE INDEX IF NOT EXISTS execution_attempts_due_idx
    ON execution_attempts (next_action_at, lease_expires_at)
    WHERE status IN ('prepared', 'submission_unknown', 'submitted', 'running', 'outcome_unknown', 'cancel_requested');

CREATE INDEX IF NOT EXISTS execution_provider_observations_latest_idx
    ON execution_provider_observations (attempt_id, sequence DESC);
