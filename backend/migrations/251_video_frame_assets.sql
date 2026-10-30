-- Keep existing reference-media families intact while allowing image-only
-- frame uploads bound to an exact, server-verified video model.
ALTER TABLE video_reference_assets
    DROP CONSTRAINT IF EXISTS video_reference_assets_endpoint_family_check;

ALTER TABLE video_reference_assets
    ALTER COLUMN endpoint_family TYPE VARCHAR(512);

ALTER TABLE video_reference_assets
    ADD CONSTRAINT video_reference_assets_endpoint_family_check
    CHECK (
        endpoint_family IN ('2.0', '2.5', '2.0-mini', '2.0-fast')
        OR (kind = 'image' AND endpoint_family LIKE 'frame:%')
    );
