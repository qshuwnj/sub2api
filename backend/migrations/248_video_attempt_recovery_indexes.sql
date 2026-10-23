-- Filter known reserved tasks before LIMIT, ordered fairly across workers.
CREATE INDEX IF NOT EXISTS video_attempts_known_reconcile
    ON video_attempts(updated_at, id)
    WHERE financial_state = 'reserved'
      AND upstream_task_id IS NOT NULL
      AND state IN ('processing', 'uncertain');

-- Supplier task IDs are only unique per bound account. Keep this lookup
-- non-unique so ambiguous caller-owned IDs can fail closed at read time.
CREATE INDEX IF NOT EXISTS video_attempts_owned_task
    ON video_attempts(user_id, api_key_id, upstream_task_id)
    WHERE upstream_task_id IS NOT NULL;
