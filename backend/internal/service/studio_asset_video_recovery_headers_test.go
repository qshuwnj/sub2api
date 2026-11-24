package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestStudioNativeVideoRecoverySuppressesInboundIdentityAndInternalHeaders(t *testing.T) {
	account := &Account{ID: 90, Platform: PlatformGrok, Type: AccountTypeAPIKey, Credentials: map[string]any{
		"api_key":                 "native-secret",
		"header_override_enabled": true,
		"header_overrides": map[string]any{
			"X-End-User":       "{{user_id}}",
			"X-Static":         "relay",
			"X-Studio-Library": "save",
			"X-Video-Quote":    "internal-quote",
			"Idempotency-Key":  "internal-key",
		},
	}}
	ctx := WithHeaderOverrideRequestScope(context.Background(), 16, 20, nil)
	calls := 0
	s := &OpenAIGatewayService{
		cfg:         &config.Config{},
		accountRepo: &studioNativeAccounts{account: account},
		httpUpstream: &quotedVideoHTTP{call: func(req *http.Request, _ int64) (*http.Response, error) {
			calls++
			require.Equal(t, http.MethodGet, req.Method)
			require.Equal(t, "Bearer native-secret", req.Header.Get("Authorization"))
			require.Empty(t, headerValuesFold(req.Header, "X-End-User"))
			require.Equal(t, []string{"relay"}, headerValuesFold(req.Header, "X-Static"))
			for _, name := range []string{"X-Studio-Library", "X-Video-Quote", "Idempotency-Key"} {
				require.Empty(t, headerValuesFold(req.Header, name), name)
			}
			if strings.HasSuffix(req.URL.Path, "/content") {
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"video/mp4"}},
					Body: io.NopCloser(strings.NewReader("video"))}, nil
			}
			return videoHTTPResponse(200, `{"request_id":"native-task","status":"done","video":{"url":"https://upstream.example/v1/videos/native-task/content"}}`), nil
		}},
	}
	v := &VideoResult{Resource: "task:native-task", TaskID: "native-task", UserID: 16, APIKeyID: 20, AccountID: 90}
	content, err := s.openStudioNativeVideoContent(ctx, v)
	require.NoError(t, err)
	require.NoError(t, content.Body.Close())
	require.Equal(t, 2, calls, "status and authenticated content requests must both strip identity")
	require.Empty(t, headerOverrideReportedHeaders(ctx, account.ID))
}
