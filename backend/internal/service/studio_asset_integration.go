package service

import (
	"context"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
)

// Install only at startup using the existing dynamic private-storage resolver.
func (s *OpenAIGatewayService) SetStudioAssetStorage(resolve ImageStorageResolver) {
	if s == nil {
		return
	}
	if repo, ok := s.usageBillingRepo.(StudioAssetRepository); ok {
		s.studioAssets = NewStudioAssetService(repo, resolve)
	}
}

func (s *OpenAIGatewayService) StudioAssets() *StudioAssetService {
	if s == nil {
		return nil
	}
	return s.studioAssets
}

func (s *OpenAIGatewayService) StartStudioAssetWorker() {
	if s == nil {
		return
	}
	s.studioAssetRuntimeMu.Lock()
	defer s.studioAssetRuntimeMu.Unlock()
	if s.studioAssets == nil || s.studioAssetCancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	s.studioAssetCancel, s.studioAssetDone = cancel, done
	go func() {
		defer close(done)
		// A single serial loop owns both queues; a slow tick never overlaps the next.
		for ctx.Err() == nil {
			s.processStudioAssetQueues(ctx)
			timer := time.NewTimer(15 * time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
}

func (s *OpenAIGatewayService) processStudioAssetQueues(ctx context.Context) {
	// At most one fixed-size warning per queue per polling cycle. Raw errors
	// can contain private sources or credentials and must never be logged here.
	reportFailure := func(queue string, err error) {
		if err == nil || ctx.Err() != nil {
			return
		}
		logger.FromContext(ctx).Warn("studio_asset.worker_process_failed",
			zap.String("queue", queue),
			zap.String("error_code", "studio_asset_unavailable"),
		)
	}
	reportFailure("archive", s.studioAssets.ProcessPending(ctx))
	if ctx.Err() != nil {
		return
	}
	reportFailure("cleanup", s.studioAssets.ProcessCleanup(ctx))
}

func (s *OpenAIGatewayService) StopStudioAssetWorker() {
	if s == nil {
		return
	}
	s.studioAssetRuntimeMu.Lock()
	defer s.studioAssetRuntimeMu.Unlock()
	if s.studioAssetCancel != nil {
		s.studioAssetCancel()
		<-s.studioAssetDone
		s.studioAssetCancel, s.studioAssetDone = nil, nil
	}
}
