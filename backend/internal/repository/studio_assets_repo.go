package repository

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
)

var _ service.StudioAssetRepository = (*usageBillingRepository)(nil)

const studioAssetColumns = `id, user_id, api_key_id, kind, generation_id, output_index,
	video_resource, model, prompt, mime, storage_key, state, last_error_code,
	source_url, source_bytes, metadata, size_bytes, generated_at, created_at, updated_at, deleted_at, lease_token`

func validStudioAssetOwner(owner service.StudioAssetOwner) bool {
	return owner.UserID > 0 && owner.APIKeyID > 0
}

func validStudioAssetID(id string) bool {
	if len(id) != 38 || !strings.HasPrefix(id, "asset_") {
		return false
	}
	for _, c := range id[6:] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func (r *usageBillingRepository) studioAssetDB() error {
	if r == nil || r.db == nil {
		return service.ErrStudioAssetUnavailable
	}
	return nil
}

func scanStudioAsset(row interface{ Scan(...any) error }) (*service.StudioAsset, error) {
	var a service.StudioAsset
	err := row.Scan(&a.ID, &a.Owner.UserID, &a.Owner.APIKeyID, &a.Kind, &a.GenerationID, &a.OutputIndex,
		&a.VideoResource, &a.Model, &a.Prompt, &a.MIME, &a.StorageKey, &a.State, &a.LastErrorCode,
		&a.SourceURL, &a.SourceBytes, &a.Metadata, &a.SizeBytes, &a.GeneratedAt, &a.CreatedAt,
		&a.UpdatedAt, &a.DeletedAt, &a.LeaseToken)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, service.ErrStudioAssetNotFound
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func (r *usageBillingRepository) UpsertStudioAsset(ctx context.Context, a *service.StudioAsset) error {
	if a == nil || !validStudioAssetOwner(a.Owner) || (a.Kind != "image" && a.Kind != "video") ||
		a.GenerationID == "" || len(a.GenerationID) > 256 || a.OutputIndex < 0 || a.GeneratedAt.IsZero() ||
		len(a.VideoResource) > 160 || len(a.MIME) > 127 || len(a.SourceURL) > 16384 ||
		a.DeletedAt != nil ||
		a.State != "" && a.State != "pending" && a.State != "retrying" && a.State != "unavailable" {
		return service.ErrStudioAssetInvalidRequest
	}
	metadata := a.Metadata
	if len(metadata) == 0 {
		metadata = json.RawMessage(`{}`)
	}
	if !json.Valid(metadata) || !strings.HasPrefix(strings.TrimSpace(string(metadata)), "{") {
		return service.ErrStudioAssetInvalidRequest
	}
	if err := r.studioAssetDB(); err != nil {
		return err
	}
	id := a.ID
	if id == "" {
		id = "asset_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	} else if !validStudioAssetID(id) {
		return service.ErrStudioAssetInvalidRequest
	}
	state := a.State
	if state == "" {
		state = "pending"
	}
	var sourceBytes any
	if len(a.SourceBytes) > 0 {
		sourceBytes = a.SourceBytes
	}
	stored, err := scanStudioAsset(r.db.QueryRowContext(ctx, `
		INSERT INTO studio_assets (id, user_id, api_key_id, kind, generation_id, output_index,
			video_resource, model, prompt, mime, state, source_url, source_bytes, metadata, generated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
		ON CONFLICT (user_id, api_key_id, generation_id, output_index) DO UPDATE SET
			source_url = CASE WHEN studio_assets.state = 'ready' OR studio_assets.lease_expires_at > NOW()
				THEN studio_assets.source_url ELSE COALESCE(NULLIF(EXCLUDED.source_url, ''), studio_assets.source_url) END,
			source_bytes = CASE WHEN studio_assets.state = 'ready' OR studio_assets.lease_expires_at > NOW()
				THEN studio_assets.source_bytes ELSE COALESCE(EXCLUDED.source_bytes, studio_assets.source_bytes) END,
			updated_at = NOW()
		WHERE studio_assets.deleted_at IS NULL
		RETURNING `+studioAssetColumns,
		id, a.Owner.UserID, a.Owner.APIKeyID, a.Kind, a.GenerationID, a.OutputIndex, a.VideoResource,
		a.Model, a.Prompt, a.MIME, state, a.SourceURL, sourceBytes, []byte(metadata), a.GeneratedAt))
	if err != nil {
		return err
	}
	*a = *stored
	return nil
}

func (r *usageBillingRepository) GetStudioAsset(ctx context.Context, owner service.StudioAssetOwner, id string) (*service.StudioAsset, error) {
	if !validStudioAssetOwner(owner) || !validStudioAssetID(id) {
		return nil, service.ErrStudioAssetInvalidRequest
	}
	if err := r.studioAssetDB(); err != nil {
		return nil, err
	}
	return scanStudioAsset(r.db.QueryRowContext(ctx, `SELECT `+studioAssetColumns+`
		FROM studio_assets WHERE user_id = $1 AND api_key_id = $2 AND id = $3 AND deleted_at IS NULL`,
		owner.UserID, owner.APIKeyID, id))
}

type studioAssetCursor struct {
	GeneratedAt time.Time `json:"generated_at"`
	ID          string    `json:"id"`
}

func decodeStudioAssetCursor(raw string) (*studioAssetCursor, error) {
	if len(raw) > 1024 {
		return nil, service.ErrStudioAssetInvalidRequest
	}
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil, service.ErrStudioAssetInvalidRequest
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	var cursor studioAssetCursor
	if err := decoder.Decode(&cursor); err != nil || cursor.GeneratedAt.IsZero() || !validStudioAssetID(cursor.ID) {
		return nil, service.ErrStudioAssetInvalidRequest
	}
	if decoder.Decode(new(any)) != io.EOF {
		return nil, service.ErrStudioAssetInvalidRequest
	}
	return &cursor, nil
}

func (r *usageBillingRepository) ListStudioAssets(ctx context.Context, owner service.StudioAssetOwner, q service.StudioAssetQuery) (*service.StudioAssetPage, error) {
	if !validStudioAssetOwner(owner) || q.Kind != "" && q.Kind != "image" && q.Kind != "video" {
		return nil, service.ErrStudioAssetInvalidRequest
	}
	limit := q.Limit
	if limit < 1 {
		limit = 24
	} else if limit > 100 {
		limit = 100
	}
	// Listing never loads potentially large private retry payloads into the grid.
	columns := strings.Replace(studioAssetColumns, "source_url, source_bytes", "'' AS source_url, NULL::bytea AS source_bytes", 1)
	query := `SELECT ` + columns + ` FROM studio_assets WHERE user_id = $1 AND api_key_id = $2 AND deleted_at IS NULL`
	args := []any{owner.UserID, owner.APIKeyID}
	if q.Kind != "" {
		args = append(args, q.Kind)
		query += fmt.Sprintf(" AND kind = $%d", len(args))
	}
	if q.Cursor != "" {
		cursor, err := decodeStudioAssetCursor(q.Cursor)
		if err != nil {
			return nil, err
		}
		args = append(args, cursor.GeneratedAt, cursor.ID)
		query += fmt.Sprintf(" AND (generated_at, id) < ($%d, $%d)", len(args)-1, len(args))
	}
	args = append(args, limit+1)
	query += fmt.Sprintf(" ORDER BY generated_at DESC, id DESC LIMIT $%d", len(args))
	if err := r.studioAssetDB(); err != nil {
		return nil, err
	}
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	page := &service.StudioAssetPage{Items: make([]*service.StudioAsset, 0, limit)}
	for rows.Next() {
		a, err := scanStudioAsset(rows)
		if err != nil {
			return nil, err
		}
		page.Items = append(page.Items, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		last := page.Items[limit-1]
		data, err := json.Marshal(studioAssetCursor{GeneratedAt: last.GeneratedAt.UTC(), ID: last.ID})
		if err != nil {
			return nil, err
		}
		page.NextCursor = base64.RawURLEncoding.EncodeToString(data)
	}
	return page, nil
}

func (r *usageBillingRepository) DeleteStudioAsset(ctx context.Context, owner service.StudioAssetOwner, id string) error {
	if !validStudioAssetOwner(owner) || !validStudioAssetID(id) {
		return service.ErrStudioAssetInvalidRequest
	}
	if err := r.studioAssetDB(); err != nil {
		return err
	}
	_, err := r.db.ExecContext(ctx, `UPDATE studio_assets SET deleted_at = NOW(), source_url = '', source_bytes = NULL, lease_token = '', lease_expires_at = NULL,
		updated_at = NOW() WHERE user_id = $1 AND api_key_id = $2 AND id = $3 AND deleted_at IS NULL`, owner.UserID, owner.APIKeyID, id)
	return err
}

func (r *usageBillingRepository) claimStudioAssets(ctx context.Context, limit int, deleted bool) ([]*service.StudioAsset, error) {
	if err := r.studioAssetDB(); err != nil {
		return nil, err
	}
	if limit < 1 {
		limit = 1
	} else if limit > 100 {
		limit = 100
	}
	eligible := `state IN ('pending', 'retrying') AND deleted_at IS NULL AND next_retry_at <= NOW()`
	order := `next_retry_at, id`
	if deleted {
		eligible = `deleted_at IS NOT NULL AND storage_key <> '' AND next_retry_at <= NOW()`
		order = `deleted_at, id`
	}
	rows, err := r.db.QueryContext(ctx, `UPDATE studio_assets SET lease_token = $1,
		lease_expires_at = NOW() + INTERVAL '4 minutes', attempts = attempts + 1, updated_at = NOW()
		WHERE id IN (SELECT id FROM studio_assets WHERE `+eligible+`
			AND (lease_expires_at IS NULL OR lease_expires_at <= NOW())
			ORDER BY `+order+` LIMIT $2 FOR UPDATE SKIP LOCKED)
		RETURNING `+studioAssetColumns, uuid.NewString(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]*service.StudioAsset, 0)
	for rows.Next() {
		a, err := scanStudioAsset(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, a)
	}
	return items, rows.Err()
}

func (r *usageBillingRepository) ClaimStudioAssets(ctx context.Context, limit int) ([]*service.StudioAsset, error) {
	return r.claimStudioAssets(ctx, limit, false)
}

func (r *usageBillingRepository) ClaimDeletedStudioAssets(ctx context.Context, limit int) ([]*service.StudioAsset, error) {
	return r.claimStudioAssets(ctx, limit, true)
}

func validStudioAssetLease(a *service.StudioAsset) bool {
	return a != nil && validStudioAssetOwner(a.Owner) && validStudioAssetID(a.ID) && a.LeaseToken != ""
}

func studioAssetLeaseResult(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n != 1 {
		return service.ErrStudioAssetUnavailable
	}
	return err
}

func (r *usageBillingRepository) CompleteStudioAsset(ctx context.Context, a *service.StudioAsset) error {
	if !validStudioAssetLease(a) || a.StorageKey == "" || a.SizeBytes <= 0 || len(a.MIME) > 127 {
		return service.ErrStudioAssetInvalidRequest
	}
	if err := r.studioAssetDB(); err != nil {
		return err
	}
	return studioAssetLeaseResult(r.db.ExecContext(ctx, `UPDATE studio_assets SET storage_key = $5,
		mime = $6, size_bytes = $7, state = 'ready', last_error_code = '',
		source_url = '', source_bytes = NULL, lease_token = '', lease_expires_at = NULL, updated_at = NOW()
		WHERE user_id = $1 AND api_key_id = $2 AND id = $3 AND lease_token = $4 AND lease_expires_at > NOW() AND deleted_at IS NULL AND state IN ('pending', 'retrying')`,
		a.Owner.UserID, a.Owner.APIKeyID, a.ID, a.LeaseToken, a.StorageKey, a.MIME, a.SizeBytes))
}

func (r *usageBillingRepository) RetryStudioAsset(ctx context.Context, a *service.StudioAsset, code string) error {
	if !validStudioAssetLease(a) || code == "" || len(code) > 64 {
		return service.ErrStudioAssetInvalidRequest
	}
	for _, c := range code {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_') {
			return service.ErrStudioAssetInvalidRequest
		}
	}
	if err := r.studioAssetDB(); err != nil {
		return err
	}
	state := "retrying"
	if a.State == "unavailable" {
		state = "unavailable"
	}
	return studioAssetLeaseResult(r.db.ExecContext(ctx, `UPDATE studio_assets SET state = '`+state+`', last_error_code = $5,
		lease_token = '', lease_expires_at = NULL, next_retry_at = NOW() + LEAST(attempts, 30) * INTERVAL '1 minute', updated_at = NOW()
		WHERE user_id = $1 AND api_key_id = $2 AND id = $3 AND lease_token = $4 AND lease_expires_at > NOW() AND deleted_at IS NULL AND state IN ('pending', 'retrying')`,
		a.Owner.UserID, a.Owner.APIKeyID, a.ID, a.LeaseToken, code))
}

func (r *usageBillingRepository) CompleteStudioAssetCleanup(ctx context.Context, a *service.StudioAsset) error {
	if !validStudioAssetLease(a) || a.StorageKey == "" {
		return service.ErrStudioAssetInvalidRequest
	}
	if err := r.studioAssetDB(); err != nil {
		return err
	}
	return studioAssetLeaseResult(r.db.ExecContext(ctx, `UPDATE studio_assets SET storage_key = '', state = 'unavailable',
		lease_token = '', lease_expires_at = NULL, updated_at = NOW()
		WHERE user_id = $1 AND api_key_id = $2 AND id = $3 AND lease_token = $4 AND lease_expires_at > NOW() AND deleted_at IS NOT NULL AND storage_key = $5`,
		a.Owner.UserID, a.Owner.APIKeyID, a.ID, a.LeaseToken, a.StorageKey))
}

// RecordStudioAssetCleanup handles an upload racing deletion, without ever
// changing a live row or claiming ownership of a shared video_results object.
func (r *usageBillingRepository) RecordStudioAssetCleanup(ctx context.Context, a *service.StudioAsset) error {
	if a == nil || !validStudioAssetOwner(a.Owner) || !validStudioAssetID(a.ID) || a.Kind != "image" ||
		!strings.HasPrefix(a.StorageKey, fmt.Sprintf("studio-results/%d/%d/", a.Owner.UserID, a.Owner.APIKeyID)) ||
		path.Clean(a.StorageKey) != a.StorageKey || strings.Contains(a.StorageKey, `\`) {
		return service.ErrStudioAssetInvalidRequest
	}
	if err := r.studioAssetDB(); err != nil {
		return err
	}
	// Invalidate any cleaner that deleted the key before this late upload
	// recreated it; its old completion must not erase the fresh cleanup intent.
	_, err := r.db.ExecContext(ctx, `UPDATE studio_assets SET storage_key = $4, lease_token = '', lease_expires_at = NULL, next_retry_at = NOW(), updated_at = NOW()
		WHERE user_id = $1 AND api_key_id = $2 AND id = $3 AND deleted_at IS NOT NULL AND (storage_key = '' OR storage_key = $4)`,
		a.Owner.UserID, a.Owner.APIKeyID, a.ID, a.StorageKey)
	return err
}

// Owned discovery is implemented separately; never acknowledge missing work.
func (r *usageBillingRepository) ReconcileStudioAssets(_ context.Context, owner service.StudioAssetOwner, _ []string) (*service.StudioAssetReconcileResult, error) {
	if !validStudioAssetOwner(owner) {
		return nil, service.ErrStudioAssetInvalidRequest
	}
	return nil, service.ErrStudioAssetUnavailable
}
