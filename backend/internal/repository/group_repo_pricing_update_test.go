package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	_ "github.com/Wei-Shaw/sub2api/ent/runtime"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

type groupPricingQueryMatcher struct{}

func (groupPricingQueryMatcher) Match(expected, actual string) error {
	switch expected {
	case "metadata update", "full update":
		if !strings.HasPrefix(actual, `UPDATE "groups" SET `) {
			return fmt.Errorf("expected group update, got %s", actual)
		}
		hasPricing := strings.Contains(actual, `"model_pricing"`)
		if hasPricing != (expected == "full update") {
			return fmt.Errorf("unexpected model_pricing assignment: %s", actual)
		}
		if !strings.Contains(actual, `"updated_at"`) {
			return errors.New("all pricing writers must advance updated_at")
		}
		return nil
	case "timestamp read":
		selectClause, _, ok := strings.Cut(actual, " FROM ")
		if !ok || selectClause != `SELECT "id", "updated_at"` {
			return fmt.Errorf("unexpected update projection: %s", actual)
		}
		return nil
	default:
		return sqlmock.QueryMatcherRegexp.Match(expected, actual)
	}
}

func newGroupPricingTestRepo(t *testing.T) (*groupRepository, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(groupPricingQueryMatcher{}))
	require.NoError(t, err)
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	t.Cleanup(func() { _ = client.Close() })
	return newGroupRepositoryWithSQL(client, db), mock
}

func groupPricingTestCard(model string, price float64) service.ChannelModelPricing {
	return service.ChannelModelPricing{
		Platform: service.PlatformOpenAI, Models: []string{model},
		BillingMode: service.BillingModePerRequest, PerRequestPrice: &price,
	}
}

func groupPricingTestGroup() *service.Group {
	return &service.Group{
		ID: 24, Name: "pricing-test", Platform: service.PlatformOpenAI,
		RateMultiplier: 1, Status: service.StatusActive,
		SubscriptionType: service.SubscriptionTypeStandard,
		UpdatedAt:        time.Date(2026, 11, 12, 10, 0, 0, 123000, time.UTC),
		ModelPricing: []service.ChannelModelPricing{
			groupPricingTestCard("unchanged-model", 1),
			groupPricingTestCard("changed-model", 2),
		},
	}
}

func expectGroupMetadataUpdate(mock sqlmock.Sqlmock, full bool, updated time.Time) {
	label := "metadata update"
	if full {
		label = "full update"
	}
	mock.ExpectExec(label).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("timestamp read").WithArgs(int64(24)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "updated_at"}).AddRow(int64(24), updated))
}

func expectGroupPricingOutbox(mock sqlmock.Sqlmock) {
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO scheduler_outbox")).
		WithArgs(service.SchedulerOutboxEventGroupChanged, nil, int64(24), nil, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
}

func TestSingleChangedGroupPrice(t *testing.T) {
	a := groupPricingTestCard("a", 1)
	b := groupPricingTestCard("b", 2)
	c := groupPricingTestCard("c", 3)
	tests := []struct {
		name      string
		previous  []service.ChannelModelPricing
		next      []service.ChannelModelPricing
		index     int
		unchanged bool
	}{
		{"nil", nil, nil, -1, true},
		{"empty", []service.ChannelModelPricing{}, []service.ChannelModelPricing{}, -1, true},
		{"nil to empty", nil, []service.ChannelModelPricing{}, -1, false},
		{"empty to nil", []service.ChannelModelPricing{}, nil, -1, false},
		{"cloned equal", []service.ChannelModelPricing{a, b}, []service.ChannelModelPricing{a.Clone(), b.Clone()}, -1, true},
		{"first changed", []service.ChannelModelPricing{a, b}, []service.ChannelModelPricing{c, b}, 0, false},
		{"last changed", []service.ChannelModelPricing{a, b}, []service.ChannelModelPricing{a, c}, 1, false},
		{"multiple changed", []service.ChannelModelPricing{a, b}, []service.ChannelModelPricing{b, c}, -1, false},
		{"reordered", []service.ChannelModelPricing{a, b}, []service.ChannelModelPricing{b, a}, -1, false},
		{"added", []service.ChannelModelPricing{a}, []service.ChannelModelPricing{a, b}, -1, false},
		{"removed", []service.ChannelModelPricing{a, b}, []service.ChannelModelPricing{a}, -1, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			index, unchanged := singleChangedGroupPrice(tc.previous, tc.next)
			require.Equal(t, tc.index, index)
			require.Equal(t, tc.unchanged, unchanged)
		})
	}
}

func TestUpdateGroupUnchangedPricingSkipsJSONBWrite(t *testing.T) {
	repo, mock := newGroupPricingTestRepo(t)
	g := groupPricingTestGroup()
	previous := []service.ChannelModelPricing{g.ModelPricing[0].Clone(), g.ModelPricing[1].Clone()}
	updated := g.UpdatedAt.Add(time.Second)
	mock.ExpectBegin()
	expectGroupMetadataUpdate(mock, false, updated)
	mock.ExpectCommit()
	expectGroupPricingOutbox(mock)

	require.NoError(t, repo.UpdateWithModelPricingSnapshot(context.Background(), g, previous))
	require.Equal(t, updated, g.UpdatedAt)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUpdateGroupSinglePricingCardIsAtomicAndNarrow(t *testing.T) {
	repo, mock := newGroupPricingTestRepo(t)
	g := groupPricingTestGroup()
	// A large unrelated card must never be sent as a SQL argument.
	g.ModelPricing[0].Models = []string{strings.Repeat("unchanged-large-tariff", 100000)}
	previous := append([]service.ChannelModelPricing(nil), g.ModelPricing...)
	g.ModelPricing[1] = groupPricingTestCard("changed-model", 3)
	card, err := json.Marshal(g.ModelPricing[1])
	require.NoError(t, err)
	require.Less(t, len(card), 1024)
	updated := g.UpdatedAt.Add(time.Second)
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(replaceGroupPricingCardSQL)).
		WithArgs(`{"1"}`, string(card), g.ID, g.UpdatedAt, 2).
		WillReturnResult(sqlmock.NewResult(0, 1))
	expectGroupMetadataUpdate(mock, false, updated)
	expectGroupPricingOutbox(mock)
	mock.ExpectCommit()

	require.NoError(t, repo.UpdateWithModelPricingSnapshot(context.Background(), g, previous))
	require.Equal(t, updated, g.UpdatedAt)
	require.NoError(t, mock.ExpectationsWereMet())
	require.Contains(t, replaceGroupPricingCardSQL, "deleted_at IS NULL")
	require.Contains(t, replaceGroupPricingCardSQL, "updated_at = $4")
	require.Contains(t, replaceGroupPricingCardSQL, "jsonb_typeof(model_pricing) = 'array'")
	require.Contains(t, replaceGroupPricingCardSQL, "jsonb_array_length(model_pricing) = $5")
}

func TestUpdateGroupSinglePricingCardFailureRollsBack(t *testing.T) {
	for _, stage := range []string{"begin", "write", "affected", "conflict", "metadata", "read", "outbox", "commit"} {
		t.Run(stage, func(t *testing.T) {
			repo, mock := newGroupPricingTestRepo(t)
			g := groupPricingTestGroup()
			previous := append([]service.ChannelModelPricing(nil), g.ModelPricing...)
			g.ModelPricing[1] = groupPricingTestCard("changed-model", 3)
			before := g.UpdatedAt
			boom := errors.New("injected " + stage + " failure")
			begin := mock.ExpectBegin()
			if stage == "begin" {
				begin.WillReturnError(boom)
			} else {
				write := mock.ExpectExec(regexp.QuoteMeta(replaceGroupPricingCardSQL))
				switch stage {
				case "write":
					write.WillReturnError(boom)
				case "affected":
					write.WillReturnResult(sqlmock.NewErrorResult(boom))
				case "conflict":
					write.WillReturnResult(sqlmock.NewResult(0, 0))
				default:
					write.WillReturnResult(sqlmock.NewResult(0, 1))
					if stage == "metadata" {
						mock.ExpectExec("metadata update").WillReturnError(boom)
					} else if stage == "read" {
						mock.ExpectExec("metadata update").WillReturnResult(sqlmock.NewResult(0, 1))
						mock.ExpectQuery("timestamp read").WillReturnError(boom)
					} else {
						expectGroupMetadataUpdate(mock, false, before.Add(time.Second))
						if stage == "outbox" {
							mock.ExpectExec(regexp.QuoteMeta("INSERT INTO scheduler_outbox")).WillReturnError(boom)
						} else {
							expectGroupPricingOutbox(mock)
						}
					}
				}
				if stage == "commit" {
					mock.ExpectCommit().WillReturnError(boom)
				} else {
					mock.ExpectRollback()
				}
			}
			err := repo.UpdateWithModelPricingSnapshot(context.Background(), g, previous)
			if stage == "conflict" {
				require.ErrorIs(t, err, service.ErrGroupUpdateConflict)
			} else {
				require.ErrorIs(t, err, boom)
			}
			require.Equal(t, before, g.UpdatedAt, "uncommitted timestamp must not escape")
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestUpdateGroupPricingStructuralChangesUseFullWrite(t *testing.T) {
	for _, change := range []string{"multiple", "added", "removed", "nil", "nil to empty", "zero timestamp"} {
		t.Run(change, func(t *testing.T) {
			repo, mock := newGroupPricingTestRepo(t)
			g := groupPricingTestGroup()
			previous := append([]service.ChannelModelPricing(nil), g.ModelPricing...)
			switch change {
			case "multiple":
				g.ModelPricing = []service.ChannelModelPricing{groupPricingTestCard("a", 4), groupPricingTestCard("b", 5)}
			case "added":
				g.ModelPricing = append(g.ModelPricing, groupPricingTestCard("c", 6))
			case "removed":
				g.ModelPricing = g.ModelPricing[:1]
			case "nil":
				g.ModelPricing = nil
			case "nil to empty":
				previous = nil
				g.ModelPricing = []service.ChannelModelPricing{}
			case "zero timestamp":
				g.UpdatedAt = time.Time{}
				g.ModelPricing[1] = groupPricingTestCard("changed-model", 4)
			}
			updated := time.Now().UTC()
			mock.ExpectBegin()
			expectGroupMetadataUpdate(mock, true, updated)
			mock.ExpectCommit()
			expectGroupPricingOutbox(mock)
			require.NoError(t, repo.UpdateWithModelPricingSnapshot(context.Background(), g, previous))
			require.Equal(t, updated, g.UpdatedAt)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestUpdateGroupPricingRejectsInvalidCardBeforeTransaction(t *testing.T) {
	repo, mock := newGroupPricingTestRepo(t)
	g := groupPricingTestGroup()
	previous := append([]service.ChannelModelPricing(nil), g.ModelPricing...)
	g.ModelPricing[1] = groupPricingTestCard("invalid-model", math.NaN())
	err := repo.UpdateWithModelPricingSnapshot(context.Background(), g, previous)
	require.ErrorContains(t, err, "marshal group model pricing card")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUpdateGroupPricingInsideCallerTransactionUsesExistingWriter(t *testing.T) {
	repo, mock := newGroupPricingTestRepo(t)
	mock.ExpectBegin()
	tx, err := repo.client.Tx(context.Background())
	require.NoError(t, err)
	scoped := newGroupRepositoryWithSQL(tx.Client(), tx)
	g := groupPricingTestGroup()
	previous := append([]service.ChannelModelPricing(nil), g.ModelPricing...)
	g.ModelPricing[1] = groupPricingTestCard("changed-model", 4)
	expectGroupMetadataUpdate(mock, true, g.UpdatedAt.Add(time.Second))
	expectGroupPricingOutbox(mock)
	require.NoError(t, scoped.UpdateWithModelPricingSnapshot(context.Background(), g, previous))
	mock.ExpectRollback()
	require.NoError(t, tx.Rollback())
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSingleChangedGroupPriceRetainsWireDecimals(t *testing.T) {
	var previousCard, nextCard service.ChannelModelPricing
	require.NoError(t, json.Unmarshal([]byte(`{"platform":"openai","models":["video"],"billing_mode":"per_request","intervals":[{"tier_label":"video:test","per_request_price":99999999999.12345671}]}`), &previousCard))
	require.NoError(t, json.Unmarshal([]byte(`{"platform":"openai","models":["video"],"billing_mode":"per_request","intervals":[{"tier_label":"video:test","per_request_price":99999999999.12345672}]}`), &nextCard))
	// Float consumers see the same number; the stored wire decimal still differs.
	require.Equal(t, *previousCard.Intervals[0].PerRequestPrice, *nextCard.Intervals[0].PerRequestPrice)
	index, unchanged := singleChangedGroupPrice([]service.ChannelModelPricing{previousCard}, []service.ChannelModelPricing{nextCard})
	require.Equal(t, 0, index)
	require.False(t, unchanged)
	encoded, err := json.Marshal(nextCard)
	require.NoError(t, err)
	require.Contains(t, string(encoded), "99999999999.12345672")
}

func TestSingleChangedGroupPriceDetectsReplacedDecimalSource(t *testing.T) {
	var original service.ChannelModelPricing
	require.NoError(t, json.Unmarshal([]byte(`{"platform":"openai","models":["video"],"billing_mode":"per_request","intervals":[{"tier_label":"video:test","per_request_price":99999999999.12345671}]}`), &original))
	next := original.Clone()
	replacement := *next.Intervals[0].PerRequestPrice
	next.Intervals[0].PerRequestPrice = &replacement
	oldJSON, err := json.Marshal(original)
	require.NoError(t, err)
	newJSON, err := json.Marshal(next)
	require.NoError(t, err)
	require.NotEqual(t, string(oldJSON), string(newJSON), "pointer replacement changes the effective wire decimal")
	index, unchanged := singleChangedGroupPrice([]service.ChannelModelPricing{original}, []service.ChannelModelPricing{next})
	require.Equal(t, 0, index)
	require.False(t, unchanged)
}

func BenchmarkGroupPricingWritePayload(b *testing.B) {
	previous := make([]service.ChannelModelPricing, 139)
	for i := range previous {
		card := groupPricingTestCard(fmt.Sprintf("synthetic-video-%d", i), 1)
		card.Intervals = make([]service.PricingInterval, 128)
		for j := range card.Intervals {
			price := float64(j+1) / 100
			card.Intervals[j] = service.PricingInterval{
				TierLabel:       fmt.Sprintf("resolution:1080p;duration:%d;aspect:16:9;synthetic:%s", j, strings.Repeat("x", 64)),
				PerRequestPrice: &price,
			}
		}
		previous[i] = card
	}
	// An HTTP request and a database read produce independent slices/pointers.
	// Do not benchmark the easier shared-memory shortcut.
	wire, err := json.Marshal(previous)
	if err != nil {
		b.Fatal(err)
	}
	var next []service.ChannelModelPricing
	if err := json.Unmarshal(wire, &previous); err != nil {
		b.Fatal(err)
	}
	if err := json.Unmarshal(wire, &next); err != nil {
		b.Fatal(err)
	}
	next[70].Intervals[0].PerRequestPrice = new(float64)
	b.Run("full_array", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			payload, err := json.Marshal(next)
			if err != nil {
				b.Fatal(err)
			}
			b.ReportMetric(float64(len(payload)), "payload-B/op")
		}
	})
	b.Run("detect_and_single_card", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			index, unchanged := singleChangedGroupPrice(previous, next)
			if index != 70 || unchanged {
				b.Fatal("changed-card detection failed")
			}
			payload, err := json.Marshal(next[index])
			if err != nil {
				b.Fatal(err)
			}
			b.ReportMetric(float64(len(payload)), "payload-B/op")
		}
	})
}
