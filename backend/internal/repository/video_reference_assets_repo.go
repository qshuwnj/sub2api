package repository

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// VideoReferenceAssetRepository stores staged reference-media metadata in the
// gateway database. The bytes themselves remain in object storage; this table
// is the ownership, expiry and attempt-binding record.
type videoReferenceAssetRepository struct {
	db *sql.DB
}

var _ service.VideoReferenceAssetRepository = (*videoReferenceAssetRepository)(nil)

func NewVideoReferenceAssetRepository(db *sql.DB) service.VideoReferenceAssetRepository {
	return &videoReferenceAssetRepository{db: db}
}

func (r *videoReferenceAssetRepository) Put(asset *service.VideoReferenceAsset) error {
	if r == nil || r.db == nil {
		return errors.New("reference asset repository unavailable")
	}
	if err := service.ValidateVideoReferenceDescriptor(asset); err != nil {
		return err
	}
	result, err := r.db.Exec(`
		INSERT INTO video_reference_assets (
			id, user_id, api_key_id, account_id, endpoint_family, kind, mime,
			size_bytes, width, height, duration_seconds, frame_rate, storage_key,
			source_url, expires_at, cleanup_state, accepted_attempt_id
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)
		ON CONFLICT (id) DO UPDATE SET
			user_id = EXCLUDED.user_id,
			api_key_id = EXCLUDED.api_key_id,
			account_id = EXCLUDED.account_id,
			endpoint_family = EXCLUDED.endpoint_family,
			kind = EXCLUDED.kind,
			mime = EXCLUDED.mime,
			size_bytes = EXCLUDED.size_bytes,
			width = EXCLUDED.width,
			height = EXCLUDED.height,
			duration_seconds = EXCLUDED.duration_seconds,
			frame_rate = EXCLUDED.frame_rate,
			storage_key = EXCLUDED.storage_key,
			source_url = EXCLUDED.source_url,
			expires_at = EXCLUDED.expires_at,
			cleanup_state = EXCLUDED.cleanup_state,
			accepted_attempt_id = EXCLUDED.accepted_attempt_id
		WHERE video_reference_assets.user_id = EXCLUDED.user_id
			AND video_reference_assets.api_key_id = EXCLUDED.api_key_id
			AND (video_reference_assets.account_id = 0
				OR video_reference_assets.account_id = EXCLUDED.account_id)
			AND video_reference_assets.accepted_attempt_id = ''
	`, asset.ID, asset.UserID, asset.APIKeyID, asset.AccountID, asset.EndpointFamily,
		asset.Kind, asset.MIME, asset.Size, asset.Width, asset.Height, asset.Duration,
		asset.FrameRate, asset.StorageKey, asset.SourceURL, asset.ExpiresAt, asset.State,
		asset.AcceptedAttempt)
	if err != nil {
		return fmt.Errorf("persist reference asset: %w", err)
	}
	// PostgreSQL reports zero rows for an ownership mismatch or an already
	// accepted asset. Do not silently treat that as a successful replacement.
	if count, err := result.RowsAffected(); err == nil && count == 0 {
		return errors.New("reference asset owner mismatch or already accepted")
	}
	return nil
}

func (r *videoReferenceAssetRepository) GetOwned(id string, userID, apiKeyID int64, accountID ...int64) (*service.VideoReferenceAsset, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("reference asset repository unavailable")
	}
	boundAccount := int64(0)
	if len(accountID) > 0 {
		boundAccount = accountID[0]
	}
	row := r.db.QueryRow(`
		SELECT id, user_id, api_key_id, account_id, endpoint_family, kind, mime,
			size_bytes, width, height, duration_seconds, frame_rate, storage_key,
			source_url, expires_at, cleanup_state, accepted_attempt_id
		FROM video_reference_assets
		WHERE id = $1 AND user_id = $2 AND api_key_id = $3
			AND (account_id = 0 OR account_id = $4)
			AND (accepted_attempt_id <> '' OR expires_at > NOW())
	`, id, userID, apiKeyID, boundAccount)
	var asset service.VideoReferenceAsset
	if err := row.Scan(
		&asset.ID, &asset.UserID, &asset.APIKeyID, &asset.AccountID, &asset.EndpointFamily,
		&asset.Kind, &asset.MIME, &asset.Size, &asset.Width, &asset.Height, &asset.Duration,
		&asset.FrameRate, &asset.StorageKey, &asset.SourceURL, &asset.ExpiresAt, &asset.State,
		&asset.AcceptedAttempt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errors.New("reference asset not found")
		}
		return nil, fmt.Errorf("load reference asset: %w", err)
	}
	return &asset, nil
}

func (r *videoReferenceAssetRepository) MarkAccepted(id, attemptID string) error {
	if r == nil || r.db == nil {
		return errors.New("reference asset repository unavailable")
	}
	if strings.TrimSpace(attemptID) == "" {
		return errors.New("accepted attempt ID is required")
	}
	result, err := r.db.Exec(`
		UPDATE video_reference_assets
		SET cleanup_state = $2, accepted_attempt_id = $3
		WHERE id = $1
	`, id, service.VideoReferenceAccepted, attemptID)
	if err != nil {
		return fmt.Errorf("accept reference asset: %w", err)
	}
	if count, err := result.RowsAffected(); err == nil && count == 0 {
		return errors.New("reference asset not found")
	}
	return nil
}

func (r *videoReferenceAssetRepository) MarkAttemptFailed(attemptID string) {
	if r == nil || r.db == nil || strings.TrimSpace(attemptID) == "" {
		return
	}
	_, _ = r.db.Exec(`
		UPDATE video_reference_assets
		SET cleanup_state = 'failed'
		WHERE accepted_attempt_id = $1
	`, attemptID)
}

func (r *videoReferenceAssetRepository) MarkCancelled(id string) error {
	if r == nil || r.db == nil {
		return errors.New("reference asset repository unavailable")
	}
	result, err := r.db.Exec(`
		UPDATE video_reference_assets
		SET cleanup_state = 'cancelled'
		WHERE id = $1 AND accepted_attempt_id = ''
	`, id)
	if err != nil {
		return fmt.Errorf("cancel reference asset: %w", err)
	}
	if count, err := result.RowsAffected(); err == nil && count == 0 {
		return errors.New("reference asset not found or already accepted")
	}
	return nil
}

func (r *videoReferenceAssetRepository) Cleanup() int {
	if r == nil || r.db == nil {
		return 0
	}
	result, err := r.db.Exec(`
		DELETE FROM video_reference_assets
		WHERE cleanup_state IN ('cancelled','failed','expired')
			OR (expires_at <= NOW() AND cleanup_state = 'staged')
	`)
	if err != nil {
		return 0
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0
	}
	return int(count)
}
