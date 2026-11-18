CREATE TABLE development_test_generation_runs (
    run_id TEXT PRIMARY KEY,
    idempotency_key TEXT NOT NULL UNIQUE,
    request_digest TEXT NOT NULL,
    state_digest TEXT NOT NULL,
    state_bytes BYTEA NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('queued', 'running', 'cancel_requested', 'completed', 'failed', 'cancelled', 'outcome_unknown')),
    stage TEXT NOT NULL CHECK (stage IN ('source', 'verify_commit', 'baseline_package', 'generate_tests', 'focused_test', 'final_verify', 'open_pr', 'done')),
    revision BIGINT NOT NULL CHECK (revision >= 1),
    next_action_at TIMESTAMPTZ,
    cancel_requested_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE development_test_generation_run_events (
    run_id TEXT NOT NULL REFERENCES development_test_generation_runs(run_id) ON DELETE CASCADE,
    event_id TEXT NOT NULL,
    expected_revision BIGINT NOT NULL CHECK (expected_revision >= 0),
    revision BIGINT NOT NULL CHECK (revision > expected_revision),
    event_type TEXT NOT NULL,
    payload_digest TEXT NOT NULL,
    payload_bytes BYTEA NOT NULL,
    occurred_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (run_id, event_id),
    UNIQUE (run_id, revision)
);

CREATE INDEX development_test_generation_runs_due_idx
    ON development_test_generation_runs (next_action_at, created_at)
    WHERE status IN ('queued', 'running', 'cancel_requested');

CREATE INDEX development_test_generation_run_events_revision_idx
    ON development_test_generation_run_events (run_id, revision DESC);
