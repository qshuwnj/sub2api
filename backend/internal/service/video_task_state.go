package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrVideoTaskStateConflict = errors.New("video task owner or create-time snapshot conflicts")
	ErrVideoTaskStateNotFound = errors.New("video task state not found")
)

// VideoTaskState stores only ownership and the immutable create-time billing
// snapshot. Provider credentials and signed result URLs do not belong here.
type VideoTaskState struct {
	RequestID string
	UserID    int64
	APIKeyID  int64
	GroupID   int64
	AccountID int64
	Pending   GrokVideoPendingBilling
}

type VideoTaskStateRepository interface {
	CheckVideoTaskStorage(context.Context) error
	SaveVideoTaskState(context.Context, VideoTaskState) error
	LoadVideoTaskState(context.Context, string, int64, int64) (*VideoTaskState, error)
	UpdateVideoTaskBilling(context.Context, string, int64, int64, GrokVideoPendingBilling) error
}

func (s *OpenAIGatewayService) videoTaskStateRepository() (VideoTaskStateRepository, bool) {
	if s == nil {
		return nil, false
	}
	repo, ok := s.usageBillingRepo.(VideoTaskStateRepository)
	return repo, ok
}

// CheckVideoTaskStorage prevents creating a paid upstream job when the durable
// store is already known to be unavailable. Redis is not an admission dependency.
func (s *OpenAIGatewayService) CheckVideoTaskStorage(ctx context.Context) error {
	if repo, ok := s.videoTaskStateRepository(); ok {
		return repo.CheckVideoTaskStorage(ctx)
	}
	return nil
}

// PersistAcceptedVideoTask atomically records owner and billing snapshot. The
// original provider ID must still be returned if this fails after acceptance.
func (s *OpenAIGatewayService) PersistAcceptedVideoTask(ctx context.Context, groupID *int64, requestID string,
	userID, apiKeyID, accountID int64, pending GrokVideoPendingBilling,
) error {
	if strings.TrimSpace(requestID) == "" || userID <= 0 || apiKeyID <= 0 || accountID <= 0 {
		return fmt.Errorf("invalid accepted video task identity")
	}
	pending = normalizeGrokVideoPendingBilling(pending)
	if repo, ok := s.videoTaskStateRepository(); ok {
		saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		return repo.SaveVideoTaskState(saveCtx, VideoTaskState{
			RequestID: strings.TrimSpace(requestID), UserID: userID, APIKeyID: apiKeyID,
			GroupID: derefGroupID(groupID), AccountID: accountID, Pending: pending,
		})
	}
	// Retained for lightweight integrations which do not wire a billing repository.
	if err := s.BindGrokMediaVideoRequestAccount(ctx, groupID, requestID, userID, apiKeyID, accountID); err != nil {
		return err
	}
	return s.StoreGrokVideoPendingBilling(ctx, requestID, userID, apiKeyID, pending)
}

// Only migrate a legacy job with both an exact owner/group binding and its
// original snapshot. A database failure must never fall back to another account.
func (s *OpenAIGatewayService) backfillLegacyVideoTask(ctx context.Context, repo VideoTaskStateRepository,
	groupID *int64, requestID string, userID, apiKeyID int64,
) (*VideoTaskState, error) {
	if s.cache == nil {
		return nil, nil
	}
	cacheKey := s.openAISessionCacheKey(GrokMediaVideoRequestSessionHash(requestID, userID, apiKeyID))
	accountID, err := s.cache.GetSessionAccountID(ctx, derefGroupID(groupID), cacheKey)
	if errors.Is(err, ErrStickySessionNotFound) {
		return nil, nil
	}
	if err != nil || accountID <= 0 {
		return nil, err
	}
	payload, err := s.cache.GetGrokVideoPendingBilling(ctx, grokVideoPendingBillingKey(requestID, userID, apiKeyID))
	if err != nil {
		return nil, err
	}
	if len(payload) == 0 {
		return nil, ErrVideoTaskStateNotFound
	}
	var pending GrokVideoPendingBilling
	if err := json.Unmarshal(payload, &pending); err != nil {
		return nil, err
	}
	state := VideoTaskState{
		RequestID: requestID, UserID: userID, APIKeyID: apiKeyID,
		GroupID: derefGroupID(groupID), AccountID: accountID, Pending: pending,
	}
	if err := repo.SaveVideoTaskState(ctx, state); err != nil {
		return nil, err
	}
	return repo.LoadVideoTaskState(ctx, requestID, userID, apiKeyID)
}

func normalizeGrokVideoPendingBilling(pending GrokVideoPendingBilling) GrokVideoPendingBilling {
	pending.Model = strings.TrimSpace(pending.Model)
	pending.BillingModel = strings.TrimSpace(pending.BillingModel)
	pending.UpstreamModel = strings.TrimSpace(pending.UpstreamModel)
	pending.OriginalModel = strings.TrimSpace(pending.OriginalModel)
	if pending.VideoResolution != "" {
		pending.VideoResolution = NormalizeVideoBillingResolutionForModel(pending.BillingModel, pending.VideoResolution)
	}
	if pending.VideoDurationSeconds > 0 {
		pending.VideoDurationSeconds = NormalizeVideoBillingDurationForModel(pending.BillingModel, pending.VideoDurationSeconds)
	}
	if strings.TrimSpace(pending.CreatedAt) == "" {
		pending.CreatedAt = GrokVideoPendingCreatedAtNow()
	} else {
		pending.CreatedAt = strings.TrimSpace(pending.CreatedAt)
	}
	return pending
}
