package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// Accepting arbitrary URLs would let a library retry become an unowned fetch.
func TestStudioAssetReconcileDoesNotAcceptURLs(t *testing.T) {
	s := NewStudioAssetService(nil, nil)
	for _, id := range []string{"https://example.com/video.mp4", "http://example.com", "task:../private"} {
		_, err := s.Reconcile(context.Background(), StudioAssetOwner{16, 20}, []string{id})
		require.ErrorIs(t, err, ErrStudioAssetInvalidRequest)
	}
}

type studioNativeAccounts struct {
	AccountRepository
	account *Account
}

func (r *studioNativeAccounts) GetByID(_ context.Context, id int64) (*Account, error) {
	if r.account == nil || r.account.ID != id {
		return nil, ErrVideoResultNotFound
	}
	return r.account, nil
}

func TestStudioAssetVideoNativeRecoveryUsesExistingOwnedGETOnly(t *testing.T) {
	for _, state := range []string{"done", "expired"} {
		calls := 0
		s := &OpenAIGatewayService{
			cfg: &config.Config{},
			accountRepo: &studioNativeAccounts{account: &Account{ID: 90, Platform: PlatformGrok, Type: AccountTypeAPIKey,
				Credentials: map[string]any{"api_key": "native-secret"}}},
			httpUpstream: &quotedVideoHTTP{call: func(req *http.Request, id int64) (*http.Response, error) {
				calls++
				require.Equal(t, http.MethodGet, req.Method)
				require.Contains(t, req.URL.Path, "/videos/native-task")
				require.NotContains(t, req.URL.String(), "atlascloud")
				require.Equal(t, int64(90), id)
				return videoHTTPResponse(200, `{"request_id":"native-task","status":"`+state+`","video":{"url":"https://vidgen.x.ai/owned.mp4"}}`), nil
			}},
		}
		source, err := s.refreshStoredVideoSource(context.Background(), &VideoResult{Resource: "task:native-task", TaskID: "native-task", UserID: 16, APIKeyID: 20, AccountID: 90})
		if state == "done" {
			require.NoError(t, err)
			require.Equal(t, "https://vidgen.x.ai/owned.mp4", source)
		} else {
			require.ErrorIs(t, err, ErrVideoResultExpired)
			require.Empty(t, source)
		}
		require.Equal(t, 1, calls)
	}
}

type studioNativeArchiveRepository struct {
	UsageBillingRepository
	videoResultMemory
	receipt bool
}

func (r *studioNativeArchiveRepository) FindStudioNativeVideoReceipt(_ context.Context, owner VideoAttemptOwner, task string) (*VideoResult, error) {
	v := r.items["task:"+task]
	if !r.receipt || v == nil || v.UserID != owner.UserID || v.APIKeyID != owner.APIKeyID {
		return nil, ErrVideoResultNotFound
	}
	return v, nil
}

func TestStudioAssetVideoNativeArchiveUsesExistingAuthenticatedContentOnce(t *testing.T) {
	repo := &studioNativeArchiveRepository{receipt: true, videoResultMemory: videoResultMemory{items: map[string]*VideoResult{
		"task:native-task": {Resource: "task:native-task", TaskID: "native-task", UserID: 16, APIKeyID: 20, AccountID: 90},
	}}}
	objects := &videoObjectMemory{}
	data := append([]byte{0, 0, 0, 24, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm', 0, 0, 0, 0, 'i', 's', 'o', 'm', 'm', 'p', '4', '2'}, make([]byte, 32)...)
	requests := []string{}
	s := &OpenAIGatewayService{cfg: &config.Config{}, usageBillingRepo: repo,
		accountRepo: &studioNativeAccounts{account: &Account{ID: 90, Platform: PlatformGrok, Type: AccountTypeAPIKey,
			Credentials: map[string]any{"api_key": "native-secret"}}},
		httpUpstream: &quotedVideoHTTP{call: func(req *http.Request, _ int64) (*http.Response, error) {
			require.Equal(t, http.MethodGet, req.Method)
			require.Equal(t, "Bearer native-secret", req.Header.Get("Authorization"))
			require.Empty(t, req.Header.Get("X-Studio-Library"))
			requests = append(requests, req.URL.Path)
			if strings.HasSuffix(req.URL.Path, "/content") {
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"video/mp4"}}, ContentLength: int64(len(data)), Body: io.NopCloser(bytes.NewReader(data))}, nil
			}
			return videoHTTPResponse(200, `{"request_id":"native-task","status":"done","video":{"url":"https://upstream.example/v1/videos/native-task/content"}}`), nil
		}},
	}
	uploader := NewImageResultUploader(objects, "images/", 32<<20, nil)
	s.SetVideoResultStorage(func() (*ImageResultUploader, bool) { return uploader, true })
	require.NoError(t, s.videoResults.ArchivePending(context.Background(), s.refreshStoredVideoSource))
	require.Equal(t, []string{"/v1/videos/native-task", "/v1/videos/native-task/content"}, requests)
	require.Equal(t, 1, objects.saves)
	require.Len(t, objects.data, 1)
	require.NoError(t, s.videoResults.ArchivePending(context.Background(), s.refreshStoredVideoSource))
	require.Equal(t, 1, objects.saves)
	require.Len(t, requests, 2, "ready native archives are not downloaded or uploaded again")
}

func TestStudioAssetVideoNativePreviouslyQueuedArchiveAndDeliveryRequireReceipt(t *testing.T) {
	for _, upstreamTask := range []string{"native-task", "different-task"} {
		t.Run(upstreamTask, func(t *testing.T) {
			s, _, objects, downloads := videoResultFixture()
			v := &VideoResult{Resource: "task:native-task", TaskID: upstreamTask, UserID: 16, APIKeyID: 20,
				AccountID: 90, SourceURL: "https://vidgen.x.ai/unbilled.mp4"}
			repo := &studioNativeArchiveRepository{videoResultMemory: videoResultMemory{items: map[string]*VideoResult{v.Resource: v}}}
			s.repo = repo
			err := s.ArchivePending(context.Background(), nil)
			require.ErrorIs(t, err, ErrVideoResultNotFound)
			require.Zero(t, objects.saves)
			require.Empty(t, *downloads, "no unbilled source can be downloaded")
			v.StorageKey, v.MIME, v.Size = "video-results/16/20/old.mp4", "video/mp4", 42
			_, err = s.Playback(context.Background(), VideoAttemptOwner{UserID: 16, APIKeyID: 20}, v.Resource)
			require.ErrorIs(t, err, ErrVideoResultNotFound)
			require.Zero(t, objects.signs, "previously ready rows cannot bypass receipt proof")
			_, err = s.Open(context.Background(), VideoAttemptOwner{UserID: 16, APIKeyID: 20}, v.Resource, "")
			require.ErrorIs(t, err, ErrVideoResultNotFound)
		})
	}
}

func TestStudioAssetReconcileLimitsCallerCandidatesToFifty(t *testing.T) {
	_, err := NewStudioAssetService(nil, nil).Reconcile(context.Background(), StudioAssetOwner{16, 20}, strings.Fields(strings.Repeat("attempt:one ", 51)))
	require.ErrorIs(t, err, ErrStudioAssetInvalidRequest)
}

type studioRecoveryProgressRepo struct {
	studioArchiveMemory
	cleanupCalls int
}

func (r *studioRecoveryProgressRepo) ClaimDeletedStudioAssets(context.Context, int) ([]*StudioAsset, error) {
	r.cleanupCalls++
	return nil, nil
}

func (*studioRecoveryProgressRepo) ListStudioVideoIntakes(_ context.Context, limit int) ([]*StudioVideoIntake, error) {
	result := make([]*StudioVideoIntake, min(limit, 100))
	for i := range result {
		result[i] = &StudioVideoIntake{Owner: StudioAssetOwner{16, 20}, Resource: "task:slow"}
	}
	return result, nil
}

func TestStudioAssetWorkerRecoveryTimeoutCannotStarvePendingImage(t *testing.T) {
	repo := &studioRecoveryProgressRepo{}
	objects := &studioArchiveObjects{}
	uploader := NewImageResultUploader(objects, "images/", 32<<20, nil)
	s := NewStudioAssetService(repo, func() (*ImageResultUploader, bool) { return uploader, true })
	require.NoError(t, s.CollectImage(context.Background(), StudioAssetOwner{16, 20}, "imgtask_live", 0, "", "", nil, StudioImageOutput{Bytes: studioPNG(t)}))
	calls := 0
	s.recoverVideo = func(ctx context.Context, _ StudioAssetOwner, _ string) error {
		calls++
		<-ctx.Done()
		return ctx.Err()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_ = s.ProcessPending(ctx)
	require.Equal(t, "ready", repo.asset.State, "archive progress must precede non-progressing supplier recovery")
	require.Equal(t, 1, objects.saved)
	require.LessOrEqual(t, calls, 2, "one tick cannot attempt a hundred sequential supplier GETs")
}

func TestStudioAssetWorkerRecoveryTimeoutsCannotStarveCleanup(t *testing.T) {
	repo := &studioRecoveryProgressRepo{}
	s := NewStudioAssetService(repo, nil)
	calls := 0
	s.recoverVideo = func(ctx context.Context, _ StudioAssetOwner, _ string) error {
		calls++
		deadline, ok := ctx.Deadline()
		require.True(t, ok)
		require.LessOrEqual(t, time.Until(deadline), 2*time.Second)
		return context.DeadlineExceeded
	}
	gateway := &OpenAIGatewayService{studioAssets: s}
	gateway.processStudioAssetQueues(context.Background())
	require.Equal(t, 2, calls)
	require.Equal(t, 1, repo.cleanupCalls)
}

func TestStudioAssetVideoNativePreviouslyReadyLibraryContentRequiresReceipt(t *testing.T) {
	s, repo, objects := studioArchiveFixture(32<<20, nil)
	repo.video = &VideoResult{Resource: "task:native", TaskID: "native", UserID: 16, APIKeyID: 20,
		AccountID: 90, StorageKey: "video-results/16/20/native.mp4", MIME: "video/mp4", Size: 42}
	repo.asset = &StudioAsset{ID: "asset_0123456789abcdef0123456789abcdef", Owner: studioArchiveOwner,
		Kind: "video", VideoResource: repo.video.Resource, State: "ready", StorageKey: repo.video.StorageKey, SizeBytes: 42}
	objects.data = map[string][]byte{repo.video.StorageKey: []byte("unbilled private video")}
	content, err := s.Open(context.Background(), studioArchiveOwner, repo.asset.ID, "")
	require.ErrorIs(t, err, ErrStudioAssetUnavailable)
	require.Nil(t, content, "an old ready library row cannot deliver unbilled native content")
}
