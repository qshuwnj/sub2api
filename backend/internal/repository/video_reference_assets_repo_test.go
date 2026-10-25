package repository

import (
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

func TestVideoReferenceAssetRepositoryPersistsOwnershipAndExpiry(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	repo := NewVideoReferenceAssetRepository(db)
	expiresAt := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	asset := &service.VideoReferenceAsset{
		ID: "asset-1", UserID: 7, APIKeyID: 8, AccountID: 9,
		EndpointFamily: "2.5", Kind: service.VideoReferenceImage,
		MIME: "image/png", Size: 1024, Width: 640, Height: 640,
		StorageKey: "video-reference/asset-1", SourceURL: "https://cdn.example/asset-1",
		ExpiresAt: expiresAt, State: service.VideoReferenceStaged,
	}

	mock.ExpectExec(regexp.QuoteMeta(`
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
	`)).WithArgs(
		asset.ID, asset.UserID, asset.APIKeyID, asset.AccountID, asset.EndpointFamily,
		asset.Kind, asset.MIME, asset.Size, asset.Width, asset.Height, asset.Duration,
		asset.FrameRate, asset.StorageKey, asset.SourceURL, asset.ExpiresAt, asset.State,
		asset.AcceptedAttempt,
	).WillReturnResult(sqlmock.NewResult(0, 1))
	if err := repo.Put(asset); err != nil {
		t.Fatal(err)
	}

	rows := sqlmock.NewRows([]string{
		"id", "user_id", "api_key_id", "account_id", "endpoint_family", "kind", "mime",
		"size_bytes", "width", "height", "duration_seconds", "frame_rate", "storage_key",
		"source_url", "expires_at", "cleanup_state", "accepted_attempt_id",
	}).AddRow(
		asset.ID, asset.UserID, asset.APIKeyID, asset.AccountID, asset.EndpointFamily, asset.Kind,
		asset.MIME, asset.Size, asset.Width, asset.Height, asset.Duration, asset.FrameRate,
		asset.StorageKey, asset.SourceURL, asset.ExpiresAt, asset.State, asset.AcceptedAttempt,
	)
	mock.ExpectQuery(regexp.QuoteMeta(`
		SELECT id, user_id, api_key_id, account_id, endpoint_family, kind, mime,
			size_bytes, width, height, duration_seconds, frame_rate, storage_key,
			source_url, expires_at, cleanup_state, accepted_attempt_id
		FROM video_reference_assets
		WHERE id = $1 AND user_id = $2 AND api_key_id = $3
			AND (account_id = 0 OR account_id = $4)
			AND (accepted_attempt_id <> '' OR expires_at > NOW())
	`)).WithArgs(asset.ID, asset.UserID, asset.APIKeyID, asset.AccountID).WillReturnRows(rows)
	got, err := repo.GetOwned(asset.ID, asset.UserID, asset.APIKeyID, asset.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != asset.ID || got.StorageKey != asset.StorageKey || !got.ExpiresAt.Equal(asset.ExpiresAt) {
		t.Fatalf("unexpected asset: %+v", got)
	}

	mock.ExpectExec(regexp.QuoteMeta(`
		UPDATE video_reference_assets
		SET cleanup_state = $2, accepted_attempt_id = $3
		WHERE id = $1
	`)).WithArgs(asset.ID, service.VideoReferenceAccepted, "attempt-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := repo.MarkAccepted(asset.ID, "attempt-1"); err != nil {
		t.Fatal(err)
	}

	mock.ExpectExec(regexp.QuoteMeta(`
		DELETE FROM video_reference_assets
		WHERE cleanup_state IN ('cancelled','failed','expired')
			OR (expires_at <= NOW() AND cleanup_state = 'staged')
	`)).WillReturnResult(sqlmock.NewResult(0, 2))
	if removed := repo.Cleanup(); removed != 2 {
		t.Fatalf("removed = %d", removed)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestVideoReferenceAssetRepositoryRejectsInvalidInputBeforeSQL(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo := NewVideoReferenceAssetRepository(db)
	if err := repo.Put(&service.VideoReferenceAsset{ID: "invalid"}); err == nil {
		t.Fatal("expected validation error")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
