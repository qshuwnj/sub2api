package repository

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
)

var _ service.VideoResultRepository = (*usageBillingRepository)(nil)
var _ service.VideoResultBackfillRepository = (*usageBillingRepository)(nil)

const videoResultColumns = `resource, upstream_task_id, user_id, api_key_id, account_id,
	source_url, storage_key, mime, size_bytes, attempts, lease_token`

func scanVideoResult(row interface{ Scan(...any) error }) (*service.VideoResult, error) {
	var v service.VideoResult
	err := row.Scan(&v.Resource, &v.TaskID, &v.UserID, &v.APIKeyID, &v.AccountID,
		&v.SourceURL, &v.StorageKey, &v.MIME, &v.Size, &v.Attempts, &v.LeaseToken)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, service.ErrVideoResultNotFound
	}
	return &v, err
}

func (r *usageBillingRepository) QueueVideoResult(ctx context.Context, v *service.VideoResult) error {
	result, err := r.db.ExecContext(ctx, `
		INSERT INTO video_results (user_id, api_key_id, resource, upstream_task_id, account_id, source_url)
		VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (user_id, api_key_id, resource) DO UPDATE SET
			source_url = CASE WHEN video_results.storage_key = '' THEN EXCLUDED.source_url ELSE '' END,
			updated_at = NOW()
		WHERE video_results.account_id = EXCLUDED.account_id
			AND video_results.upstream_task_id = EXCLUDED.upstream_task_id
	`, v.UserID, v.APIKeyID, v.Resource, v.TaskID, v.AccountID, v.SourceURL)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n == 0 {
		return service.ErrVideoAttemptConflict
	}
	return err
}

func (r *usageBillingRepository) GetVideoResult(ctx context.Context, owner service.VideoAttemptOwner, resource string) (*service.VideoResult, error) {
	return scanVideoResult(r.db.QueryRowContext(ctx, `SELECT `+videoResultColumns+`
		FROM video_results WHERE user_id = $1 AND api_key_id = $2 AND resource = $3`,
		owner.UserID, owner.APIKeyID, resource))
}

// A committed financial result is the durable outbox. Recover missing jobs
// after an enqueue error or process exit without touching any billing state.
func (r *usageBillingRepository) BackfillVideoResults(ctx context.Context, limit int) error {
	if limit < 1 || limit > 100 {
		limit = 100
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO video_results (user_id, api_key_id, resource, upstream_task_id, account_id, source_url)
		SELECT a.user_id, a.api_key_id, 'attempt:' || a.id, a.upstream_task_id, a.account_id, a.result_urls->>0
		FROM video_attempts a
		WHERE a.state = 'completed' AND a.financial_state = 'captured'
			AND a.receipt_usage_log_id IS NOT NULL
			AND jsonb_array_length(a.result_urls) = 1
			AND a.upstream_task_id ~ '^atlascloud:[A-Za-z0-9_.:-]+$'
			AND length(a.upstream_task_id) <= 128 AND a.result_urls->>0 LIKE 'https://%'
			AND NOT EXISTS (SELECT 1 FROM video_results v
				WHERE v.user_id = a.user_id AND v.api_key_id = a.api_key_id AND v.resource = 'attempt:' || a.id)
		ORDER BY a.updated_at, a.id LIMIT $1
		ON CONFLICT (user_id, api_key_id, resource) DO NOTHING`, limit)
	if err != nil {
		return err
	}
	// Older clients predate video_attempts. Their committed usage receipt is
	// enough to queue a GET-only recovery; ambiguous account bindings fail shut.
	_, err = r.db.ExecContext(ctx, `
		INSERT INTO video_results (user_id, api_key_id, resource, upstream_task_id, account_id)
		SELECT u.user_id, u.api_key_id, 'task:' || SUBSTRING(u.request_id FROM 12),
			SUBSTRING(u.request_id FROM 12), MIN(u.account_id)
		FROM usage_logs u
		WHERE u.video_count > 0 AND u.request_id ~ '^grok-video:atlascloud:[A-Za-z0-9_.:-]{1,117}$'
			AND (
				EXISTS (SELECT 1 FROM usage_billing_dedup d WHERE d.request_id = u.request_id AND d.api_key_id = u.api_key_id)
				OR EXISTS (SELECT 1 FROM usage_billing_dedup_archive d WHERE d.request_id = u.request_id AND d.api_key_id = u.api_key_id)
			)
			AND NOT EXISTS (SELECT 1 FROM video_results v
				WHERE v.user_id = u.user_id AND v.api_key_id = u.api_key_id
					AND v.resource = 'task:' || SUBSTRING(u.request_id FROM 12))
		GROUP BY u.user_id, u.api_key_id, u.request_id
		HAVING COUNT(DISTINCT u.account_id) = 1 AND MIN(u.account_id) > 0
		ORDER BY MIN(u.created_at), u.user_id, u.api_key_id, u.request_id LIMIT $1
		ON CONFLICT (user_id, api_key_id, resource) DO NOTHING`, limit)
	return err
}

func (r *usageBillingRepository) ClaimVideoResults(ctx context.Context, limit int) ([]*service.VideoResult, error) {
	if limit < 1 || limit > 10 {
		limit = 1
	}
	rows, err := r.db.QueryContext(ctx, `
		UPDATE video_results SET lease_token = $1, lease_expires_at = NOW() + INTERVAL '4 minutes',
			attempts = attempts + 1, updated_at = NOW()
		WHERE (user_id, api_key_id, resource) IN (
			SELECT user_id, api_key_id, resource FROM video_results
			WHERE storage_key = '' AND next_retry_at <= NOW()
				AND (lease_expires_at IS NULL OR lease_expires_at <= NOW())
			ORDER BY next_retry_at LIMIT $2 FOR UPDATE SKIP LOCKED
		) RETURNING `+videoResultColumns, uuid.NewString(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []*service.VideoResult
	for rows.Next() {
		v, err := scanVideoResult(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, v)
	}
	return items, rows.Err()
}

func (r *usageBillingRepository) CompleteVideoResult(ctx context.Context, v *service.VideoResult) error {
	result, err := r.db.ExecContext(ctx, `UPDATE video_results SET storage_key = $5,
		source_url = '', mime = $6, size_bytes = $7, lease_token = '', lease_expires_at = NULL, updated_at = NOW()
		WHERE user_id = $1 AND api_key_id = $2 AND resource = $3 AND lease_token = $4
			AND account_id = $8 AND upstream_task_id = $9 AND storage_key = ''`,
		v.UserID, v.APIKeyID, v.Resource, v.LeaseToken, v.StorageKey, v.MIME, v.Size, v.AccountID, v.TaskID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n != 1 {
		return service.ErrVideoResultUnavailable
	}
	return err
}

func (r *usageBillingRepository) RetryVideoResult(ctx context.Context, v *service.VideoResult) error {
	_, err := r.db.ExecContext(ctx, `UPDATE video_results SET lease_token = '', lease_expires_at = NULL,
		next_retry_at = NOW() + LEAST(attempts, 30) * INTERVAL '1 minute', updated_at = NOW()
		WHERE user_id = $1 AND api_key_id = $2 AND resource = $3 AND lease_token = $4 AND storage_key = ''`,
		v.UserID, v.APIKeyID, v.Resource, v.LeaseToken)
	return err
}

// Redis ownership bindings expire; a durable, committed customer usage receipt
// is the only allowed fallback. Never discover tasks using a caller-supplied URL.
func (r *usageBillingRepository) FindLegacyVideoResult(ctx context.Context, owner service.VideoAttemptOwner, task string) (*service.VideoResult, error) {
	if !strings.HasPrefix(task, "atlascloud:") || !service.ValidVideoAttemptID(task) || owner.UserID <= 0 || owner.APIKeyID <= 0 {
		return nil, service.ErrVideoResultNotFound
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT DISTINCT u.account_id FROM usage_logs u
		WHERE u.request_id = $1 AND u.user_id = $2 AND u.api_key_id = $3 AND u.video_count > 0
			AND (
				EXISTS (SELECT 1 FROM usage_billing_dedup d WHERE d.request_id = u.request_id AND d.api_key_id = u.api_key_id)
				OR EXISTS (SELECT 1 FROM usage_billing_dedup_archive d WHERE d.request_id = u.request_id AND d.api_key_id = u.api_key_id)
			)
		LIMIT 2`, service.StableGrokVideoBillingRequestID(task), owner.UserID, owner.APIKeyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var account int64
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, service.ErrVideoResultNotFound
	}
	if err := rows.Scan(&account); err != nil {
		return nil, err
	}
	if rows.Next() {
		return nil, service.ErrVideoAttemptConflict
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if account <= 0 {
		return nil, service.ErrVideoResultNotFound
	}
	return &service.VideoResult{Resource: "task:" + task, TaskID: task,
		UserID: owner.UserID, APIKeyID: owner.APIKeyID, AccountID: account}, nil
}
