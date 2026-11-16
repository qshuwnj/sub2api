//go:build unit

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

func TestDapacaiContentRedirectDestination(t *testing.T) {
	base := "https://dapacaihk-media.oss-cn-hongkong.aliyuncs.com"
	valid := base + "/video/2026/11/task_abc.mp4?Expires=123&Signature=fixture"
	got, err := dapacaiContentRedirectURL(valid, "dapacai:task_abc")
	require.NoError(t, err)
	require.Equal(t, valid, got)
	media := "https://media.dapacai.com/dapacai-media/video/2026/11/task_abc.mp4?X-Amz-Signature=fixture"
	got, err = dapacaiContentRedirectURL(media, "dapacai:task_abc")
	require.NoError(t, err)
	require.Equal(t, media, got)
	for _, target := range []string{
		"http" + strings.TrimPrefix(valid, "https"),
		strings.Replace(valid, "aliyuncs.com", "aliyuncs.com.evil.test", 1),
		strings.Replace(valid, base, "https://127.0.0.1", 1),
		strings.Replace(valid, base, "https://169.254.169.254", 1),
		strings.Replace(valid, base, base+":443", 1),
		strings.Replace(valid, "task_abc.mp4", "task_other.mp4", 1),
		strings.Replace(valid, "/video/", "/private/", 1),
		strings.Replace(valid, "/task_abc", "/%74ask_abc", 1),
		strings.Replace(valid, "https://", "https://user:password@", 1),
		valid + "#fragment", "//example.test/video.mp4", "/video.mp4", "",
		strings.Replace(media, "media.dapacai.com", "media.dapacai.com.evil.test", 1),
		strings.Replace(media, "media.dapacai.com", "media.dapacai.com:443", 1),
		strings.Replace(media, "/dapacai-media/video/", "/video/", 1),
		strings.Replace(media, "task_abc.mp4", "task_other.mp4", 1),
		strings.Replace(media, "/task_abc", "/%74ask_abc", 1),
		strings.Replace(media, "/video/2026/11/", "/video/2026/13/", 1),
		strings.Replace(media, "https://", "http://", 1),
		strings.Replace(media, "https://", "https://user:password@", 1),
		media + "#fragment",
		strings.Replace(valid, "/video/", "/dapacai-media/video/", 1),
	} {
		_, err := dapacaiContentRedirectURL(target, "dapacai:task_abc")
		require.Error(t, err, target)
	}
	_, err = dapacaiContentRedirectURL(valid, "task_abc")
	require.Error(t, err)
}

func TestForwardDapacaiContentRedirectStripsSecretsAndPreservesRange(t *testing.T) {
	const target = "https://dapacaihk-media.oss-cn-hongkong.aliyuncs.com/video/2026/11/task_abc.mp4?Signature=fixture"
	upstream := &grokMediaContentUpstreamStub{responses: []*http.Response{
		{StatusCode: 302, Header: http.Header{"Location": {target}}, Body: io.NopCloser(strings.NewReader(""))},
		{StatusCode: 206, Header: http.Header{"Content-Type": {"video/mp4"}, "Content-Range": {"bytes 0-12/100"}, "Content-Length": {"13"}},
			Body: io.NopCloser(strings.NewReader("video-payload"))},
	}}
	svc := &OpenAIGatewayService{httpUpstream: upstream}
	c, recorder := grokMediaContentTestContext("GET", "https://gateway.example/v1/videos/dapacai:task_abc/content",
		map[string]string{"Range": "bytes=0-12", "Cookie": "private", "Authorization": "Bearer customer"})
	_, err := svc.ForwardDapacaiVideo(context.Background(), c, dapacaiTestAccount(), DapacaiEndpointVideoContent, "dapacai:task_abc", nil)
	require.NoError(t, err)
	require.Equal(t, 206, recorder.Code)
	require.Equal(t, "video-payload", recorder.Body.String())
	require.Len(t, upstream.requests, 2)
	require.Equal(t, "Bearer test-upstream-key", upstream.requests[0].Header.Get("Authorization"))
	cdn := upstream.requests[1]
	require.Equal(t, target, cdn.URL.String())
	require.Empty(t, cdn.Header.Get("Authorization"))
	require.Empty(t, cdn.Header.Get("Cookie"))
	require.Equal(t, "bytes=0-12", cdn.Header.Get("Range"))
	require.True(t, HTTPUpstreamRedirectsDisabled(cdn.Context()))
	require.True(t, HTTPUpstreamPublicHostsOnly(cdn.Context()))
}

func TestForwardDapacaiNewMediaRedirectStripsSecretsAndPreservesRange(t *testing.T) {
	const target = "https://media.dapacai.com/dapacai-media/video/2026/11/task_abc.mp4?X-Amz-Signature=fixture"
	upstream := &grokMediaContentUpstreamStub{responses: []*http.Response{
		{StatusCode: 302, Header: http.Header{"Location": {target}}, Body: io.NopCloser(strings.NewReader(""))},
		{StatusCode: 206, Header: http.Header{"Content-Type": {"video/mp4"}, "Content-Range": {"bytes 0-12/100"}},
			Body: io.NopCloser(strings.NewReader("video-payload"))},
	}}
	svc := &OpenAIGatewayService{httpUpstream: upstream}
	c, recorder := grokMediaContentTestContext("GET", "https://gateway.example/v1/videos/dapacai:task_abc/content",
		map[string]string{"Range": "bytes=0-12", "Cookie": "private", "Authorization": "Bearer customer"})
	_, err := svc.ForwardDapacaiVideo(context.Background(), c, dapacaiTestAccount(), DapacaiEndpointVideoContent, "dapacai:task_abc", nil)
	require.NoError(t, err)
	require.Equal(t, 206, recorder.Code)
	require.Equal(t, "video-payload", recorder.Body.String())
	require.Len(t, upstream.requests, 2)
	require.Equal(t, http.MethodGet, upstream.requests[0].Method)
	require.Equal(t, http.MethodGet, upstream.requests[1].Method)
	require.Equal(t, target, upstream.requests[1].URL.String())
	require.Empty(t, upstream.requests[1].Header.Get("Authorization"))
	require.Empty(t, upstream.requests[1].Header.Get("Cookie"))
	require.Equal(t, "bytes=0-12", upstream.requests[1].Header.Get("Range"))
	require.True(t, HTTPUpstreamRedirectsDisabled(upstream.requests[1].Context()))
	require.True(t, HTTPUpstreamPublicHostsOnly(upstream.requests[1].Context()))
}

func TestDapacaiVideoArchiveFollowsNewMediaWithoutRecreatingTask(t *testing.T) {
	const task = "dapacai:task_saved"
	const target = "https://media.dapacai.com/dapacai-media/video/2026/11/task_saved.mp4?X-Amz-Signature=fixture"
	repo := &studioNativeArchiveRepository{receipt: true, videoResultMemory: videoResultMemory{items: map[string]*VideoResult{
		"task:" + task: {Resource: "task:" + task, TaskID: task, UserID: 16, APIKeyID: 20, AccountID: 19},
	}}}
	objects := &videoObjectMemory{}
	data := append([]byte{0, 0, 0, 24, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm', 0, 0, 0, 0, 'i', 's', 'o', 'm', 'm', 'p', '4', '2'}, make([]byte, 32)...)
	var paths []string
	s := &OpenAIGatewayService{cfg: &config.Config{}, usageBillingRepo: repo,
		accountRepo: &studioNativeAccounts{account: dapacaiTestAccount()},
		httpUpstream: &quotedVideoHTTP{call: func(req *http.Request, _ int64) (*http.Response, error) {
			require.Equal(t, http.MethodGet, req.Method)
			paths = append(paths, req.URL.Path)
			if req.URL.Host == "media.dapacai.com" {
				require.Equal(t, target, req.URL.String())
				require.Empty(t, req.Header.Get("Authorization"))
				require.Empty(t, req.Header.Get("Cookie"))
				require.True(t, HTTPUpstreamPublicHostsOnly(req.Context()))
				require.True(t, HTTPUpstreamRedirectsDisabled(req.Context()))
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"video/mp4"}}, ContentLength: int64(len(data)), Body: io.NopCloser(bytes.NewReader(data))}, nil
			}
			require.Equal(t, "api.dapacai.com", req.URL.Host)
			require.Equal(t, "Bearer test-upstream-key", req.Header.Get("Authorization"))
			if strings.HasSuffix(req.URL.Path, "/content") {
				return &http.Response{StatusCode: 302, Header: http.Header{"Location": {target}}, Body: io.NopCloser(strings.NewReader(""))}, nil
			}
			return videoHTTPResponse(200, `{"id":"task_saved","status":"completed"}`), nil
		}},
	}
	uploader := NewImageResultUploader(objects, "images/", 32<<20, nil)
	s.SetVideoResultStorage(func() (*ImageResultUploader, bool) { return uploader, true })
	require.NoError(t, s.videoResults.ArchivePending(context.Background(), s.refreshStoredVideoSource))
	require.Equal(t, []string{"/v1/videos/task_saved", "/v1/videos/task_saved/content", "/dapacai-media/video/2026/11/task_saved.mp4"}, paths)
	require.Equal(t, 1, objects.saves)
	require.NoError(t, s.videoResults.ArchivePending(context.Background(), s.refreshStoredVideoSource))
	require.Equal(t, 1, objects.saves)
	require.Len(t, paths, 3)
}

func TestDapacaiContentRejectsAdditionalRedirectAndNonVideo(t *testing.T) {
	const target = "https://dapacaihk-media.oss-cn-hongkong.aliyuncs.com/video/2026/11/task_abc.mp4"
	upstream := &grokMediaContentUpstreamStub{response: &http.Response{
		StatusCode: 302, Header: http.Header{"Location": {"https://evil.test"}}, Body: io.NopCloser(strings.NewReader("")),
	}}
	svc := &OpenAIGatewayService{httpUpstream: upstream}
	_, err := svc.followDapacaiContentRedirect(context.Background(), dapacaiTestAccount(), "dapacai:task_abc", "", "",
		&http.Response{StatusCode: 302, Header: http.Header{"Location": {target}}, Body: io.NopCloser(strings.NewReader(""))})
	require.ErrorContains(t, err, "more than once")
	require.Len(t, upstream.requests, 1)
	require.True(t, dapacaiVideoContentType("application/octet-stream; charset=binary"))
	require.True(t, dapacaiVideoContentType("video/mp4"))
	require.False(t, dapacaiVideoContentType("application/json"))
	require.False(t, dapacaiVideoContentType("text/html"))
}
