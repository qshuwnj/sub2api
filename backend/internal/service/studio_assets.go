package service

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

var (
	ErrStudioAssetNotFound       = errors.New("studio asset not found")
	ErrStudioAssetUnavailable    = errors.New("studio asset storage is temporarily unavailable")
	ErrStudioAssetInvalidRequest = errors.New("invalid studio asset request")
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
