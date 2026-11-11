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

func TestQuotaFlowV0221UpgradeRehearsal(t *testing.T) {
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
		// The only intended changes to pricing rows are nullable new columns.
		query := `SELECT md5(COALESCE(string_agg(row_text, '' ORDER BY row_text), ''))
			FROM (SELECT (to_jsonb(t)-'cache_write_12h_price')::text AS row_text FROM ` + table + ` t) s`
		require.NoError(t, db.QueryRowContext(ctx, query).Scan(&value), table)
		return value
	}
	before := map[string]string{}
	for _, table := range protected {
		before[table] = signature(table)
	}
	var beforeCount int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT count(*) FROM schema_migrations").Scan(&beforeCount))
	for _, name := range []string{
		"249_add_usage_logs_api_key_cost_index_notx.sql",
		"250_openai_cache_write_ttl.sql",
	} {
		var exists bool
		require.NoError(t, db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE filename=$1)", name).Scan(&exists))
		require.False(t, exists, "fresh baseline already contains %s", name)
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
	for _, name := range []string{
		"249_mcp_oauth.sql", "249_video_attempts_r2v.sql",
		"250_video_reference_assets.sql", "252_dapacai_video_reference_assets.sql",
		"249_add_usage_logs_api_key_cost_index_notx.sql", "250_openai_cache_write_ttl.sql",
	} {
		var count int
		require.NoError(t, db.QueryRowContext(ctx, "SELECT count(*) FROM schema_migrations WHERE filename=$1", name).Scan(&count))
		require.Equal(t, 1, count, name)
	}
	var valid bool
	require.NoError(t, db.QueryRowContext(ctx, `SELECT indisvalid FROM pg_index
		WHERE indexrelid='idx_usage_logs_api_key_created_cost'::regclass`).Scan(&valid))
	require.True(t, valid)
	var invalidRows int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM usage_logs
		WHERE cache_creation_30m_tokens <> 0 OR cache_creation_12h_tokens <> 0`).Scan(&invalidRows))
	require.Zero(t, invalidRows)
	t.Log("Applied exactly 2 migrations twice; 12 protected table signatures unchanged; custom migrations retained; concurrent index valid")
}
