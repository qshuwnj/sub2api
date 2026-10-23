//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestAtlasMediaImageAdmission(t *testing.T) {
	for _, model := range []string{
		"bytedance/seedream-v4.5",
		"openai/gpt-image-2/text-to-image",
		"openai/gpt-image-2.5-flare/text-to-image",
		"xai/grok-imagine-image-2.0-developer/text-to-image",
	} {
		t.Run(model, func(t *testing.T) {
			body := []byte(`{"model":"` + model + `","prompt":"test","n":1}`)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", bytes.NewReader(body))
			c.Request.Header.Set("Content-Type", "application/json")
			req, err := (&OpenAIGatewayService{}).ParseOpenAIImagesRequest(c, body)
			require.NoError(t, err)
			require.Equal(t, model, req.Model)
			require.Equal(t, OpenAIImagesCapabilityAPIKey, req.RequiredCapability)
			require.Equal(t, OpenAIImagesCapabilityAPIKey, (&OpenAIImagesRequest{}).RequiredCapabilityForModel(model))
		})
	}
	for _, model := range []string{"openai/gpt-image-unknown/text-to-image", "xai/grok-imagine-video/text-to-video"} {
		require.Error(t, validateCompatibleImagesModel(model))
	}
}

func TestAtlasMediaExactVideoConversion(t *testing.T) {
	info, raw, err := buildAtlasCloudVideoPayload([]byte(`{"model":"bytedance/seedance-2.5/text-to-video","prompt":"test","duration":30,"resolution":"1080p","ratio":"16:9"}`))
	require.NoError(t, err)
	require.Equal(t, 30, info.DurationSeconds)
	require.JSONEq(t, `{"model":"bytedance/seedance-2.5/text-to-video","prompt":"test","duration":30,"resolution":"1080p","ratio":"16:9","generate_audio":true,"watermark":false,"return_last_frame":false,"output_format":"mp4","draft":false}`, string(raw))
	_, _, err = buildAtlasCloudVideoPayload([]byte(`{"model":"bytedance/seedance-2.0-fast/text-to-video","prompt":"test","resolution":"1080p"}`))
	require.Error(t, err)
	_, _, err = buildAtlasCloudVideoPayload([]byte(`{"model":"bytedance/seedance-2.5/text-to-video","prompt":"test","duration":-1}`))
	require.ErrorContains(t, err, "duration", "automatic duration cannot be priced as one second")
	_, _, err = buildAtlasCloudVideoPayload([]byte(`{"model":"google/veo3.1/text-to-video","prompt":"test","resolution":"4k","duration":4}`))
	require.Error(t, err, "conditional duration must be enforced")
}

func TestAtlasMediaExactImageConversion(t *testing.T) {
	body := []byte(`{"model":"openai/gpt-image-1-mini/edit","prompt":"test","images_list":["https://assets.example/input.png"],"size":"1024x1536","n":1,"response_format":"url"}`)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	req, err := (&OpenAIGatewayService{}).ParseOpenAIImagesRequest(c, body)
	require.NoError(t, err)
	raw, err := buildAtlasCloudImagePayload(req, req.Model)
	require.NoError(t, err)
	var payload map[string]any
	require.NoError(t, json.Unmarshal(raw, &payload))
	require.Equal(t, []any{"https://assets.example/input.png"}, payload["images"])
	require.NotContains(t, payload, "images_list")
	require.NotContains(t, payload, "response_format")
	require.Equal(t, "1024x1536", payload["size"])
	require.Equal(t, "medium", payload["quality"])
	require.Equal(t, false, payload["enable_base64_output"])
	req.Body = []byte(`{"model":"openai/gpt-image-1-mini/edit","prompt":"test","images_list":["http://127.0.0.1/private"],"n":1}`)
	_, err = buildAtlasCloudImagePayload(req, req.Model)
	require.Error(t, err, "unsafe media references must never reach upstream")
}

func TestAtlasMediaThirtySecondBilling(t *testing.T) {
	model := "bytedance/seedance-2.5/text-to-video"
	cost := (&BillingService{}).CalculateVideoCost(model, "1080p", 1, 30,
		&VideoPriceConfig{ModelPrices: map[string]map[string]float64{
			model: {"1080p": 1},
		}}, 1)
	require.Equal(t, float64(30), cost.ActualCost, "never clamp a documented 30-second job to Grok's 15-second ceiling")
}

func TestAtlasMediaImageResolutionControlsBillingTier(t *testing.T) {
	body := []byte(`{"model":"google/nano-banana-2/text-to-image","prompt":"test","resolution":"4k","n":1}`)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	req, err := (&OpenAIGatewayService{}).ParseOpenAIImagesRequest(c, body)
	require.NoError(t, err)
	require.Equal(t, ImageBillingSize4K, req.SizeTier)
	body = []byte(`{"model":"alibaba/wan-2.7/text-to-image","prompt":"test","size":"2K","n":1}`)
	req, err = (&OpenAIGatewayService{}).ParseOpenAIImagesRequest(c, body)
	require.NoError(t, err)
	require.Equal(t, ImageBillingSize2K, req.SizeTier)
}

func TestAtlasMediaImageDimensions(t *testing.T) {
	model := "qwen/qwen-image-2.0-pro/text-to-image"
	for _, size := range []string{"2049*1024", "1024x1024", "512*0", "garbage"} {
		_, err := buildAtlasMediaContractPayload([]byte(`{"model":"`+model+`","prompt":"test","size":"`+size+`"}`), atlasMediaModel(model, "image"), true)
		require.Error(t, err, size)
	}
	_, err := buildAtlasMediaContractPayload([]byte(`{"model":"`+model+`","prompt":"test","size":"1280*720"}`), atlasMediaModel(model, "image"), true)
	require.NoError(t, err)
}

func TestAtlasMediaInlineReferenceUploadBeforeGeneration(t *testing.T) {
	upstream := &atlasImageUpstream{replies: []string{
		`{"code":200,"data":{"download_url":"https://storage.atlascloud.ai/test/reference.png"}}`,
		`{"code":200,"data":{"id":"image-inline","status":"completed","outputs":["https://storage.atlascloud.ai/test/output.png"]}}`,
	}}
	svc, c, w, account, _ := atlasImageFixture(t, upstream)
	body := []byte(`{"model":"openai/gpt-image-1-mini/edit","prompt":"test","images":["data:image/png;base64,iVBORw0KGgo="],"n":1}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	req, err := svc.ParseOpenAIImagesRequest(c, body)
	require.NoError(t, err)
	result, err := svc.forwardOpenAIImagesAPIKey(context.Background(), c, account, body, req, "")
	require.NoError(t, err)
	require.Equal(t, 1, result.ImageCount)
	require.Len(t, upstream.urls, 2)
	require.Equal(t, "https://api.atlascloud.ai/api/v1/model/uploadMedia", upstream.urls[0])
	require.JSONEq(t, `["https://storage.atlascloud.ai/test/reference.png"]`, gjson.GetBytes(upstream.bodies[1], "images").Raw)
	require.NotContains(t, string(upstream.bodies[1]), "data:")
	require.Empty(t, w.Body.String(), "paid output still waits for settlement")
}

func TestAtlasMediaUploadFailureNeverSubmitsGeneration(t *testing.T) {
	for _, reply := range []string{
		`{"code":500,"message":"secret-upstream-token"}`,
		`{"code":200,"data":{"download_url":"http://127.0.0.1/private"}}`,
		`{"code":200,"data":{}}`,
	} {
		upstream := &atlasImageUpstream{replies: []string{reply}}
		svc, c, w, account, _ := atlasImageFixture(t, upstream)
		body := []byte(`{"model":"openai/gpt-image-1-mini/edit","prompt":"test","images":["data:image/png;base64,iVBORw0KGgo="],"n":1}`)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", bytes.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		req, err := svc.ParseOpenAIImagesRequest(c, body)
		require.NoError(t, err)
		_, err = svc.forwardOpenAIImagesAPIKey(context.Background(), c, account, body, req, "")
		require.Error(t, err)
		require.Len(t, upstream.urls, 1)
		require.NotContains(t, w.Body.String(), "secret-upstream-token")
	}
}

func TestAtlasMediaFrontendContractMatrix(t *testing.T) {
	raw, err := os.ReadFile("testdata/atlas_media_frontend_requests.json")
	require.NoError(t, err)
	var fixture struct {
		Requests []struct {
			Media string          `json:"media"`
			Model string          `json:"model"`
			Body  json.RawMessage `json:"body"`
		} `json:"requests"`
		Rejected []struct {
			ID string `json:"id"`
		} `json:"rejected"`
	}
	require.NoError(t, json.Unmarshal(raw, &fixture))
	images, videos := map[string]bool{}, map[string]bool{}
	for index, sample := range fixture.Requests {
		t.Run(fmt.Sprintf("%s/%d", sample.Model, index), func(t *testing.T) {
			var converted []byte
			if sample.Media == "image" {
				images[sample.Model] = true
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", bytes.NewReader(sample.Body))
				c.Request.Header.Set("Content-Type", "application/json")
				req, err := (&OpenAIGatewayService{}).ParseOpenAIImagesRequest(c, sample.Body)
				require.NoError(t, err)
				converted, err = buildAtlasCloudImagePayload(req, sample.Model)
				require.NoError(t, err)
			} else {
				videos[sample.Model] = true
				info, body, err := buildAtlasCloudVideoPayload(sample.Body)
				require.NoError(t, err)
				require.Positive(t, info.DurationSeconds)
				require.NotEmpty(t, NormalizeVideoBillingResolutionForModel(sample.Model, info.Resolution))
				converted = body
			}
			var output any
			require.NoError(t, json.Unmarshal(converted, &output))
			require.NoError(t, validateAtlasMediaValue(output, atlasMediaModel(sample.Model, sample.Media).Input, "request", true))
		})
	}
	require.Len(t, images, 58)
	require.Len(t, videos, 87)
	require.Len(t, fixture.Rejected, 12)
	for _, sample := range fixture.Rejected {
		require.Nil(t, atlasMediaModel(sample.ID, "image"))
		require.Nil(t, atlasMediaModel(sample.ID, "video"))
	}
}
