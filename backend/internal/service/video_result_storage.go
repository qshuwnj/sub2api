package service

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

var (
	ErrVideoResultNotFound    = errors.New("video result not found")
	ErrVideoResultUnavailable = errors.New("video playback is temporarily unavailable")
	ErrVideoResultExpired     = errors.New("the supplier no longer provides a playable video")
)

// VideoResult is separate from settlement: archiving must never reserve,
// capture, release, or resubmit a paid generation.
type VideoResult struct {
	Resource, TaskID, SourceURL, StorageKey, MIME, LeaseToken string
	UserID, APIKeyID, AccountID, Size                         int64
	Attempts                                                  int
}

type VideoResultRepository interface {
	QueueVideoResult(context.Context, *VideoResult) error
	GetVideoResult(context.Context, VideoAttemptOwner, string) (*VideoResult, error)
	ClaimVideoResults(context.Context, int) ([]*VideoResult, error)
	CompleteVideoResult(context.Context, *VideoResult) error
	RetryVideoResult(context.Context, *VideoResult) error
	FindLegacyVideoResult(context.Context, VideoAttemptOwner, string) (*VideoResult, error)
}

type VideoResultBackfillRepository interface {
	BackfillVideoResults(context.Context, int) error
}

type VideoResultContent struct {
	Body         io.ReadCloser
	MIME         string
	Size         int64
	Status       int
	ContentRange string
}

// Optional streaming capabilities of the existing private S3/R2 store.
type VideoObjectStore interface {
	SaveVideo(context.Context, string, string, io.ReadSeeker, int64) error
	VideoURL(context.Context, string) (string, error)
	OpenVideo(context.Context, string, string) (*VideoResultContent, error)
}

type VideoPlayback struct {
	URL         string `json:"url"`
	ContentPath string `json:"content_path"`
	Archived    bool   `json:"archived"`
}

type VideoResultService struct {
	repo    VideoResultRepository
	resolve ImageStorageResolver
}

func NewVideoResultService(repo VideoResultRepository, resolve ImageStorageResolver) *VideoResultService {
	return &VideoResultService{repo: repo, resolve: resolve}
}

func validVideoResultURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.User == nil && u.Hostname() != "" &&
		!isBlockedImageDownloadHost(u.Hostname()) && u.Fragment == "" && len(raw) <= 16384
}

func videoResultContentPath(resource string) string {
	if id, ok := strings.CutPrefix(resource, "attempt:"); ok && ValidVideoAttemptID(id) {
		return "/v1/videos/attempts/" + id + "/content"
	}
	if id, ok := strings.CutPrefix(resource, "task:"); ok && ValidVideoAttemptID(id) && strings.HasPrefix(id, "atlascloud:") {
		return "/v1/videos/" + id + "/content"
	}
	return ""
}

func (s *VideoResultService) Queue(ctx context.Context, v *VideoResult) error {
	if s == nil || s.repo == nil || v == nil || v.UserID <= 0 || v.APIKeyID <= 0 || v.AccountID <= 0 ||
		videoResultContentPath(v.Resource) == "" || !strings.HasPrefix(v.TaskID, "atlascloud:") ||
		!ValidVideoAttemptID(v.TaskID) || !validVideoResultURL(v.SourceURL) ||
		strings.HasPrefix(v.Resource, "task:") && v.Resource != "task:"+v.TaskID {
		return ErrVideoResultUnavailable
	}
	return s.repo.QueueVideoResult(ctx, v)
}

func (s *VideoResultService) objects() (*ImageResultUploader, VideoObjectStore, error) {
	if s == nil || s.resolve == nil {
		return nil, nil, ErrVideoResultUnavailable
	}
	u, enabled := s.resolve()
	if !enabled || u == nil {
		return nil, nil, ErrVideoResultUnavailable
	}
	store, ok := u.storage.(VideoObjectStore)
	if !ok {
		return nil, nil, ErrVideoResultUnavailable
	}
	return u, store, nil
}

func (s *VideoResultService) Playback(ctx context.Context, owner VideoAttemptOwner, resource string) (*VideoPlayback, error) {
	if s == nil || s.repo == nil {
		return nil, ErrVideoResultNotFound
	}
	v, err := s.repo.GetVideoResult(ctx, owner, resource)
	if err != nil {
		return nil, err
	}
	if v.StorageKey == "" {
		return nil, ErrVideoResultUnavailable
	}
	_, store, err := s.objects()
	if err != nil {
		return nil, err
	}
	link, err := store.VideoURL(ctx, v.StorageKey)
	if err != nil || !validVideoResultURL(link) {
		return nil, ErrVideoResultUnavailable
	}
	return &VideoPlayback{URL: link, ContentPath: videoResultContentPath(resource), Archived: true}, nil
}

func (s *VideoResultService) Open(ctx context.Context, owner VideoAttemptOwner, resource, byteRange string) (*VideoResultContent, error) {
	if s == nil || s.repo == nil {
		return nil, ErrVideoResultNotFound
	}
	v, err := s.repo.GetVideoResult(ctx, owner, resource)
	if err != nil {
		return nil, err
	}
	if v.StorageKey == "" {
		return nil, ErrVideoResultUnavailable
	}
	_, store, err := s.objects()
	if err != nil {
		return nil, err
	}
	return store.OpenVideo(ctx, v.StorageKey, byteRange)
}

func videoSignedURLExpired(raw string, now time.Time) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return true
	}
	q := u.Query()
	for _, prefix := range []string{"X-Tos-", "X-Amz-"} {
		if q.Get(prefix+"Date") == "" {
			continue
		}
		start, err := time.Parse("20060102T150405Z", q.Get(prefix+"Date"))
		seconds, parseErr := strconv.ParseInt(q.Get(prefix+"Expires"), 10, 64)
		if err != nil || parseErr != nil || seconds <= 0 || seconds > 7*86400 {
			return true
		}
		return !now.Add(30 * time.Second).Before(start.Add(time.Duration(seconds) * time.Second))
	}
	return false
}

type VideoResultRefresher func(context.Context, *VideoResult) (string, error)

// Claiming rows uses database leases, so multiple web instances can safely run
// this worker. Failed archives keep their source and are retried independently.
func (s *VideoResultService) ArchivePending(ctx context.Context, refresh VideoResultRefresher) error {
	u, store, err := s.objects()
	if err != nil {
		return err
	}
	if repo, ok := s.repo.(VideoResultBackfillRepository); ok {
		if err := repo.BackfillVideoResults(ctx, 100); err != nil {
			return err
		}
	}
	items, err := s.repo.ClaimVideoResults(ctx, 1)
	if err != nil {
		return err
	}
	var first error
	for _, v := range items {
		err := s.archive(ctx, u, store, v, refresh)
		if err != nil {
			retryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			_ = s.repo.RetryVideoResult(retryCtx, v)
			cancel()
			if first == nil {
				first = err
			}
		}
	}
	return first
}

func (s *VideoResultService) archive(ctx context.Context, u *ImageResultUploader, store VideoObjectStore, v *VideoResult, refresh VideoResultRefresher) error {
	source := v.SourceURL
	if source == "" || videoSignedURLExpired(source, time.Now()) {
		if refresh == nil {
			return ErrVideoResultExpired
		}
		var err error
		source, err = refresh(ctx, v)
		if err != nil {
			return err
		}
	}
	if !validVideoResultURL(source) || videoSignedURLExpired(source, time.Now()) {
		return ErrVideoResultExpired
	}
	client := *u.httpClient
	client.Timeout = 110 * time.Second
	// Retain the existing SSRF-safe transport; validate redirects as well.
	priorRedirect := client.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 || !validVideoResultURL(req.URL.String()) {
			return ErrVideoResultUnavailable
		}
		if priorRedirect != nil {
			return priorRedirect(req, via)
		}
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return ErrVideoResultUnavailable
	}
	resp, err := client.Do(req)
	if err != nil {
		return ErrVideoResultUnavailable // Never log bearer-like signed URLs.
	}
	defer resp.Body.Close()
	const maxBytes = int64(300 << 20)
	if resp.StatusCode != http.StatusOK || resp.ContentLength > maxBytes {
		return ErrVideoResultUnavailable
	}
	file, err := os.CreateTemp("", "quotaflow-video-*.part")
	if err != nil {
		return ErrVideoResultUnavailable
	}
	defer func() { _ = file.Close(); _ = os.Remove(file.Name()) }()
	size, err := io.Copy(file, io.LimitReader(resp.Body, maxBytes+1))
	if err != nil || size <= 0 || size > maxBytes {
		return ErrVideoResultUnavailable
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return ErrVideoResultUnavailable
	}
	header := make([]byte, 512)
	n, _ := io.ReadFull(file, header)
	mime := http.DetectContentType(header[:n])
	extension := ".mp4"
	if mime == "video/webm" {
		extension = ".webm"
	} else if mime != "video/mp4" {
		return ErrVideoResultUnavailable
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return ErrVideoResultUnavailable
	}
	hash := sha256.Sum256([]byte(v.Resource))
	key := fmt.Sprintf("video-results/%d/%d/%x%s", v.UserID, v.APIKeyID, hash, extension)
	if err := store.SaveVideo(ctx, key, mime, file, size); err != nil {
		return ErrVideoResultUnavailable
	}
	completed := *v
	completed.StorageKey, completed.SourceURL, completed.MIME, completed.Size = key, "", mime, size
	// Commit independently of the HTTP poll that discovered the result.
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	return s.repo.CompleteVideoResult(saveCtx, &completed)
}
