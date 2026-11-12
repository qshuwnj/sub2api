-- Tariffs are opaque documents decoded by Go, never searched/indexed as JSONB.
-- Keeping JSON text avoids expanding every tariff into a JSONB tree on saves.
-- Preserve all fields and numeric values; only change the storage representation.
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '60s';

ALTER TABLE groups
    ALTER COLUMN model_pricing TYPE JSON USING model_pricing::json;

COMMENT ON COLUMN groups.model_pricing IS
    'Opaque group model-pricing document, stored as JSON text to bound update memory; decoded and validated by the application';
