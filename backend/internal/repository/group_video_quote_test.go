package repository

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/stretchr/testify/require"
)

type videoGroupQueryMatcher struct{}

func (videoGroupQueryMatcher) Match(expected, actual string) error {
	if expected == "lean group metadata" {
		projection, _, ok := strings.Cut(actual, " FROM ")
		if !ok || strings.Contains(projection, `"model_pricing"`) {
			return fmt.Errorf("full tariff document must not be selected: %s", actual)
		}
		return nil
	}
	return sqlmock.QueryMatcherRegexp.Match(expected, actual)
}

func TestGroupRepositoryVideoQuoteProjection(t *testing.T) {
	for _, tc := range []struct {
		name string
		data string
		err  error
		race bool
	}{
		{name: "exact card", data: `[{"platform":"openai","models":["exact/model"],"billing_mode":"per_request"}]`},
		{name: "model absent", data: `[]`},
		{name: "concurrent update", race: true},
		{name: "database failure", err: errors.New("database recovering")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(videoGroupQueryMatcher{}))
			require.NoError(t, err)
			client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
			t.Cleanup(func() { _ = client.Close() })
			repo := newGroupRepositoryWithSQL(client, db)
			updated := time.Date(2026, 11, 13, 3, 0, 0, 0, time.UTC)
			mock.ExpectQuery("lean group metadata").
				WillReturnRows(sqlmock.NewRows([]string{"id", "name", "updated_at"}).
					AddRow(24, "protected metadata", updated))
			query := mock.ExpectQuery(regexp.QuoteMeta(videoQuoteModelPricingSQL)).
				WithArgs(int64(24), "exact/model", updated)
			switch {
			case tc.err != nil:
				query.WillReturnError(tc.err)
			case tc.race:
				query.WillReturnRows(sqlmock.NewRows([]string{"pricing"}))
			default:
				query.WillReturnRows(sqlmock.NewRows([]string{"pricing"}).AddRow([]byte(tc.data)))
			}
			got, err := repo.GetByIDForVideoQuote(context.Background(), 24, "exact/model")
			if tc.err != nil || tc.race {
				require.Error(t, err)
				require.Nil(t, got)
			} else {
				require.NoError(t, err)
				require.Equal(t, "protected metadata", got.Name)
				require.Equal(t, updated, got.UpdatedAt)
				if tc.data == "[]" {
					require.Empty(t, got.ModelPricing)
				} else {
					require.Equal(t, []string{"exact/model"}, got.ModelPricing[0].Models)
				}
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestAPIKeyRepositoryVideoQuoteProjectionDoesNotLoadEdges(t *testing.T) {
	repo, client := newAPIKeyRepoSQLite(t)
	ctx := context.Background()
	user := mustCreateAPIKeyRepoUser(t, ctx, client, "lean-video-quote@test.local")
	group, err := client.Group.Create().SetName("large-tariffs-not-loaded").Save(ctx)
	require.NoError(t, err)
	key, err := client.APIKey.Create().SetUserID(user.ID).SetKey("synthetic-lean-video-key").
		SetName("lean quote").SetGroupID(group.ID).SetQuota(120).SetQuotaUsed(23).Save(ctx)
	require.NoError(t, err)
	fresh, err := repo.GetByIDForVideoQuote(ctx, key.ID)
	require.NoError(t, err)
	require.Equal(t, &group.ID, fresh.GroupID)
	require.Equal(t, float64(120), fresh.Quota)
	require.Equal(t, float64(23), fresh.QuotaUsed)
	require.Nil(t, fresh.Group)
	require.Nil(t, fresh.User)
}
