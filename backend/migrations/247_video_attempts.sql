-- Fixed customer-price native videos. Reservations share the existing balance,
-- quota and usage-dedup ledger. No content or credentials are retained.
CREATE TABLE IF NOT EXISTS video_attempts (
    id VARCHAR(128) PRIMARY KEY,
    quote_id VARCHAR(128) NOT NULL UNIQUE,
    request_fingerprint CHAR(64) NOT NULL,
    user_id BIGINT NOT NULL REFERENCES users(id),
    api_key_id BIGINT NOT NULL REFERENCES api_keys(id),
    group_id BIGINT NOT NULL REFERENCES groups(id),
    account_id BIGINT NOT NULL REFERENCES accounts(id),
    subscription_id BIGINT REFERENCES user_subscriptions(id),
    model VARCHAR(100) NOT NULL,
    mode VARCHAR(3) NOT NULL CHECK (mode IN ('t2v','i2v')),
    controls JSONB NOT NULL CHECK (jsonb_typeof(controls) = 'object'),
    base_price NUMERIC(20,8) NOT NULL CHECK (base_price >= 0 AND base_price < 10000000000),
    total_price NUMERIC(20,8) NOT NULL CHECK (total_price >= 0 AND total_price < 10000000000),
    multiplier NUMERIC NOT NULL CHECK (multiplier >= 0),
    account_cost NUMERIC(20,8) NOT NULL CHECK (account_cost >= 0 AND account_cost < 10000000000),
    account_multiplier NUMERIC NOT NULL CHECK (account_multiplier >= 0),
    tariff_revision TEXT NOT NULL,
    contract_revision TEXT NOT NULL,
    supplier_estimated BOOLEAN NOT NULL,
    state VARCHAR(16) NOT NULL DEFAULT 'accepted'
        CHECK (state IN ('accepted','submitting','uncertain','processing','completed','failed')),
    financial_state VARCHAR(16) NOT NULL DEFAULT 'reserved'
        CHECK (financial_state IN ('reserved','captured','released')),
    upstream_task_id VARCHAR(255),
    result_urls JSONB NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(result_urls) = 'array'),
    receipt_usage_log_id BIGINT REFERENCES usage_logs(id),
    receipt_captured_at TIMESTAMPTZ,
    lease_owner TEXT,
    lease_token TEXT,
    lease_expires_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK ((financial_state = 'captured' AND state = 'completed' AND receipt_usage_log_id IS NOT NULL AND receipt_captured_at IS NOT NULL)
        OR (financial_state = 'released' AND state = 'failed' AND receipt_usage_log_id IS NULL AND receipt_captured_at IS NULL)
        OR (financial_state = 'reserved' AND state NOT IN ('completed','failed') AND receipt_usage_log_id IS NULL AND receipt_captured_at IS NULL)),
    CHECK (request_fingerprint ~ '^[0-9a-f]{64}$')
);
CREATE UNIQUE INDEX IF NOT EXISTS video_attempts_account_upstream_unique
    ON video_attempts(account_id, upstream_task_id) WHERE upstream_task_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS video_attempts_owner ON video_attempts(api_key_id,user_id,created_at);
CREATE INDEX IF NOT EXISTS video_attempts_reconcile ON video_attempts(updated_at)
    WHERE financial_state = 'reserved';
CREATE INDEX IF NOT EXISTS video_attempts_reserved_user ON video_attempts(user_id) WHERE financial_state = 'reserved';
CREATE INDEX IF NOT EXISTS video_attempts_reserved_key ON video_attempts(api_key_id) WHERE financial_state = 'reserved';
CREATE INDEX IF NOT EXISTS video_attempts_reserved_account ON video_attempts(account_id) WHERE financial_state = 'reserved';
CREATE INDEX IF NOT EXISTS video_attempts_reserved_subscription ON video_attempts(subscription_id) WHERE financial_state = 'reserved';
