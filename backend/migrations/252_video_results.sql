-- Video output retention is independent of the short-lived input-asset TTL
-- and independent of financial settlement. Store object keys, not signatures.
CREATE TABLE IF NOT EXISTS video_results (
    user_id BIGINT NOT NULL REFERENCES users(id),
    api_key_id BIGINT NOT NULL REFERENCES api_keys(id),
    resource VARCHAR(160) NOT NULL,
    upstream_task_id VARCHAR(128) NOT NULL,
    account_id BIGINT NOT NULL,
    source_url TEXT NOT NULL DEFAULT '',
    storage_key TEXT NOT NULL DEFAULT '',
    mime VARCHAR(64) NOT NULL DEFAULT '',
    size_bytes BIGINT NOT NULL DEFAULT 0,
    attempts INTEGER NOT NULL DEFAULT 0,
    next_retry_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    lease_token TEXT NOT NULL DEFAULT '',
    lease_expires_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (user_id, api_key_id, resource),
    CHECK (size_bytes >= 0),
    CHECK (storage_key = '' OR (source_url = '' AND size_bytes > 0))
);
CREATE INDEX IF NOT EXISTS video_results_pending_idx
    ON video_results (next_retry_at) WHERE storage_key = '';
