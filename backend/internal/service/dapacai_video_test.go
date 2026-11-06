//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func dapacaiTestAccount() *Account {
	return &Account{ID: 19, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{
		"base_url": "https://api.dapacai.com", "api_key": "test-upstream-key",
	}}
}
func dapacaiTestBody(model string) []byte {
	p := dapacaiVideoModel(model)
	req := map[string]any{"model": model, "prompt": "test", "duration": p.durations[0], "ratio": p.ratios[0]}
	if len(p.resolutions) > 0 {
		req["resolution"] = p.resolutions[0]
	}
	if p.requiresReference {
		req["images"] = []string{"https://assets.example/image.png"}
	}
	body, _ := json.Marshal(req)
	return body
}

func TestDapacaiAccountCapabilityCannotLeakToOldProvider(t *testing.T) {
	a := dapacaiTestAccount()
	require.True(t, a.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityDapacaiVideo))
	require.False(t, a.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityAtlasCloudVideo))
	for _, base := range []string{"http://api.dapacai.com", "https://api.dapacai.com.evil.test", "https://api.dapacai.com@evil.test", "https://api.atlascloud.ai", "https://api.dapacai.com:443", "https://api.dapacai.com/v1/../evil", "https://api.dapacai.com?token=x"} {
		a.Credentials["base_url"] = base
		require.False(t, a.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityDapacaiVideo), base)
	}
	a.Credentials["base_url"] = "https://api.atlascloud.ai"
	require.True(t, a.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityAtlasCloudVideo))
}

func TestDapacaiContractsUseExactVariantControls(t *testing.T) {
	models := []string{"seedance-2.0", "seedance-2.0-plus", "seedance-2.5-plus", "seedance-2.5-plus-1", "seedance-2.5-plus-2", "seedance-2.5-plus-3", "seedance-2.5-plus-4", "seedance-2.5-plus-5", "seedance-2.5-plus-6", "seedance-2.5-plus-7", "wan-3.0"}
	for _, model := range models {
		t.Run(model, func(t *testing.T) {
			info, err := ParseDapacaiVideoRequest(dapacaiTestBody(model))
			require.NoError(t, err)
			require.Equal(t, model, info.Model)
			require.False(t, IsAtlasCloudVideoModel(model))
			require.NotEmpty(t, info.Resolution)
			require.Equal(t, 30, NormalizeVideoBillingDurationForModel("seedance-2.5-plus-1", 30))
			_, err = VideoReferenceFamilyForModel(model)
			require.NoError(t, err)
			_, err = VideoFrameFamilyForModel(model)
			require.NoError(t, err)
		})
	}
	for _, body := range []string{
		`{"model":"minimax-h3","prompt":"test","duration":5,"ratio":"16:9"}`,
		`{"model":"seedance-2.5-plus-99","prompt":"test","duration":5,"ratio":"16:9"}`,
		`{"model":"seedance-2.0","prompt":"test","duration":30,"ratio":"16:9"}`,
		`{"model":"seedance-2.0","prompt":"test","duration":5,"ratio":"16:9","resolution":"720p"}`,
		`{"model":"seedance-2.5-plus-4","prompt":"test","duration":5,"ratio":"16:9","resolution":"720p"}`,
		`{"model":"seedance-2.5-plus-4","prompt":"test","duration":15,"ratio":"16:9","resolution":"1080p"}`,
		`{"model":"seedance-2.5-plus","prompt":"test","duration":5,"ratio":"16:9","resolution":"480p"}`,
		`{"model":"seedance-2.0","prompt":"test","duration":5,"ratio":"16:9","images":["http://example.com/a.png"]}`,
		`{"model":"seedance-2.0","prompt":"test","duration":5,"ratio":"16:9","images":["https://127.0.0.1/a.png"]}`,
		`{"model":"seedance-2.0","prompt":"test","duration":5,"ratio":"16:9","speed":"fast"}`,
		`{"model":"seedance-2.0","prompt":"test","duration":5,"ratio":"16:9"} {}`,
	} {
		_, err := ParseDapacaiVideoRequest([]byte(body))
		require.Error(t, err, body)
	}
}

func TestDapacaiBillingNeverUsesAtlasOrFlatFallbackAndPreservesThirtySeconds(t *testing.T) {
	flat := 42.0
	key := &APIKey{Group: &Group{VideoPrice480P: &flat, VideoModelPrices: map[string]map[string]float64{
		"seedance-2.5-plus-1":                  {"native": 0.1},
		"bytedance/seedance-2.0/text-to-video": {"480p": 9},
	}}}
	require.True(t, HasDapacaiVideoPrice(key, "seedance-2.5-plus-1", "native"))
	require.False(t, HasDapacaiVideoPrice(key, "seedance-2.0", "native"))
	cost := (&BillingService{}).CalculateVideoCost("seedance-2.5-plus-1", "native", 1, 30, videoPriceConfigFromAPIKey(key), 1.2)
	require.InDelta(t, 3, cost.TotalCost, 1e-10)
	require.InDelta(t, 3.6, cost.ActualCost, 1e-10)
	normalized := NormalizeVideoModelPrices(key.Group.VideoModelPrices)
	require.Equal(t, 0.1, normalized["seedance-2.5-plus-1"]["native"])
	require.Equal(t, 9.0, normalized["bytedance/seedance-2.0/text-to-video"]["480p"])
}

func TestForwardDapacaiSubmitsExactlyOnceAndNamespacesTask(t *testing.T) {
	upstream := &grokMediaContentUpstreamStub{response: grokMediaContentStatusResponse(`{"id":"task_abc","status":"queued","model":"seedance-2.0"}`)}
	svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
	c, recorder := grokMediaContentTestContext("POST", "https://gateway.example/v1/videos", map[string]string{"x-api-key": "customer-key", "Cookie": "private-cookie"})
	result, err := svc.ForwardDapacaiVideo(context.Background(), c, dapacaiTestAccount(), DapacaiEndpointVideoCreate, "", dapacaiTestBody("seedance-2.0"))
	require.NoError(t, err)
	require.Equal(t, "dapacai:task_abc", result.ResponseID)
	require.Equal(t, 0, result.VideoCount)
	require.Equal(t, "", recorder.Body.String(), "handler must bind ownership before writing acceptance")
	require.Len(t, upstream.requests, 1)
	require.Equal(t, "/v1/videos", upstream.request.URL.Path)
	require.Equal(t, "Bearer test-upstream-key", upstream.request.Header.Get("Authorization"))
	require.Empty(t, upstream.request.Header.Get("x-api-key"))
	require.Empty(t, upstream.request.Header.Get("Cookie"))
	require.Empty(t, upstream.request.Header.Get("Idempotency-Key"))
}

func TestForwardDapacaiNumberedPlusAllowsPromptOnlyWithoutSubstitution(t *testing.T) {
	for _, model := range []string{"seedance-2.5-plus-6", "seedance-2.5-plus-7"} {
		t.Run(model, func(t *testing.T) {
			body := dapacaiTestBody(model)
			info, err := ParseDapacaiVideoRequest(body)
			require.NoError(t, err)
			require.Equal(t, model, info.Model)
			upstream := &grokMediaContentUpstreamStub{response: grokMediaContentStatusResponse(`{"id":"task_text","status":"queued","model":"` + model + `"}`)}
			svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
			c, recorder := grokMediaContentTestContext("POST", "https://gateway.example/v1/videos", nil)
			result, err := svc.ForwardDapacaiVideo(context.Background(), c, dapacaiTestAccount(), DapacaiEndpointVideoCreate, "", body)
			require.NoError(t, err)
			require.Equal(t, "dapacai:task_text", result.ResponseID)
			require.Empty(t, recorder.Body.String())
			require.Len(t, upstream.requests, 1)
			require.Equal(t, "/v1/videos", upstream.request.URL.Path)
			forwarded, err := io.ReadAll(upstream.request.Body)
			require.NoError(t, err)
			require.JSONEq(t, string(body), string(forwarded), "preserve the model and do not inject dummy media")
			var payload map[string]any
			require.NoError(t, json.Unmarshal(forwarded, &payload))
			require.Equal(t, model, payload["model"])
			for _, field := range []string{"input", "images", "image_url", "media", "videos", "audios"} {
				require.NotContains(t, payload, field)
			}
		})
	}
}

func TestForwardDapacaiUploadedMediaKeepsExactURLsAndTypes(t *testing.T) {
	for _, model := range []string{"seedance-2.0", "seedance-2.0-plus", "seedance-2.5-plus", "seedance-2.5-plus-1", "seedance-2.5-plus-2", "seedance-2.5-plus-3", "seedance-2.5-plus-4", "seedance-2.5-plus-5", "seedance-2.5-plus-6", "seedance-2.5-plus-7", "wan-3.0"} {
		t.Run(model, func(t *testing.T) {
			var payload map[string]any
			require.NoError(t, json.Unmarshal(dapacaiTestBody(model), &payload))
			delete(payload, "images")
			media := []map[string]string{{"type": "image", "url": "https://r2.example/reference.png"}}
			if dapacaiVideoModel(model).audios > 0 {
				media = append(media, map[string]string{"type": "audio", "url": "https://r2.example/reference.mp3?signature=fixture"})
			}
			payload["input"] = map[string]any{"prompt": "test", "media": media}
			body, err := json.Marshal(payload)
			require.NoError(t, err)
			upstream := &grokMediaContentUpstreamStub{response: grokMediaContentStatusResponse(`{"id":"task_media","status":"queued","model":"` + model + `"}`)}
			svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
			c, _ := grokMediaContentTestContext("POST", "https://gateway.example/v1/videos", nil)
			result, err := svc.ForwardDapacaiVideo(context.Background(), c, dapacaiTestAccount(), DapacaiEndpointVideoCreate, "", body)
			require.NoError(t, err)
			require.Equal(t, model, result.UpstreamModel)
			require.Len(t, upstream.requests, 1)
			require.Equal(t, "https://api.dapacai.com/v1/videos", upstream.request.URL.String())
			forwarded, err := io.ReadAll(upstream.request.Body)
			require.NoError(t, err)
			require.JSONEq(t, string(body), string(forwarded))
		})
	}
}

func TestForwardDapacaiEveryModelPreservesExactIDAndRejectsRemapping(t *testing.T) {
	for _, model := range []string{"seedance-2.0", "seedance-2.0-plus", "seedance-2.5-plus", "seedance-2.5-plus-1", "seedance-2.5-plus-2", "seedance-2.5-plus-3", "seedance-2.5-plus-4", "seedance-2.5-plus-5", "seedance-2.5-plus-6", "seedance-2.5-plus-7", "wan-3.0"} {
		t.Run(model, func(t *testing.T) {
			body := dapacaiTestBody(model)
			upstream := &grokMediaContentUpstreamStub{response: grokMediaContentStatusResponse(`{"id":"task_exact","status":"queued","model":"` + model + `"}`)}
			svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
			account := dapacaiTestAccount()
			account.Credentials["model_mapping"] = map[string]any{model: model}
			c, _ := grokMediaContentTestContext("POST", "https://gateway.example/v1/videos", nil)
			result, err := svc.ForwardDapacaiVideo(context.Background(), c, account, DapacaiEndpointVideoCreate, "", body)
			require.NoError(t, err)
			require.Equal(t, model, result.Model)
			require.Equal(t, model, result.UpstreamModel)
			require.Equal(t, model, result.BillingModel)
			require.Len(t, upstream.requests, 1)
			require.Equal(t, "https://api.dapacai.com/v1/videos", upstream.request.URL.String())
			forwarded, err := io.ReadAll(upstream.request.Body)
			require.NoError(t, err)
			require.JSONEq(t, string(body), string(forwarded))

			for _, target := range []string{"seedance-2.5-plus-7", "bytedance/seedance-2.0/text-to-video"} {
				if target == model {
					target = "seedance-2.0"
				}
				account = dapacaiTestAccount()
				account.Credentials["model_mapping"] = map[string]any{model: target}
				c, _ = grokMediaContentTestContext("POST", "https://gateway.example/v1/videos", nil)
				_, err = svc.ForwardDapacaiVideo(context.Background(), c, account, DapacaiEndpointVideoCreate, "", body)
				require.ErrorContains(t, err, "requires the exact upstream model ID")
				require.Len(t, upstream.requests, 1, "reject substitution before contacting the supplier")
			}
		})
	}
}

func TestDapacaiFramesRequireExplicitModelRoles(t *testing.T) {
	for _, model := range []string{"seedance-2.0", "seedance-2.0-plus", "seedance-2.5-plus", "seedance-2.5-plus-1", "seedance-2.5-plus-2", "seedance-2.5-plus-3", "seedance-2.5-plus-4", "seedance-2.5-plus-5", "seedance-2.5-plus-6", "seedance-2.5-plus-7", "wan-3.0"} {
		t.Run(model, func(t *testing.T) {
			var payload map[string]any
			require.NoError(t, json.Unmarshal(dapacaiTestBody(model), &payload))
			delete(payload, "images")
			payload["input"] = map[string]any{"prompt": "test", "media": []map[string]string{
				{"type": "first_frame", "url": "https://assets.example/first.png"},
				{"type": "last_frame", "url": "https://assets.example/last.png"},
			}}
			body, err := json.Marshal(payload)
			require.NoError(t, err)
			info, err := ParseDapacaiVideoRequest(body)
			if model == "seedance-2.0" {
				require.NoError(t, err)
				require.Equal(t, model, info.Model)
				require.Len(t, info.InputImageURLs, 2)
			} else {
				require.ErrorContains(t, err, "does not support start/end frames")
			}
		})
	}
}

func TestForwardDapacaiCompletionNeedsNoPublicOutputURL(t *testing.T) {
	upstream := &grokMediaContentUpstreamStub{response: grokMediaContentStatusResponse(`{"id":"task_abc","status":"completed"}`)}
	svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
	c, recorder := grokMediaContentTestContext("GET", "https://gateway.example/v1/videos/dapacai:task_abc", nil)
	result, err := svc.ForwardDapacaiVideo(context.Background(), c, dapacaiTestAccount(), DapacaiEndpointVideoStatus, "dapacai:task_abc", nil)
	require.NoError(t, err)
	require.Equal(t, 1, result.VideoCount)
	require.Empty(t, recorder.Body.String(), "no unpaid completed response")
	require.True(t, dapacaiCompletedStatus([]byte(`{"id":"task_abc","status":"completed"}`), "dapacai:task_abc"))
	require.False(t, dapacaiCompletedStatus([]byte(`{"id":"task_other","status":"completed"}`), "dapacai:task_abc"))
}

func TestForwardDapacaiDoesNotFollowRedirectOrExposeRawErrors(t *testing.T) {
	for _, status := range []int{302, 401, 402, 503} {
		upstream := &grokMediaContentUpstreamStub{response: &http.Response{StatusCode: status,
			Header: http.Header{"Location": []string{"https://untrusted.example"}}, Body: io.NopCloser(strings.NewReader("secret-upstream-body"))}}
		svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
		c, recorder := grokMediaContentTestContext("POST", "https://gateway.example/v1/videos", nil)
		_, err := svc.ForwardDapacaiVideo(context.Background(), c, dapacaiTestAccount(), DapacaiEndpointVideoCreate, "", dapacaiTestBody("seedance-2.0"))
		require.Error(t, err)
		require.Len(t, upstream.requests, 1)
		require.NotContains(t, recorder.Body.String(), "secret-upstream-body")
		require.NotContains(t, recorder.Body.String(), "test-upstream-key")
	}
}

func TestForwardDapacaiContentUsesOwnedAccountAndRange(t *testing.T) {
	upstream := &grokMediaContentUpstreamStub{response: &http.Response{StatusCode: 206,
		Header: http.Header{"Content-Type": []string{"video/mp4"}, "Content-Range": []string{"bytes 0-3/100"}}, Body: io.NopCloser(strings.NewReader("data"))}}
	svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
	c, recorder := grokMediaContentTestContext("GET", "https://gateway.example/v1/videos/dapacai:task_abc/content", map[string]string{"Range": "bytes=0-3"})
	_, err := svc.ForwardDapacaiVideo(context.Background(), c, dapacaiTestAccount(), DapacaiEndpointVideoContent, "dapacai:task_abc", nil)
	require.NoError(t, err)
	require.Equal(t, "/v1/videos/task_abc/content", upstream.request.URL.Path)
	require.Equal(t, "bytes=0-3", upstream.request.Header.Get("Range"))
	require.Equal(t, 206, recorder.Code)
	require.Equal(t, "data", recorder.Body.String())
}

func TestDapacaiVideoArchiveUsesAuthenticatedContentAndDoesNotRecreateTask(t *testing.T) {
	task := "dapacai:task_saved"
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
			require.Equal(t, "Bearer test-upstream-key", req.Header.Get("Authorization"))
			paths = append(paths, req.URL.Path)
			if strings.HasSuffix(req.URL.Path, "/content") {
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"video/mp4"}}, ContentLength: int64(len(data)), Body: io.NopCloser(bytes.NewReader(data))}, nil
			}
			return videoHTTPResponse(200, `{"id":"task_saved","status":"completed"}`), nil
		}},
	}
	uploader := NewImageResultUploader(objects, "images/", 32<<20, nil)
	s.SetVideoResultStorage(func() (*ImageResultUploader, bool) { return uploader, true })
	require.NoError(t, s.videoResults.ArchivePending(context.Background(), s.refreshStoredVideoSource))
	require.Equal(t, []string{"/v1/videos/task_saved", "/v1/videos/task_saved/content"}, paths)
	require.Equal(t, 1, objects.saves)
	require.NoError(t, s.videoResults.ArchivePending(context.Background(), s.refreshStoredVideoSource))
	require.Equal(t, 1, objects.saves)
	require.Len(t, paths, 2)
}
