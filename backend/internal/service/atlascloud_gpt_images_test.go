//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestAtlasCloudGPTImagePayloadDefaults(t *testing.T) {
	parsed := &OpenAIImagesRequest{Prompt: "draw", N: 1}
	payload, err := buildAtlasCloudImagePayload(parsed, "openai/gpt-image-2/text-to-image")
	require.NoError(t, err)
	require.JSONEq(t, `{
		"model":"openai/gpt-image-2/text-to-image","prompt":"draw","size":"1024x1024",
		"quality":"medium","output_format":"jpeg","enable_sync_mode":false,"enable_base64_output":false
	}`, string(payload))
}

func TestAtlasCloudGPTImagePayloadOptions(t *testing.T) {
	for _, size := range []string{
		"1024x1024", "1024x768", "768x1024", "1024x1536", "1536x1024",
		"2048x2048", "2048x1152", "1152x2048", "2560x1088", "1088x2560",
		"2880x2160", "2160x2880", "3840x2160", "2160x3840",
	} {
		t.Run(size, func(t *testing.T) {
			payload, err := buildAtlasCloudImagePayload(&OpenAIImagesRequest{
				Prompt: "draw", N: 1, Size: size, Quality: "high", OutputFormat: "png", ResponseFormat: "url",
			}, "openai/gpt-image-2/text-to-image")
			require.NoError(t, err)
			require.Equal(t, size, gjson.GetBytes(payload, "size").String())
			require.Equal(t, "high", gjson.GetBytes(payload, "quality").String())
			require.Equal(t, "png", gjson.GetBytes(payload, "output_format").String())
			require.False(t, gjson.GetBytes(payload, "response_format").Exists())
		})
	}
	for _, quality := range []string{"low", "medium", "high", "auto"} {
		t.Run(quality, func(t *testing.T) {
			payload, err := buildAtlasCloudImagePayload(&OpenAIImagesRequest{
				Prompt: "draw", N: 1, Size: "1024*1536", Quality: quality, OutputFormat: "jpeg",
			}, "openai/gpt-image-2/text-to-image")
			require.NoError(t, err)
			want := quality
			if quality == "auto" {
				want = "medium"
			}
			require.Equal(t, want, gjson.GetBytes(payload, "quality").String())
			require.Equal(t, "1024x1536", gjson.GetBytes(payload, "size").String())
		})
	}
}

func atlasGPTImageFixture(t *testing.T, upstream *atlasImageUpstream, body []byte) (*OpenAIGatewayService, *gin.Context, *httptest.ResponseRecorder, *Account, *OpenAIImagesRequest) {
	t.Helper()
	svc, _, _, account, _ := atlasImageFixture(t, upstream)
	account.Credentials["model_mapping"] = map[string]any{
		"gpt-image-2": "openai/gpt-image-2/text-to-image",
	}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, openAIImagesGenerationsEndpoint, bytes.NewReader(body))
	parsed, err := svc.ParseOpenAIImagesRequest(c, body)
	require.NoError(t, err)
	return svc, c, rec, account, parsed
}

func TestAtlasCloudGPTImagesRequireAPIKey(t *testing.T) {
	_, _, _, _, parsed := atlasGPTImageFixture(t, &atlasImageUpstream{},
		[]byte(`{"model":"openai/gpt-image-2/text-to-image","prompt":"draw","quality":"high"}`))
	require.Equal(t, OpenAIImagesCapabilityAPIKey, parsed.RequiredCapability)
	require.Equal(t, OpenAIImagesCapabilityAPIKey,
		(&OpenAIImagesRequest{RequiredCapability: OpenAIImagesCapabilityNative}).RequiredCapabilityForModel("openai/gpt-image-2/text-to-image"))
	for _, typ := range []string{AccountTypeOAuth, AccountTypeSetupToken} {
		require.False(t, (&Account{Platform: PlatformOpenAI, Type: typ}).SupportsOpenAIImageCapability(parsed.RequiredCapability))
	}
	require.False(t, IsGPTImageGenerationModel("openai/gpt-image-2/text-to-image"))
	require.NoError(t, validateCompatibleImagesModel("openai/gpt-image-2/edit"), "exact edit contract is now implemented")
	require.Error(t, validateCompatibleImagesModel("openai/gpt-image-2.5/text-to-image"))
	require.Error(t, validateCompatibleImagesModel("openai/gpt-image-2.5/edit"))
}

func TestAtlasCloudGPTImagesForwardPollsCreatedJob(t *testing.T) {
	upstream := &atlasImageUpstream{replies: []string{
		`{"code":200,"data":{"id":"pred-gpt","status":"created","model":"openai/gpt-image-2/text-to-image","outputs":[]}}`,
		`{"data":{"id":"pred-gpt","status":"completed","model":"openai/gpt-image-2/text-to-image","outputs":["https://storage.atlascloud.ai/outputs/result.png"]}}`,
	}}
	body := []byte(`{"model":"openai/gpt-image-2/text-to-image","prompt":"draw","size":"1024x1536","quality":"high","output_format":"png","response_format":"url"}`)
	svc, c, rec, account, parsed := atlasGPTImageFixture(t, upstream, body)
	result, err := svc.ForwardImages(context.Background(), c, account, body, parsed, "")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Empty(t, rec.Body.String(), "withhold paid output until settlement")
	require.Equal(t, "https://storage.atlascloud.ai/outputs/result.png", gjson.GetBytes(result.AtlasCloudImageResponse, "data.0.url").String())
	require.False(t, gjson.GetBytes(result.AtlasCloudImageResponse, "data.0.b64_json").Exists())
	require.Equal(t, "pred-gpt", result.RequestID)
	require.Equal(t, 1, result.ImageCount)
	require.Equal(t, "openai/gpt-image-2/text-to-image", result.UpstreamModel)
	require.Equal(t, []string{"POST", "GET"}, upstream.methods)
	require.Equal(t, []string{
		"https://api.atlascloud.ai/api/v1/model/generateImage",
		"https://api.atlascloud.ai/api/v1/model/prediction/pred-gpt",
	}, upstream.urls)
	require.Equal(t, []string{"Bearer upstream-only", "Bearer upstream-only"}, upstream.auth)
	require.JSONEq(t, `{
		"model":"openai/gpt-image-2/text-to-image","prompt":"draw","size":"1024x1536",
		"quality":"high","output_format":"png","enable_sync_mode":false,"enable_base64_output":false
	}`, string(upstream.bodies[0]))
}

func TestAtlasCloudGPTImagesOmittedResponseFormatDoesNotDownloadOutput(t *testing.T) {
	image, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aEAAAAABJRU5ErkJggg==")
	require.NoError(t, err)
	upstream := &atlasImageUpstream{replies: []string{
		`{"data":{"id":"ready","status":"completed","outputs":["https://93.184.216.34/result.png"]}}`,
		string(image),
	}}
	body := []byte(`{"model":"openai/gpt-image-2/text-to-image","prompt":"draw"}`)
	svc, c, rec, account, parsed := atlasGPTImageFixture(t, upstream, body)
	account.Extra = map[string]any{AccountExtraImagesURLToB64JSON: true}
	result, err := svc.ForwardImages(context.Background(), c, account, body, parsed, "")
	require.NoError(t, err)
	require.Equal(t, 1, result.ImageCount)
	require.Empty(t, rec.Body.String(), "withhold paid output until settlement")
	require.Equal(t, "https://93.184.216.34/result.png", gjson.GetBytes(result.AtlasCloudImageResponse, "data.0.url").String())
	require.False(t, gjson.GetBytes(result.AtlasCloudImageResponse, "data.0.b64_json").Exists())
	require.Len(t, upstream.methods, 1)
	require.Empty(t, parsed.ResponseFormat)
}

func TestAtlasCloudGPTImagesMappedModelBilling(t *testing.T) {
	for _, tc := range []struct{ size, normalized, tier string }{
		{"", "1024x1024", "1K"}, {"auto", "1024x1024", "1K"},
		{"1024*768", "1024x768", "1K"}, {"1024x1536", "1024x1536", "2K"},
		{"2048x2048", "2048x2048", "2K"}, {"3840x2160", "3840x2160", "4K"},
	} {
		t.Run(tc.size, func(t *testing.T) {
			upstream := &atlasImageUpstream{replies: []string{`{"data":{"id":"ready","status":"completed","outputs":["https://cdn.example/1.png"]}}`}}
			body := []byte(`{"model":"gpt-image-2","prompt":"draw","size":"` + tc.size + `","response_format":"url"}`)
			svc, c, _, account, parsed := atlasGPTImageFixture(t, upstream, body)
			result, err := svc.ForwardImages(context.Background(), c, account, body, parsed, "")
			require.NoError(t, err)
			require.Equal(t, "gpt-image-2", result.Model)
			require.Equal(t, tc.normalized, gjson.GetBytes(upstream.bodies[0], "size").String())
			ApplyOpenAIImageBillingResolution(result)
			require.Equal(t, tc.tier, result.ImageSize)
			require.Equal(t, tc.normalized, result.ImageInputSize)
			require.Equal(t, ImageSizeSourceInput, result.ImageSizeSource)
			require.Nil(t, result.ImageSizeBreakdown)
		})
	}
}

func TestAtlasCloudGPTImagesParsedBillingTier(t *testing.T) {
	for _, tc := range []struct{ size, tier string }{
		{"", "1K"}, {"auto", "1K"}, {"1024*768", "1K"}, {"2048x2048", "2K"}, {"3840x2160", "4K"},
	} {
		t.Run(tc.size, func(t *testing.T) {
			_, _, _, _, parsed := atlasGPTImageFixture(t, &atlasImageUpstream{},
				[]byte(`{"model":"openai/gpt-image-2/text-to-image","prompt":"draw","size":"`+tc.size+`"}`))
			require.Equal(t, tc.tier, parsed.SizeTier)
		})
	}
}

func TestAtlasCloudGPTImagesRejectUnsupportedOptionsBeforeSubmission(t *testing.T) {
	for _, option := range []string{
		`"n":2`, `"stream":true`, `"prompt":""`, `"size":"999x999"`, `"size":"4096x4096"`,
		`"quality":"standard"`, `"quality":"xhigh"`, `"output_format":"webp"`,
		`"response_format":"b64_json"`, `"background":"transparent"`, `"output_compression":80`,
		`"partial_images":1`, `"style":"vivid"`, `"input_fidelity":"high"`, `"moderation":"low"`,
		`"seed":1`, `"image":"https://example.com/a.png"`, `"enable_base64_output":true`, `"enable_sync_mode":true`,
	} {
		t.Run(option, func(t *testing.T) {
			upstream := &atlasImageUpstream{replies: []string{`{"data":{"id":"unexpected","status":"completed","outputs":["https://cdn.example/1.png"]}}`}}
			fields := map[string]any{"model": "openai/gpt-image-2/text-to-image", "prompt": "draw"}
			var override map[string]any
			require.NoError(t, json.Unmarshal([]byte("{"+option+"}"), &override))
			for key, value := range override {
				fields[key] = value
			}
			body, err := json.Marshal(fields)
			require.NoError(t, err)
			svc, c, rec, account, parsed := atlasGPTImageFixture(t, upstream, body)
			result, err := svc.ForwardImages(context.Background(), c, account, body, parsed, "")
			require.Error(t, err)
			require.Nil(t, result)
			require.Equal(t, http.StatusBadRequest, rec.Code)
			require.Empty(t, upstream.methods)
		})
	}
}

func TestAtlasCloudGPTImagesFailuresDoNotResubmit(t *testing.T) {
	for _, tc := range []struct {
		name, reply string
		status      int
	}{
		{"failed", `{"data":{"id":"failed","status":"failed","error":"upstream-only"}}`, 200},
		{"unauthorized", `{"error":"upstream-only"}`, 401},
		{"rate_limit", `{"error":"upstream-only"}`, 429},
		{"empty", `{"data":{"id":"empty","status":"completed","outputs":[]}}`, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := &atlasImageUpstream{replies: []string{tc.reply}, status: tc.status}
			body := []byte(`{"model":"openai/gpt-image-2/text-to-image","prompt":"draw"}`)
			svc, c, rec, account, parsed := atlasGPTImageFixture(t, upstream, body)
			result, err := svc.ForwardImages(context.Background(), c, account, body, parsed, "")
			require.Error(t, err)
			require.Nil(t, result)
			require.NotContains(t, rec.Body.String(), "upstream-only")
			require.Len(t, upstream.methods, 1)
			wantStatus := http.StatusBadGateway
			if tc.status == 429 {
				wantStatus = 429
			}
			require.Equal(t, wantStatus, rec.Code)
		})
	}
}

func TestAtlasCloudGPTImagesHonorsDeadline(t *testing.T) {
	upstream := &atlasImageUpstream{replies: []string{`{"data":{"id":"pending","status":"processing","outputs":[]}}`}}
	body := []byte(`{"model":"openai/gpt-image-2/text-to-image","prompt":"draw"}`)
	svc, c, rec, account, parsed := atlasGPTImageFixture(t, upstream, body)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	result, err := svc.ForwardImages(ctx, c, account, body, parsed, "")
	require.Error(t, err)
	require.Nil(t, result)
	require.Equal(t, http.StatusGatewayTimeout, rec.Code)
	require.Len(t, upstream.methods, 1)
}
