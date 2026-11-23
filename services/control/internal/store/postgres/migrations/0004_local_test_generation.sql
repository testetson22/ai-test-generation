ALTER TABLE development_test_generation_runs
    ADD COLUMN claim_holder TEXT,
    ADD COLUMN claim_lease_until TIMESTAMPTZ,
    ADD COLUMN claim_fence BIGINT NOT NULL DEFAULT 0 CHECK (claim_fence >= 0),
    ADD CONSTRAINT development_test_generation_runs_claim_check
        CHECK ((claim_holder IS NULL) = (claim_lease_until IS NULL));

ALTER TABLE development_test_generation_runs
    DROP CONSTRAINT development_test_generation_runs_stage_check,
    ADD CONSTRAINT development_test_generation_runs_stage_check
        CHECK (stage IN ('source', 'verify_commit', 'baseline_package', 'generate_tests',
                        'artifact_input', 'focused_test', 'final_verify', 'open_pr', 'publish_patch', 'done'));

CREATE TABLE development_test_generation_operations (
    run_id TEXT NOT NULL REFERENCES development_test_generation_runs(run_id) ON DELETE CASCADE,
    operation_key TEXT NOT NULL CHECK (operation_key ~ '^[A-Za-z0-9][A-Za-z0-9._:/-]{0,255}$'),
    kind TEXT NOT NULL CHECK (kind ~ '^[A-Za-z0-9][A-Za-z0-9._:/-]{0,255}$'),
    input_digest TEXT NOT NULL CHECK (input_digest ~ '^sha256:[a-f0-9]{64}$'),
    state TEXT NOT NULL CHECK (state IN ('prepared', 'completed', 'failed', 'unknown')),
    result_bytes BYTEA CHECK (octet_length(result_bytes) <= 65536),
    diagnostic TEXT NOT NULL DEFAULT '' CHECK (octet_length(diagnostic) <= 1024),
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (run_id, operation_key),
    CHECK (updated_at >= created_at),
    CHECK ((state = 'prepared') = (result_bytes IS NULL)),
    CHECK (state <> 'prepared' OR (result_bytes IS NULL AND diagnostic = ''))
);
