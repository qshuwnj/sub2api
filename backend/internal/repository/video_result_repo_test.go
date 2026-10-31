package repository

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestVideoResultLookupRequiresBothOwnerIDs(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	r := &usageBillingRepository{db: db}
	mock.ExpectQuery(`FROM video_results WHERE user_id = \$1 AND api_key_id = \$2 AND resource = \$3`).
		WithArgs(int64(16), int64(20), "task:atlascloud:job").WillReturnRows(sqlmock.NewRows([]string{"resource"}))
	_, err = r.GetVideoResult(context.Background(), service.VideoAttemptOwner{UserID: 16, APIKeyID: 20}, "task:atlascloud:job")
	require.ErrorIs(t, err, service.ErrVideoResultNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestVideoResultCompletionRequiresLeaseAndNeverOverwritesBinding(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	r := &usageBillingRepository{db: db}
	v := &service.VideoResult{Resource: "attempt:job", UserID: 16, APIKeyID: 20, AccountID: 30,
		TaskID: "atlascloud:job", LeaseToken: "lease", StorageKey: "video-results/16/20/job.mp4", MIME: "video/mp4", Size: 24}
	mock.ExpectExec(`UPDATE video_results SET storage_key`).
		WithArgs(v.UserID, v.APIKeyID, v.Resource, v.LeaseToken, v.StorageKey, v.MIME, v.Size, v.AccountID, v.TaskID).
		WillReturnResult(sqlmock.NewResult(0, 0))
	require.Error(t, r.CompleteVideoResult(context.Background(), v))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestVideoResultBackfillUsesOnlyCompletedCapturedAttempts(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	r := &usageBillingRepository{db: db}
	backfill, ok := any(r).(interface {
		BackfillVideoResults(context.Context, int) error
	})
	require.True(t, ok, "a restart must recover a committed completion even if enqueue failed")
	mock.ExpectExec(`(?s)INSERT INTO video_results.*FROM video_attempts a.*state = 'completed'.*financial_state = 'captured'.*receipt_usage_log_id IS NOT NULL.*jsonb_array_length\(a.result_urls\) = 1.*NOT EXISTS.*LIMIT \$1.*ON CONFLICT.*DO NOTHING`).
		WithArgs(100).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`(?s)INSERT INTO video_results.*FROM usage_logs u.*u.video_count > 0.*EXISTS.*usage_billing_dedup.*usage_billing_dedup_archive.*NOT EXISTS.*GROUP BY.*HAVING COUNT\(DISTINCT u.account_id\) = 1.*LIMIT \$1.*ON CONFLICT.*DO NOTHING`).
		WithArgs(100).WillReturnResult(sqlmock.NewResult(0, 1))
	require.NoError(t, backfill.BackfillVideoResults(context.Background(), 100))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestLegacyVideoRecoveryRequiresCommittedOwnedReceiptAndRejectsCollision(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	r := &usageBillingRepository{db: db}
	mock.ExpectQuery(`SELECT DISTINCT u.account_id`).
		WithArgs(service.StableGrokVideoBillingRequestID("atlascloud:job"), int64(16), int64(20)).
		WillReturnRows(sqlmock.NewRows([]string{"account_id"}).AddRow(30).AddRow(31))
	_, err = r.FindLegacyVideoResult(context.Background(), service.VideoAttemptOwner{UserID: 16, APIKeyID: 20}, "atlascloud:job")
	require.ErrorIs(t, err, service.ErrVideoAttemptConflict)
	require.NoError(t, mock.ExpectationsWereMet())
}
