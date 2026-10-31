package service

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type studioWorkerRepo struct {
	UsageBillingRepository
	StudioAssetRepository
	entered chan struct{}
	exited  chan struct{}
	claims  atomic.Int32
}

func (r *studioWorkerRepo) ClaimStudioAssets(ctx context.Context, _ int) ([]*StudioAsset, error) {
	r.claims.Add(1)
	close(r.entered)
	<-ctx.Done()
	close(r.exited)
	return nil, ctx.Err()
}
func (r *studioWorkerRepo) ClaimDeletedStudioAssets(context.Context, int) ([]*StudioAsset, error) {
	return nil, nil
}
func TestStudioAssetWorkerSerialCancelAndWait(t *testing.T) {
	repo := &studioWorkerRepo{entered: make(chan struct{}), exited: make(chan struct{})}
	gateway := &OpenAIGatewayService{usageBillingRepo: repo}
	gateway.SetStudioAssetStorage(nil)
	t.Cleanup(gateway.StopStudioAssetWorker)
	gateway.StartStudioAssetWorker()
	gateway.StartStudioAssetWorker()
	select {
	case <-repo.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not start")
	}
	done := make(chan struct{})
	go func() { gateway.StopStudioAssetWorker(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("worker shutdown did not cancel/wait")
	}
	select {
	case <-repo.exited:
	default:
		t.Fatal("stop returned before active iteration exited")
	}
	require.EqualValues(t, 1, repo.claims.Load())
	gateway.StopStudioAssetWorker()
}
func TestStudioAssetOptionalInstallation(t *testing.T) {
	gateway := &OpenAIGatewayService{}
	gateway.SetStudioAssetStorage(nil)
	gateway.StartStudioAssetWorker()
	gateway.StopStudioAssetWorker()
	_, err := gateway.StudioAssets().List(context.Background(), StudioAssetOwner{UserID: 7, APIKeyID: 100}, StudioAssetQuery{})
	require.ErrorIs(t, err, ErrStudioAssetUnavailable)
}
