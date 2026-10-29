-- Durable ownership and lifecycle metadata for Seedance reference media.
-- Media bytes are stored separately in object storage; this table never stores
-- the uploaded content itself.
CREATE TABLE IF NOT EXISTS video_reference_assets (
    id VARCHAR(128) PRIMARY KEY,
    user_id BIGINT NOT NULL REFERENCES users(id),
    api_key_id BIGINT NOT NULL REFERENCES api_keys(id),
    account_id BIGINT NOT NULL DEFAULT 0,
    endpoint_family VARCHAR(32) NOT NULL
        CHECK (endpoint_family IN ('2.0','2.5','2.0-mini','2.0-fast')),
    kind VARCHAR(16) NOT NULL
        CHECK (kind IN ('image','video','audio')),
    mime VARCHAR(128) NOT NULL,
    size_bytes BIGINT NOT NULL CHECK (size_bytes > 0),
    width INTEGER NOT NULL DEFAULT 0 CHECK (width >= 0),
    height INTEGER NOT NULL DEFAULT 0 CHECK (height >= 0),
    duration_seconds DOUBLE PRECISION NOT NULL DEFAULT 0 CHECK (duration_seconds >= 0),
    frame_rate DOUBLE PRECISION NOT NULL DEFAULT 0 CHECK (frame_rate >= 0),
    storage_key TEXT NOT NULL,
    source_url TEXT NOT NULL DEFAULT '',
    expires_at TIMESTAMPTZ NOT NULL,
    cleanup_state VARCHAR(16) NOT NULL DEFAULT 'staged'
        CHECK (cleanup_state IN ('staged','accepted','cancelled','expired','failed')),
    accepted_attempt_id VARCHAR(128) NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS video_reference_assets_owner
    ON video_reference_assets(user_id, api_key_id, created_at);
CREATE INDEX IF NOT EXISTS video_reference_assets_cleanup
    ON video_reference_assets(expires_at, cleanup_state);
CREATE INDEX IF NOT EXISTS video_reference_assets_attempt
    ON video_reference_assets(accepted_attempt_id)
    WHERE accepted_attempt_id <> '';
