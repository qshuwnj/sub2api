package repository

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
)

// Only durable owned completed/captured attempts or committed task receipts are
// discovery candidates. Pending intakes and video URLs alone prove neither.
const studioOwnedHistoryCandidates = `
	SELECT user_id,api_key_id,'attempt:'||id AS resource FROM video_attempts
	WHERE state='completed' AND financial_state='captured' AND receipt_usage_log_id IS NOT NULL
	UNION SELECT u.user_id,u.api_key_id,'task:'||SUBSTRING(u.request_id FROM 12) AS resource
	FROM usage_logs u WHERE u.request_id ~ '^grok-video:[A-Za-z0-9_.:-]{1,128}$'
	AND ((u.request_id LIKE 'grok-video:seedance:%' AND u.output_tokens>0)
		OR (u.request_id NOT LIKE 'grok-video:seedance:%' AND u.video_count>0))
	AND (EXISTS(SELECT 1 FROM usage_billing_dedup d WHERE d.request_id=u.request_id AND d.api_key_id=u.api_key_id)
		OR EXISTS(SELECT 1 FROM usage_billing_dedup_archive d WHERE d.request_id=u.request_id AND d.api_key_id=u.api_key_id))`

// Native completion billing uses the same stable task key as the existing
// handlers. Seedance is token-billed, while Grok uses a positive video count.
func (r *usageBillingRepository) FindStudioNativeVideoReceipt(ctx context.Context, owner service.VideoAttemptOwner, task string) (*service.VideoResult, error) {
	if owner.UserID <= 0 || owner.APIKeyID <= 0 || !service.ValidVideoAttemptID(task) ||
		strings.HasPrefix(task, "atlascloud:") || strings.HasPrefix(task, "grok-video:") {
		return nil, service.ErrVideoResultNotFound
	}
	rows, err := r.db.QueryContext(ctx, `SELECT DISTINCT u.account_id FROM usage_logs u
		WHERE u.user_id=$1 AND u.api_key_id=$2 AND u.request_id=$3
		AND (($4 AND u.output_tokens>0) OR (NOT $4 AND u.video_count>0))
		AND (EXISTS(SELECT 1 FROM usage_billing_dedup d WHERE d.request_id=u.request_id AND d.api_key_id=u.api_key_id)
			OR EXISTS(SELECT 1 FROM usage_billing_dedup_archive d WHERE d.request_id=u.request_id AND d.api_key_id=u.api_key_id))
		LIMIT 2`, owner.UserID, owner.APIKeyID, service.StableGrokVideoBillingRequestID(task), strings.HasPrefix(task, "seedance:"))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var accounts []int64
	for rows.Next() {
		var account int64
		if err := rows.Scan(&account); err != nil {
			return nil, err
		}
		accounts = append(accounts, account)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(accounts) != 1 || accounts[0] <= 0 {
		return nil, service.ErrVideoResultNotFound
	}
	return &service.VideoResult{Resource: "task:" + task, TaskID: task,
		UserID: owner.UserID, APIKeyID: owner.APIKeyID, AccountID: accounts[0]}, nil
}

func (r *usageBillingRepository) SaveStudioVideoIntake(ctx context.Context, in *service.StudioVideoIntake) error {
	if in == nil || !validStudioAssetOwner(in.Owner) || !service.ValidStudioVideoResource(in.Resource) {
		return service.ErrStudioAssetInvalidRequest
	}
	if err := r.studioAssetDB(); err != nil {
		return err
	}
	// Retry must never overwrite the original prompt/model, even if a caller
	// reuses its attempt identity with altered library metadata.
	result, err := r.db.ExecContext(ctx, `INSERT INTO studio_video_intakes
		(user_id,api_key_id,resource,generation_id,model,prompt,metadata,completed,account_id)
		VALUES($1,$2,$3,$3,$4,$5,$6,$7,$8)
		ON CONFLICT(user_id,api_key_id,resource) DO UPDATE SET
		completed=studio_video_intakes.completed OR EXCLUDED.completed
		WHERE studio_video_intakes.account_id=EXCLUDED.account_id`,
		in.Owner.UserID, in.Owner.APIKeyID, in.Resource, in.Model, in.Prompt, []byte(in.Metadata), in.Completed, in.AccountID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n != 1 {
		return service.ErrVideoAttemptConflict
	}
	return err
}

func (r *usageBillingRepository) GetStudioVideoIntake(ctx context.Context, owner service.StudioAssetOwner, resource string) (*service.StudioVideoIntake, error) {
	in := &service.StudioVideoIntake{Owner: owner, Resource: resource}
	err := r.db.QueryRowContext(ctx, `SELECT model,prompt,metadata,completed,account_id FROM studio_video_intakes
		WHERE user_id=$1 AND api_key_id=$2 AND resource=$3`, owner.UserID, owner.APIKeyID, resource).
		Scan(&in.Model, &in.Prompt, &in.Metadata, &in.Completed, &in.AccountID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, service.ErrStudioAssetNotFound
	}
	return in, err
}

func (r *usageBillingRepository) FindStudioAssetGeneration(ctx context.Context, owner service.StudioAssetOwner, generation string) (*service.StudioAsset, error) {
	return scanStudioAsset(r.db.QueryRowContext(ctx, `SELECT `+studioAssetColumns+` FROM studio_assets
		WHERE user_id=$1 AND api_key_id=$2 AND generation_id=$3 AND output_index=0`,
		owner.UserID, owner.APIKeyID, generation))
}

// Include tombstones: recovery must distinguish a deleted output from a missing
// one, independently of any sibling's index or archive state.
func (r *usageBillingRepository) FindStudioAssetOutput(ctx context.Context, owner service.StudioAssetOwner, generation string, index int) (*service.StudioAsset, error) {
	return scanStudioAsset(r.db.QueryRowContext(ctx, `SELECT `+studioAssetColumns+` FROM studio_assets
		WHERE user_id=$1 AND api_key_id=$2 AND generation_id=$3 AND output_index=$4`,
		owner.UserID, owner.APIKeyID, generation, index))
}

func (r *usageBillingRepository) ListStudioAssetHistory(ctx context.Context, owner service.StudioAssetOwner, limit int) ([]string, error) {
	if limit < 1 || limit > 100 {
		limit = 100
	}
	rows, err := r.db.QueryContext(ctx, `WITH candidates AS (`+studioOwnedHistoryCandidates+`),
		due AS (SELECT c.user_id,c.api_key_id,c.resource FROM candidates c
			LEFT JOIN studio_asset_recovery_checks h ON h.user_id=c.user_id AND h.api_key_id=c.api_key_id
				AND h.resource=c.resource AND h.purpose='history'
			WHERE c.user_id=$1 AND c.api_key_id=$2 AND (h.next_retry_at IS NULL OR h.next_retry_at<=NOW())
			AND NOT EXISTS(SELECT 1 FROM studio_assets a
				WHERE a.user_id=c.user_id AND a.api_key_id=c.api_key_id AND a.generation_id=c.resource)
			ORDER BY h.last_checked_at NULLS FIRST,c.resource LIMIT $3),
		checked AS (INSERT INTO studio_asset_recovery_checks(user_id,api_key_id,resource,purpose,last_checked_at,next_retry_at)
			SELECT user_id,api_key_id,resource,'history',NOW(),NOW()+INTERVAL '5 minutes' FROM due
			ON CONFLICT(user_id,api_key_id,resource,purpose) DO UPDATE
			SET last_checked_at=NOW(),next_retry_at=NOW()+INTERVAL '5 minutes'
			WHERE studio_asset_recovery_checks.next_retry_at<=NOW() RETURNING resource)
		SELECT resource FROM checked`, owner.UserID, owner.APIKeyID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func studioVideoResource(id string) string {
	if strings.HasPrefix(id, "attempt:") || strings.HasPrefix(id, "task:") {
		return id
	}
	if strings.HasPrefix(id, "atlascloud:") {
		return "task:" + id
	}
	return "attempt:" + id
}

func (r *usageBillingRepository) ReconcileStudioAssets(ctx context.Context, owner service.StudioAssetOwner, ids []string) (*service.StudioAssetReconcileResult, error) {
	if !validStudioAssetOwner(owner) || len(ids) > 50 {
		return nil, service.ErrStudioAssetInvalidRequest
	}
	for _, id := range ids {
		if !service.ValidStudioGenerationID(id) {
			return nil, service.ErrStudioAssetInvalidRequest
		}
	}
	if err := r.studioAssetDB(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		var err error
		ids, err = r.ListStudioAssetHistory(ctx, owner, 100)
		if err != nil {
			return nil, err
		}
	}
	result := &service.StudioAssetReconcileResult{}
	seen := make(map[string]bool)
	for _, id := range ids {
		resource := studioVideoResource(id)
		if task, ok := strings.CutPrefix(resource, "task:"); ok {
			rows, err := r.db.QueryContext(ctx, `SELECT id FROM video_attempts
				WHERE user_id=$1 AND api_key_id=$2 AND upstream_task_id=$3 LIMIT 2`, owner.UserID, owner.APIKeyID, task)
			if err != nil {
				return nil, err
			}
			var matches []string
			for rows.Next() {
				var match string
				if err := rows.Scan(&match); err != nil {
					rows.Close()
					return nil, err
				}
				matches = append(matches, match)
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return nil, err
			}
			if len(matches) > 1 {
				result.Unrecoverable++
				continue
			}
			if len(matches) == 1 {
				resource = "attempt:" + matches[0]
			}
		}
		if seen[resource] {
			continue
		}
		seen[resource] = true
		outcome, err := r.reconcileStudioVideo(ctx, owner, resource)
		if err != nil {
			return nil, err
		}
		switch outcome {
		case "queued":
			result.Queued++
		case "saved":
			result.AlreadySaved++
		default:
			result.Unrecoverable++
		}
	}
	return result, nil
}

func (r *usageBillingRepository) reconcileStudioVideo(ctx context.Context, owner service.StudioAssetOwner, resource string) (string, error) {
	if task, native := strings.CutPrefix(resource, "task:"); native && !strings.HasPrefix(task, "atlascloud:") {
		proof, err := r.FindStudioNativeVideoReceipt(ctx, service.VideoAttemptOwner{UserID: owner.UserID, APIKeyID: owner.APIKeyID}, task)
		if errors.Is(err, service.ErrVideoResultNotFound) {
			return "", nil
		}
		if err != nil {
			return "", err
		}
		v, err := r.GetVideoResult(ctx, service.VideoAttemptOwner{UserID: owner.UserID, APIKeyID: owner.APIKeyID}, resource)
		if errors.Is(err, service.ErrVideoResultNotFound) {
			return "", nil
		}
		if err != nil {
			return "", err
		}
		if v.AccountID != proof.AccountID {
			return "", nil
		}
	}
	var deleted bool
	var state string
	err := r.db.QueryRowContext(ctx, `SELECT deleted_at IS NOT NULL,state FROM studio_assets
		WHERE user_id=$1 AND api_key_id=$2 AND generation_id=$3 AND output_index=0`,
		owner.UserID, owner.APIKeyID, resource).Scan(&deleted, &state)
	retry := err == nil && state == "unavailable"
	if err == nil {
		if deleted {
			return "", nil
		}
		if !retry {
			return "saved", nil
		}
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	in, err := r.GetStudioVideoIntake(ctx, owner, resource)
	if errors.Is(err, service.ErrStudioAssetNotFound) {
		in, err = &service.StudioVideoIntake{Owner: owner, Resource: resource, Metadata: []byte(`{}`)}, nil
	}
	if err != nil {
		return "", err
	}
	var generatedAt time.Time
	if id, attempt := strings.CutPrefix(resource, "attempt:"); attempt {
		err = r.db.QueryRowContext(ctx, `SELECT created_at FROM video_attempts
			WHERE user_id=$1 AND api_key_id=$2 AND id=$3 AND state='completed'
			AND financial_state='captured' AND receipt_usage_log_id IS NOT NULL`,
			owner.UserID, owner.APIKeyID, id).Scan(&generatedAt)
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		if err != nil {
			return "", err
		}
	} else if task := strings.TrimPrefix(resource, "task:"); strings.HasPrefix(task, "atlascloud:") {
		// A task ID supplied by the caller is not proof; re-use the existing
		// durable, unambiguous owned receipt lookup even for old archived tasks.
		_, err := r.FindLegacyVideoResult(ctx, service.VideoAttemptOwner{UserID: owner.UserID, APIKeyID: owner.APIKeyID}, task)
		if errors.Is(err, service.ErrVideoResultNotFound) || errors.Is(err, service.ErrVideoAttemptConflict) {
			return "", nil
		}
		if err != nil {
			return "", err
		}
	}
	v, err := r.GetVideoResult(ctx, service.VideoAttemptOwner{UserID: owner.UserID, APIKeyID: owner.APIKeyID}, resource)
	if errors.Is(err, service.ErrVideoResultNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if v.UserID != owner.UserID || v.APIKeyID != owner.APIKeyID || v.Resource != resource {
		return "", nil
	}
	if !service.StudioVideoSourceUsable(v) && !service.StudioVideoPendingNativeContent(v, in) {
		return "", nil
	}
	if !strings.HasPrefix(resource, "attempt:") && !strings.HasPrefix(resource, "task:atlascloud:") &&
		!in.Completed && v.StorageKey == "" {
		return "", nil
	}
	if retry {
		result, err := r.db.ExecContext(ctx, `UPDATE studio_assets SET state='retrying',
			last_error_code='',next_retry_at=NOW(),updated_at=NOW()
			WHERE user_id=$1 AND api_key_id=$2 AND generation_id=$3 AND output_index=0
			AND state='unavailable' AND deleted_at IS NULL`, owner.UserID, owner.APIKeyID, resource)
		if err != nil {
			return "", err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return "", err
		}
		if n == 1 {
			return "queued", nil
		}
		return r.reconcileStudioVideo(ctx, owner, resource)
	}
	if generatedAt.IsZero() {
		err = r.db.QueryRowContext(ctx, `SELECT created_at FROM video_results
			WHERE user_id=$1 AND api_key_id=$2 AND resource=$3`, owner.UserID, owner.APIKeyID, resource).Scan(&generatedAt)
		if err != nil {
			return "", err
		}
	}
	// Insert atomically and do not update conflicts: a concurrent delete is an
	// authoritative tombstone and concurrent reconcilers cannot double-count.
	n, err := r.db.ExecContext(ctx, `INSERT INTO studio_assets
		(id,user_id,api_key_id,kind,generation_id,output_index,video_resource,model,prompt,metadata,generated_at)
		VALUES($1,$2,$3,'video',$4,0,$4,$5,$6,$7,$8)
		ON CONFLICT(user_id,api_key_id,generation_id,output_index) DO NOTHING`,
		"asset_"+strings.ReplaceAll(uuid.NewString(), "-", ""), owner.UserID, owner.APIKeyID,
		resource, in.Model, in.Prompt, []byte(in.Metadata), generatedAt)
	if err != nil {
		return "", err
	}
	affected, err := n.RowsAffected()
	if err != nil {
		return "", err
	}
	if affected == 1 {
		return "queued", nil
	}
	// Re-read the winning row so deleted races aren't counted as saved.
	return r.reconcileStudioVideo(ctx, owner, resource)
}

func (r *usageBillingRepository) ProcessStudioVideoIntakes(ctx context.Context, limit int) error {
	candidates, err := r.listStudioIntakeCandidates(ctx, limit, "index")
	if err != nil {
		return err
	}
	for _, c := range candidates {
		if _, err := r.reconcileStudioVideo(ctx, c.Owner, c.Resource); err != nil {
			return err
		}
	}
	return nil
}

func (r *usageBillingRepository) ListStudioVideoIntakes(ctx context.Context, limit int) ([]*service.StudioVideoIntake, error) {
	return r.listStudioIntakeCandidates(ctx, limit, "lookup")
}

func (r *usageBillingRepository) listStudioIntakeCandidates(ctx context.Context, limit int, purpose string) ([]*service.StudioVideoIntake, error) {
	if limit < 1 || limit > 100 {
		limit = 100
	}
	rows, err := r.db.QueryContext(ctx, `WITH candidates AS (`+studioOwnedHistoryCandidates+`),
		due AS (SELECT i.user_id,i.api_key_id,i.resource FROM studio_video_intakes i
			JOIN candidates c ON c.user_id=i.user_id AND c.api_key_id=i.api_key_id AND c.resource=i.resource
			LEFT JOIN studio_asset_recovery_checks h ON h.user_id=i.user_id AND h.api_key_id=i.api_key_id
				AND h.resource=i.resource AND h.purpose=$2
			WHERE (h.next_retry_at IS NULL OR h.next_retry_at<=NOW())
			AND NOT EXISTS(SELECT 1 FROM studio_assets a WHERE a.user_id=i.user_id
				AND a.api_key_id=i.api_key_id AND a.generation_id=i.resource)
			AND EXISTS(SELECT 1 FROM video_results v WHERE v.user_id=i.user_id
				AND v.api_key_id=i.api_key_id AND v.resource=i.resource)=($2='index')
			ORDER BY h.last_checked_at NULLS FIRST,i.created_at,i.resource LIMIT $1),
		checked AS (INSERT INTO studio_asset_recovery_checks(user_id,api_key_id,resource,purpose,last_checked_at,next_retry_at)
			SELECT user_id,api_key_id,resource,$2,NOW(),NOW()+INTERVAL '5 minutes' FROM due
			ON CONFLICT(user_id,api_key_id,resource,purpose) DO UPDATE
			SET last_checked_at=NOW(),next_retry_at=NOW()+INTERVAL '5 minutes'
			WHERE studio_asset_recovery_checks.next_retry_at<=NOW() RETURNING user_id,api_key_id,resource)
		SELECT i.user_id,i.api_key_id,i.resource,i.model,i.prompt,i.metadata,i.completed,i.account_id
		FROM studio_video_intakes i JOIN checked c
			ON c.user_id=i.user_id AND c.api_key_id=i.api_key_id AND c.resource=i.resource`, limit, purpose)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var intakes []*service.StudioVideoIntake
	for rows.Next() {
		var in service.StudioVideoIntake
		if err := rows.Scan(&in.Owner.UserID, &in.Owner.APIKeyID, &in.Resource, &in.Model, &in.Prompt, &in.Metadata, &in.Completed, &in.AccountID); err != nil {
			return nil, err
		}
		intakes = append(intakes, &in)
	}
	return intakes, rows.Err()
}
