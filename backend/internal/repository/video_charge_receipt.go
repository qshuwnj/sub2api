package repository

import (
	"context"
	"regexp"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

var _ service.VideoChargeReceiptReader = (*usageBillingRepository)(nil)
var videoReceiptAmount = regexp.MustCompile(`^(?:0|[1-9][0-9]{0,11})\.[0-9]{1,10}$`)

func (r *usageBillingRepository) FindVideoChargeReceipt(ctx context.Context, owner service.VideoAttemptOwner, task string) (*service.VideoChargeReceipt, error) {
	if owner.UserID <= 0 || owner.APIKeyID <= 0 || !service.ValidVideoReceiptTaskID(task) {
		return nil, service.ErrVideoResultNotFound
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT u.id, u.request_id, u.actual_cost::text, u.created_at
		FROM usage_logs u
		WHERE u.request_id = $1 AND u.user_id = $2 AND u.api_key_id = $3
			AND (u.video_count > 0 OR ($1 LIKE 'grok-video:seedance:%' AND u.output_tokens > 0))
			AND (
				EXISTS (SELECT 1 FROM usage_billing_dedup d WHERE d.request_id = u.request_id AND d.api_key_id = u.api_key_id)
				OR EXISTS (SELECT 1 FROM usage_billing_dedup_archive d WHERE d.request_id = u.request_id AND d.api_key_id = u.api_key_id)
			)
		LIMIT 2`, service.StableGrokVideoBillingRequestID(task), owner.UserID, owner.APIKeyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, service.ErrVideoResultNotFound
	}
	receipt := &service.VideoChargeReceipt{Currency: "USD"}
	if err := rows.Scan(&receipt.UsageLogID, &receipt.RequestID, &receipt.Amount, &receipt.CapturedAt); err != nil {
		return nil, err
	}
	if rows.Next() {
		return nil, service.ErrVideoAttemptConflict
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if receipt.UsageLogID <= 0 || !videoReceiptAmount.MatchString(receipt.Amount) {
		return nil, service.ErrVideoResultUnavailable
	}
	return receipt, nil
}
