ALTER TABLE execution_attempts
    ADD COLUMN termination_confirmed BOOLEAN NOT NULL DEFAULT FALSE;

CREATE TABLE execution_attempt_results (
    attempt_id TEXT PRIMARY KEY REFERENCES execution_attempts(attempt_id) ON DELETE CASCADE,
    expected_task_revision BIGINT NOT NULL CHECK (expected_task_revision >= 1),
    provisional_reference_bytes BYTEA NOT NULL,
    finalized_reference_bytes BYTEA,
    projected_revision BIGINT,
    provisional_at TIMESTAMPTZ NOT NULL,
    finalized_at TIMESTAMPTZ
);

-- Recover every recorded delivery, not just the task's latest result pointer.
INSERT INTO execution_attempt_results (
    attempt_id, expected_task_revision, provisional_reference_bytes,
    finalized_reference_bytes, projected_revision, provisional_at, finalized_at
)
SELECT p.payload ->> 'attempt_id', p.expected_revision,
       convert_to((p.payload -> 'result')::text, 'UTF8'),
       CASE WHEN f.payload IS NOT NULL THEN convert_to((f.payload -> 'result')::text, 'UTF8') END,
       p.revision, p.occurred_at, f.occurred_at
FROM (
    SELECT e.*, convert_from(e.payload_bytes, 'UTF8')::jsonb AS payload
    FROM execution_task_events e WHERE e.event_type = 'result_provisional'
) p
JOIN execution_attempts a ON a.attempt_id = p.payload ->> 'attempt_id' AND a.task_id = p.task_id
LEFT JOIN LATERAL (
    SELECT convert_from(e.payload_bytes, 'UTF8')::jsonb AS payload, e.occurred_at
    FROM execution_task_events e
    WHERE e.task_id = p.task_id AND e.event_type = 'execution_result_finalized'
      AND convert_from(e.payload_bytes, 'UTF8')::jsonb ->> 'attempt_id' = a.attempt_id
      AND convert_from(e.payload_bytes, 'UTF8')::jsonb -> 'provisional_result' = p.payload -> 'result'
    ORDER BY e.revision LIMIT 1
) f ON TRUE;

-- Older terminal statuses alone do not establish termination. Only reuse proof
-- that the previous implementation already required or durably recorded.
UPDATE execution_attempts a SET termination_confirmed = TRUE
WHERE a.status = 'cancelled' OR EXISTS (
    SELECT 1 FROM execution_task_events e
    WHERE e.task_id = a.task_id AND e.event_type = 'execution_result_finalized'
      AND convert_from(e.payload_bytes, 'UTF8')::jsonb ->> 'attempt_id' = a.attempt_id
      AND convert_from(e.payload_bytes, 'UTF8')::jsonb -> 'cleanup' ->> 'termination_confirmed' = 'true'
);
