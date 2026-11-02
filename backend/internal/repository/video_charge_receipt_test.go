package repository

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestVideoChargeReceiptReadsOnlyUniqueCommittedOwnedUsage(t *testing.T) {
	for _, scenario := range []string{"charged", "free", "missing", "ambiguous", "invalid amount", "query failure"} {
		t.Run(scenario, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			reader, ok := any(&usageBillingRepository{db: db}).(interface {
				FindVideoChargeReceipt(context.Context, service.VideoAttemptOwner, string) (*service.VideoChargeReceipt, error)
			})
			require.True(t, ok, "legacy videos need a read-only actual-charge lookup")
			captured := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
			rows := sqlmock.NewRows([]string{"id", "request_id", "actual_cost", "created_at"})
			amount := "3.2500000000"
			if scenario == "free" {
				amount = "0.0000000000"
			}
			if scenario == "invalid amount" {
				amount = "NaN"
			}
			if scenario != "missing" {
				rows.AddRow(42, "grok-video:atlascloud:job", amount, captured)
			}
			if scenario == "ambiguous" {
				rows.AddRow(43, "grok-video:atlascloud:job", amount, captured)
			}
			expect := mock.ExpectQuery(`(?s)SELECT u.id, u.request_id, u.actual_cost::text, u.created_at.*FROM usage_logs u.*u.request_id = \$1.*u.user_id = \$2.*u.api_key_id = \$3.*u.video_count > 0.*EXISTS.*usage_billing_dedup.*usage_billing_dedup_archive.*LIMIT 2`).
				WithArgs("grok-video:atlascloud:job", int64(16), int64(20))
			if scenario == "query failure" {
				expect.WillReturnError(context.DeadlineExceeded)
			} else {
				expect.WillReturnRows(rows)
			}
			receipt, err := reader.FindVideoChargeReceipt(context.Background(), service.VideoAttemptOwner{UserID: 16, APIKeyID: 20}, "atlascloud:job")
			switch scenario {
			case "charged", "free":
				require.NoError(t, err)
				require.Equal(t, amount, receipt.Amount, "do not reprice or round the stored actual debit")
				require.Equal(t, "USD", receipt.Currency)
				require.Equal(t, int64(42), receipt.UsageLogID)
				require.Equal(t, captured, receipt.CapturedAt)
			default:
				require.Error(t, err)
				require.Nil(t, receipt, "a missing or ambiguous bill must never become a zero charge")
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestVideoChargeReceiptRejectsInvalidOwnerOrTaskWithoutQuerying(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	reader, ok := any(&usageBillingRepository{db: db}).(interface {
		FindVideoChargeReceipt(context.Context, service.VideoAttemptOwner, string) (*service.VideoChargeReceipt, error)
	})
	require.True(t, ok)
	for _, input := range []struct {
		owner service.VideoAttemptOwner
		task  string
	}{
		{service.VideoAttemptOwner{APIKeyID: 20}, "atlascloud:job"},
		{service.VideoAttemptOwner{UserID: 16}, "atlascloud:job"},
		{service.VideoAttemptOwner{UserID: 16, APIKeyID: 20}, ""},
		{service.VideoAttemptOwner{UserID: 16, APIKeyID: 20}, "../another"},
		{service.VideoAttemptOwner{UserID: 16, APIKeyID: 20}, "grok-video:atlascloud:job"},
	} {
		receipt, err := reader.FindVideoChargeReceipt(context.Background(), input.owner, input.task)
		require.ErrorIs(t, err, service.ErrVideoResultNotFound)
		require.Nil(t, receipt)
	}
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestVideoChargeReceiptIncludesNativeTokenBilledSeedance(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	r := &usageBillingRepository{db: db}
	mock.ExpectQuery(`(?s)FROM usage_logs u.*u.video_count > 0 OR.*LIKE 'grok-video:seedance:%'.*u.output_tokens > 0.*usage_billing_dedup`).
		WithArgs("grok-video:seedance:task-ark", int64(16), int64(20)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "request_id", "actual_cost", "created_at"}).
			AddRow(43, "grok-video:seedance:task-ark", "1.2500000000", time.Now()))
	receipt, err := r.FindVideoChargeReceipt(context.Background(), service.VideoAttemptOwner{UserID: 16, APIKeyID: 20}, "seedance:task-ark")
	require.NoError(t, err)
	require.Equal(t, "1.2500000000", receipt.Amount)
	require.NoError(t, mock.ExpectationsWereMet())
}
