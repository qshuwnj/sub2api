-- Separate scheduling state for server-proven owned candidates, never asset
-- tombstones or untrusted caller identities. A failed check backs off fairly.
CREATE TABLE IF NOT EXISTS studio_asset_recovery_checks (
    user_id BIGINT NOT NULL REFERENCES users(id),
    api_key_id BIGINT NOT NULL REFERENCES api_keys(id),
    resource VARCHAR(160) NOT NULL CHECK (resource ~ '^(attempt|task):[A-Za-z0-9_.:-]{1,128}$'),
    purpose VARCHAR(16) NOT NULL CHECK (purpose IN ('history', 'lookup', 'index')),
    last_checked_at TIMESTAMPTZ NOT NULL,
    next_retry_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (user_id, api_key_id, resource, purpose)
);
CREATE INDEX IF NOT EXISTS studio_asset_recovery_checks_due_idx
    ON studio_asset_recovery_checks(purpose, next_retry_at, last_checked_at);
