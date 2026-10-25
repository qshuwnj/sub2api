-- Reserve durable storage for reference-to-video attempts without changing
-- existing t2v/i2v rows or their settlement invariants.
DO $$
DECLARE
    constraint_name TEXT;
BEGIN
    SELECT con.conname
    INTO constraint_name
    FROM pg_constraint AS con
    WHERE con.conrelid = 'video_attempts'::regclass
      AND con.contype = 'c'
      AND pg_get_constraintdef(con.oid) LIKE '%mode%'
      AND pg_get_constraintdef(con.oid) LIKE '%t2v%'
    LIMIT 1;

    IF constraint_name IS NOT NULL THEN
        EXECUTE format('ALTER TABLE video_attempts DROP CONSTRAINT %I', constraint_name);
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pg_constraint
        WHERE conrelid = 'video_attempts'::regclass
          AND conname = 'video_attempts_mode_check'
    ) THEN
        ALTER TABLE video_attempts
            ADD CONSTRAINT video_attempts_mode_check
            CHECK (mode IN ('t2v','i2v','r2v'));
    END IF;
END
$$;

ALTER TABLE video_attempts
    ADD COLUMN IF NOT EXISTS reference_descriptors JSONB NOT NULL DEFAULT '{}'::jsonb
        CHECK (jsonb_typeof(reference_descriptors) = 'object');

ALTER TABLE video_attempts
    ADD COLUMN IF NOT EXISTS reference_fingerprint CHAR(64) NOT NULL DEFAULT ''
        CHECK (reference_fingerprint = '' OR reference_fingerprint ~ '^[0-9a-f]{64}$');
