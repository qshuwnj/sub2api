package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type videoAttemptRepoStub struct {
	VideoAttemptRepository
	cmd *VideoAttemptAcceptCommand
}

func (r *videoAttemptRepoStub) VideoQuotedBillingReady() bool { return true }
func (r *videoAttemptRepoStub) AcceptVideoAttempt(_ context.Context, c *VideoAttemptAcceptCommand) (*VideoAttemptAcceptResult, error) {
	r.cmd = c
	return &VideoAttemptAcceptResult{Attempt: c.Attempt, Created: true}, nil
}

func TestVideoAttemptAcceptUsesOnlyTrustedExactSnapshotAndContentHash(t *testing.T) {
	r := &videoAttemptRepoStub{}
	s := NewVideoAttemptService(r)
	now := time.Now()
	snapshot := &VideoQuoteSnapshot{claims: videoQuoteClaims{
		ID: "quote", KeyID: 2, UserID: 3, GroupID: 4,
		Selection: VideoQuoteRequest{Model: "model", Mode: "t2v", Controls: map[string]any{"duration": 8}},
		BasePrice: "9999999999.12345678", TotalPrice: "9999999999.12345678", Multiplier: "1",
		TariffRevision: "tariff", ContractRevision: "contract", Expires: now.Add(time.Minute).Unix(),
	}}
	body := []byte(`{"prompt":"private prompt","image":"https://private.example/reference"}`)
	got, err := s.Accept(context.Background(), snapshot, "attempt", body, 5)
	require.NoError(t, err)
	require.True(t, got.Created)
	require.Equal(t, "9999999999.12345678", r.cmd.Attempt.TotalPrice)
	require.Equal(t, "video-quote:quote", r.cmd.Attempt.RequestID())
	require.Equal(t, HashUsageRequestPayload(body), r.cmd.Attempt.RequestFingerprint)
	require.NotContains(t, r.cmd.Attempt.Selection.Controls, "prompt")
	require.NotContains(t, r.cmd.Attempt.Selection.Controls, "image")
	require.Equal(t, int64(5), r.cmd.Attempt.AccountID)
	snapshot.claims.Selection.Controls["duration"] = 10
	require.EqualValues(t, 8, r.cmd.Attempt.Selection.Controls["duration"])
}

func TestVideoAttemptRejectsInvalidSnapshotsAndUnavailableCapability(t *testing.T) {
	for _, amount := range []string{"10000000000.00000000", "0.000000001", "-1.00000000", "NaN", "1e3"} {
		t.Run(amount, func(t *testing.T) {
			r := &videoAttemptRepoStub{}
			s := NewVideoAttemptService(r)
			_, err := s.Accept(context.Background(), &VideoQuoteSnapshot{claims: videoQuoteClaims{
				ID: "q", UserID: 1, KeyID: 2, GroupID: 3, TotalPrice: amount,
				BasePrice: amount, Multiplier: "1", Expires: time.Now().Add(time.Minute).Unix(),
			}}, "a", []byte("body"), 4)
			require.Error(t, err)
			require.Nil(t, r.cmd)
		})
	}
	_, err := NewVideoAttemptService(nil).Accept(context.Background(), nil, "a", nil, 1)
	require.Error(t, err)
}
