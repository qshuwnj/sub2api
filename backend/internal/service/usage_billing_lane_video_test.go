//go:build unit

package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

type videoLaneTestRepo struct {
	*laneTestRepo
	state        videoStateRepoStub
	receipt      *VideoChargeReceipt
	receiptOwner VideoAttemptOwner
	receiptTask  string
	receiptErr   error
}

func (r *videoLaneTestRepo) CheckVideoTaskStorage(ctx context.Context) error {
	return r.state.CheckVideoTaskStorage(ctx)
}

func (r *videoLaneTestRepo) SaveVideoTaskState(ctx context.Context, state VideoTaskState) error {
	return r.state.SaveVideoTaskState(ctx, state)
}

func (r *videoLaneTestRepo) LoadVideoTaskState(ctx context.Context, task string, user, key int64) (*VideoTaskState, error) {
	return r.state.LoadVideoTaskState(ctx, task, user, key)
}

func (r *videoLaneTestRepo) UpdateVideoTaskBilling(ctx context.Context, task string, user, key int64, pending GrokVideoPendingBilling) error {
	return r.state.UpdateVideoTaskBilling(ctx, task, user, key, pending)
}

func (r *videoLaneTestRepo) FindVideoChargeReceipt(ctx context.Context, owner VideoAttemptOwner, task string) (*VideoChargeReceipt, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.receiptOwner, r.receiptTask = owner, task
	return r.receipt, r.receiptErr
}

func TestUsageBillingLanePreservesVideoChargeReceiptReader(t *testing.T) {
	repo := &videoLaneTestRepo{laneTestRepo: newLaneTestRepo(),
		receipt: &VideoChargeReceipt{UsageLogID: 42, RequestID: "grok-video:dapacai:existing", Amount: "1.5600000000", Currency: "USD"}}
	lanes := NewUsageBillingLaneCombiner(repo)
	defer lanes.Stop()
	svc := &OpenAIGatewayService{usageBillingRepo: ProvideUsageBillingRepository(lanes)}
	owner := VideoAttemptOwner{UserID: 41, APIKeyID: 51}
	receipt, err := svc.LookupVideoChargeReceipt(context.Background(), owner, "dapacai:existing")
	require.NoError(t, err, "the production billing wrapper must expose the committed video receipt")
	require.Equal(t, repo.receipt, receipt)
	require.Equal(t, owner, repo.receiptOwner)
	require.Equal(t, "dapacai:existing", repo.receiptTask)
	require.Empty(t, repo.applyCalls, "recovering a paid video must not charge it again")
	repo.receipt, repo.receiptErr = nil, errors.New("receipt database unavailable")
	receipt, err = svc.LookupVideoChargeReceipt(context.Background(), owner, "dapacai:existing")
	require.ErrorIs(t, err, repo.receiptErr)
	require.Nil(t, receipt, "storage errors must not become free receipts")
}

func TestUsageBillingLanePreservesDurableVideoTaskState(t *testing.T) {
	repo := &videoLaneTestRepo{laneTestRepo: newLaneTestRepo()}
	lanes := NewUsageBillingLaneCombiner(repo)
	defer lanes.Stop()
	cache := &videoLegacyCacheStub{err: errors.New("Redis unavailable")}
	svc := &OpenAIGatewayService{usageBillingRepo: ProvideUsageBillingRepository(lanes), cache: cache}
	_, durable := svc.videoTaskStateRepository()
	require.True(t, durable, "production wiring must not silently fall back to Redis task state")
	ctx := context.Background()
	group := int64(7)
	require.NoError(t, svc.CheckVideoTaskStorage(ctx))
	require.NoError(t, svc.PersistAcceptedVideoTask(ctx, &group, "dapacai:existing", 41, 51, 63,
		GrokVideoPendingBilling{Model: "seedance-2.0", BillingModel: "seedance-2.0", VideoResolution: "native"}))
	account, err := svc.ResolveGrokMediaVideoRequestAccount(ctx, &group, "dapacai:existing", 41, 51)
	require.NoError(t, err)
	require.EqualValues(t, 63, account)
	pending, err := svc.LoadGrokVideoPendingBilling(ctx, "dapacai:existing", 41, 51)
	require.NoError(t, err)
	require.Equal(t, "seedance-2.0", pending.BillingModel)
	admit, err := svc.ClaimGrokVideoBilling(ctx, "dapacai:existing", 41, 51)
	require.NoError(t, err)
	require.True(t, admit)
	pending.Settled = true
	require.NoError(t, svc.StoreGrokVideoPendingBilling(ctx, "dapacai:existing", 41, 51, *pending))
	admit, err = svc.ClaimGrokVideoBilling(ctx, "dapacai:existing", 41, 51)
	require.NoError(t, err)
	require.False(t, admit)
	other, err := svc.LoadGrokVideoPendingBilling(ctx, "dapacai:existing", 42, 51)
	require.NoError(t, err)
	require.Nil(t, other, "owner identity must pass through unchanged")
	require.Zero(t, cache.reads)
	repo.state.err = errors.New("video database unavailable")
	require.ErrorIs(t, svc.CheckVideoTaskStorage(ctx), repo.state.err)
	_, err = svc.LoadGrokVideoPendingBilling(ctx, "dapacai:existing", 41, 51)
	require.ErrorIs(t, err, repo.state.err)
	require.Zero(t, cache.reads, "a database failure must not fall back to Redis")
}

func TestUsageBillingLaneUnsupportedVideoStorageFailsClosed(t *testing.T) {
	for _, lanes := range []*UsageBillingLaneCombiner{nil, NewUsageBillingLaneCombiner(newLaneTestRepo())} {
		if lanes != nil {
			defer lanes.Stop()
		}
		repo, ok := any(lanes).(VideoTaskStateRepository)
		require.True(t, ok)
		ctx := context.Background()
		require.Error(t, repo.CheckVideoTaskStorage(ctx))
		require.Error(t, repo.SaveVideoTaskState(ctx, VideoTaskState{}))
		state, err := repo.LoadVideoTaskState(ctx, "dapacai:existing", 41, 51)
		require.Error(t, err)
		require.Nil(t, state)
		require.Error(t, repo.UpdateVideoTaskBilling(ctx, "dapacai:existing", 41, 51, GrokVideoPendingBilling{}))
		reader, ok := any(lanes).(VideoChargeReceiptReader)
		require.True(t, ok)
		receipt, err := reader.FindVideoChargeReceipt(ctx, VideoAttemptOwner{UserID: 41, APIKeyID: 51}, "dapacai:existing")
		require.Error(t, err)
		require.Nil(t, receipt)
	}
}
