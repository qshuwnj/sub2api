//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDapacaiContentRedirectDestination(t *testing.T) {
	base := "https://dapacaihk-media.oss-cn-hongkong.aliyuncs.com"
	valid := base + "/video/2026/11/task_abc.mp4?Expires=123&Signature=fixture"
	got, err := dapacaiContentRedirectURL(valid, "dapacai:task_abc")
	require.NoError(t, err)
	require.Equal(t, valid, got)
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
