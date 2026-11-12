-- Redis sticky sessions are expendable; accepted paid video jobs are not.
CREATE TABLE IF NOT EXISTS video_task_states (
    request_id VARCHAR(200) NOT NULL,
    user_id BIGINT NOT NULL REFERENCES users(id),
    api_key_id BIGINT NOT NULL REFERENCES api_keys(id),
    group_id BIGINT NOT NULL,
    account_id BIGINT NOT NULL CHECK (account_id > 0),
    pending_billing JSONB NOT NULL CHECK (jsonb_typeof(pending_billing) = 'object'),
    settled BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (request_id, user_id, api_key_id)
);
