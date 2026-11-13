package repository

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestAPIKeyAuthCacheBoundsOversizedSnapshots(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	cache := &apiKeyCache{rdb: client}
	ctx := context.Background()
	key := "synthetic-hash"
	entry := &service.APIKeyAuthCacheEntry{Snapshot: &service.APIKeyAuthSnapshot{
		Version: 1, APIKeyID: 437, Name: "normal",
	}}
	require.NoError(t, cache.SetAuthCache(ctx, key, entry, time.Minute))
	loaded, err := cache.GetAuthCache(ctx, key)
	require.NoError(t, err)
	require.Equal(t, entry, loaded)
	require.Greater(t, server.TTL(apiKeyAuthCacheKey(key)), time.Duration(0))

	// An unrelated scheduler key must not be evicted or explicitly deleted.
	require.NoError(t, client.Set(ctx, "sched:synthetic", "preserved", 0).Err())
	entry.Snapshot.Name = strings.Repeat("x", apiKeyAuthCacheMaxBytes)
	payload, err := json.Marshal(entry)
	require.NoError(t, err)
	require.Greater(t, len(payload), apiKeyAuthCacheMaxBytes)
	require.NoError(t, cache.SetAuthCache(ctx, key, entry, time.Minute))
	require.False(t, server.Exists(apiKeyAuthCacheKey(key)), "stale snapshot must be removed")
	require.True(t, server.Exists("sched:synthetic"))
	require.Len(t, entry.Snapshot.Name, apiKeyAuthCacheMaxBytes, "L1 source entry must remain intact")
	_, err = cache.GetAuthCache(ctx, key)
	require.ErrorIs(t, err, redis.Nil)
}
