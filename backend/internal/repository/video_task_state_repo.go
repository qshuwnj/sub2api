package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

var _ service.VideoTaskStateRepository = (*usageBillingRepository)(nil)

func (r *usageBillingRepository) CheckVideoTaskStorage(ctx context.Context) error {
	if r == nil || r.db == nil {
		return errors.New("video task database is unavailable")
	}
	var present bool
	return r.db.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM video_task_states LIMIT 1)").Scan(&present)
}

func (r *usageBillingRepository) SaveVideoTaskState(ctx context.Context, state service.VideoTaskState) error {
	if r == nil || r.db == nil {
		return errors.New("video task database is unavailable")
	}
	if strings.TrimSpace(state.RequestID) == "" || state.UserID <= 0 || state.APIKeyID <= 0 || state.AccountID <= 0 {
		return service.ErrVideoTaskStateConflict
	}
	settled := state.Pending.Settled
	state.Pending.Settled = false
	payload, err := json.Marshal(state.Pending)
	if err != nil {
		return err
	}
	result, err := r.db.ExecContext(ctx, `
		INSERT INTO video_task_states (request_id, user_id, api_key_id, group_id, account_id, pending_billing, settled)
		VALUES ($1,$2,$3,$4,$5,$6::jsonb,$7)
		ON CONFLICT (request_id,user_id,api_key_id) DO UPDATE
		SET settled=video_task_states.settled OR EXCLUDED.settled
		WHERE video_task_states.group_id=EXCLUDED.group_id
		  AND video_task_states.account_id=EXCLUDED.account_id
		  AND video_task_states.pending_billing=EXCLUDED.pending_billing
	`, state.RequestID, state.UserID, state.APIKeyID, state.GroupID, state.AccountID, string(payload), settled)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err == nil && affected != 1 {
		return service.ErrVideoTaskStateConflict
	}
	return err
}

func (r *usageBillingRepository) LoadVideoTaskState(ctx context.Context, requestID string, userID, apiKeyID int64) (*service.VideoTaskState, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("video task database is unavailable")
	}
	if strings.TrimSpace(requestID) == "" || userID <= 0 || apiKeyID <= 0 {
		return nil, service.ErrVideoTaskStateConflict
	}
	state := &service.VideoTaskState{RequestID: requestID, UserID: userID, APIKeyID: apiKeyID}
	var payload []byte
	var settled bool
	err := r.db.QueryRowContext(ctx, `
		SELECT group_id,account_id,pending_billing,settled FROM video_task_states
		WHERE request_id=$1 AND user_id=$2 AND api_key_id=$3
	`, requestID, userID, apiKeyID).Scan(&state.GroupID, &state.AccountID, &payload, &settled)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(payload, &state.Pending); err != nil {
		return nil, err
	}
	state.Pending.Settled = settled
	return state, nil
}

func (r *usageBillingRepository) UpdateVideoTaskBilling(ctx context.Context, requestID string, userID, apiKeyID int64, pending service.GrokVideoPendingBilling) error {
	if r == nil || r.db == nil {
		return errors.New("video task database is unavailable")
	}
	if strings.TrimSpace(requestID) == "" || userID <= 0 || apiKeyID <= 0 {
		return service.ErrVideoTaskStateConflict
	}
	settled := pending.Settled
	pending.Settled = false
	payload, err := json.Marshal(pending)
	if err != nil {
		return err
	}
	// Never replace create-time fields, and never regress a settled job.
	result, err := r.db.ExecContext(ctx, `
		UPDATE video_task_states SET settled=settled OR $4, updated_at=NOW()
		WHERE request_id=$1 AND user_id=$2 AND api_key_id=$3 AND pending_billing=$5::jsonb
	`, requestID, userID, apiKeyID, settled, string(payload))
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err == nil && affected != 1 {
		return service.ErrVideoTaskStateConflict
	}
	return err
}
