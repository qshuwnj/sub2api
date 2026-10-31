package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type videoResultMemory struct {
	items map[string]*VideoResult
}

func (r *videoResultMemory) QueueVideoResult(_ context.Context, v *VideoResult) error {
	if r.items == nil {
		r.items = map[string]*VideoResult{}
	}
	if old := r.items[v.Resource]; old != nil && old.StorageKey != "" {
		return nil
	}
	copy := *v
	r.items[v.Resource] = &copy
	return nil
}
func (r *videoResultMemory) GetVideoResult(_ context.Context, owner VideoAttemptOwner, resource string) (*VideoResult, error) {
	v := r.items[resource]
	if v == nil || v.UserID != owner.UserID || v.APIKeyID != owner.APIKeyID {
		return nil, ErrVideoResultNotFound
	}
	copy := *v
	return &copy, nil
}
func (r *videoResultMemory) ClaimVideoResults(_ context.Context, _ int) ([]*VideoResult, error) {
	var pending []*VideoResult
	for _, v := range r.items {
		if v.StorageKey == "" {
			copy := *v
			pending = append(pending, &copy)
		}
	}
	return pending, nil
}
func (r *videoResultMemory) CompleteVideoResult(_ context.Context, v *VideoResult) error {
	copy := *v
	r.items[v.Resource] = &copy
	return nil
}
func (r *videoResultMemory) RetryVideoResult(context.Context, *VideoResult) error { return nil }
func (r *videoResultMemory) FindLegacyVideoResult(context.Context, VideoAttemptOwner, string) (*VideoResult, error) {
	return nil, ErrVideoResultNotFound
}

type videoObjectMemory struct {
	data  map[string][]byte
	signs int
	saves int
	fail  bool
}

func (*videoObjectMemory) Save(context.Context, string, string, []byte) (string, error) {
	return "", errors.New("video must use a streaming object upload")
}
func (s *videoObjectMemory) SaveVideo(_ context.Context, key, mime string, body io.ReadSeeker, size int64) error {
	requireType := mime == "video/mp4" && size > 0
	if s.fail || !requireType {
		return errors.New("synthetic storage unavailable")
	}
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	s.saves++
	if s.data == nil {
		s.data = map[string][]byte{}
	}
	s.data[key] = data
	return nil
}
func (s *videoObjectMemory) VideoURL(_ context.Context, _ string) (string, error) {
	s.signs++
	if s.signs == 1 {
		return "https://r2.example/video.mp4?signature=first", nil
	}
	return "https://r2.example/video.mp4?signature=renewed", nil
}
func (s *videoObjectMemory) OpenVideo(_ context.Context, key, _ string) (*VideoResultContent, error) {
	data := s.data[key]
	return &VideoResultContent{Body: io.NopCloser(bytes.NewReader(data)), MIME: "video/mp4", Size: int64(len(data)), Status: http.StatusOK}, nil
}

type videoResultTransport func(*http.Request) (*http.Response, error)

func (f videoResultTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func videoResultFixture() (*VideoResultService, *videoResultMemory, *videoObjectMemory, *[]string) {
	repo, objects, urls := &videoResultMemory{}, &videoObjectMemory{}, &[]string{}
	client := &http.Client{Transport: videoResultTransport(func(req *http.Request) (*http.Response, error) {
		*urls = append(*urls, req.URL.Host)
		// A small ISO BMFF/MP4 header, not HTML hidden behind a video MIME.
		data := append([]byte{0, 0, 0, 24, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm', 0, 0, 0, 0, 'i', 's', 'o', 'm', 'm', 'p', '4', '2'}, make([]byte, 32)...)
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"video/mp4"}}, Body: io.NopCloser(bytes.NewReader(data))}, nil
	})}
	uploader := NewImageResultUploader(objects, "images/", 32<<20, client)
	return NewVideoResultService(repo, func() (*ImageResultUploader, bool) { return uploader, true }), repo, objects, urls
}

type videoBackfillMemory struct {
	*videoResultMemory
	completed *VideoResult
	backfills int
}

func (r *videoBackfillMemory) BackfillVideoResults(ctx context.Context, limit int) error {
	r.backfills++
	if r.items[r.completed.Resource] == nil {
		return r.QueueVideoResult(ctx, r.completed)
	}
	return nil
}

func TestVideoResultBackfillsCommittedOutputAfterMissedEnqueue(t *testing.T) {
	s, memory, objects, _ := videoResultFixture()
	repo := &videoBackfillMemory{videoResultMemory: memory, completed: &VideoResult{
		Resource: "attempt:committed-before-restart", TaskID: "atlascloud:job",
		UserID: 16, APIKeyID: 20, AccountID: 30, SourceURL: "https://supplier.example/result.mp4",
	}}
	s.repo = repo
	// The financial receipt already committed, but a crash prevented enqueue.
	require.Empty(t, memory.items)
	require.NoError(t, s.ArchivePending(context.Background(), nil))
	require.Equal(t, 1, repo.backfills)
	require.NotNil(t, memory.items[repo.completed.Resource])
	require.NotEmpty(t, memory.items[repo.completed.Resource].StorageKey)
	require.NoError(t, s.ArchivePending(context.Background(), nil))
	require.Equal(t, 1, objects.saves, "backfill is idempotent and never regenerates a paid output")
}

func TestVideoResultPersistsObjectAndRenewsURLInsteadOfPersistingSignature(t *testing.T) {
	s, repo, objects, downloads := videoResultFixture()
	owner := VideoAttemptOwner{UserID: 16, APIKeyID: 20}
	v := &VideoResult{Resource: "attempt:old-attempt", TaskID: "atlascloud:job", UserID: 16, APIKeyID: 20, AccountID: 30, SourceURL: "https://supplier.example/result.mp4"}
	require.NoError(t, s.Queue(context.Background(), v))
	require.NoError(t, s.ArchivePending(context.Background(), nil))
	stored := repo.items[v.Resource]
	require.NotEmpty(t, stored.StorageKey)
	require.Empty(t, stored.SourceURL, "an archived record retains its object key, not an expiring provider URL")
	require.Equal(t, 1, objects.saves)
	require.Equal(t, []string{"supplier.example"}, *downloads)
	first, err := s.Playback(context.Background(), owner, v.Resource)
	require.NoError(t, err)
	second, err := s.Playback(context.Background(), owner, v.Resource)
	require.NoError(t, err)
	require.NotEqual(t, first.URL, second.URL)
	require.Equal(t, "/v1/videos/attempts/old-attempt/content", second.ContentPath)
	require.NoError(t, s.ArchivePending(context.Background(), nil))
	require.Equal(t, 1, objects.saves, "re-login renews access, not the stored video")
	_, err = s.Playback(context.Background(), VideoAttemptOwner{UserID: 17, APIKeyID: 20}, v.Resource)
	require.ErrorIs(t, err, ErrVideoResultNotFound)
	require.Equal(t, 2, objects.signs, "ownership must be checked before signing")
}

func TestVideoResultFailedArchiveKeepsOriginalForRetry(t *testing.T) {
	s, repo, objects, _ := videoResultFixture()
	v := &VideoResult{Resource: "task:atlascloud:job", TaskID: "atlascloud:job", UserID: 16, APIKeyID: 20, AccountID: 30, SourceURL: "https://supplier.example/result.mp4"}
	require.NoError(t, s.Queue(context.Background(), v))
	objects.fail = true
	require.Error(t, s.ArchivePending(context.Background(), nil))
	require.Equal(t, v.SourceURL, repo.items[v.Resource].SourceURL)
	require.Empty(t, repo.items[v.Resource].StorageKey)
	objects.fail = false
	require.NoError(t, s.ArchivePending(context.Background(), nil))
	require.NotEmpty(t, repo.items[v.Resource].StorageKey)
}

func TestVideoResultExpiredSourceRefreshesKnownTaskWithoutPOST(t *testing.T) {
	s, _, _, downloads := videoResultFixture()
	v := &VideoResult{Resource: "task:atlascloud:job", TaskID: "atlascloud:job", UserID: 16, APIKeyID: 20, AccountID: 30, SourceURL: "https://expired.example/result.mp4?X-Tos-Date=20000101T000000Z&X-Tos-Expires=86400"}
	require.NoError(t, s.Queue(context.Background(), v))
	calls := 0
	require.NoError(t, s.ArchivePending(context.Background(), func(_ context.Context, got *VideoResult) (string, error) {
		calls++
		require.Equal(t, "atlascloud:job", got.TaskID)
		require.Equal(t, int64(30), got.AccountID)
		return "https://supplier.example/refreshed.mp4", nil
	}))
	require.Equal(t, 1, calls)
	require.Equal(t, []string{"supplier.example"}, *downloads)
}

func TestVideoResultLegacyBackfillWithoutSourceReadsOnlyTheOwnedTask(t *testing.T) {
	s, repo, objects, _ := videoResultFixture()
	repo.items = map[string]*VideoResult{"task:atlascloud:legacy": {
		Resource: "task:atlascloud:legacy", TaskID: "atlascloud:legacy",
		UserID: 16, APIKeyID: 20, AccountID: 30,
	}}
	reads := 0
	err := s.ArchivePending(context.Background(), func(_ context.Context, v *VideoResult) (string, error) {
		reads++
		require.Equal(t, "atlascloud:legacy", v.TaskID)
		require.Equal(t, int64(30), v.AccountID)
		return "https://supplier.example/legacy.mp4", nil
	})
	require.NoError(t, err)
	require.Equal(t, 1, reads)
	require.Equal(t, 1, objects.saves)
	require.NotEmpty(t, repo.items["task:atlascloud:legacy"].StorageKey)
}

func TestVideoResultRejectsPrivateURLsAndFalseVideoResponses(t *testing.T) {
	for _, rawURL := range []string{"http://127.0.0.1/x.mp4", "https://localhost/x.mp4", "file:///tmp/x.mp4", "https://user:pass@example.com/x.mp4"} {
		s, _, _, _ := videoResultFixture()
		require.Error(t, s.Queue(context.Background(), &VideoResult{Resource: "task:atlascloud:job", TaskID: "atlascloud:job", UserID: 16, APIKeyID: 20, AccountID: 30, SourceURL: rawURL}))
	}
	s, repo, objects, _ := videoResultFixture()
	u, _ := s.resolve()
	u.httpClient = &http.Client{Transport: videoResultTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"video/mp4"}}, Body: io.NopCloser(strings.NewReader("<html>Access denied</html>"))}, nil
	})}
	require.NoError(t, s.Queue(context.Background(), &VideoResult{Resource: "task:atlascloud:job", TaskID: "atlascloud:job", UserID: 16, APIKeyID: 20, AccountID: 30, SourceURL: "https://supplier.example/result.mp4"}))
	require.Error(t, s.ArchivePending(context.Background(), nil))
	require.Zero(t, objects.saves)
	require.Empty(t, repo.items["task:atlascloud:job"].StorageKey)
}

func TestVideoSignedURLExpiryIncludesTosAndS3ButNotPermanentURLs(t *testing.T) {
	now := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	require.True(t, videoSignedURLExpired("https://cdn.example/x?X-Tos-Date=20261030T050939Z&X-Tos-Expires=86400", now))
	require.True(t, videoSignedURLExpired("https://cdn.example/x?X-Amz-Date=20261030T050939Z&X-Amz-Expires=86400", now))
	require.False(t, videoSignedURLExpired("https://cdn.example/x?X-Amz-Date=20261031T090000Z&X-Amz-Expires=3600", now))
	require.False(t, videoSignedURLExpired("https://cdn.example/x.mp4", now))
}
