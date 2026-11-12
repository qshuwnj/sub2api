//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

type groupPricingUpdateRepoStub struct {
	groupPlatformRepoStub
	snapshotCalls int
	previous      []ChannelModelPricing
	saveErr       error
}

func (r *groupPricingUpdateRepoStub) UpdateWithModelPricingSnapshot(_ context.Context, group *Group, previous []ChannelModelPricing) error {
	r.snapshotCalls++
	r.previous = previous
	if r.saveErr != nil {
		return r.saveErr
	}
	r.updated = group
	return nil
}

func TestAdminGroupPricingUpdateUsesStoredSnapshot(t *testing.T) {
	originalPrice, newPrice := 1.0, 2.0
	original := []ChannelModelPricing{{
		Platform: PlatformOpenAI, Models: []string{"test-model"},
		BillingMode: BillingModePerRequest, PerRequestPrice: &originalPrice,
	}}
	next := []ChannelModelPricing{original[0].Clone()}
	next[0].PerRequestPrice = &newPrice
	repo := &groupPricingUpdateRepoStub{groupPlatformRepoStub: groupPlatformRepoStub{
		group: &Group{ID: 24, Name: "g", Platform: PlatformOpenAI, ModelPricing: original},
	}}
	svc := &adminServiceImpl{groupRepo: repo}
	got, err := svc.UpdateGroup(context.Background(), 24, &UpdateGroupInput{ModelPricing: &next})
	require.NoError(t, err)
	require.Equal(t, 1, repo.snapshotCalls)
	require.Equal(t, original, repo.previous)
	require.Equal(t, originalPrice, *repo.previous[0].PerRequestPrice)
	require.Equal(t, newPrice, *got.ModelPricing[0].PerRequestPrice)
	require.Equal(t, originalPrice, *repo.group.ModelPricing[0].PerRequestPrice, "normalization must not mutate the saved snapshot")
}

func TestAdminGroupPricingOmittedPreservesSnapshot(t *testing.T) {
	original := []ChannelModelPricing{{Models: []string{"test-model"}}}
	repo := &groupPricingUpdateRepoStub{groupPlatformRepoStub: groupPlatformRepoStub{
		group: &Group{ID: 24, Name: "g", Platform: PlatformOpenAI, ModelPricing: original},
	}}
	svc := &adminServiceImpl{groupRepo: repo}
	description := "updated"
	got, err := svc.UpdateGroup(context.Background(), 24, &UpdateGroupInput{Description: &description})
	require.NoError(t, err)
	require.Equal(t, 1, repo.snapshotCalls)
	require.Equal(t, original, repo.previous)
	require.Equal(t, original, got.ModelPricing)
	require.Equal(t, "updated", got.Description)
}

func TestAdminGroupPricingConflictDoesNotInvalidateCache(t *testing.T) {
	repo := &groupPricingUpdateRepoStub{
		groupPlatformRepoStub: groupPlatformRepoStub{group: &Group{ID: 24, Name: "g", Platform: PlatformOpenAI}},
		saveErr:               ErrGroupUpdateConflict,
	}
	cache := &channelCacheInvalidatorSpy{}
	svc := &adminServiceImpl{groupRepo: repo, channelCacheInvalidator: cache}
	got, err := svc.UpdateGroup(context.Background(), 24, &UpdateGroupInput{Platform: PlatformAnthropic})
	require.ErrorIs(t, err, ErrGroupUpdateConflict)
	require.Nil(t, got)
	require.Nil(t, repo.updated)
	require.Zero(t, cache.calls)
}

func TestGroupPricingPersistenceFallsBackForExistingRepositories(t *testing.T) {
	repo := &groupPlatformRepoStub{group: &Group{ID: 24, Name: "g", Platform: PlatformOpenAI}}
	svc := &adminServiceImpl{groupRepo: repo}
	description := "updated"
	got, err := svc.UpdateGroup(context.Background(), 24, &UpdateGroupInput{Description: &description})
	require.NoError(t, err)
	require.Same(t, got, repo.updated)
}

func TestGroupServiceMetadataUpdateUsesStoredPricingSnapshot(t *testing.T) {
	original := []ChannelModelPricing{{Models: []string{"test-model"}}}
	repo := &groupPricingUpdateRepoStub{groupPlatformRepoStub: groupPlatformRepoStub{
		group: &Group{ID: 24, Name: "g", Platform: PlatformOpenAI, ModelPricing: original},
	}}
	svc := &GroupService{groupRepo: repo}
	description := "updated"
	got, err := svc.Update(context.Background(), 24, UpdateGroupRequest{Description: &description})
	require.NoError(t, err)
	require.Equal(t, 1, repo.snapshotCalls)
	require.Equal(t, original, repo.previous)
	require.Equal(t, original, got.ModelPricing)
	require.Equal(t, description, got.Description)
}
