package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

var (
	ErrStudioAssetNotFound       = errors.New("studio asset not found")
	ErrStudioAssetUnavailable    = errors.New("studio asset storage is temporarily unavailable")
	ErrStudioAssetInvalidRequest = errors.New("invalid studio asset request")
	ErrStudioAssetInvalidRange   = fmt.Errorf("%w: invalid byte range", ErrStudioAssetInvalidRequest)
)

type StudioAssetOwner struct {
	UserID, APIKeyID int64
}

// StudioAsset is the private durable index record, not the public API DTO.
// Retry sources and worker credentials must never be serialized to a client.
// Collectors must bound SourceBytes against the effective image-storage limit
// before persistence; the index must not impose a different fixed image limit.
type StudioAsset struct {
	ID            string           `json:"id"`
	Kind          string           `json:"type"`
	GenerationID  string           `json:"-"`
	VideoResource string           `json:"-"`
	Owner         StudioAssetOwner `json:"-"`
	OutputIndex   int              `json:"-"`
	Model         string           `json:"model"`
	Prompt        string           `json:"prompt"`
	MIME          string           `json:"mime_type"`
	StorageKey    string           `json:"-"`
	State         string           `json:"status"`
	LastErrorCode string           `json:"-"`
	SourceURL     string           `json:"-"`
	SourceBytes   []byte           `json:"-"`
	Metadata      json.RawMessage  `json:"metadata"`
	SizeBytes     int64            `json:"size_bytes"`
	GeneratedAt   time.Time        `json:"generated_at"`
	CreatedAt     time.Time        `json:"-"`
	UpdatedAt     time.Time        `json:"-"`
	DeletedAt     *time.Time       `json:"-"`
	LeaseToken    string           `json:"-"`
}

type StudioAssetQuery struct {
	Kind, Cursor string
	Limit        int
}

type StudioAssetPage struct {
	Items      []*StudioAsset
	NextCursor string
}

type StudioAssetRepository interface {
	UpsertStudioAsset(context.Context, *StudioAsset) error
	GetStudioAsset(context.Context, StudioAssetOwner, string) (*StudioAsset, error)
	ListStudioAssets(context.Context, StudioAssetOwner, StudioAssetQuery) (*StudioAssetPage, error)
	DeleteStudioAsset(context.Context, StudioAssetOwner, string) error
	ClaimStudioAssets(context.Context, int) ([]*StudioAsset, error)
	CompleteStudioAsset(context.Context, *StudioAsset) error
	// RetryStudioAsset retains private sources. State "unavailable" marks a
	// terminal failure; all other states schedule a bounded retry. The error
	// code must be a sanitized lowercase identifier, never a raw error string.
	RetryStudioAsset(context.Context, *StudioAsset, string) error
	ClaimDeletedStudioAssets(context.Context, int) ([]*StudioAsset, error)
	CompleteStudioAssetCleanup(context.Context, *StudioAsset) error
	ReconcileStudioAssets(context.Context, StudioAssetOwner, []string) (*StudioAssetReconcileResult, error)
}

type StudioAssetReconcileResult struct {
	Queued, AlreadySaved, Unrecoverable int
}

func ValidStudioAssetID(id string) bool {
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

func validStudioOwner(owner StudioAssetOwner) bool { return owner.UserID > 0 && owner.APIKeyID > 0 }

func (s *StudioAssetService) StorageAvailable() bool { _, _, err := s.objects(); return err == nil }

func (s *StudioAssetService) List(ctx context.Context, owner StudioAssetOwner, q StudioAssetQuery) (*StudioAssetPage, error) {
	if !validStudioOwner(owner) || q.Kind != "" && q.Kind != "image" && q.Kind != "video" || q.Limit < 0 {
		return nil, ErrStudioAssetInvalidRequest
	}
	if q.Cursor != "" {
		if len(q.Cursor) > 1024 {
			return nil, ErrStudioAssetInvalidRequest
		}
		data, err := base64.RawURLEncoding.DecodeString(q.Cursor)
		if err != nil {
			return nil, ErrStudioAssetInvalidRequest
		}
		var cursor struct {
			GeneratedAt time.Time `json:"generated_at"`
			ID          string    `json:"id"`
		}
		d := json.NewDecoder(strings.NewReader(string(data)))
		d.DisallowUnknownFields()
		if d.Decode(&cursor) != nil || cursor.GeneratedAt.IsZero() || !ValidStudioAssetID(cursor.ID) || d.Decode(new(any)) != io.EOF {
			return nil, ErrStudioAssetInvalidRequest
		}
	}
	if s == nil || s.repo == nil {
		return nil, ErrStudioAssetUnavailable
	}
	if q.Limit == 0 {
		q.Limit = 24
	}
	if q.Limit > 100 {
		q.Limit = 100
	}
	page, err := s.repo.ListStudioAssets(ctx, owner, q)
	if err != nil {
		return nil, err
	}
	if page == nil {
		return nil, ErrStudioAssetUnavailable
	}
	// Keep the service boundary owner-scoped even for optional repository implementations.
	for _, a := range page.Items {
		if a == nil || a.Owner != owner || a.DeletedAt != nil {
			return nil, ErrStudioAssetUnavailable
		}
	}
	return page, nil
}

func (s *StudioAssetService) Get(ctx context.Context, owner StudioAssetOwner, id string) (*StudioAsset, error) {
	if !validStudioOwner(owner) || !ValidStudioAssetID(id) {
		return nil, ErrStudioAssetInvalidRequest
	}
	if s == nil || s.repo == nil {
		return nil, ErrStudioAssetUnavailable
	}
	a, err := s.repo.GetStudioAsset(ctx, owner, id)
	if err != nil {
		return nil, err
	}
	if a == nil || a.Owner != owner || a.ID != id || a.DeletedAt != nil {
		return nil, ErrStudioAssetNotFound
	}
	return a, nil
}

func (s *StudioAssetService) Delete(ctx context.Context, owner StudioAssetOwner, id string) error {
	if !validStudioOwner(owner) || !ValidStudioAssetID(id) {
		return ErrStudioAssetInvalidRequest
	}
	if s == nil || s.repo == nil {
		return ErrStudioAssetUnavailable
	}
	err := s.repo.DeleteStudioAsset(ctx, owner, id)
	if errors.Is(err, ErrStudioAssetNotFound) {
		return nil
	}
	return err
}

func (s *StudioAssetService) Reconcile(ctx context.Context, owner StudioAssetOwner, ids []string) (*StudioAssetReconcileResult, error) {
	if !validStudioOwner(owner) || len(ids) > 50 {
		return nil, ErrStudioAssetInvalidRequest
	}
	for _, id := range ids {
		if !ValidStudioGenerationID(id) {
			return nil, ErrStudioAssetInvalidRequest
		}
	}
	if s == nil || s.repo == nil {
		return nil, ErrStudioAssetUnavailable
	}
	result, err := s.reconcileOwnedHistory(ctx, owner, ids)
	if err == nil && result == nil {
		return nil, ErrStudioAssetUnavailable
	}
	return result, err
}
