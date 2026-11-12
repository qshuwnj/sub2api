//go:build integration

package repository

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
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
