package repository

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/stretchr/testify/require"
)

func TestQuotaFlowV0224UpgradeRehearsal(t *testing.T) {
	dsn := os.Getenv("QUOTAFLOW_REHEARSAL_DSN")
	if dsn == "" {
		t.Skip("isolated restored database required")
	}
	target, err := url.Parse(dsn)
	require.NoError(t, err)
	require.Equal(t, "127.0.0.1", target.Hostname())
	require.Equal(t, "55438", target.Port())
	require.Equal(t, "/restore_check", target.Path)
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	protected := []string{
		"users", "accounts", "api_keys", "groups", "settings",
		"payment_orders", "payment_provider_instances", "channels",
		"channel_model_pricing", "channel_pricing_intervals",
		"channel_account_stats_model_pricing", "channel_account_stats_pricing_intervals",
		"batch_image_events", "batch_image_items", "batch_image_jobs",
		"billing_usage_entries", "studio_video_intakes", "usage_billing_dedup",
		"usage_billing_dedup_archive", "video_attempts", "video_reference_assets",
		"video_results", "video_task_states", "usage_logs", "schema_migrations",
	}
	signature := func(table string) string {
		var value string
		query := `SELECT md5(COALESCE(string_agg(row_text, '' ORDER BY row_text), ''))
			FROM (SELECT to_jsonb(t)::text AS row_text FROM ` + table + ` t) s`
		require.NoError(t, db.QueryRowContext(ctx, query).Scan(&value), table)
		return value
	}
	before := map[string]string{}
	for _, table := range protected {
		before[table] = signature(table)
	}
	for _, name := range []string{
		"252_channel_pricing_ultrafast_multiplier.sql",
		"251_group_force_openai_ultrafast.sql", "251_group_usage_rollup_nonblocking_build.sql",
		"251_video_frame_assets.sql", "252_dapacai_video_reference_assets.sql",
		"256_group_pricing_json_storage.sql", "257_video_task_states.sql",
	} {
		var count int
		require.NoError(t, db.QueryRowContext(ctx, "SELECT count(*) FROM schema_migrations WHERE filename=$1", name).Scan(&count))
		require.Equal(t, 1, count, "expected current production baseline: %s", name)
	}
	var beforeCount int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT count(*) FROM schema_migrations").Scan(&beforeCount))
	for pass := 1; pass <= 2; pass++ {
		require.NoError(t, applyMigrationsFS(ctx, db, migrations.FS))
		var afterCount int
		require.NoError(t, db.QueryRowContext(ctx, "SELECT count(*) FROM schema_migrations").Scan(&afterCount))
		require.Equal(t, beforeCount, afterCount, "v0.2.24 must not introduce migrations; pass %d", pass)
		for _, table := range protected {
			require.Equal(t, before[table], signature(table), "protected table changed on pass %d: %s", pass, table)
		}
	}
	t.Logf("Applied current migration set twice; zero new migrations; all %d protected table signatures unchanged", len(protected))
}
