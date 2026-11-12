package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type videoStateRepoStub struct {
	UsageBillingRepository
	states map[string]VideoTaskState
	err    error
}

func (r *videoStateRepoStub) CheckVideoTaskStorage(context.Context) error { return r.err }
func (r *videoStateRepoStub) SaveVideoTaskState(ctx context.Context, state VideoTaskState) error {
	if r.err != nil {
		return r.err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.states == nil {
		r.states = map[string]VideoTaskState{}
	}
	r.states[grokVideoPendingBillingKey(state.RequestID, state.UserID, state.APIKeyID)] = state
	return nil
}
func (r *videoStateRepoStub) LoadVideoTaskState(_ context.Context, id string, user, key int64) (*VideoTaskState, error) {
	if r.err != nil {
		return nil, r.err
	}
	state, ok := r.states[grokVideoPendingBillingKey(id, user, key)]
	if !ok {
		return nil, nil
	}
	return &state, nil
}
func (r *videoStateRepoStub) UpdateVideoTaskBilling(_ context.Context, id string, user, key int64, pending GrokVideoPendingBilling) error {
	if r.err != nil {
		return r.err
	}
	k := grokVideoPendingBillingKey(id, user, key)
	state, ok := r.states[k]
	if !ok {
		return ErrVideoTaskStateNotFound
	}
	state.Pending.Settled = state.Pending.Settled || pending.Settled
	r.states[k] = state
	return nil
}

type videoLegacyCacheStub struct {
	GatewayCache
	bindings map[string]int64
	payload  []byte
	reads    int
	err      error
}

func (c *videoLegacyCacheStub) GetSessionAccountID(_ context.Context, group int64, hash string) (int64, error) {
	c.reads++
	if c.err != nil {
		return 0, c.err
	}
	id := c.bindings[fmt.Sprintf("%d:%s", group, hash)]
	if id == 0 {
		return 0, ErrStickySessionNotFound
	}
	return id, nil
}
func (c *videoLegacyCacheStub) GetGrokVideoPendingBilling(context.Context, string) ([]byte, error) {
	c.reads++
	return c.payload, c.err
}

func TestDurableVideoTaskSurvivesCacheLossAndScopesOwnership(t *testing.T) {
	repo := &videoStateRepoStub{}
	cache := &videoLegacyCacheStub{err: errors.New("OOM cache unavailable")}
	svc := &OpenAIGatewayService{usageBillingRepo: repo, cache: cache}
	group := int64(7)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.NoError(t, svc.PersistAcceptedVideoTask(ctx, &group, "dapacai:existing", 41, 51, 63,
		GrokVideoPendingBilling{Model: "seedance-2.0", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}))
	require.NoError(t, svc.CheckVideoTaskStorage(context.Background()))
	account, err := svc.ResolveGrokMediaVideoRequestAccount(context.Background(), &group, "dapacai:existing", 41, 51)
	require.NoError(t, err)
	require.EqualValues(t, 63, account)
	pending, err := svc.LoadGrokVideoPendingBilling(context.Background(), "dapacai:existing", 41, 51)
	require.NoError(t, err)
	require.Equal(t, "seedance-2.0", pending.Model)
	for i := 0; i < 2; i++ {
		admit, err := svc.ClaimGrokVideoBilling(context.Background(), "dapacai:existing", 41, 51)
		require.NoError(t, err)
		require.True(t, admit, "SQL usage dedup, not Redis, arbitrates settlement")
	}
	pending.Settled = true
	require.NoError(t, svc.StoreGrokVideoPendingBilling(context.Background(), "dapacai:existing", 41, 51, *pending))
	admit, err := svc.ClaimGrokVideoBilling(context.Background(), "dapacai:existing", 41, 51)
	require.NoError(t, err)
	require.False(t, admit)
	require.Zero(t, cache.reads, "a durable task must not depend on Redis")
	otherGroup := group + 1
	account, err = svc.ResolveGrokMediaVideoRequestAccount(context.Background(), &otherGroup, "dapacai:existing", 41, 51)
	require.NoError(t, err)
	require.Zero(t, account)
	for _, owner := range [][2]int64{{42, 51}, {41, 52}} {
		pending, err := svc.LoadGrokVideoPendingBilling(context.Background(), "dapacai:existing", owner[0], owner[1])
		require.NoError(t, err)
		require.Nil(t, pending)
	}
}

func TestDurableVideoDatabaseFailureNeverFallsBackToCache(t *testing.T) {
	failure := errors.New("database offline")
	repo := &videoStateRepoStub{err: failure}
	cache := &videoLegacyCacheStub{}
	svc := &OpenAIGatewayService{usageBillingRepo: repo, cache: cache}
	require.ErrorIs(t, svc.CheckVideoTaskStorage(context.Background()), failure)
	account, err := svc.ResolveGrokMediaVideoRequestAccount(context.Background(), nil, "job", 1, 2)
	require.Zero(t, account)
	require.ErrorIs(t, err, failure)
	_, err = svc.ClaimGrokVideoBilling(context.Background(), "job", 1, 2)
	require.ErrorIs(t, err, failure)
	require.Zero(t, cache.reads)
}

func TestDurableVideoLegacyBackfillRequiresOwnerGroupAndSnapshot(t *testing.T) {
	for _, missingSnapshot := range []bool{false, true} {
		t.Run(fmt.Sprint(missingSnapshot), func(t *testing.T) {
			repo := &videoStateRepoStub{}
			cache := &videoLegacyCacheStub{bindings: map[string]int64{}}
			svc := &OpenAIGatewayService{usageBillingRepo: repo, cache: cache}
			group := int64(7)
			key := svc.openAISessionCacheKey(GrokMediaVideoRequestSessionHash("legacy", 41, 51))
			cache.bindings[fmt.Sprintf("%d:%s", group, key)] = 63
			if !missingSnapshot {
				cache.payload, _ = json.Marshal(GrokVideoPendingBilling{Model: "model", Settled: true})
			}
			account, err := svc.ResolveGrokMediaVideoRequestAccount(context.Background(), &group, "legacy", 41, 51)
			if missingSnapshot {
				require.ErrorIs(t, err, ErrVideoTaskStateNotFound)
				require.Zero(t, account)
				require.Empty(t, repo.states)
				return
			}
			require.NoError(t, err)
			require.EqualValues(t, 63, account)
			pending, err := svc.LoadGrokVideoPendingBilling(context.Background(), "legacy", 41, 51)
			require.NoError(t, err)
			require.True(t, pending.Settled)
			for _, owner := range [][2]int64{{42, 51}, {41, 52}} {
				account, err = svc.ResolveGrokMediaVideoRequestAccount(context.Background(), &group, "legacy", owner[0], owner[1])
				require.NoError(t, err)
				require.Zero(t, account)
			}
			otherGroup := group + 1
			account, err = svc.ResolveGrokMediaVideoRequestAccount(context.Background(), &otherGroup, "legacy", 41, 51)
			require.NoError(t, err)
			require.Zero(t, account)
		})
	}
}
