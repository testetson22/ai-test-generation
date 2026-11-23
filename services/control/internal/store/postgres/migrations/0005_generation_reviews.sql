ALTER TABLE development_test_generation_runs
    DROP CONSTRAINT development_test_generation_runs_status_check,
    ADD CONSTRAINT development_test_generation_runs_status_check
        CHECK (status IN ('queued', 'running', 'cancel_requested', 'completed', 'failed',
                          'cancelled', 'outcome_unknown', 'waiting_review', 'rejected', 'blocked'));
