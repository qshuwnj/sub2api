package service

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
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

type studioWorkerFailureRepo struct {
	StudioAssetRepository
	pendingErr, cleanupErr       error
	cancelPending, cancelCleanup context.CancelFunc
	pendingCalls, cleanupCalls   int
}

func (r *studioWorkerFailureRepo) ClaimStudioAssets(context.Context, int) ([]*StudioAsset, error) {
	r.pendingCalls++
	if r.cancelPending != nil {
		r.cancelPending()
	}
	return nil, r.pendingErr
}

func (r *studioWorkerFailureRepo) ClaimDeletedStudioAssets(context.Context, int) ([]*StudioAsset, error) {
	r.cleanupCalls++
	if r.cancelCleanup != nil {
		r.cancelCleanup()
	}
	return nil, r.cleanupErr
}

func TestStudioAssetWorkerFailuresAreSanitized(t *testing.T) {
	privateErr := errors.New(strings.Repeat("source=https://private/source?credential=secret object_key=private/key ", 1000))
	for _, tc := range []struct {
		name                   string
		pendingErr, cleanupErr error
		queues                 []string
	}{
		{name: "archive", pendingErr: privateErr, queues: []string{"archive"}},
		{name: "cleanup", cleanupErr: privateErr, queues: []string{"cleanup"}},
		{name: "both", pendingErr: privateErr, cleanupErr: privateErr, queues: []string{"archive", "cleanup"}},
		{name: "healthy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			core, logs := observer.New(zap.WarnLevel)
			ctx := logger.IntoContext(context.Background(), zap.New(core))
			repo := &studioWorkerFailureRepo{pendingErr: tc.pendingErr, cleanupErr: tc.cleanupErr}
			gateway := &OpenAIGatewayService{studioAssets: NewStudioAssetService(repo, nil)}

			gateway.processStudioAssetQueues(ctx)

			require.Equal(t, 1, repo.pendingCalls)
			require.Equal(t, 1, repo.cleanupCalls)
			entries := logs.All()
			require.Len(t, entries, len(tc.queues))
			for i, queue := range tc.queues {
				require.Equal(t, "studio_asset.worker_process_failed", entries[i].Message)
				// Fixed fields exclude raw errors, sources, credentials and object keys.
				require.Equal(t, map[string]any{"queue": queue, "error_code": "studio_asset_unavailable"}, entries[i].ContextMap())
			}
		})
	}
}

func TestStudioAssetWorkerCancellationIsQuiet(t *testing.T) {
	for _, queue := range []string{"archive", "cleanup"} {
		t.Run(queue, func(t *testing.T) {
			core, logs := observer.New(zap.WarnLevel)
			ctx, cancel := context.WithCancel(logger.IntoContext(context.Background(), zap.New(core)))
			defer cancel()
			repo := &studioWorkerFailureRepo{}
			if queue == "archive" {
				repo.cancelPending, repo.pendingErr = cancel, context.Canceled
			} else {
				repo.cancelCleanup, repo.cleanupErr = cancel, context.Canceled
			}
			gateway := &OpenAIGatewayService{studioAssets: NewStudioAssetService(repo, nil)}

			gateway.processStudioAssetQueues(ctx)

			require.ErrorIs(t, ctx.Err(), context.Canceled)
			require.Empty(t, logs.All())
			require.Equal(t, 1, repo.pendingCalls)
			if queue == "archive" {
				require.Zero(t, repo.cleanupCalls)
			} else {
				require.Equal(t, 1, repo.cleanupCalls)
			}
		})
	}
}
