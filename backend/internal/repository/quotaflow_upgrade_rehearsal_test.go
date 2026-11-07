package repository

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"github.com/Wei-Shaw/sub2api/migrations"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func TestQuotaFlowV0220UpgradeRehearsal(t *testing.T) {
	dsn := os.Getenv("QUOTAFLOW_REHEARSAL_DSN")
	if dsn == "" {
		t.Skip("isolated restored database required")
	}
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	defer db.Close()
	ctx := context.Background()
	var database string
	require.NoError(t, db.QueryRowContext(ctx, "SELECT current_database()").Scan(&database))
	require.Equal(t, "restore_check", database)
	protected := []string{"users", "accounts", "api_keys", "groups", "settings", "payment_orders", "payment_provider_instances"}
	signature := func(table string) string {
		var value string
		err := db.QueryRowContext(ctx, `SELECT md5(COALESCE(string_agg(row_text, '' ORDER BY row_text), '')) FROM (SELECT to_jsonb(t)::text AS row_text FROM `+table+` t) s`).Scan(&value)
		require.NoError(t, err, table)
		return value
	}
	before := map[string]string{}
	for _, table := range protected {
		before[table] = signature(table)
	}
	require.NoError(t, applyMigrationsFS(ctx, db, migrations.FS))
	require.NoError(t, applyMigrationsFS(ctx, db, migrations.FS))
	for _, table := range protected {
		require.Equal(t, before[table], signature(table), "protected table changed: %s", table)
	}
	var count int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT count(*) FROM schema_migrations WHERE filename = '249_mcp_oauth.sql'").Scan(&count))
	require.Equal(t, 1, count)
	t.Log("Upgrade applied twice; users, accounts, keys, groups, settings and payment records unchanged")
}
