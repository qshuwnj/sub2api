//go:build integration

package repository

import (
	"context"
	"testing"

	dbmigrations "github.com/Wei-Shaw/sub2api/migrations"
	"github.com/stretchr/testify/require"
)

func TestVideoFrameAssetMigrationPreservesReferencesAndRejectsNonImageFrames(t *testing.T) {
	tx := testTx(t)
	ctx := context.Background()
	_, err := tx.ExecContext(ctx, `
		CREATE SCHEMA video_frame_migration_test;
		SET LOCAL search_path TO video_frame_migration_test;
		CREATE TABLE users (id BIGINT PRIMARY KEY);
		CREATE TABLE api_keys (id BIGINT PRIMARY KEY);
		INSERT INTO users VALUES (1);
		INSERT INTO api_keys VALUES (2);
	`)
	require.NoError(t, err)
	previous, err := dbmigrations.FS.ReadFile("250_video_reference_assets.sql")
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, string(previous))
	require.NoError(t, err)
	const insert = `
		INSERT INTO video_reference_assets
		(id,user_id,api_key_id,endpoint_family,kind,mime,size_bytes,storage_key,expires_at)
		VALUES ($1,1,2,$2,$3,'image/png',100,'fixture',NOW()+INTERVAL '30 minutes')
	`
	_, err = tx.ExecContext(ctx, insert, "legacy", "2.0", "image")
	require.NoError(t, err)
	migration, err := dbmigrations.FS.ReadFile("251_video_frame_assets.sql")
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, string(migration))
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, insert, "frame", "frame:bytedance/seedance-2.0-fast/image-to-video", "image")
	require.NoError(t, err)
	for _, tc := range []struct{ family, kind string }{
		{"unrecognized", "image"},
		{"frame:bytedance/seedance-2.0/image-to-video", "video"},
		{"frame:bytedance/seedance-2.0/image-to-video", "audio"},
	} {
		_, err = tx.ExecContext(ctx, "SAVEPOINT invalid_frame")
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, insert, "invalid", tc.family, tc.kind)
		require.Error(t, err)
		_, err = tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT invalid_frame")
		require.NoError(t, err)
	}
	// The migration is replayable and never rewrites or removes staged rows.
	_, err = tx.ExecContext(ctx, string(migration))
	require.NoError(t, err)
	var count int
	require.NoError(t, tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM video_reference_assets").Scan(&count))
	require.Equal(t, 2, count)
	var family string
	require.NoError(t, tx.QueryRowContext(ctx, "SELECT endpoint_family FROM video_reference_assets WHERE id='legacy'").Scan(&family))
	require.Equal(t, "2.0", family)
}
