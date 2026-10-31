-- Private Studio intent is deliberately separate from immutable quote snapshots.
-- Native tasks are bound only by an authenticated successful gateway submission.
CREATE TABLE IF NOT EXISTS studio_video_intakes (
    user_id BIGINT NOT NULL REFERENCES users(id),
    api_key_id BIGINT NOT NULL REFERENCES api_keys(id),
    resource VARCHAR(160) NOT NULL CHECK (resource ~ '^(attempt|task):[A-Za-z0-9_.:-]{1,128}$'),
    generation_id VARCHAR(160) NOT NULL CHECK (generation_id = resource),
    model TEXT NOT NULL,
    prompt TEXT NOT NULL,
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(metadata) = 'object'),
    completed BOOLEAN NOT NULL DEFAULT FALSE,
    account_id BIGINT NOT NULL DEFAULT 0 CHECK (account_id >= 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (user_id, api_key_id, resource)
);
CREATE INDEX IF NOT EXISTS studio_video_intakes_discovery_idx
    ON studio_video_intakes(created_at, user_id, api_key_id, resource);
