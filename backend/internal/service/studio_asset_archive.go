package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type StudioImageOutput struct {
	URL   string
	Bytes []byte
	MIME  string
}

// Private reads address the object directly, never this store's signed URL.
type StudioAssetObjectStore interface {
	OpenStudioAsset(context.Context, string, string) (*VideoResultContent, error)
	StudioAssetURL(context.Context, string, time.Duration) (string, error)
}

// A late upload can only enqueue cleanup on an owned tombstone. It must not
// delete a deterministic key that another live lease may have completed.
type studioAssetCleanupRecorder interface {
	RecordStudioAssetCleanup(context.Context, *StudioAsset) error
}

type studioAssetVideoReader interface {
	GetVideoResult(context.Context, VideoAttemptOwner, string) (*VideoResult, error)
}

type StudioAssetService struct {
	repo         StudioAssetRepository
	resolve      ImageStorageResolver
	imageTasks   *ImageTaskService
	recoverVideo func(context.Context, StudioAssetOwner, string) error
}

func NewStudioAssetService(repo StudioAssetRepository, resolve ImageStorageResolver) *StudioAssetService {
	return &StudioAssetService{repo: repo, resolve: resolve}
}

func (s *StudioAssetService) objects() (*ImageResultUploader, StudioAssetObjectStore, error) {
	if s == nil || s.resolve == nil {
		return nil, nil, ErrStudioAssetUnavailable
	}
	u, enabled := s.resolve()
	if u == nil {
		return nil, nil, ErrStudioAssetUnavailable
	}
	store, ok := u.storage.(StudioAssetObjectStore)
	if !enabled || !ok {
		return u, nil, ErrStudioAssetUnavailable
	}
	return u, store, nil
}

func studioImageLimit(u *ImageResultUploader) int64 {
	if u != nil && u.maxDownloadBytes > 0 {
		return u.maxDownloadBytes
	}
	return defaultImageMaxDownloadBytes
}

func studioImageMIME(declared string, data []byte) (string, bool) {
	declared = strings.ToLower(strings.TrimSpace(strings.Split(declared, ";")[0]))
	if declared == "image/jpg" {
		declared = "image/jpeg"
	}
	if declared != "" && declared != "image/png" && declared != "image/jpeg" && declared != "image/webp" && declared != "image/gif" {
		return "", false
	}
	if data == nil {
		return declared, true
	}
	detected := detectedImageContentType(data)
	if detected == "" || declared != "" && declared != detected {
		return "", false
	}
	return detected, true
}

func (s *StudioAssetService) CollectImage(ctx context.Context, owner StudioAssetOwner, generationID string, index int, model, prompt string, metadata json.RawMessage, source StudioImageOutput) error {
	_, err := s.collectImageAt(ctx, owner, generationID, index, model, prompt, metadata, source, time.Now().UTC())
	return err
}

// The boolean distinguishes a durable queued source from the independent
// storage warning, so history counters never label a persisted source lost.
func (s *StudioAssetService) collectImageAt(ctx context.Context, owner StudioAssetOwner, generationID string, index int, model, prompt string, metadata json.RawMessage, source StudioImageOutput, generatedAt time.Time) (bool, error) {
	if s == nil || s.repo == nil {
		return false, ErrStudioAssetUnavailable
	}
	u, _, storageErr := s.objects()
	if len(source.Bytes) > 0 && u == nil {
		// A failed configuration lookup is not evidence that the default limit
		// applies. Fail closed instead of persisting potentially oversized bytes.
		return false, ErrStudioAssetUnavailable
	}
	if owner.UserID <= 0 || owner.APIKeyID <= 0 || generationID == "" || len(generationID) > 256 || index < 0 ||
		len(source.Bytes) == 0 && !validVideoResultURL(source.URL) || int64(len(source.Bytes)) > studioImageLimit(u) {
		return false, ErrStudioAssetInvalidRequest
	}
	if len(metadata) == 0 {
		metadata = json.RawMessage(`{}`)
	}
	if !json.Valid(metadata) || !strings.HasPrefix(strings.TrimSpace(string(metadata)), "{") {
		return false, ErrStudioAssetInvalidRequest
	}
	mime, valid := studioImageMIME(source.MIME, source.Bytes)
	if !valid {
		return false, ErrStudioAssetInvalidRequest
	}
	a := &StudioAsset{Owner: owner, Kind: "image", GenerationID: generationID, OutputIndex: index,
		Model: model, Prompt: prompt, Metadata: metadata, MIME: mime, SourceURL: source.URL,
		SourceBytes: source.Bytes, State: "pending", GeneratedAt: generatedAt}
	if len(source.Bytes) > 0 {
		a.SourceURL = ""
	}
	// Bound and validate bytes before persistence, including when storage is down.
	if err := s.repo.UpsertStudioAsset(ctx, a); err != nil {
		return false, ErrStudioAssetUnavailable
	}
	return true, storageErr
}

func studioAssetImageKey(a *StudioAsset) string {
	digest := sha256.Sum256([]byte(a.GenerationID))
	return fmt.Sprintf("studio-results/%d/%d/%x/%d", a.Owner.UserID, a.Owner.APIKeyID, digest, a.OutputIndex)
}

func validStudioAssetRange(value string) bool {
	if value == "" {
		return true
	}
	raw, ok := strings.CutPrefix(value, "bytes=")
	if !ok || strings.ContainsAny(raw, ", \t\r\n+") {
		return false
	}
	start, end, ok := strings.Cut(raw, "-")
	if !ok || start == "" && end == "" {
		return false
	}
	parse := func(s string) (int64, bool) { n, err := strconv.ParseInt(s, 10, 64); return n, err == nil && n >= 0 }
	if start == "" {
		n, valid := parse(end)
		return valid && n > 0
	}
	n, valid := parse(start)
	if !valid || end == "" {
		return valid
	}
	m, valid := parse(end)
	return valid && m >= n
}

func (s *StudioAssetService) Open(ctx context.Context, owner StudioAssetOwner, id, byteRange string) (*VideoResultContent, error) {
	if !validStudioAssetRange(byteRange) {
		return nil, ErrStudioAssetInvalidRange
	}
	if s == nil || s.repo == nil {
		return nil, ErrStudioAssetUnavailable
	}
	a, err := s.Get(ctx, owner, id)
	if errors.Is(err, ErrStudioAssetInvalidRequest) {
		return nil, err
	}
	if errors.Is(err, ErrStudioAssetNotFound) {
		return nil, ErrStudioAssetNotFound
	}
	if err != nil || a == nil || a.Owner != owner || a.DeletedAt != nil || a.State != "ready" || a.StorageKey == "" {
		return nil, ErrStudioAssetUnavailable
	}
	if byteRange != "" && a.SizeBytes > 0 {
		raw := strings.TrimPrefix(byteRange, "bytes=")
		start, _, _ := strings.Cut(raw, "-")
		if start != "" {
			n, _ := strconv.ParseInt(start, 10, 64)
			if n >= a.SizeBytes {
				return nil, ErrStudioAssetInvalidRange
			}
		}
	}
	_, store, err := s.objects()
	if err != nil {
		return nil, ErrStudioAssetUnavailable
	}
	if a.Kind == "video" {
		v, _, _ := s.videoReference(ctx, a)
		if v == nil || v.StorageKey != a.StorageKey {
			return nil, ErrStudioAssetUnavailable
		}
	} else if a.Kind != "image" || a.StorageKey != studioAssetImageKey(a) {
		return nil, ErrStudioAssetUnavailable
	}
	content, err := store.OpenStudioAsset(ctx, a.StorageKey, byteRange)
	if err != nil || content == nil {
		return nil, ErrStudioAssetUnavailable
	}
	return content, nil
}

func (s *StudioAssetService) ProcessPending(ctx context.Context) (result error) {
	if s == nil || s.repo == nil {
		return ErrStudioAssetUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	// Existing images/video references get archive progress before any supplier
	// GET. Intake work has its own bounded budget and cannot consume this lease.
	defer func() {
		if ctx.Err() != nil {
			return
		}
		recoveryCtx, recoveryCancel := context.WithTimeout(ctx, 5*time.Second)
		defer recoveryCancel()
		if err := s.processVideoIntakes(recoveryCtx); err != nil && result == nil {
			result = err
		}
	}()
	items, err := s.repo.ClaimStudioAssets(ctx, 1)
	if err != nil {
		return ErrStudioAssetUnavailable
	}
	if len(items) == 0 {
		return nil
	}
	a := items[0]
	if a == nil {
		return ErrStudioAssetUnavailable
	}
	u, _, storageErr := s.objects()
	code, terminal := "storage_unavailable", false
	if storageErr == nil {
		if a.Kind == "video" {
			var v *VideoResult
			v, code, terminal = s.videoReference(ctx, a)
			if v != nil {
				a.StorageKey, a.MIME, a.SizeBytes = v.StorageKey, v.MIME, v.Size
			}
		} else if a.Kind == "image" {
			code, terminal = s.archiveImage(ctx, u, a)
		} else {
			code, terminal = "invalid_kind", true
		}
		if code == "" {
			if err := s.repo.CompleteStudioAsset(ctx, a); err == nil {
				return nil
			}
			s.recordLateImageCleanup(ctx, a)
			return ErrStudioAssetUnavailable
		}
	}
	s.recordLateImageCleanup(ctx, a)
	if terminal {
		a.State = "unavailable"
	}
	retryCtx, retryCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer retryCancel()
	_ = s.repo.RetryStudioAsset(retryCtx, a, code)
	return ErrStudioAssetUnavailable
}

func (s *StudioAssetService) recordLateImageCleanup(ctx context.Context, a *StudioAsset) {
	if a.Kind != "image" || a.StorageKey == "" {
		return
	}
	if recorder, ok := s.repo.(studioAssetCleanupRecorder); ok {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = recorder.RecordStudioAssetCleanup(cleanupCtx, a)
	}
}

func (s *StudioAssetService) videoReference(ctx context.Context, a *StudioAsset) (*VideoResult, string, bool) {
	r, ok := s.repo.(studioAssetVideoReader)
	if !ok {
		return nil, "video_pending", false
	}
	v, err := r.GetVideoResult(ctx, VideoAttemptOwner{UserID: a.Owner.UserID, APIKeyID: a.Owner.APIKeyID}, a.VideoResource)
	if errors.Is(err, ErrVideoResultNotFound) {
		return nil, "video_pending", false
	}
	if err != nil {
		return nil, "video_unavailable", false
	}
	if v == nil || v.UserID != a.Owner.UserID || v.APIKeyID != a.Owner.APIKeyID || v.Resource != a.VideoResource {
		return nil, "video_not_found", true
	}
	if err := requireStudioNativeVideoReceipt(ctx, s.repo, v); err != nil {
		return nil, "video_receipt_unavailable", false
	}
	if v.StorageKey == "" {
		return nil, "video_pending", false
	}
	if v.Size <= 0 || v.Size > 300<<20 || v.MIME != "video/mp4" && v.MIME != "video/webm" {
		return nil, "invalid_video", true
	}
	return v, "", false
}

func (s *StudioAssetService) archiveImage(ctx context.Context, u *ImageResultUploader, a *StudioAsset) (string, bool) {
	data := a.SourceBytes
	if len(data) == 0 {
		if !validVideoResultURL(a.SourceURL) {
			return "invalid_source", true
		}
		client := *u.httpClient
		priorRedirect := client.CheckRedirect
		client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 || !validVideoResultURL(req.URL.String()) {
				return ErrStudioAssetUnavailable
			}
			if priorRedirect != nil {
				return priorRedirect(req, via)
			}
			return nil
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.SourceURL, nil)
		if err != nil {
			return "invalid_source", true
		}
		resp, err := client.Do(req)
		if err != nil {
			return "source_unavailable", false
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return "source_unavailable", false
		}
		limit := studioImageLimit(u)
		if resp.ContentLength > limit {
			return "image_too_large", true
		}
		data, err = io.ReadAll(io.LimitReader(resp.Body, limit+1))
		if err != nil {
			return "source_unavailable", false
		}
	}
	if int64(len(data)) > studioImageLimit(u) {
		return "image_too_large", true
	}
	mime, valid := studioImageMIME(a.MIME, data)
	if !valid || len(data) == 0 {
		return "invalid_image", true
	}
	key := studioAssetImageKey(a)
	// Save can upload successfully before its URL-signing step fails. Retain
	// this attempted key so a concurrent tombstone still owns durable cleanup.
	a.StorageKey = key
	if _, err := u.Save(ctx, key, mime, data); err != nil {
		return "storage_unavailable", false
	}
	a.StorageKey, a.MIME, a.SizeBytes = key, mime, int64(len(data))
	return "", false
}

func (s *StudioAssetService) ProcessCleanup(ctx context.Context) error {
	if s == nil || s.repo == nil {
		return ErrStudioAssetUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	items, err := s.repo.ClaimDeletedStudioAssets(ctx, 1)
	if err != nil {
		return ErrStudioAssetUnavailable
	}
	if len(items) == 0 {
		return nil
	}
	a := items[0]
	if a == nil || a.DeletedAt == nil {
		return ErrStudioAssetUnavailable
	}
	if a.Kind == "image" && a.StorageKey == "" {
		// A crash or database outage may prevent the late upload from recording
		// its key. Durable tombstone identity still owns deterministic cleanup.
		a.StorageKey = studioAssetImageKey(a)
	}
	if a.Kind == "image" && a.StorageKey == studioAssetImageKey(a) {
		u, _, err := s.objects()
		if err != nil || u.Delete(ctx, a.StorageKey) != nil {
			// Keep the tombstone key and lease; the expired lease is a durable retry.
			return ErrStudioAssetUnavailable
		}
	}
	// Video objects belong to video_results, not the library. Unknown keys are
	// also never deleted; release only this index's reference to shared data.
	if err := s.repo.CompleteStudioAssetCleanup(ctx, a); err != nil {
		return ErrStudioAssetUnavailable
	}
	return nil
}
