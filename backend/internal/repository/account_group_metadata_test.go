package repository

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/group"
	"github.com/stretchr/testify/require"
)

type accountGroupMetadataMatcher struct{}

func (accountGroupMetadataMatcher) Match(_, actual string) error {
	projection, _, ok := strings.Cut(actual, " FROM ")
	if !ok || strings.Contains(projection, `"model_pricing"`) {
		return fmt.Errorf("account edges must not read billing tariffs: %s", actual)
	}
	for _, field := range group.Columns {
		if field != group.FieldModelPricing &&
			!strings.Contains(projection, `"groups"."`+field+`"`) {
			return fmt.Errorf("account group metadata omitted %q", field)
		}
	}
	return nil
}

func TestAccountGroupMetadataExcludesTariffs(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(accountGroupMetadataMatcher{}))
	require.NoError(t, err)
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	t.Cleanup(func() { _ = client.Close() })
	repo := newAccountRepositoryWithSQL(client, db, nil)
	mock.ExpectQuery("all metadata without tariffs").
		WithArgs(int64(24), int64(25)).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "name", "platform", "status", "rate_multiplier",
			"require_privacy_set", "require_oauth_only", "profit_control_enabled",
			"profit_min_margin", "profit_safety_buffer", "rpm_limit",
		}).AddRow(24, "video metadata", "openai", "active", 1.2,
			true, true, true, 0.2, 0.1, 7).
			AddRow(25, "second group", "composite", "active", 2,
				false, false, false, 0, 0, 0))

	got, err := repo.loadGroups(context.Background(), []int64{24, 25, 24, 0})
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, "video metadata", got[24].Name)
	require.Equal(t, 1.2, got[24].RateMultiplier)
	require.True(t, got[24].RequirePrivacySet)
	require.True(t, got[24].RequireOAuthOnly)
	require.True(t, got[24].ProfitControlEnabled)
	require.Equal(t, 0.2, got[24].ProfitMinMargin)
	require.Equal(t, 0.1, got[24].ProfitSafetyBuffer)
	require.Equal(t, 7, got[24].RPMLimit)
	require.Empty(t, got[24].ModelPricing)
	require.Equal(t, "composite", got[25].Platform)
	require.NoError(t, mock.ExpectationsWereMet())
}
