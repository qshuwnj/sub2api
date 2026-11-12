package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGroupPricingJSONStorageMigration(t *testing.T) {
	content, err := FS.ReadFile("256_group_pricing_json_storage.sql")
	require.NoError(t, err)
	sql := strings.Join(strings.Fields(string(content)), " ")
	require.Contains(t, sql, "ALTER COLUMN model_pricing TYPE JSON USING model_pricing::json")
	require.Contains(t, sql, "SET LOCAL lock_timeout = '5s'")
	require.Contains(t, sql, "SET LOCAL statement_timeout = '60s'")
	require.NotContains(t, sql, "DROP COLUMN")
	require.NotContains(t, sql, "UPDATE groups")
}
