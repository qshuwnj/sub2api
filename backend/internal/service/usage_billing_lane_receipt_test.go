//go:build unit

package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

type receiptLaneTestRepo struct {
	*laneTestRepo
	requestID string
	apiKeyID  int64
	committed bool
	err       error
}

func (r *receiptLaneTestRepo) HasCommittedUsageBilling(_ context.Context, requestID string, apiKeyID int64) (bool, error) {
	r.requestID, r.apiKeyID = requestID, apiKeyID
	return r.committed, r.err
}

func TestUsageBillingLanePreservesCommittedVideoReceipts(t *testing.T) {
	repo := &receiptLaneTestRepo{laneTestRepo: newLaneTestRepo(), committed: true}
	lanes := NewUsageBillingLaneCombiner(repo)
	defer lanes.Stop()
	var reader CommittedUsageBillingReader = lanes
	ok, err := reader.HasCommittedUsageBilling(context.Background(), "grok-video:atlascloud:job", 42)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "grok-video:atlascloud:job", repo.requestID)
	require.EqualValues(t, 42, repo.apiKeyID)
	require.Empty(t, repo.applyCalls, "receipt lookup must not perform another charge")
	repo.err = errors.New("receipt lookup failed")
	repo.committed = false
	ok, err = reader.HasCommittedUsageBilling(context.Background(), "job", 42)
	require.ErrorIs(t, err, repo.err)
	require.False(t, ok)
}

func TestUsageBillingLaneMissingReceiptReaderFailsClosed(t *testing.T) {
	lanes := NewUsageBillingLaneCombiner(newLaneTestRepo())
	defer lanes.Stop()
	ok, err := lanes.HasCommittedUsageBilling(context.Background(), "job", 42)
	require.Error(t, err)
	require.False(t, ok)
	var absent *UsageBillingLaneCombiner
	ok, err = absent.HasCommittedUsageBilling(context.Background(), "job", 42)
	require.Error(t, err)
	require.False(t, ok)
}
