package service

import (
	"context"
	"errors"
	"time"
)

// Installed once by the production provider with the existing dynamic R2
// resolver. No storage credential or customer billing configuration is changed.
func (s *OpenAIGatewayService) SetVideoResultStorage(resolve ImageStorageResolver) {
	if repo, ok := s.usageBillingRepo.(VideoResultRepository); ok {
		s.videoResults = NewVideoResultService(repo, resolve)
	}
}

func (s *OpenAIGatewayService) VideoPlayback(ctx context.Context, owner VideoAttemptOwner, resource string) (*VideoPlayback, error) {
	return s.videoResults.Playback(ctx, owner, resource)
}

func (s *OpenAIGatewayService) OpenVideoResult(ctx context.Context, owner VideoAttemptOwner, resource, byteRange string) (*VideoResultContent, error) {
	return s.videoResults.Open(ctx, owner, resource, byteRange)
}

func (s *OpenAIGatewayService) QueueVideoResult(ctx context.Context, v *VideoResult) error {
	if s.videoResults == nil {
		return nil // Existing/test installations remain usable without R2.
	}
	return s.videoResults.Queue(ctx, v)
}

func (s *OpenAIGatewayService) refreshStoredVideoSource(ctx context.Context, v *VideoResult) (string, error) {
	account, err := s.accountRepo.GetByID(ctx, v.AccountID)
	if err != nil || account == nil || account.ID != v.AccountID {
		return "", ErrVideoResultUnavailable
	}
	result, err := s.requestQuotedAtlasVideo(ctx, account, v.TaskID, nil)
	if err != nil {
		return "", ErrVideoResultUnavailable
	}
	if result.State != VideoAttemptCompleted || len(result.ResultURLs) != 1 ||
		!validVideoResultURL(result.ResultURLs[0]) || videoSignedURLExpired(result.ResultURLs[0], time.Now()) {
		return "", ErrVideoResultExpired
	}
	return result.ResultURLs[0], nil
}

func (s *OpenAIGatewayService) refreshCompletedVideo(ctx context.Context, a *VideoAttempt, force bool) (*VideoAttempt, error) {
	if a.FinancialState != VideoFinancialCaptured || a.Receipt == nil || a.UpstreamTaskID == "" || len(a.ResultURLs) != 1 {
		return a, nil
	}
	owner := VideoAttemptOwner{UserID: a.UserID, APIKeyID: a.APIKeyID}
	resource := "attempt:" + a.ID
	if playback, err := s.VideoPlayback(ctx, owner, resource); err == nil {
		copy := *a
		copy.ResultURLs = []string{playback.URL}
		return &copy, nil
	}
	v := &VideoResult{Resource: resource, TaskID: a.UpstreamTaskID, UserID: a.UserID,
		APIKeyID: a.APIKeyID, AccountID: a.AccountID, SourceURL: a.ResultURLs[0]}
	if force || videoSignedURLExpired(v.SourceURL, time.Now()) {
		link, err := s.refreshStoredVideoSource(ctx, v)
		if err != nil {
			return a, err
		}
		v.SourceURL = link
	}
	_ = s.QueueVideoResult(ctx, v) // Archive failure is not a settlement failure.
	copy := *a
	copy.ResultURLs = []string{v.SourceURL}
	return &copy, nil
}

// Recovery of pre-quote tasks is authorized by durable metadata or a committed
// owned usage receipt, never by stale Redis state or current model pricing.
func (s *OpenAIGatewayService) RecoverLegacyVideoResult(ctx context.Context, owner VideoAttemptOwner, task string) (*VideoPlayback, error) {
	if s.videoResults == nil {
		return nil, ErrVideoResultNotFound
	}
	resource := "task:" + task
	if playback, err := s.VideoPlayback(ctx, owner, resource); err == nil {
		return playback, nil
	}
	v, err := s.videoResults.repo.GetVideoResult(ctx, owner, resource)
	if errors.Is(err, ErrVideoResultNotFound) {
		v, err = s.videoResults.repo.FindLegacyVideoResult(ctx, owner, task)
	}
	if err != nil {
		return nil, err
	}
	if v.StorageKey != "" {
		return nil, ErrVideoResultUnavailable
	}
	link, err := s.refreshStoredVideoSource(ctx, v)
	if err != nil {
		return nil, err
	}
	v.SourceURL = link
	if err := s.QueueVideoResult(ctx, v); err != nil {
		return nil, ErrVideoResultUnavailable
	}
	return &VideoPlayback{URL: link}, nil
}
