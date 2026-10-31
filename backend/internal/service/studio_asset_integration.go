package service

import (
	"context"
	"time"
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
			_ = s.studioAssets.ProcessPending(ctx)
			if ctx.Err() != nil {
				return
			}
			_ = s.studioAssets.ProcessCleanup(ctx)
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
