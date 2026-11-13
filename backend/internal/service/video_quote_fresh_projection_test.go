package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type leanVideoQuoteKeys struct {
	videoQuoteKeys
	fresh *APIKey
	err   error
}

func (r *leanVideoQuoteKeys) GetByIDForVideoQuote(context.Context, int64) (*APIKey, error) {
	return r.fresh, r.err
}

type leanVideoQuoteGroups struct {
	videoQuoteGroups
	fresh *Group
	err   error
	model string
}

func (r *leanVideoQuoteGroups) GetByIDForVideoQuote(_ context.Context, _ int64, model string) (*Group, error) {
	r.model = model
	return r.fresh, r.err
}

func TestVideoQuoteFreshProjection(t *testing.T) {
	t.Run("fresh exact projection wins over stale fallback", func(t *testing.T) {
		s, key := videoFixture(t)
		s.billingCacheService.apiKeyRateLimitLoader = &leanVideoQuoteKeys{
			videoQuoteKeys: videoQuoteKeys{key: &APIKey{}}, fresh: key,
		}
		groups := &leanVideoQuoteGroups{
			videoQuoteGroups: videoQuoteGroups{group: &Group{}}, fresh: key.Group,
		}
		s.channelService.groupRepo = groups
		got, err := s.RefreshVideoQuoteKey(context.Background(), key, videoTestRequest().Model, time.Now())
		require.NoError(t, err)
		require.Equal(t, key.ID, got.ID)
		require.Equal(t, videoTestRequest().Model, groups.model)
	})
	for _, part := range []string{"key", "group"} {
		t.Run(part+" database error is unavailable, not invalid credentials", func(t *testing.T) {
			s, key := videoFixture(t)
			dbErr := errors.New("database is recovering")
			if part == "key" {
				s.billingCacheService.apiKeyRateLimitLoader = &leanVideoQuoteKeys{
					videoQuoteKeys: videoQuoteKeys{key: key}, err: dbErr,
				}
			} else {
				s.channelService.groupRepo = &leanVideoQuoteGroups{
					videoQuoteGroups: videoQuoteGroups{group: key.Group}, err: dbErr,
				}
			}
			got, err := s.RefreshVideoQuoteKey(context.Background(), key, videoTestRequest().Model, time.Now())
			require.Nil(t, got)
			require.ErrorIs(t, err, ErrVideoQuoteUnavailable)
			require.ErrorIs(t, err, dbErr)
		})
	}
	t.Run("missing key never falls back to cached permission", func(t *testing.T) {
		s, key := videoFixture(t)
		s.billingCacheService.apiKeyRateLimitLoader = &leanVideoQuoteKeys{
			videoQuoteKeys: videoQuoteKeys{key: key}, fresh: key, err: ErrAPIKeyNotFound,
		}
		got, err := s.RefreshVideoQuoteKey(context.Background(), key, videoTestRequest().Model, time.Now())
		require.Nil(t, got)
		require.Error(t, err)
		require.NotErrorIs(t, err, ErrVideoQuoteUnavailable)
	})
}
