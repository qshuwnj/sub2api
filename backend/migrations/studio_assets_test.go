package migrations

import (
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// These are SQL schema-contract checks, not PostgreSQL execution tests.
// Run the actual migration against disposable PostgreSQL before deployment.
func TestStudioAssetMigrationDefinesPrivateOwnerScopedIndex(t *testing.T) {
	content, err := FS.ReadFile("253_studio_assets.sql")
	require.NoError(t, err)
	sql := strings.Join(strings.Fields(string(content)), " ")
	for _, clause := range []string{
		"CREATE TABLE IF NOT EXISTS studio_assets",
		"user_id BIGINT NOT NULL REFERENCES users(id)",
		"api_key_id BIGINT NOT NULL REFERENCES api_keys(id)",
		"CHECK (id ~ '^asset_[0-9a-f]{32}$')",
		"CHECK (kind IN ('image', 'video'))",
		"CHECK (output_index >= 0)",
		"CHECK (state IN ('pending', 'ready', 'retrying', 'unavailable'))",
		"source_bytes BYTEA",
		"metadata JSONB NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(metadata) = 'object')",
		"CHECK (state <> 'ready' OR (storage_key <> '' AND size_bytes > 0 AND source_url = '' AND source_bytes IS NULL))",
		"CHECK ((lease_token = '') = (lease_expires_at IS NULL))",
		"CREATE UNIQUE INDEX IF NOT EXISTS studio_assets_generation_idx ON studio_assets(user_id,api_key_id,generation_id,output_index)",
		"CREATE INDEX IF NOT EXISTS studio_assets_owner_page_idx ON studio_assets(user_id,api_key_id,generated_at DESC,id DESC) WHERE deleted_at IS NULL",
		"WHERE deleted_at IS NULL AND state IN ('pending', 'retrying')",
		"WHERE deleted_at IS NOT NULL AND storage_key <> ''",
	} {
		require.Contains(t, sql, clause)
	}
}

func TestStudioAssetMigrationNeverMutatesBillingOrRemovesTombstones(t *testing.T) {
	content, err := FS.ReadFile("253_studio_assets.sql")
	require.NoError(t, err)
	// Only the new index table may be the target of a schema statement.
	withoutComments := regexp.MustCompile(`(?m)--[^\n]*`).ReplaceAllString(string(content), "")
	for _, statement := range strings.Split(withoutComments, ";") {
		statement = strings.TrimSpace(statement)
		if statement == "" {
			continue
		}
		require.Regexp(t, `(?is)^CREATE (?:TABLE IF NOT EXISTS studio_assets\s*\(|(?:UNIQUE )?INDEX IF NOT EXISTS studio_assets_[a-z_]+\s+ON studio_assets\s*\()`, statement)
	}
	require.NotContains(t, strings.ToUpper(withoutComments), "ON DELETE CASCADE")
}
