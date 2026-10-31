package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

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
}

func TestStudioAssetVideoNativeArchiveUsesExistingAuthenticatedContentOnce(t *testing.T) {
	repo := &studioNativeArchiveRepository{videoResultMemory: videoResultMemory{items: map[string]*VideoResult{
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

func TestStudioAssetReconcileLimitsCallerCandidatesToFifty(t *testing.T) {
	_, err := NewStudioAssetService(nil, nil).Reconcile(context.Background(), StudioAssetOwner{16, 20}, strings.Fields(strings.Repeat("attempt:one ", 51)))
	require.ErrorIs(t, err, ErrStudioAssetInvalidRequest)
}
