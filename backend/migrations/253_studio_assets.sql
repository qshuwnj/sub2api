-- The private media index is independent of generation settlement and caches.
-- Keep deleted rows as generation/output tombstones even after object cleanup.
CREATE TABLE IF NOT EXISTS studio_assets (
    id VARCHAR(38) PRIMARY KEY CHECK (id ~ '^asset_[0-9a-f]{32}$'),
    user_id BIGINT NOT NULL REFERENCES users(id),
    api_key_id BIGINT NOT NULL REFERENCES api_keys(id),
    kind VARCHAR(8) NOT NULL CHECK (kind IN ('image', 'video')),
    generation_id VARCHAR(256) NOT NULL CHECK (generation_id <> ''),
    output_index INTEGER NOT NULL CHECK (output_index >= 0),
    video_resource VARCHAR(160) NOT NULL DEFAULT '',
    model TEXT NOT NULL DEFAULT '',
    prompt TEXT NOT NULL DEFAULT '',
    mime VARCHAR(127) NOT NULL DEFAULT '',
    storage_key TEXT NOT NULL DEFAULT '',
    state VARCHAR(16) NOT NULL DEFAULT 'pending'
        CHECK (state IN ('pending', 'ready', 'retrying', 'unavailable')),
    last_error_code VARCHAR(64) NOT NULL DEFAULT '',
    source_url TEXT NOT NULL DEFAULT '',
    source_bytes BYTEA,
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(metadata) = 'object'),
    size_bytes BIGINT NOT NULL DEFAULT 0 CHECK (size_bytes >= 0),
    generated_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at TIMESTAMPTZ,
    attempts INTEGER NOT NULL DEFAULT 0,
    next_retry_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    lease_token TEXT NOT NULL DEFAULT '',
    lease_expires_at TIMESTAMPTZ,
    CHECK (state <> 'ready' OR (storage_key <> '' AND size_bytes > 0
        AND source_url = '' AND source_bytes IS NULL)),
    CHECK (storage_key = '' OR state = 'ready'),
    CHECK ((lease_token = '') = (lease_expires_at IS NULL))
);

CREATE UNIQUE INDEX IF NOT EXISTS studio_assets_generation_idx
    ON studio_assets(user_id,api_key_id,generation_id,output_index);
CREATE INDEX IF NOT EXISTS studio_assets_owner_page_idx
    ON studio_assets(user_id,api_key_id,generated_at DESC,id DESC)
    WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS studio_assets_pending_idx
    ON studio_assets(next_retry_at,id)
    WHERE deleted_at IS NULL AND state IN ('pending', 'retrying');
CREATE INDEX IF NOT EXISTS studio_assets_deleted_cleanup_idx
    ON studio_assets(deleted_at,id)
    WHERE deleted_at IS NOT NULL AND storage_key <> '';
