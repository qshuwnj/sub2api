//go:build unit

package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestAtlasCloudImagePayload(t *testing.T) {
	parsed := &OpenAIImagesRequest{Prompt: "draw", N: 1, Size: "2848x1600", Quality: "standard"}
	payload, err := buildAtlasCloudImagePayload(parsed, "bytedance/seedream-v4.5")
	require.NoError(t, err)
	require.JSONEq(t, `{"model":"bytedance/seedream-v4.5","prompt":"draw","size":"2848*1600"}`, string(payload))
	for _, tc := range []struct {
		name string
		req  OpenAIImagesRequest
	}{
		{"batch", OpenAIImagesRequest{Prompt: "draw", N: 2}},
		{"stream", OpenAIImagesRequest{Prompt: "draw", N: 1, Stream: true}},
		{"bad_size", OpenAIImagesRequest{Prompt: "draw", N: 1, Size: "1024x1024"}},
		{"empty_prompt", OpenAIImagesRequest{N: 1}},
		{"edit", OpenAIImagesRequest{Prompt: "draw", N: 1, Endpoint: openAIImagesEditsEndpoint}},
		{"unsupported_quality", OpenAIImagesRequest{Prompt: "draw", N: 1, Quality: "high"}},
		{"seed", OpenAIImagesRequest{Prompt: "draw", N: 1, Body: []byte(`{"prompt":"draw","seed":123}`)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := buildAtlasCloudImagePayload(&tc.req, "bytedance/seedream-v4.5")
			require.Error(t, err)
		})
	}
	_, err = buildAtlasCloudImagePayload(parsed, "bytedance/seedance-video")
	require.Error(t, err)
}

func TestAtlasCloudGenericImagePayload(t *testing.T) {
	model := "google/nano-banana-2-lite/text-to-image"
	parsed := &OpenAIImagesRequest{
		Model:          model,
		Prompt:         "draw a quiet mountain lake",
		N:              1,
		ResponseFormat: "url",
		Body:           []byte(`{"model":"google/nano-banana-2-lite/text-to-image","prompt":"draw a quiet mountain lake","n":1,"response_format":"url"}`),
	}
	payload, err := buildAtlasCloudImagePayload(parsed, model)
	require.NoError(t, err)
	require.JSONEq(t, `{"model":"google/nano-banana-2-lite/text-to-image","prompt":"draw a quiet mountain lake","n":1,"response_format":"url"}`, string(payload))
}

func TestAtlasCloudImagesResponseSizeIsBounded(t *testing.T) {
	upstream := &atlasImageUpstream{replies: []string{strings.Repeat("x", atlasCloudBodyLimit+1)}}
	svc, c, rec, account, parsed := atlasImageFixture(t, upstream)
	_, err := svc.ForwardImages(context.Background(), c, account, parsed.Body, parsed, "")
	require.Error(t, err)
	require.Equal(t, 502, rec.Code)
	require.Len(t, upstream.methods, 1)
}

func TestAtlasCloudImagesRequireAPIKey(t *testing.T) {
	for _, model := range []string{"bytedance-seedream-v4.5", "bytedance/seedream-v4.5"} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		body := []byte(`{"model":"` + model + `","prompt":"draw","size":"2048x2048"}`)
		c.Request = httptest.NewRequest(http.MethodPost, openAIImagesGenerationsEndpoint, bytes.NewReader(body))
		parsed, err := (&OpenAIGatewayService{}).ParseOpenAIImagesRequest(c, body)
		require.NoError(t, err)
		require.Equal(t, OpenAIImagesCapabilityAPIKey, parsed.RequiredCapability)
		require.False(t, (&Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}).SupportsOpenAIImageCapability(parsed.RequiredCapability))
		require.Equal(t, OpenAIImagesCapabilityAPIKey, (&OpenAIImagesRequest{}).RequiredCapabilityForModel(model))
	}
	require.Error(t, validateCompatibleImagesModel("atlas-anything"))
	require.Error(t, validateCompatibleImagesModel("seedream-video"))
}

type atlasImageUpstream struct {
	HTTPUpstream
	replies []string
	status  int
	methods []string
	urls    []string
	auth    []string
	bodies  [][]byte
}

func (u *atlasImageUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.methods = append(u.methods, req.Method)
	u.urls = append(u.urls, req.URL.String())
	u.auth = append(u.auth, req.Header.Get("Authorization"))
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
	}
	u.bodies = append(u.bodies, body)
	reply := u.replies[0]
	if len(u.replies) > 1 {
		u.replies = u.replies[1:]
	}
	status := u.status
	if status == 0 {
		status = 200
	}
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(reply))}, nil
}

func atlasImageFixture(t *testing.T, upstream *atlasImageUpstream) (*OpenAIGatewayService, *gin.Context, *httptest.ResponseRecorder, *Account, *OpenAIImagesRequest) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"bytedance-seedream-v4.5","prompt":"draw","size":"2048x2048","response_format":"url"}`)
	c.Request = httptest.NewRequest(http.MethodPost, openAIImagesGenerationsEndpoint, bytes.NewReader(body))
	c.Request.Header.Set("x-api-key", "customer-only")
	svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
	parsed, err := svc.ParseOpenAIImagesRequest(c, body)
	require.NoError(t, err)
	account := &Account{ID: 7, Concurrency: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{
		"api_key": "upstream-only", "base_url": "https://api.atlascloud.ai/api/v1",
		"model_mapping": map[string]any{"bytedance-seedream-v4.5": "bytedance/seedream-v4.5"},
	}}
	return svc, c, rec, account, parsed
}

func TestAtlasCloudImagesForwardPollsAndReturnsActualImageCount(t *testing.T) {
	upstream := &atlasImageUpstream{replies: []string{
		`{"data":{"id":"prediction-1","status":"processing","outputs":[]}}`,
		`{"data":{"id":"prediction-1","status":"completed","outputs":["https://cdn.example/1.png","https://cdn.example/2.png"]}}`,
	}}
	svc, c, rec, account, parsed := atlasImageFixture(t, upstream)
	result, err := svc.ForwardImages(context.Background(), c, account, parsed.Body, parsed, "")
	require.NoError(t, err)
	require.Equal(t, 2, result.ImageCount)
	require.Equal(t, "prediction-1", result.RequestID)
	require.Equal(t, "bytedance/seedream-v4.5", result.UpstreamModel)
	require.Greater(t, result.Duration, time.Duration(0))
	require.Len(t, gjson.Get(rec.Body.String(), "data").Array(), 2)
	require.Equal(t, []string{"POST", "GET"}, upstream.methods)
	require.Equal(t, []string{
		"https://api.atlascloud.ai/api/v1/model/generateImage",
		"https://api.atlascloud.ai/api/v1/model/prediction/prediction-1",
	}, upstream.urls)
	require.Equal(t, []string{"Bearer upstream-only", "Bearer upstream-only"}, upstream.auth)
	require.JSONEq(t, `{"model":"bytedance/seedream-v4.5","prompt":"draw","size":"2048*2048"}`, string(upstream.bodies[0]))
}

func TestAtlasCloudGenericImageForwardPreservesModel(t *testing.T) {
	upstream := &atlasImageUpstream{replies: []string{
		`{"data":{"id":"generic-image-1","status":"completed","outputs":["https://cdn.example/generic.png"]}}`,
	}}
	svc, c, _, account, _ := atlasImageFixture(t, upstream)
	model := "google/nano-banana-2-lite/text-to-image"
	body := []byte(`{"model":"google/nano-banana-2-lite/text-to-image","prompt":"draw a quiet mountain lake","n":1,"response_format":"url"}`)
	parsed, err := svc.ParseOpenAIImagesRequest(c, body)
	require.NoError(t, err)
	account.Credentials["model_mapping"] = map[string]any{model: model}
	result, err := svc.ForwardImages(context.Background(), c, account, body, parsed, "")
	require.NoError(t, err)
	require.Equal(t, model, result.UpstreamModel)
	require.Equal(t, model, gjson.GetBytes(upstream.bodies[0], "model").String())
	require.Equal(t, "https://api.atlascloud.ai/api/v1/model/generateImage", upstream.urls[0])
}

func TestAtlasCloudImagesErrorsAreWrittenWithoutRetryOrSecrets(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reply  string
		status int
	}{
		{"failed", `{"data":{"id":"x","status":"failed","error":"provider failure upstream-only"}}`, 200},
		{"cancelled", `{"data":{"id":"x","status":"cancelled","outputs":["https://cdn.example/not-complete.png"]}}`, 200},
		{"completed_empty", `{"data":{"id":"x","status":"completed","outputs":[]}}`, 200},
		{"invalid", `not json`, 200},
		{"missing_id", `{"data":{"status":"processing"}}`, 200},
		{"rate_limit", `{"error":{"message":"upstream-only"}}`, 429},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := &atlasImageUpstream{replies: []string{tc.reply}, status: tc.status}
			svc, c, rec, account, parsed := atlasImageFixture(t, upstream)
			result, err := svc.ForwardImages(context.Background(), c, account, parsed.Body, parsed, "")
			require.Error(t, err)
			require.Nil(t, result)
			require.True(t, c.Writer.Written())
			require.True(t, gjson.Get(rec.Body.String(), "error.message").Exists())
			require.NotContains(t, rec.Body.String(), "upstream-only")
			require.Len(t, upstream.methods, 1)
		})
	}
}

func TestAtlasCloudImagesHonorsDeadline(t *testing.T) {
	upstream := &atlasImageUpstream{replies: []string{`{"data":{"id":"pending","status":"processing"}}`}}
	svc, c, rec, account, parsed := atlasImageFixture(t, upstream)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	result, err := svc.ForwardImages(ctx, c, account, parsed.Body, parsed, "")
	require.Error(t, err)
	require.Nil(t, result)
	require.Equal(t, 504, rec.Code)
	require.Len(t, upstream.methods, 1)
}

func TestAtlasCloudImagesBillingUsesProviderPresetTier(t *testing.T) {
	for _, tc := range []struct{ size, tier string }{
		{"2848x1600", "2K"}, {"3136x1344", "2K"}, {"5504x3040", "4K"}, {"", "2K"},
	} {
		t.Run(tc.size, func(t *testing.T) {
			upstream := &atlasImageUpstream{replies: []string{`{"id":"ready","status":"completed","outputs":["https://cdn.example/1.png"]}`}}
			svc, c, _, account, _ := atlasImageFixture(t, upstream)
			body := []byte(`{"model":"bytedance-seedream-v4.5","prompt":"draw","size":"` + tc.size + `"}`)
			parsed, err := svc.ParseOpenAIImagesRequest(c, body)
			require.NoError(t, err)
			require.Equal(t, tc.tier, parsed.SizeTier)
			result, err := svc.ForwardImages(context.Background(), c, account, body, parsed, "")
			require.NoError(t, err)
			ApplyOpenAIImageBillingResolution(result)
			require.Equal(t, tc.tier, result.ImageSize)
			require.Equal(t, ImageSizeSourceInput, result.ImageSizeSource)
			require.Equal(t, map[string]int{tc.tier: 1}, result.ImageSizeBreakdown)
			require.Len(t, upstream.methods, 1)
		})
	}
}

func TestAtlasCloudImagesRejectInvalidConfigurationBeforeSubmission(t *testing.T) {
	for _, base := range []string{"http://api.atlascloud.ai/api/v1", "https://api.atlascloud.ai/api/v1?key=secret",
		"https://api.atlascloud.ai/wrong-path", "https://user:pass@api.atlascloud.ai/api/v1"} {
		t.Run(base, func(t *testing.T) {
			upstream := &atlasImageUpstream{}
			svc, c, rec, account, parsed := atlasImageFixture(t, upstream)
			account.Credentials["base_url"] = base
			_, err := svc.ForwardImages(context.Background(), c, account, parsed.Body, parsed, "")
			require.Error(t, err)
			require.Equal(t, 502, rec.Code)
			require.Empty(t, upstream.methods)
		})
	}
	require.False(t, isAtlasCloudImagesBaseURL("https://evil.atlascloud.ai"))
	require.False(t, isAtlasCloudImagesBaseURL("https://api.atlascloud.ai.evil.example"))
}

func TestAtlasCloudImagesNativeAccountsRejectBeforeSubmission(t *testing.T) {
	for _, typ := range []string{AccountTypeOAuth, AccountTypeSetupToken} {
		upstream := &atlasImageUpstream{}
		svc, c, _, account, parsed := atlasImageFixture(t, upstream)
		account.Type = typ
		_, err := svc.ForwardImages(context.Background(), c, account, parsed.Body, parsed, "")
		require.Error(t, err)
		require.Empty(t, upstream.methods)
	}
}
