//go:build unit

package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestDapacaiVideoNestedRejectionsKeepSafeOpsReason(t *testing.T) {
	for _, tc := range []struct{ code, want string }{
		{"INVALID_DURATION", "视频时长"},
		{"INVALID_RATIO", "视频比例"},
		{"INVALID_RESOLUTION", "视频清晰度"},
		{"INVALID_PROMPT", "视频提示词"},
		{"INSUFFICIENT_POINTS", "账号额度不足"},
		{"task_not_exist", "没有找到这个视频任务"},
	} {
		t.Run(tc.code, func(t *testing.T) {
			inner, err := json.Marshal(map[string]any{"error": map[string]string{
				"code": tc.code, "message": "supplier validation detail", "request_id": "req_fixture_123",
			}})
			require.NoError(t, err)
			body, err := json.Marshal(map[string]string{"code": "fail_to_fetch_task", "message": string(inner)})
			require.NoError(t, err)
			upstream := &grokMediaContentUpstreamStub{response: &http.Response{
				StatusCode: http.StatusBadRequest, Header: http.Header{},
				Body: io.NopCloser(strings.NewReader(string(body))),
			}}
			svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
			c, recorder := grokMediaContentTestContext("POST", "https://gateway.example/v1/videos", nil)
			_, err = svc.ForwardDapacaiVideo(context.Background(), c, dapacaiTestAccount(),
				DapacaiEndpointVideoCreate, "", dapacaiTestBody("seedance-2.5-plus-8"))
			require.ErrorContains(t, err, tc.code)
			require.Equal(t, http.StatusBadRequest, recorder.Code)
			require.Contains(t, recorder.Body.String(), tc.want)
			require.NotContains(t, recorder.Body.String(), "supplier validation detail")
			require.Len(t, upstream.requests, 1, "never retry a rejected generation")
			require.Equal(t, http.StatusBadRequest, c.GetInt(OpsUpstreamStatusCodeKey))
			require.Equal(t, "supplier validation detail", c.GetString(OpsUpstreamErrorMessageKey))
			raw, exists := c.Get(OpsUpstreamErrorsKey)
			require.True(t, exists)
			events := raw.([]*OpsUpstreamErrorEvent)
			require.Len(t, events, 1)
			require.Equal(t, tc.code, events[0].Reason)
			require.Equal(t, "req_fixture_123", events[0].UpstreamRequestID)
			require.Equal(t, "https://api.dapacai.com/v1/videos", events[0].UpstreamURL)
		})
	}
}

func TestDapacaiVideoFailureParsingAndBounds(t *testing.T) {
	for _, body := range []string{
		`{"error":{"code":"INVALID_RATIO","message":"unsupported ratio"}}`,
		`{"code":"INVALID_RATIO","message":"unsupported ratio"}`,
	} {
		failure := parseDapacaiVideoFailure([]byte(body), 0)
		require.Equal(t, "INVALID_RATIO", failure.Code)
		require.Equal(t, "unsupported ratio", failure.Message)
	}
	require.Empty(t, parseDapacaiVideoFailure([]byte(`<html>not JSON</html>`), 0))
	require.Empty(t, parseDapacaiVideoFailure([]byte(`{"error":`), 0))
	require.Empty(t, parseDapacaiVideoFailure([]byte(`{"code":"INVALID_RATIO"}`), 5))
	for _, status := range []int{302, 400, 401, 402, 403, 404, 429, 500, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			credential := "test-upstream-key"
			body := `{"error":{"code":"UNRECOGNIZED","message":"` + credential +
				` https://assets.example/private.png?signature=secret Bearer other-secret sk-abcdefghijklmnopqrstuvwxyz"}}`
			upstream := &grokMediaContentUpstreamStub{response: &http.Response{
				StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)),
			}}
			svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
			c, recorder := grokMediaContentTestContext("POST", "https://gateway.example/v1/videos", nil)
			_, err := svc.ForwardDapacaiVideo(context.Background(), c, dapacaiTestAccount(),
				DapacaiEndpointVideoCreate, "", dapacaiTestBody("seedance-2.5-plus-8"))
			require.Error(t, err)
			wantStatus, _ := dapacaiVideoFailureMessage(status, "")
			require.Equal(t, wantStatus, recorder.Code)
			diagnostics := c.GetString(OpsUpstreamErrorDetailKey)
			for _, secret := range []string{credential, "private.png", "signature=secret", "other-secret", "sk-abcdefghijklmnopqrstuvwxyz"} {
				require.NotContains(t, diagnostics, secret)
				require.NotContains(t, recorder.Body.String(), secret)
				require.NotContains(t, err.Error(), secret)
			}
			require.NotContains(t, recorder.Body.String(), "UNRECOGNIZED")
			require.Len(t, upstream.requests, 1)
			require.Equal(t, status, c.GetInt(OpsUpstreamStatusCodeKey))
		})
	}
	for _, body := range []string{"<html>secret-upstream-body</html>", strings.Repeat("x", (32<<10)+1)} {
		upstream := &grokMediaContentUpstreamStub{response: &http.Response{
			StatusCode: 400, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)),
		}}
		svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
		c, recorder := grokMediaContentTestContext("POST", "https://gateway.example/v1/videos", nil)
		_, err := svc.ForwardDapacaiVideo(context.Background(), c, dapacaiTestAccount(),
			DapacaiEndpointVideoCreate, "", dapacaiTestBody("seedance-2.5-plus-8"))
		require.Error(t, err)
		require.NotContains(t, recorder.Body.String(), body)
		require.NotContains(t, c.GetString(OpsUpstreamErrorDetailKey), body)
		require.Len(t, upstream.requests, 1)
	}
	require.True(t, utf8.ValidString(sanitizeDapacaiVideoDiagnostic(strings.Repeat("界", 600), "")))
	require.LessOrEqual(t, len([]rune(sanitizeDapacaiVideoDiagnostic(strings.Repeat("界", 600), ""))), 515)
}
