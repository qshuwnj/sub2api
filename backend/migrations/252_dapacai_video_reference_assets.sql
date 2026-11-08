-- Allow reference media to be staged for exact Dapacai video models.
-- The service validates each model and its supported media kinds before insert.
ALTER TABLE video_reference_assets
    DROP CONSTRAINT IF EXISTS video_reference_assets_endpoint_family_check;

ALTER TABLE video_reference_assets
    ADD CONSTRAINT video_reference_assets_endpoint_family_check
    CHECK (
        endpoint_family IN ('2.0', '2.5', '2.0-mini', '2.0-fast')
        OR (kind = 'image' AND endpoint_family LIKE 'frame:%')
        OR endpoint_family ~ '^dapacai:[a-z0-9][a-z0-9.-]*$'
    );
