package repository

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// This opt-in fixture is intentionally restricted to the controller's disposable
// local PostgreSQL database. It never accepts a production database URL.
func TestStudioAssetPostgresDeletedInflightUploadRecoversWithoutLateRecord(t *testing.T) {
	dsn := os.Getenv("STUDIO_ASSET_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set STUDIO_ASSET_TEST_POSTGRES_DSN for the disposable local fixture")
	}
	u, err := url.Parse(dsn)
	require.NoError(t, err)
	require.Equal(t, "127.0.0.1", u.Hostname())
	require.Equal(t, "55439", u.Port())
	require.Equal(t, "/studio_library_fixture", u.Path)
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	schema := "studio_task2_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	quotedSchema := pq.QuoteIdentifier(schema)
	_, err = db.ExecContext(ctx, "CREATE SCHEMA "+quotedSchema)
	require.NoError(t, err)
	t.Cleanup(func() { _, dropErr := db.Exec("DROP SCHEMA " + quotedSchema + " CASCADE"); require.NoError(t, dropErr) })
	_, err = db.ExecContext(ctx, "SET search_path TO "+quotedSchema)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, "CREATE TABLE users(id BIGINT PRIMARY KEY); CREATE TABLE api_keys(id BIGINT PRIMARY KEY); INSERT INTO users VALUES (16); INSERT INTO api_keys VALUES (20)")
	require.NoError(t, err)
	migration, err := os.ReadFile(filepath.Join("..", "..", "migrations", "253_studio_assets.sql"))
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, string(migration))
	require.NoError(t, err)
	r := &usageBillingRepository{db: db}
	a := studioFixture()
	require.NoError(t, r.UpsertStudioAsset(ctx, a))
	claimed, err := r.ClaimStudioAssets(ctx, 1)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	require.NoError(t, r.DeleteStudioAsset(ctx, a.Owner, a.ID))
	_, err = r.GetStudioAsset(ctx, a.Owner, a.ID)
	require.ErrorIs(t, err, service.ErrStudioAssetNotFound)
	var token string
	var horizon sql.NullTime
	require.NoError(t, db.QueryRowContext(ctx, "SELECT lease_token, lease_expires_at FROM studio_assets WHERE id=$1", a.ID).Scan(&token, &horizon))
	require.True(t, horizon.Valid, "deletion must preserve the active lease expiry instead of making in-flight cleanup immediately eligible")
	require.Equal(t, claimed[0].LeaseToken, token, "deletion must preserve the matched active lease until its upload horizon")
	require.True(t, horizon.Time.After(time.Now().Add(3*time.Minute)))
	cleanup, err := r.ClaimDeletedStudioAssets(ctx, 1)
	require.NoError(t, err)
	require.Empty(t, cleanup, "cleanup must not run before an in-flight archive lease horizon")
	// No RecordStudioAssetCleanup call succeeds: the tombstone alone must recover.
	_, err = db.ExecContext(ctx, "UPDATE studio_assets SET lease_expires_at=NOW()-INTERVAL '1 second' WHERE id=$1", a.ID)
	require.NoError(t, err)
	cleanup, err = r.ClaimDeletedStudioAssets(ctx, 1)
	require.NoError(t, err)
	require.Len(t, cleanup, 1)
	require.Empty(t, cleanup[0].StorageKey)
	cleanup[0].StorageKey = "studio-results/16/20/6b598d3d55679cadfaa60ffa00eaae588211fd2dee326eaf855bbd1f8653c398/0"
	require.NoError(t, r.CompleteStudioAssetCleanup(ctx, cleanup[0]))
	cleanupAgain, err := r.ClaimDeletedStudioAssets(ctx, 1)
	require.NoError(t, err)
	require.Empty(t, cleanupAgain, "completed empty-key image tombstones need a completion marker")
	// A genuinely late result requeues cleanup without violating ready/source or
	// matched-token/expiry CHECK constraints, and invalidates the older cleaner.
	require.NoError(t, r.RecordStudioAssetCleanup(ctx, cleanup[0]))
	require.ErrorIs(t, r.CompleteStudioAssetCleanup(ctx, cleanup[0]), service.ErrStudioAssetUnavailable)
	var state, key string
	require.NoError(t, db.QueryRowContext(ctx, "SELECT state, storage_key, lease_token, lease_expires_at FROM studio_assets WHERE id=$1", a.ID).Scan(&state, &key, &token, &horizon))
	require.Equal(t, "pending", state)
	require.Empty(t, key)
	require.NotEmpty(t, token)
	require.NotEqual(t, cleanup[0].LeaseToken, token)
	require.True(t, horizon.Valid)
	require.True(t, horizon.Time.After(time.Now().Add(3*time.Minute)))
	_, err = db.ExecContext(ctx, "UPDATE studio_assets SET lease_expires_at=NOW()-INTERVAL '1 second' WHERE id=$1", a.ID)
	require.NoError(t, err)
	cleanup, err = r.ClaimDeletedStudioAssets(ctx, 1)
	require.NoError(t, err)
	require.Len(t, cleanup, 1)
	cleanup[0].StorageKey = "studio-results/16/20/6b598d3d55679cadfaa60ffa00eaae588211fd2dee326eaf855bbd1f8653c398/0"
	// An active cleaner may have deleted the old object just before a late
	// upload recreates it. Recording that upload must invalidate this cleaner.
	require.NoError(t, r.RecordStudioAssetCleanup(ctx, cleanup[0]))
	require.ErrorIs(t, r.CompleteStudioAssetCleanup(ctx, cleanup[0]), service.ErrStudioAssetUnavailable)
	require.NoError(t, db.QueryRowContext(ctx, "SELECT lease_token FROM studio_assets WHERE id=$1", a.ID).Scan(&token))
	require.NotEqual(t, cleanup[0].LeaseToken, token)
	_, err = db.ExecContext(ctx, "UPDATE studio_assets SET lease_expires_at=NOW()-INTERVAL '1 second' WHERE id=$1", a.ID)
	require.NoError(t, err)
	cleanup, err = r.ClaimDeletedStudioAssets(ctx, 1)
	require.NoError(t, err)
	require.Len(t, cleanup, 1)
	cleanup[0].StorageKey = "studio-results/16/20/6b598d3d55679cadfaa60ffa00eaae588211fd2dee326eaf855bbd1f8653c398/0"
	require.NoError(t, r.CompleteStudioAssetCleanup(ctx, cleanup[0]))
	// Cleanup recording must never change a newer live ready row or its key.
	live := studioFixture()
	live.GenerationID = "newer-live-generation"
	require.NoError(t, r.UpsertStudioAsset(ctx, live))
	claimed, err = r.ClaimStudioAssets(ctx, 1)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	claimed[0].StorageKey, claimed[0].MIME, claimed[0].SizeBytes = "studio-results/16/20/newer-live/0", "image/png", 42
	require.NoError(t, r.CompleteStudioAsset(ctx, claimed[0]))
	require.NoError(t, r.RecordStudioAssetCleanup(ctx, claimed[0]))
	live, err = r.GetStudioAsset(ctx, live.Owner, live.ID)
	require.NoError(t, err)
	require.Equal(t, "ready", live.State)
	require.Equal(t, "studio-results/16/20/newer-live/0", live.StorageKey)
	require.Empty(t, live.LeaseToken)

	t.Run("terminal archive outcome is not cleanup completion", func(t *testing.T) {
		terminal := studioFixture()
		terminal.OutputIndex = 1
		require.NoError(t, r.UpsertStudioAsset(ctx, terminal))
		archive, err := r.ClaimStudioAssets(ctx, 1)
		require.NoError(t, err)
		require.Len(t, archive, 1)
		// Save may have accepted this deterministic object before returning an error.
		const attemptedKey = "studio-results/16/20/6b598d3d55679cadfaa60ffa00eaae588211fd2dee326eaf855bbd1f8653c398/1"
		archive[0].StorageKey = attemptedKey
		require.NoError(t, r.RecordStudioAssetCleanup(ctx, archive[0]))
		require.NoError(t, r.RetryStudioAsset(ctx, archive[0], "storage_unavailable"))
		stored, err := r.GetStudioAsset(ctx, terminal.Owner, terminal.ID)
		require.NoError(t, err)
		require.Equal(t, "retrying", stored.State)
		require.Empty(t, stored.StorageKey)
		require.NotEmpty(t, stored.SourceURL)
		_, err = db.ExecContext(ctx, "UPDATE studio_assets SET next_retry_at=NOW() WHERE id=$1", terminal.ID)
		require.NoError(t, err)
		archive, err = r.ClaimStudioAssets(ctx, 1)
		require.NoError(t, err)
		require.Len(t, archive, 1)
		archive[0].State = "unavailable"
		require.NoError(t, r.RetryStudioAsset(ctx, archive[0], "image_too_large"))
		stored, err = r.GetStudioAsset(ctx, terminal.Owner, terminal.ID)
		require.NoError(t, err)
		require.Equal(t, "unavailable", stored.State)
		require.Empty(t, stored.StorageKey)

		require.NoError(t, r.DeleteStudioAsset(ctx, terminal.Owner, terminal.ID))
		require.NoError(t, db.QueryRowContext(ctx, "SELECT state FROM studio_assets WHERE id=$1", terminal.ID).Scan(&state))
		require.Equal(t, "pending", state, "new empty-key image tombstones must begin unfinished physical cleanup")
		_, err = db.ExecContext(ctx, "UPDATE studio_assets SET next_retry_at=NOW() WHERE id=$1", terminal.ID)
		require.NoError(t, err)
		cleanup, err := r.ClaimDeletedStudioAssets(ctx, 1)
		require.NoError(t, err)
		require.Len(t, cleanup, 1)
		require.Equal(t, terminal.ID, cleanup[0].ID)
		require.Empty(t, cleanup[0].StorageKey)
		cleanup[0].StorageKey = attemptedKey
		require.NoError(t, r.CompleteStudioAssetCleanup(ctx, cleanup[0]))
		require.NoError(t, r.DeleteStudioAsset(ctx, terminal.Owner, terminal.ID))
		cleanup, err = r.ClaimDeletedStudioAssets(ctx, 1)
		require.NoError(t, err)
		require.Empty(t, cleanup, "repeated delete must not reset a completed tombstone")
	})
}
