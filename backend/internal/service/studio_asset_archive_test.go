package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type studioArchiveMemory struct {
	StudioAssetRepository
	asset             *StudioAsset
	video             *VideoResult
	claimLimit        int
	deadline          time.Duration
	beforeComplete    func()
	retryCode         string
	cleanupRecorded   bool
	failCleanupRecord bool
}

func cloneStudioArchive(a *StudioAsset) *StudioAsset {
	if a == nil {
		return nil
	}
	c := *a
	c.SourceBytes = bytes.Clone(a.SourceBytes)
	return &c
}
func (r *studioArchiveMemory) UpsertStudioAsset(_ context.Context, a *StudioAsset) error {
	if r.asset != nil {
		*a = *cloneStudioArchive(r.asset)
		return nil
	}
	a.ID = "asset_0123456789abcdef0123456789abcdef"
	r.asset = cloneStudioArchive(a)
	return nil
}
func (r *studioArchiveMemory) GetStudioAsset(_ context.Context, o StudioAssetOwner, id string) (*StudioAsset, error) {
	if r.asset == nil || r.asset.Owner != o || r.asset.ID != id || r.asset.DeletedAt != nil {
		return nil, ErrStudioAssetNotFound
	}
	return cloneStudioArchive(r.asset), nil
}
func (r *studioArchiveMemory) DeleteStudioAsset(_ context.Context, o StudioAssetOwner, id string) error {
	if r.asset == nil || r.asset.Owner != o || r.asset.ID != id || r.asset.DeletedAt != nil {
		return nil
	}
	now := time.Now()
	r.asset.DeletedAt, r.asset.SourceURL, r.asset.SourceBytes = &now, "", nil
	if r.asset.Kind == "image" && r.asset.StorageKey == "" {
		r.asset.State = "pending"
	}
	return nil
}
func (r *studioArchiveMemory) ClaimStudioAssets(ctx context.Context, limit int) ([]*StudioAsset, error) {
	r.claimLimit = limit
	if deadline, ok := ctx.Deadline(); ok {
		r.deadline = time.Until(deadline)
	}
	if r.asset == nil || r.asset.DeletedAt != nil || r.asset.State == "ready" || r.asset.State == "unavailable" {
		return nil, nil
	}
	r.asset.LeaseToken = "worker-lease"
	return []*StudioAsset{cloneStudioArchive(r.asset)}, nil
}
func (r *studioArchiveMemory) CompleteStudioAsset(_ context.Context, a *StudioAsset) error {
	if r.beforeComplete != nil {
		r.beforeComplete()
	}
	if r.asset.DeletedAt != nil || r.asset.LeaseToken != a.LeaseToken {
		return ErrStudioAssetUnavailable
	}
	r.asset = cloneStudioArchive(a)
	r.asset.State = "ready"
	r.asset.SourceBytes = nil
	r.asset.SourceURL = ""
	return nil
}
func (r *studioArchiveMemory) RetryStudioAsset(_ context.Context, a *StudioAsset, code string) error {
	r.retryCode = code
	if r.asset.DeletedAt != nil || r.asset.LeaseToken != a.LeaseToken {
		return ErrStudioAssetUnavailable
	}
	r.asset.State = "retrying"
	if a.State == "unavailable" {
		r.asset.State = "unavailable"
	}
	r.asset.LeaseToken = ""
	return nil
}
func (r *studioArchiveMemory) RecordStudioAssetCleanup(_ context.Context, a *StudioAsset) error {
	if r.failCleanupRecord {
		return errors.New("database unavailable")
	}
	if r.asset.DeletedAt != nil && r.asset.Owner == a.Owner && r.asset.ID == a.ID {
		if r.asset.StorageKey == "" {
			r.asset.State = "pending"
		}
		r.asset.LeaseToken = "late-cleanup-horizon"
		r.cleanupRecorded = true
	}
	return nil
}
func (r *studioArchiveMemory) ClaimDeletedStudioAssets(_ context.Context, limit int) ([]*StudioAsset, error) {
	r.claimLimit = limit
	if r.asset == nil || r.asset.DeletedAt == nil || r.asset.StorageKey == "" && (r.asset.Kind != "image" || r.asset.State == "unavailable") {
		return nil, nil
	}
	r.asset.LeaseToken = "cleanup-lease"
	return []*StudioAsset{cloneStudioArchive(r.asset)}, nil
}
func (r *studioArchiveMemory) CompleteStudioAssetCleanup(_ context.Context, a *StudioAsset) error {
	if r.asset.LeaseToken != a.LeaseToken {
		return ErrStudioAssetUnavailable
	}
	r.asset.StorageKey = ""
	r.asset.State = "unavailable"
	return nil
}
func (r *studioArchiveMemory) GetVideoResult(_ context.Context, o VideoAttemptOwner, resource string) (*VideoResult, error) {
	if r.video == nil || r.video.UserID != o.UserID || r.video.APIKeyID != o.APIKeyID || r.video.Resource != resource {
		return nil, ErrVideoResultNotFound
	}
	c := *r.video
	return &c, nil
}

type studioArchiveObjects struct {
	data                 map[string][]byte
	failSave, failDelete bool
	saved, deleted       int
	lastRange            string
	afterSave            func()
}

func (o *studioArchiveObjects) Save(_ context.Context, key, _ string, data []byte) (string, error) {
	if o.failSave {
		return "", errors.New("secret payload https://private.example?signature=secret")
	}
	if o.data == nil {
		o.data = map[string][]byte{}
	}
	o.saved++
	o.data[key] = bytes.Clone(data)
	if o.afterSave != nil {
		o.afterSave()
		return "", errors.New("upload completed but signature failed: secret")
	}
	return "https://private.example?signature=secret", nil
}
func (o *studioArchiveObjects) Delete(_ context.Context, key string) error {
	if o.failDelete {
		return errors.New("secret delete error")
	}
	o.deleted++
	delete(o.data, key)
	return nil
}
func (o *studioArchiveObjects) OpenStudioAsset(_ context.Context, key, byteRange string) (*VideoResultContent, error) {
	o.lastRange = byteRange
	return &VideoResultContent{Body: io.NopCloser(bytes.NewReader(o.data[key])), MIME: "image/png", Size: int64(len(o.data[key])), Status: 200}, nil
}
func (*studioArchiveObjects) StudioAssetURL(context.Context, string, time.Duration) (string, error) {
	return "https://private.example?signature=secret", nil
}

func studioPNG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, image.NewNRGBA(image.Rect(0, 0, 1, 1))))
	return buf.Bytes()
}
func studioArchiveFixture(limit int64, transport http.RoundTripper) (*StudioAssetService, *studioArchiveMemory, *studioArchiveObjects) {
	r, o := &studioArchiveMemory{}, &studioArchiveObjects{}
	var client *http.Client
	if transport != nil {
		client = &http.Client{Transport: transport}
	}
	u := NewImageResultUploader(o, "images/", limit, client)
	return NewStudioAssetService(r, func() (*ImageResultUploader, bool) { return u, true }), r, o
}

var studioArchiveOwner = StudioAssetOwner{UserID: 16, APIKeyID: 20}

func collectStudioPNG(t *testing.T, s *StudioAssetService) {
	t.Helper()
	require.NoError(t, s.CollectImage(context.Background(), studioArchiveOwner, "generation-1", 0, "original-model", "original prompt", json.RawMessage(`{"aspect_ratio":"3:4"}`), StudioImageOutput{Bytes: studioPNG(t), MIME: "image/png"}))
}

// Removing unavailable handling must not permit library failure to enter any paid interface.
func TestStudioAssetLibraryFailureCannotInvokeGeneration(t *testing.T) {
	s := NewStudioAssetService(nil, nil)
	err := s.CollectImage(context.Background(), studioArchiveOwner, "generation-1", 0, "image-model", "prompt", json.RawMessage(`{}`), StudioImageOutput{MIME: "image/png", Bytes: []byte("bad")})
	require.ErrorIs(t, err, ErrStudioAssetUnavailable)
}

// Moving validation after Upsert would persist oversized or non-image private bytes.
func TestStudioAssetCollectValidatesBeforePersistingEffectiveLimit(t *testing.T) {
	for _, tc := range []struct {
		name   string
		limit  int64
		output StudioImageOutput
	}{
		{"false mime", 1024, StudioImageOutput{Bytes: []byte("not a PNG"), MIME: "image/png"}},
		{"wrong declared mime", 1024, StudioImageOutput{Bytes: studioPNG(t), MIME: "text/html"}},
		{"effective limit", 1, StudioImageOutput{Bytes: studioPNG(t), MIME: "image/png"}},
		{"unsafe url", 1024, StudioImageOutput{URL: "https://127.0.0.1/private?secret=1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, r, o := studioArchiveFixture(tc.limit, nil)
			err := s.CollectImage(context.Background(), studioArchiveOwner, "generation", 0, "model", "prompt", nil, tc.output)
			require.ErrorIs(t, err, ErrStudioAssetInvalidRequest)
			require.Nil(t, r.asset)
			require.Zero(t, o.saved)
			require.NotContains(t, err.Error(), "secret")
		})
	}
}

func TestStudioAssetImageLimitCanExceedDefault(t *testing.T) {
	data := append(studioPNG(t), make([]byte, 33<<20)...)
	s, r, _ := studioArchiveFixture(34<<20, nil)
	require.NoError(t, s.CollectImage(context.Background(), studioArchiveOwner, "large", 0, "model", "prompt", nil, StudioImageOutput{Bytes: data, MIME: "image/png"}))
	require.Len(t, r.asset.SourceBytes, len(data))
}

// Clearing sources before storage/CAS completion would lose recoverable output on restart.
func TestStudioAssetRetriesAfterRestartThenOpensPrivateObject(t *testing.T) {
	s, r, o := studioArchiveFixture(1024, nil)
	collectStudioPNG(t, s)
	o.failSave = true
	err := s.ProcessPending(context.Background())
	require.ErrorIs(t, err, ErrStudioAssetUnavailable)
	require.EqualError(t, err, "studio asset storage is temporarily unavailable", "provider payloads and signed URLs must not escape through errors")
	require.Equal(t, "retrying", r.asset.State)
	require.NotEmpty(t, r.asset.SourceBytes)
	require.Empty(t, r.asset.StorageKey)
	require.Regexp(t, `^[a-z0-9_]{1,64}$`, r.retryCode)
	o.failSave = false
	restarted := NewStudioAssetService(r, s.resolve)
	require.NoError(t, restarted.ProcessPending(context.Background()))
	require.Equal(t, 1, r.claimLimit)
	require.InDelta(t, 120, r.deadline.Seconds(), 2)
	require.Equal(t, "ready", r.asset.State)
	require.Empty(t, r.asset.SourceBytes)
	require.Empty(t, r.asset.SourceURL)
	require.NotContains(t, r.asset.StorageKey, "signature")
	require.Equal(t, 1, o.saved)
	content, err := restarted.Open(context.Background(), studioArchiveOwner, r.asset.ID, "bytes=0-3")
	require.NoError(t, err)
	defer content.Body.Close()
	got, err := io.ReadAll(content.Body)
	require.NoError(t, err)
	require.Equal(t, studioPNG(t), got)
	require.Equal(t, "bytes=0-3", o.lastRange)
	_, err = restarted.Open(context.Background(), StudioAssetOwner{16, 21}, r.asset.ID, "")
	require.ErrorIs(t, err, ErrStudioAssetNotFound)
}

func TestStudioAssetOpenRejectsMalformedMultipleAndUnboundedRanges(t *testing.T) {
	s, r, o := studioArchiveFixture(1024, nil)
	collectStudioPNG(t, s)
	require.NoError(t, s.ProcessPending(context.Background()))
	for _, value := range []string{"bytes=0-1,4-5", "bytes=-0", "bytes=4-1", "bytes=+1-2", "bytes=0-9223372036854775808", "items=0-1", "bytes=-", "bytes=0-1\r\nSecret: value"} {
		_, err := s.Open(context.Background(), studioArchiveOwner, r.asset.ID, value)
		require.ErrorIs(t, err, ErrStudioAssetInvalidRequest)
	}
	require.Empty(t, o.lastRange)
	for _, value := range []string{"bytes=0-", "bytes=-4", "bytes=0-3"} {
		content, err := s.Open(context.Background(), studioArchiveOwner, r.asset.ID, value)
		require.NoError(t, err)
		require.NoError(t, content.Body.Close())
		require.Equal(t, value, o.lastRange)
	}
}

func TestStudioAssetCleanupNeverDeletesForeignOrSharedImageKey(t *testing.T) {
	for _, key := range []string{"video-results/16/20/shared.mp4", "studio-results/16/21/foreign/0", "unrelated-owned-object"} {
		s, r, o := studioArchiveFixture(1024, nil)
		collectStudioPNG(t, s)
		now := time.Now()
		r.asset.DeletedAt, r.asset.StorageKey = &now, key
		require.NoError(t, s.ProcessCleanup(context.Background()))
		require.Zero(t, o.deleted)
		require.Empty(t, r.asset.StorageKey)
	}
}

func TestStudioAssetUnavailableStoreRetainsDurableSource(t *testing.T) {
	r := &studioArchiveMemory{}
	u := NewImageResultUploader(nil, "", 1024, nil)
	s := NewStudioAssetService(r, func() (*ImageResultUploader, bool) { return u, false })
	require.ErrorIs(t, s.CollectImage(context.Background(), studioArchiveOwner, "generation", 0, "model", "prompt", nil, StudioImageOutput{Bytes: studioPNG(t), MIME: "image/png"}), ErrStudioAssetUnavailable)
	require.NotEmpty(t, r.asset.SourceBytes)
	require.ErrorIs(t, s.ProcessPending(context.Background()), ErrStudioAssetUnavailable)
	require.Equal(t, "retrying", r.asset.State)
}

// Without a resolved effective limit, falling back to 32 MiB could persist data forbidden by a smaller configured limit.
func TestStudioAssetUnknownEffectiveLimitDoesNotPersistInlineSource(t *testing.T) {
	r := &studioArchiveMemory{}
	s := NewStudioAssetService(r, nil)
	require.ErrorIs(t, s.CollectImage(context.Background(), studioArchiveOwner, "generation", 0, "model", "prompt", nil, StudioImageOutput{Bytes: studioPNG(t), MIME: "image/png"}), ErrStudioAssetUnavailable)
	require.Nil(t, r.asset)
	require.ErrorIs(t, s.CollectImage(context.Background(), studioArchiveOwner, "generation", 0, "model", "prompt", nil, StudioImageOutput{URL: "https://supplier.example/result.png"}), ErrStudioAssetUnavailable)
	require.Equal(t, "https://supplier.example/result.png", r.asset.SourceURL)
}

// Redirecting a public supplier URL to metadata must never read the second response.
func TestStudioAssetRejectsUnsafeRedirectAndFalseImage(t *testing.T) {
	for _, redirect := range []bool{true, false} {
		t.Run(strings.ToLower(map[bool]string{true: "redirect", false: "false image"}[redirect]), func(t *testing.T) {
			requests := 0
			transport := videoResultTransport(func(req *http.Request) (*http.Response, error) {
				requests++
				h := http.Header{"Content-Type": {"image/png"}}
				status := 200
				if redirect {
					status = 302
					h.Set("Location", "https://169.254.169.254/private?secret=1")
				}
				return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader("not image")), Request: req}, nil
			})
			s, r, o := studioArchiveFixture(1024, transport)
			require.NoError(t, s.CollectImage(context.Background(), studioArchiveOwner, "url", 0, "model", "prompt", nil, StudioImageOutput{URL: "https://supplier.example/result?secret=1"}))
			err := s.ProcessPending(context.Background())
			require.ErrorIs(t, err, ErrStudioAssetUnavailable)
			require.NotContains(t, err.Error(), "secret")
			require.Equal(t, 1, requests)
			require.Zero(t, o.saved)
			require.NotEmpty(t, r.asset.SourceURL)
		})
	}
}

// A deleted row's late upload must be durably cleaned, while a newer live lease must retain its object.
func TestStudioAssetCASFailureSchedulesOnlyTombstonedCleanup(t *testing.T) {
	for _, deleted := range []bool{true, false} {
		t.Run(map[bool]string{true: "deleted", false: "reassigned"}[deleted], func(t *testing.T) {
			s, r, o := studioArchiveFixture(1024, nil)
			collectStudioPNG(t, s)
			r.beforeComplete = func() {
				if deleted {
					now := time.Now()
					r.asset.DeletedAt = &now
				} else {
					r.asset.LeaseToken = "new-worker"
				}
			}
			require.ErrorIs(t, s.ProcessPending(context.Background()), ErrStudioAssetUnavailable)
			require.Equal(t, deleted, r.cleanupRecorded)
			require.Zero(t, o.deleted)
			if deleted {
				o.failDelete = true
				require.ErrorIs(t, s.ProcessCleanup(context.Background()), ErrStudioAssetUnavailable)
				require.Equal(t, "pending", r.asset.State)
				o.failDelete = false
				require.NoError(t, s.ProcessCleanup(context.Background()))
				require.Empty(t, r.asset.StorageKey)
				require.Equal(t, 1, o.deleted)
			} else {
				require.NoError(t, s.ProcessCleanup(context.Background()))
				require.Zero(t, o.deleted)
				require.Len(t, o.data, 1)
			}
		})
	}
}

// A provider can accept PutObject but fail while signing its returned URL; that late object still needs cleanup.
func TestStudioAssetAmbiguousUploadFailureAfterDeleteRetainsDurableCleanup(t *testing.T) {
	s, r, o := studioArchiveFixture(1024, nil)
	collectStudioPNG(t, s)
	o.afterSave = func() { now := time.Now(); r.asset.DeletedAt = &now }
	require.ErrorIs(t, s.ProcessPending(context.Background()), ErrStudioAssetUnavailable)
	require.True(t, r.cleanupRecorded)
	require.Equal(t, "pending", r.asset.State)
	require.NoError(t, s.ProcessCleanup(context.Background()))
	require.Empty(t, o.data)
}

func TestStudioAssetAmbiguousUploadThenTerminalRetryDeleteCleansDeterministicObject(t *testing.T) {
	s, r, o := studioArchiveFixture(1024, nil)
	collectStudioPNG(t, s)
	o.afterSave = func() {}
	require.ErrorIs(t, s.ProcessPending(context.Background()), ErrStudioAssetUnavailable)
	require.Equal(t, "retrying", r.asset.State)
	require.Empty(t, r.asset.StorageKey)
	require.False(t, r.cleanupRecorded, "a live row must not schedule tombstone cleanup")
	const key = "studio-results/16/20/6b598d3d55679cadfaa60ffa00eaae588211fd2dee326eaf855bbd1f8653c398/0"
	require.Equal(t, studioPNG(t), o.data[key])

	// A changed effective limit makes the retained source terminal without a second upload.
	u := NewImageResultUploader(o, "", 1, nil)
	s.resolve = func() (*ImageResultUploader, bool) { return u, true }
	require.ErrorIs(t, s.ProcessPending(context.Background()), ErrStudioAssetUnavailable)
	require.Equal(t, "image_too_large", r.retryCode)
	require.Equal(t, "unavailable", r.asset.State)
	require.Empty(t, r.asset.StorageKey)
	require.Equal(t, 1, o.saved)
	require.NoError(t, s.ProcessCleanup(context.Background()))
	require.Zero(t, o.deleted, "terminal live assets must never delete their object")

	require.NoError(t, r.DeleteStudioAsset(context.Background(), studioArchiveOwner, r.asset.ID))
	require.Empty(t, r.asset.SourceBytes)
	require.Equal(t, "pending", r.asset.State, "archive failure is not physical cleanup completion")
	require.NoError(t, s.ProcessCleanup(context.Background()))
	require.Empty(t, o.data)
	require.Equal(t, 1, o.deleted)
	require.Equal(t, "unavailable", r.asset.State)
	require.NoError(t, r.DeleteStudioAsset(context.Background(), studioArchiveOwner, r.asset.ID))
	require.NoError(t, s.ProcessCleanup(context.Background()))
	require.Equal(t, 1, o.deleted, "idempotent deletion must not revive completed cleanup")
}

// The tombstone itself must recover an orphan even when the post-upload cleanup-record write fails during a DB outage.
func TestStudioAssetDeletedImageCleanupReconstructsKeyAfterDatabaseRecovery(t *testing.T) {
	s, r, o := studioArchiveFixture(1024, nil)
	collectStudioPNG(t, s)
	r.failCleanupRecord = true
	r.beforeComplete = func() {
		now := time.Now()
		r.asset.DeletedAt = &now
		r.asset.SourceBytes = nil
		r.asset.LeaseToken = ""
	}
	require.ErrorIs(t, s.ProcessPending(context.Background()), ErrStudioAssetUnavailable)
	require.False(t, r.cleanupRecorded)
	require.Empty(t, r.asset.StorageKey)
	require.Len(t, o.data, 1)
	r.failCleanupRecord = false
	require.NoError(t, s.ProcessCleanup(context.Background()))
	require.Empty(t, o.data)
	require.Equal(t, 1, o.deleted)
	require.Equal(t, "unavailable", r.asset.State)
	require.NoError(t, s.ProcessCleanup(context.Background()))
	require.Equal(t, 1, o.deleted, "completed tombstones must not be reclaimed forever")
}

// Re-archiving or deleting a video reference must never upload/delete the shared video_results object.
func TestStudioAssetVideoReusesOwnedArchiveAndNeverDeletesSharedObject(t *testing.T) {
	s, r, o := studioArchiveFixture(1024, nil)
	r.asset = &StudioAsset{ID: "asset_0123456789abcdef0123456789abcdef", Owner: studioArchiveOwner, Kind: "video", GenerationID: "video-generation", VideoResource: "attempt:existing", State: "pending"}
	r.video = &VideoResult{Resource: "attempt:existing", UserID: 16, APIKeyID: 20, StorageKey: "video-results/16/20/existing.mp4", MIME: "video/mp4", Size: 42}
	require.NoError(t, s.ProcessPending(context.Background()))
	require.Equal(t, r.video.StorageKey, r.asset.StorageKey)
	require.Zero(t, o.saved)
	now := time.Now()
	r.asset.DeletedAt = &now
	require.NoError(t, s.ProcessCleanup(context.Background()))
	require.Zero(t, o.deleted)
	require.NotEmpty(t, r.video.StorageKey)
}

func TestStudioAssetVideoPendingOwnershipAndMaximumSize(t *testing.T) {
	for _, tc := range []struct {
		name  string
		video *VideoResult
		state string
	}{
		{"pending", &VideoResult{Resource: "attempt:existing", UserID: 16, APIKeyID: 20}, "retrying"},
		{"wrong key invisible", &VideoResult{Resource: "attempt:existing", UserID: 16, APIKeyID: 21, StorageKey: "video-results/private", Size: 1}, "retrying"},
		{"oversize", &VideoResult{Resource: "attempt:existing", UserID: 16, APIKeyID: 20, StorageKey: "video-results/16/20/existing.mp4", Size: 300<<20 + 1}, "unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, r, o := studioArchiveFixture(1024, nil)
			r.asset = &StudioAsset{ID: "asset_0123456789abcdef0123456789abcdef", Owner: studioArchiveOwner, Kind: "video", GenerationID: "video", VideoResource: "attempt:existing", State: "pending"}
			r.video = tc.video
			require.ErrorIs(t, s.ProcessPending(context.Background()), ErrStudioAssetUnavailable)
			require.Equal(t, tc.state, r.asset.State)
			require.Zero(t, o.saved)
		})
	}
}
