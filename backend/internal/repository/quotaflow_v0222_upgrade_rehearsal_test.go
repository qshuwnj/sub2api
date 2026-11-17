package repository

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"testing"

	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/stretchr/testify/require"
)

func TestQuotaFlowV0222UpgradeRehearsal(t *testing.T) {
	dsn := os.Getenv("QUOTAFLOW_REHEARSAL_DSN")
	if dsn == "" {
		t.Skip("isolated restored database required")
	}
	target, err := url.Parse(dsn)
	require.NoError(t, err)
	require.Equal(t, "127.0.0.1", target.Hostname())
	require.Equal(t, "/restore_check", target.Path)
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	defer db.Close()
	ctx := context.Background()
	protected := []string{
		"users", "accounts", "api_keys", "groups", "settings",
		"payment_orders", "payment_provider_instances", "channels",
		"channel_model_pricing", "channel_pricing_intervals",
		"channel_account_stats_model_pricing", "channel_account_stats_pricing_intervals",
	}
	signature := func(table string) string {
		var value string
		query := `SELECT md5(COALESCE(string_agg(row_text, '' ORDER BY row_text), ''))
			FROM (SELECT (to_jsonb(t)-'force_openai_ultrafast')::text AS row_text FROM ` + table + ` t) s`
		require.NoError(t, db.QueryRowContext(ctx, query).Scan(&value), table)
		return value
	}
	before := map[string]string{}
	for _, table := range protected {
		before[table] = signature(table)
	}
	var beforeCount int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT count(*) FROM schema_migrations").Scan(&beforeCount))
	for _, name := range []string{"251_group_force_openai_ultrafast.sql", "251_group_usage_rollup_nonblocking_build.sql"} {
		var exists bool
		require.NoError(t, db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE filename=$1)", name).Scan(&exists))
		require.False(t, exists, "backup already contains %s", name)
	}
	require.NoError(t, applyMigrationsFS(ctx, db, migrations.FS))
	var afterCount int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT count(*) FROM schema_migrations").Scan(&afterCount))
	require.Equal(t, beforeCount+2, afterCount)
	require.NoError(t, applyMigrationsFS(ctx, db, migrations.FS))
	var secondCount int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT count(*) FROM schema_migrations").Scan(&secondCount))
	require.Equal(t, afterCount, secondCount)
	for _, table := range protected {
		require.Equal(t, before[table], signature(table), "protected table changed: %s", table)
	}
	var enabled int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT count(*) FROM groups WHERE force_openai_ultrafast").Scan(&enabled))
	require.Zero(t, enabled, "upgrade must not enable premium service tiers")
	for _, name := range []string{"251_video_frame_assets.sql", "252_dapacai_video_reference_assets.sql", "256_group_pricing_json_storage.sql", "257_video_task_states.sql"} {
		var count int
		require.NoError(t, db.QueryRowContext(ctx, "SELECT count(*) FROM schema_migrations WHERE filename=$1", name).Scan(&count))
		require.Equal(t, 1, count, name)
	}
	t.Log("Applied exactly 2 migrations twice; 12 protected data signatures unchanged; Ultra Fast remains disabled; custom migrations retained")
}
