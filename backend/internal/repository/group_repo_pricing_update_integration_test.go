//go:build integration

package repository

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	dbmigrations "github.com/Wei-Shaw/sub2api/migrations"
	"github.com/stretchr/testify/require"
)

func createGroupPricingIntegrationFixture(t *testing.T) (*groupRepository, *service.Group) {
	t.Helper()
	ctx := context.Background()
	repo := newGroupRepositoryWithSQL(testEntClient(t), integrationDB)
	g := groupPricingTestGroup()
	g.ID = 0
	g.Name = t.Name()
	require.NoError(t, repo.Create(ctx, g))
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(ctx, "DELETE FROM scheduler_outbox WHERE group_id=$1", g.ID)
		_, _ = integrationDB.ExecContext(ctx, "DELETE FROM groups WHERE id=$1", g.ID)
	})
	loaded, err := repo.GetByID(ctx, g.ID)
	require.NoError(t, err)
	return repo, loaded
}

func TestGroupPricingPostgresSingleCardPreservesOtherCards(t *testing.T) {
	repo, g := createGroupPricingIntegrationFixture(t)
	ctx := context.Background()
	previous := g.ModelPricing
	g.ModelPricing = append([]service.ChannelModelPricing(nil), previous...)
	g.ModelPricing[1] = groupPricingTestCard("changed-model", 4)
	g.Description = "saved with pricing"
	before := g.UpdatedAt
	require.NoError(t, repo.UpdateWithModelPricingSnapshot(ctx, g, previous))
	require.True(t, g.UpdatedAt.After(before))
	got, err := repo.GetByID(ctx, g.ID)
	require.NoError(t, err)
	wantJSON, err := json.Marshal(g.ModelPricing)
	require.NoError(t, err)
	gotJSON, err := json.Marshal(got.ModelPricing)
	require.NoError(t, err)
	require.JSONEq(t, string(wantJSON), string(gotJSON))
	require.Equal(t, g.Description, got.Description)

	previous = got.ModelPricing
	got.Description = "metadata only"
	require.NoError(t, repo.UpdateWithModelPricingSnapshot(ctx, got, previous))
	after, err := repo.GetByID(ctx, g.ID)
	require.NoError(t, err)
	require.Equal(t, previous, after.ModelPricing)
	require.Equal(t, "metadata only", after.Description)
	var columnType string
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT pg_typeof(model_pricing)::text FROM groups WHERE id=$1`, g.ID).Scan(&columnType))
	require.Equal(t, "json", columnType)
}

func TestGroupPricingPostgresConcurrentSnapshotRejectsStaleWriter(t *testing.T) {
	repo, first := createGroupPricingIntegrationFixture(t)
	ctx := context.Background()
	second, err := repo.GetByID(ctx, first.ID)
	require.NoError(t, err)
	snapshots := []*service.Group{first, second}
	results := make(chan error, 2)
	start := make(chan struct{})
	for i, g := range snapshots {
		previous := g.ModelPricing
		g.ModelPricing = append([]service.ChannelModelPricing(nil), previous...)
		g.ModelPricing[i] = groupPricingTestCard("changed-model", float64(i+5))
		go func() {
			<-start
			results <- repo.UpdateWithModelPricingSnapshot(ctx, g, previous)
		}()
	}
	close(start)
	success, conflict := 0, 0
	for range 2 {
		err := <-results
		switch {
		case err == nil:
			success++
		case errors.Is(err, service.ErrGroupUpdateConflict):
			conflict++
		default:
			t.Fatalf("unexpected concurrent save error: %v", err)
		}
	}
	require.Equal(t, 1, success)
	require.Equal(t, 1, conflict)
}

func TestGroupPricingPostgresDeletedGroupRejectsCardWrite(t *testing.T) {
	repo, g := createGroupPricingIntegrationFixture(t)
	ctx := context.Background()
	previous := g.ModelPricing
	g.ModelPricing = append([]service.ChannelModelPricing(nil), previous...)
	g.ModelPricing[1] = groupPricingTestCard("changed-model", 5)
	_, err := integrationDB.ExecContext(ctx, "UPDATE groups SET deleted_at=NOW() WHERE id=$1", g.ID)
	require.NoError(t, err)
	require.ErrorIs(t, repo.UpdateWithModelPricingSnapshot(ctx, g, previous), service.ErrGroupUpdateConflict)
}

func TestGroupPricingPostgresStructuralChanges(t *testing.T) {
	repo, g := createGroupPricingIntegrationFixture(t)
	ctx := context.Background()
	for _, next := range [][]service.ChannelModelPricing{
		{groupPricingTestCard("new-model", 0.12345678)},
		{groupPricingTestCard("new-model", 0.12345678), groupPricingTestCard("added-model", 4.56789012)},
		{},
		nil,
	} {
		previous := g.ModelPricing
		g.ModelPricing = next
		require.NoError(t, repo.UpdateWithModelPricingSnapshot(ctx, g, previous))
		got, err := repo.GetByID(ctx, g.ID)
		require.NoError(t, err)
		require.Equal(t, next, got.ModelPricing)
		g = got
	}
}

func TestGroupPricingPostgresLargeDocumentRoundTrip(t *testing.T) {
	repo, g := createGroupPricingIntegrationFixture(t)
	ctx := context.Background()
	var next []service.ChannelModelPricing
	if fixture := os.Getenv("SUB2API_TEST_PRICING_DOCUMENT"); fixture != "" {
		// An explicit local fixture lets release checks use the failed save's
		// actual document without committing customer configuration to Git.
		wire, err := os.ReadFile(fixture)
		require.NoError(t, err)
		var request struct {
			ModelPricing []service.ChannelModelPricing `json:"model_pricing"`
		}
		require.NoError(t, json.Unmarshal(wire, &request))
		next = request.ModelPricing
	} else {
		for i := range 139 {
			card := groupPricingTestCard(fmt.Sprintf("synthetic-video-%d", i), 0.12345678)
			for j := range 128 {
				price := float64(j+1) / 1000000
				card.Intervals = append(card.Intervals, service.PricingInterval{
					TierLabel:       fmt.Sprintf("video:synthetic-%d-%d:%s", i, j, strings.Repeat("x", 384)),
					PerRequestPrice: &price,
				})
			}
			next = append(next, card)
		}
	}
	wire, err := json.Marshal(next)
	require.NoError(t, err)
	require.Greater(t, len(wire), 8_000_000)
	previous := g.ModelPricing
	g.ModelPricing = next
	require.NoError(t, repo.UpdateWithModelPricingSnapshot(ctx, g, previous))
	got, err := repo.GetByID(ctx, g.ID)
	require.NoError(t, err)
	actual, err := json.Marshal(got.ModelPricing)
	require.NoError(t, err)
	require.True(t, bytes.Equal(wire, actual), "large document must round-trip without price or card changes")
	require.Len(t, got.ModelPricing, len(next))

	// A small subsequent edit must not alter any unrelated price decimals.
	var cloned []service.ChannelModelPricing
	require.NoError(t, json.Unmarshal(actual, &cloned))
	previous = got.ModelPricing
	got.ModelPricing = cloned
	price := 0.98765432
	got.ModelPricing[0].PerRequestPrice = &price
	require.NoError(t, repo.UpdateWithModelPricingSnapshot(ctx, got, previous))
	expected, err := json.Marshal(got.ModelPricing)
	require.NoError(t, err)
	reread, err := repo.GetByID(ctx, g.ID)
	require.NoError(t, err)
	actual, err = json.Marshal(reread.ModelPricing)
	require.NoError(t, err)
	require.True(t, bytes.Equal(expected, actual), "single-card update must retain the remaining large tariff exactly")
	t.Logf("saved and reread %d cards / %d JSON bytes twice", len(next), len(wire))
}

func TestGroupPricingPostgresExactDecimalRoundTrip(t *testing.T) {
	repo, g := createGroupPricingIntegrationFixture(t)
	ctx := context.Background()
	var card service.ChannelModelPricing
	require.NoError(t, json.Unmarshal([]byte(`{"platform":"openai","models":["video"],"billing_mode":"per_request","intervals":[{"tier_label":"video:test","per_request_price":99999999999.12345672}]}`), &card))
	previous := g.ModelPricing
	g.ModelPricing = []service.ChannelModelPricing{card}
	require.NoError(t, repo.UpdateWithModelPricingSnapshot(ctx, g, previous))
	got, err := repo.GetByID(ctx, g.ID)
	require.NoError(t, err)
	wire, err := json.Marshal(got.ModelPricing)
	require.NoError(t, err)
	require.Contains(t, string(wire), "99999999999.12345672")
}

func TestGroupPricingPostgresOutboxFailureRollsBackDocumentAndMetadata(t *testing.T) {
	repo, g := createGroupPricingIntegrationFixture(t)
	ctx := context.Background()
	previous := g.ModelPricing
	before, err := json.Marshal(previous)
	require.NoError(t, err)
	beforeTimestamp, beforeDescription := g.UpdatedAt, g.Description
	// NOT VALID allows the create event to remain, while rejecting this fixture's
	// next event. The real database must roll the entire save back.
	_, err = integrationDB.ExecContext(ctx, fmt.Sprintf(
		`ALTER TABLE scheduler_outbox ADD CONSTRAINT pricing_test_reject_outbox CHECK (group_id IS DISTINCT FROM %d) NOT VALID`, g.ID))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := integrationDB.ExecContext(ctx, `ALTER TABLE scheduler_outbox DROP CONSTRAINT IF EXISTS pricing_test_reject_outbox`)
		require.NoError(t, err)
	})
	g.ModelPricing = []service.ChannelModelPricing{groupPricingTestCard("should-not-persist", 9)}
	g.Description = "should not persist"
	err = repo.UpdateWithModelPricingSnapshot(ctx, g, previous)
	require.ErrorContains(t, err, "enqueue group pricing update")
	require.Equal(t, beforeTimestamp, g.UpdatedAt)
	got, err := repo.GetByID(ctx, g.ID)
	require.NoError(t, err)
	after, err := json.Marshal(got.ModelPricing)
	require.NoError(t, err)
	require.Equal(t, before, after)
	require.Equal(t, beforeTimestamp, got.UpdatedAt)
	require.Equal(t, beforeDescription, got.Description)
}

func TestGroupPricingPostgresMigrationPreservesDocumentAndMetadata(t *testing.T) {
	tx := testTx(t)
	ctx := context.Background()
	_, err := tx.ExecContext(ctx, `ALTER TABLE groups ALTER COLUMN model_pricing TYPE JSONB USING model_pricing::jsonb`)
	require.NoError(t, err)
	for i, document := range []any{
		nil,
		"null",
		"[]",
		`[{"models":["video"],"intervals":[{"tier_label":"video:test","per_request_price":99999999999.12345672}],"unknown_future_key":{"preserve":true}}]`,
	} {
		_, err := tx.ExecContext(ctx, `INSERT INTO groups(name, platform, model_pricing) VALUES($1,'openai',$2::jsonb)`,
			fmt.Sprintf("migration-256-pricing-%d", i), document)
		require.NoError(t, err)
	}
	var before, after string
	const snapshot = `SELECT jsonb_agg(jsonb_build_array(id,model_pricing::text,updated_at) ORDER BY id)::text FROM groups`
	require.NoError(t, tx.QueryRowContext(ctx, snapshot).Scan(&before))
	migration, err := dbmigrations.FS.ReadFile("256_group_pricing_json_storage.sql")
	require.NoError(t, err)
	for range 2 {
		_, err = tx.ExecContext(ctx, string(migration))
		require.NoError(t, err)
		require.NoError(t, tx.QueryRowContext(ctx, snapshot).Scan(&after))
		require.Equal(t, before, after, "migration must not modify pricing or group timestamps")
		var columnType string
		require.NoError(t, tx.QueryRowContext(ctx, `SELECT udt_name FROM information_schema.columns WHERE table_schema='public' AND table_name='groups' AND column_name='model_pricing'`).Scan(&columnType))
		require.Equal(t, "json", columnType)
	}
	// Rehearse the required schema rollback locally, not just a binary rollback.
	_, err = tx.ExecContext(ctx, `ALTER TABLE groups ALTER COLUMN model_pricing TYPE JSONB USING model_pricing::jsonb`)
	require.NoError(t, err)
	require.NoError(t, tx.QueryRowContext(ctx, snapshot).Scan(&after))
	require.Equal(t, before, after)
}
