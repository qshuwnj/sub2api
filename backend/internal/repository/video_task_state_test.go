package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestVideoTaskStateSQLScopedLoadAndUnavailable(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	repo := &usageBillingRepository{db: db}
	payload, _ := json.Marshal(service.GrokVideoPendingBilling{Model: "seedance-2.0"})
	mock.ExpectQuery(`WHERE request_id=\$1 AND user_id=\$2 AND api_key_id=\$3`).
		WithArgs("task", int64(1), int64(2)).
		WillReturnRows(sqlmock.NewRows([]string{"group_id", "account_id", "pending_billing", "settled"}).AddRow(3, 4, payload, true))
	state, err := repo.LoadVideoTaskState(context.Background(), "task", 1, 2)
	require.NoError(t, err)
	require.EqualValues(t, 4, state.AccountID)
	require.True(t, state.Pending.Settled)
	mock.ExpectQuery(`SELECT group_id`).WithArgs("task", int64(9), int64(2)).WillReturnError(sql.ErrNoRows)
	state, err = repo.LoadVideoTaskState(context.Background(), "task", 9, 2)
	require.NoError(t, err)
	require.Nil(t, state)
	mock.ExpectQuery(`SELECT group_id`).WithArgs("task", int64(1), int64(2)).WillReturnError(sql.ErrConnDone)
	_, err = repo.LoadVideoTaskState(context.Background(), "task", 1, 2)
	require.ErrorIs(t, err, sql.ErrConnDone)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestVideoTaskStateSQLGuardsImmutableSnapshotAndMonotonicSettlement(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	repo := &usageBillingRepository{db: db}
	state := service.VideoTaskState{RequestID: "task", UserID: 1, APIKeyID: 2, GroupID: 3, AccountID: 4,
		Pending: service.GrokVideoPendingBilling{Model: "model", Settled: true}}
	payload, _ := json.Marshal(service.GrokVideoPendingBilling{Model: "model"})
	mock.ExpectExec(`(?s)INSERT INTO video_task_states.*settled=video_task_states.settled OR EXCLUDED.settled.*account_id=EXCLUDED.account_id.*pending_billing=EXCLUDED.pending_billing`).
		WithArgs("task", int64(1), int64(2), int64(3), int64(4), string(payload), true).
		WillReturnResult(sqlmock.NewResult(0, 1))
	require.NoError(t, repo.SaveVideoTaskState(context.Background(), state))
	mock.ExpectExec(`(?s)UPDATE video_task_states SET settled=settled OR \$4.*pending_billing=\$5::jsonb`).
		WithArgs("task", int64(1), int64(2), true, string(payload)).
		WillReturnResult(sqlmock.NewResult(0, 0))
	require.ErrorIs(t, repo.UpdateVideoTaskBilling(context.Background(), "task", 1, 2, state.Pending), service.ErrVideoTaskStateConflict)
	require.NoError(t, mock.ExpectationsWereMet())
}

// Real PostgreSQL coverage is opt-in and only uses the disposable video fixture.
func TestVideoTaskStatePostgresSurvivesRedisLossAndChargesOnce(t *testing.T) {
	f := newVideoFixture(t, "100")
	ctx := context.Background()
	state := service.VideoTaskState{RequestID: "video-state-test", UserID: f.u, APIKeyID: f.k, GroupID: f.g, AccountID: f.a,
		Pending: service.GrokVideoPendingBilling{Model: "seedance-2.0", VideoResolution: "720p", VideoDurationSeconds: 15, CreatedAt: "2026-11-13T00:00:00Z"}}
	require.NoError(t, f.r.CheckVideoTaskStorage(ctx))
	require.NoError(t, f.r.SaveVideoTaskState(ctx, state))
	require.NoError(t, f.r.SaveVideoTaskState(ctx, state))
	wrong := state
	wrong.AccountID++
	require.ErrorIs(t, f.r.SaveVideoTaskState(ctx, wrong), service.ErrVideoTaskStateConflict)
	wrong = state
	wrong.Pending.VideoDurationSeconds++
	require.ErrorIs(t, f.r.SaveVideoTaskState(ctx, wrong), service.ErrVideoTaskStateConflict)
	missing, err := f.r.LoadVideoTaskState(ctx, state.RequestID, f.u, f.k+1)
	require.NoError(t, err)
	require.Nil(t, missing)
	// Concurrent polls and a crash between billing and the settled flag must not
	// deduct a second time. This calls the production SQL billing implementation.
	var charged atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := &service.UsageBillingCommand{RequestID: service.StableGrokVideoBillingRequestID(state.RequestID),
				APIKeyID: f.k, UserID: f.u, AccountID: f.a, Model: state.Pending.Model, BalanceCost: 1.25}
			result, err := f.r.Apply(ctx, cmd)
			if err != nil {
				t.Errorf("settlement: %v", err)
				return
			}
			if result.Applied {
				charged.Add(1)
			}
		}()
	}
	wg.Wait()
	require.EqualValues(t, 1, charged.Load())
	var balance float64
	require.NoError(t, f.db.QueryRow(`SELECT balance FROM users WHERE id=$1`, f.u).Scan(&balance))
	require.InDelta(t, 98.75, balance, 0.00000001)
	state.Pending.Settled = true
	require.NoError(t, f.r.UpdateVideoTaskBilling(ctx, state.RequestID, f.u, f.k, state.Pending))
	state.Pending.Settled = false
	require.NoError(t, f.r.UpdateVideoTaskBilling(ctx, state.RequestID, f.u, f.k, state.Pending))
	saved, err := f.r.LoadVideoTaskState(ctx, state.RequestID, f.u, f.k)
	require.NoError(t, err)
	require.True(t, saved.Pending.Settled)
}
