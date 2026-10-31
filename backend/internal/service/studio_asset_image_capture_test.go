package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/xai"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func studioCapturePNG(t *testing.T, value uint8) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: value, A: 255})
	var out bytes.Buffer
	require.NoError(t, png.Encode(&out, img))
	return out.Bytes()
}

func TestStudioLibraryOptInIsExact(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, value := range []string{"", "save", "true", "save,public", " save", "SAVE"} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
		c.Request.Header.Set("X-Studio-Library", value)
		require.Equal(t, value == "save", StudioLibraryRequested(c), value)
	}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
	c.Request.Header.Add("X-Studio-Library", "save")
	c.Request.Header.Add("X-Studio-Library", "public")
	require.False(t, StudioLibraryRequested(c))
}

func TestStudioAssetImagePartialPreviewsNeverCollect(t *testing.T) {
	for _, transport := range []string{"raw", "responses"} {
		t.Run(transport, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
			c.Request.Header.Set("X-Studio-Library", "save")
			calls := 0
			SetStudioImageCollector(c, func([]StudioImageOutput) *StudioLibraryStatus {
				calls++
				return &StudioLibraryStatus{Status: "queued", GenerationID: "must_not_exist"}
			})
			body := "data: " + `{"type":"image_generation.partial_image","b64_json":"cHJldmlldw=="}` + "\n\n"
			if transport == "responses" {
				body = "data: " + `{"type":"response.image_generation_call.partial_image","partial_image_b64":"cHJldmlldw=="}` + "\n\n"
			}
			resp := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewBufferString(body))}
			svc := &OpenAIGatewayService{}
			if transport == "raw" {
				_, _, _, _, _ = svc.handleOpenAIImagesStreamingResponse(resp, c, time.Now(), nil)
			} else {
				_, _, _, _, _ = svc.handleOpenAIImagesOAuthStreamingResponse(resp, c, time.Now(), "b64_json", "image_generation", "gpt-image-2")
			}
			require.Zero(t, calls)
			require.Empty(t, studioImageOutputs(c))
			require.NotContains(t, rec.Body.String(), `"library"`)
		})
	}
}

func TestStudioAssetImageRawCapturePreservesOutputsAndFailureDelivery(t *testing.T) {
	first, second := studioCapturePNG(t, 30), studioCapturePNG(t, 230)
	raw, err := json.Marshal(map[string]any{"data": []any{
		map[string]string{"b64_json": base64.StdEncoding.EncodeToString(first)},
		map[string]string{"b64_json": base64.StdEncoding.EncodeToString(second)},
	}})
	require.NoError(t, err)
	for _, endpoint := range []string{"/v1/images/generations", "/v1/images/edits"} {
		for _, optIn := range []bool{false, true} {
			for _, status := range []string{"queued", "warning"} {
				t.Run(endpoint+"/"+status+map[bool]string{false: "/api", true: "/studio"}[optIn], func(t *testing.T) {
					rec := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(rec)
					c.Request = httptest.NewRequest(http.MethodPost, endpoint, nil)
					if optIn {
						c.Request.Header.Set("X-Studio-Library", "save")
					}
					calls := 0
					SetStudioImageCollector(c, func(outputs []StudioImageOutput) *StudioLibraryStatus {
						calls++
						require.False(t, c.Writer.Written(), "persist before completed response")
						require.Len(t, outputs, 2)
						require.Equal(t, first, outputs[0].Bytes)
						require.Equal(t, second, outputs[1].Bytes)
						return &StudioLibraryStatus{Status: status, GenerationID: "image_server_owned"}
					})
					resp := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(raw))}
					svc := &OpenAIGatewayService{}
					_, count, _, err := svc.handleOpenAIImagesNonStreamingResponse(context.Background(), resp, c, &Account{}, &OpenAIImagesRequest{})
					require.NoError(t, err)
					require.Equal(t, 2, count)
					require.Equal(t, map[bool]int{false: 0, true: 1}[optIn], calls)
					require.Equal(t, http.StatusOK, rec.Code)
					var delivered map[string]json.RawMessage
					require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &delivered))
					var original map[string]json.RawMessage
					require.NoError(t, json.Unmarshal(raw, &original))
					require.JSONEq(t, string(original["data"]), string(delivered["data"]))
					if optIn {
						require.Contains(t, string(delivered["library"]), `"status":"`+status+`"`)
						require.Len(t, studioImageOutputs(c), 2)
					} else {
						require.NotContains(t, delivered, "library")
					}
				})
			}
		}
	}
}

func TestStudioAssetImageResponsesCaptureOnlyFinalOutputs(t *testing.T) {
	first, second := studioCapturePNG(t, 1), studioCapturePNG(t, 2)
	body := "data: " + `{"type":"response.image_generation_call.partial_image","partial_image_b64":"preview"}` + "\n\n"
	event, err := json.Marshal(map[string]any{
		"type": "response.completed", "response": map[string]any{
			"id": "resp_final", "created_at": 42,
			"output": []any{
				map[string]any{"type": "image_generation_call", "id": "ig_1", "result": base64.StdEncoding.EncodeToString(first), "output_format": "png"},
				map[string]any{"type": "image_generation_call", "id": "ig_2", "result": base64.StdEncoding.EncodeToString(second), "output_format": "png"},
			},
		},
	})
	require.NoError(t, err)
	body += "data: " + string(event) + "\n\n"
	for _, stream := range []bool{false, true} {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
		c.Request.Header.Set("X-Studio-Library", "save")
		calls := 0
		SetStudioImageCollector(c, func(outputs []StudioImageOutput) *StudioLibraryStatus {
			calls++
			require.Len(t, outputs, 2)
			require.Equal(t, first, outputs[0].Bytes)
			require.Equal(t, second, outputs[1].Bytes)
			return &StudioLibraryStatus{Status: "queued", GenerationID: "image_final"}
		})
		resp := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewBufferString(body))}
		svc := &OpenAIGatewayService{}
		if stream {
			_, count, _, _, err := svc.handleOpenAIImagesOAuthStreamingResponse(resp, c, time.Now(), "b64_json", "image_generation", "gpt-image-2")
			require.NoError(t, err)
			require.Equal(t, 2, count)
		} else {
			_, count, _, err := svc.handleOpenAIImagesOAuthNonStreamingResponse(resp, c, "b64_json", "gpt-image-2")
			require.NoError(t, err)
			require.Equal(t, 2, count)
		}
		require.Equal(t, 1, calls)
		require.Contains(t, rec.Body.String(), `"library":{"status":"queued","generation_id":"image_final"}`)
	}
}

func TestStudioAssetImageRawStreamAddsStatusOnlyAtFinalCompletion(t *testing.T) {
	imageBytes := studioCapturePNG(t, 8)
	body := "data: " + `{"type":"image_generation.partial_image","b64_json":"cHJldmlldw=="}` + "\n\n"
	body += "event: image_generation.completed\nid: output-1\n: provider-comment\n"
	body += "data: " + `{"type":"image_generation.completed",` + "\n"
	body += "data: " + `"b64_json":"` + base64.StdEncoding.EncodeToString(imageBytes) + `"}` + "\n\n"
	body += "data: [DONE]\n\n"
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
	c.Request.Header.Set("X-Studio-Library", "save")
	calls := 0
	SetStudioImageCollector(c, func(outputs []StudioImageOutput) *StudioLibraryStatus {
		calls++
		require.Len(t, outputs, 1)
		require.Equal(t, imageBytes, outputs[0].Bytes)
		require.NotContains(t, rec.Body.String(), `"type":"image_generation.completed"`, "collection precedes final delivery")
		return &StudioLibraryStatus{Status: "queued", GenerationID: "image_stream"}
	})
	resp := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewBufferString(body))}
	_, count, _, _, err := (&OpenAIGatewayService{}).handleOpenAIImagesStreamingResponse(resp, c, time.Now(), nil)
	require.NoError(t, err)
	require.Equal(t, 1, count)
	require.Equal(t, 1, calls)
	require.Contains(t, rec.Body.String(), `"library":{"status":"queued","generation_id":"image_stream"}`)
	require.Contains(t, rec.Body.String(), "data: [DONE]")
	require.Contains(t, rec.Body.String(), "event: image_generation.completed\nid: output-1\n: provider-comment\n")
	forEachOpenAISSEDataPayload(rec.Body.String(), func(data []byte) {
		if bytes.Contains(data, []byte(`image_generation.completed`)) {
			require.True(t, json.Valid(data), "rewritten completion remains valid SSE JSON")
		}
	})
}

func TestStudioAssetImageRawStreamJSONFallbackDeliversExactlyOnce(t *testing.T) {
	for _, suffix := range []string{"\n\n", "\r\n\r\n", "\n \n"} {
		t.Run(strings.ReplaceAll(suffix, "\n", "LF"), func(t *testing.T) {
			body := `{"data":[{"url":"https://images.example/final.png"}],"usage":{"images":1}}` + suffix
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
			c.Request.Header.Set("X-Studio-Library", "save")
			calls := 0
			deliveredBeforeCollection := false
			SetStudioImageCollector(c, func(outputs []StudioImageOutput) *StudioLibraryStatus {
				calls++
				deliveredBeforeCollection = c.Writer.Written()
				require.Len(t, outputs, 1)
				require.Equal(t, "https://images.example/final.png", outputs[0].URL)
				return &StudioLibraryStatus{Status: "queued", GenerationID: "image_fallback"}
			})
			resp := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
			_, count, _, _, err := (&OpenAIGatewayService{}).handleOpenAIImagesStreamingResponse(resp, c, time.Now(), nil)
			require.NoError(t, err)
			require.Equal(t, 1, count)
			require.Equal(t, 1, calls)
			require.False(t, deliveredBeforeCollection, "persist before fallback JSON delivery")
			decoder := json.NewDecoder(rec.Body)
			var delivered map[string]json.RawMessage
			require.NoError(t, decoder.Decode(&delivered))
			require.Contains(t, string(delivered["library"]), `"status":"queued"`)
			require.JSONEq(t, `[{"url":"https://images.example/final.png"}]`, string(delivered["data"]))
			require.ErrorIs(t, decoder.Decode(&delivered), io.EOF, "deliver one JSON document only")
		})
	}
}

func TestStudioAssetImageRawStreamOversizedJSONFallbackPreservesMedia(t *testing.T) {
	for _, suffix := range []string{"", "\n\n"} {
		t.Run(map[bool]string{false: "no_delimiter", true: "blank_delimiter"}[suffix != ""], func(t *testing.T) {
			body := `{"data":[{"url":"https://images.example/final.png"}],"padding":"` + strings.Repeat("x", 256) + `"}` + suffix
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
			c.Request.Header.Set("X-Studio-Library", "save")
			calls := 0
			SetStudioImageCollector(c, func([]StudioImageOutput) *StudioLibraryStatus {
				calls++
				return &StudioLibraryStatus{Status: "queued", GenerationID: "must_not_exist"}
			})
			cfg := &config.Config{}
			cfg.Gateway.UpstreamResponseReadMaxBytes = 96
			resp := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
			_, _, _, _, err := (&OpenAIGatewayService{cfg: cfg}).handleOpenAIImagesStreamingResponse(resp, c, time.Now(), nil)
			require.NoError(t, err)
			require.Equal(t, body, rec.Body.String(), "oversized fallback still delivers original media exactly once")
			require.Zero(t, calls, "do not collect beyond the bounded parsing limit")
			require.Empty(t, studioImageOutputs(c))
			require.Equal(t, "studio_asset_unavailable", rec.Result().Header.Get("X-Studio-Library-Warning"))
			require.NotContains(t, rec.Body.String(), `"library"`)
		})
	}
}

type studioCaptureObservedReader struct {
	io.Reader
	beforeRead func()
}

func (r studioCaptureObservedReader) Read(p []byte) (int, error) {
	r.beforeRead()
	return r.Reader.Read(p)
}

func TestStudioAssetImageRawStreamOversizedJSONFallbackStreamsBeforeEOF(t *testing.T) {
	body := `{"data":[{"url":"https://images.example/final.png"}],"padding":"` + strings.Repeat("x", 8192) + `"}`
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
	c.Request.Header.Set("X-Studio-Library", "save")
	reads := 0
	reader := studioCaptureObservedReader{Reader: strings.NewReader(body), beforeRead: func() {
		if reads > 0 {
			require.NotEmpty(t, rec.Body.String(), "flush bounded fallback before reading the remaining body")
			require.Equal(t, "studio_asset_unavailable", rec.Result().Header.Get("X-Studio-Library-Warning"))
		}
		reads++
	}}
	cfg := &config.Config{}
	cfg.Gateway.UpstreamResponseReadMaxBytes = 96
	resp := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(reader)}
	_, _, _, _, err := (&OpenAIGatewayService{cfg: cfg}).handleOpenAIImagesStreamingResponse(resp, c, time.Now(), nil)
	require.NoError(t, err)
	require.Equal(t, body, rec.Body.String())
	require.Greater(t, reads, 1)
}

func TestStudioAssetImageNativeDirectCapturePreservesDelivery(t *testing.T) {
	imageBytes := studioCapturePNG(t, 17)
	encoded := base64.StdEncoding.EncodeToString(imageBytes)
	for _, stream := range []bool{false, true} {
		for _, mode := range []string{"api", "queued", "warning"} {
			t.Run(map[bool]string{false: "json/", true: "sse/"}[stream]+mode, func(t *testing.T) {
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
				if mode != "api" {
					c.Request.Header.Set("X-Studio-Library", "save")
				}
				calls := 0
				SetStudioImageCollector(c, func(outputs []StudioImageOutput) *StudioLibraryStatus {
					calls++
					require.Len(t, outputs, 1)
					require.Equal(t, imageBytes, outputs[0].Bytes)
					require.NotContains(t, rec.Body.String(), encoded, "persist before native final output")
					return &StudioLibraryStatus{Status: mode, GenerationID: "image_native", Code: map[string]string{"warning": "studio_asset_unavailable"}[mode]}
				})
				body := `{"data":[{"b64_json":"` + encoded + `","model":"supplier-model"}],"usage":{"input_tokens":3,"output_tokens":7}}`
				if stream {
					body = "data: " + `{"type":"image_generation.partial_image","b64_json":"cHJldmlldw=="}` + "\n\n" +
						"data: " + `{"type":"image_generation.completed","b64_json":"` + encoded + `","model":"supplier-model","usage":{"input_tokens":3,"output_tokens":7}}` + "\n\n"
				}
				resp := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
				svc := &OpenAIGatewayService{}
				parsed := &OpenAIImagesRequest{Model: "client-model", ResponseFormat: "b64_json", OutputFormat: "png"}
				var usage OpenAIUsage
				var count int
				var err error
				if stream {
					usage, count, _, _, err = svc.handleOpenAIImagesStreamingResponse(resp, c, time.Now(), parsed)
				} else {
					usage, count, _, err = svc.handleCodexDirectImagesNonStreamingResponse(resp, c, parsed)
				}
				require.NoError(t, err)
				require.Equal(t, 1, count)
				require.Equal(t, 3, usage.InputTokens)
				require.Equal(t, 7, usage.OutputTokens)
				require.Contains(t, rec.Body.String(), encoded)
				require.Contains(t, rec.Body.String(), `"model":"client-model"`)
				if mode == "api" {
					require.Zero(t, calls)
					require.NotContains(t, rec.Body.String(), `"library"`)
				} else {
					require.Equal(t, 1, calls)
					require.Contains(t, rec.Body.String(), `"status":"`+mode+`"`)
					require.Contains(t, rec.Body.String(), `"generation_id":"image_native"`)
				}
			})
		}
	}
}

func TestStudioAssetImageNativeDirectPreviewOnlyNeverCollects(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "json", true: "sse"}[stream], func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
			c.Request.Header.Set("X-Studio-Library", "save")
			calls := 0
			SetStudioImageCollector(c, func([]StudioImageOutput) *StudioLibraryStatus {
				calls++
				return &StudioLibraryStatus{Status: "queued", GenerationID: "must_not_exist"}
			})
			body := `{"data":[],"partial_image_b64":"cHJldmlldw=="}`
			if stream {
				body = "data: " + `{"type":"image_generation.partial_image","b64_json":"cHJldmlldw=="}` + "\n\n"
			}
			resp := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
			svc := &OpenAIGatewayService{}
			parsed := &OpenAIImagesRequest{Model: "client-model", ResponseFormat: "b64_json"}
			var count int
			var err error
			if stream {
				_, count, _, _, err = svc.handleOpenAIImagesStreamingResponse(resp, c, time.Now(), parsed)
			} else {
				_, count, _, err = svc.handleCodexDirectImagesNonStreamingResponse(resp, c, parsed)
			}
			require.Error(t, err, "preserve existing no-final-output error")
			require.Zero(t, count)
			require.Zero(t, calls)
			require.Empty(t, studioImageOutputs(c))
			require.NotContains(t, rec.Body.String(), `"library"`)
		})
	}
}

func TestStudioAssetImageGrokAdaptersReturnTypedFinalOutputsAndStripOptIn(t *testing.T) {
	t.Setenv(xai.EnvAllowUnsafeURLOverrides, "true")
	for _, endpoint := range []GrokMediaEndpoint{GrokMediaEndpointImagesGenerations, GrokMediaEndpointImagesEdits} {
		t.Run(string(endpoint), func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
			c.Request.Header.Set("X-Studio-Library", "save")
			collected := 0
			SetStudioImageCollector(c, func(outputs []StudioImageOutput) *StudioLibraryStatus {
				collected += len(outputs)
				return &StudioLibraryStatus{Status: "warning", GenerationID: "image_grok", Code: "studio_asset_unavailable"}
			})
			upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewBufferString(`{"data":[{"url":"https://images.example/1.png"},{"url":"https://images.example/2.png"}]}`))}}
			svc := &OpenAIGatewayService{httpUpstream: upstream}
			account := &Account{ID: 8, Platform: PlatformGrok, Type: AccountTypeAPIKey, Credentials: map[string]any{
				"api_key": "fixture", "base_url": "https://xai.test/v1", "header_override_enabled": true,
				"header_overrides": map[string]any{"X-Studio-Library": "save"},
			}}
			body := []byte(`{"model":"grok-imagine-image","prompt":"original prompt","image":{"url":"https://assets.example/input.png"}}`)
			result, err := svc.ForwardGrokMedia(context.Background(), c, account, endpoint, "", body, "application/json")
			require.NoError(t, err)
			require.Equal(t, 2, collected)
			require.Len(t, result.StudioImageOutputs, 2)
			require.Equal(t, "https://images.example/2.png", result.StudioImageOutputs[1].URL)
			require.Empty(t, upstream.lastReq.Header.Get("X-Studio-Library"))
			require.Contains(t, rec.Body.String(), `"status":"warning"`)
			require.Contains(t, rec.Body.String(), "https://images.example/1.png")
		})
	}
}

func TestStudioAssetImageAtlasDefersCollectionAndDeliveryUntilHandlerSettlement(t *testing.T) {
	upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(
		`{"data":{"id":"paid_prediction","status":"completed","outputs":["https://images.example/1.png","https://images.example/2.png"]}}`,
	))}}
	svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"bytedance-seedream-v4.5","prompt":"draw","size":"2048x2048","response_format":"url"}`)
	c.Request = httptest.NewRequest(http.MethodPost, openAIImagesGenerationsEndpoint, bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	account := &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{
		"api_key": "fixture", "base_url": "https://api.atlascloud.ai/api/v1",
		"model_mapping": map[string]any{"bytedance-seedream-v4.5": "bytedance/seedream-v4.5"},
	}}
	parsed, err := svc.ParseOpenAIImagesRequest(c, body)
	require.NoError(t, err)
	c.Request.Header.Set("X-Studio-Library", "save")
	calls := 0
	SetStudioImageCollector(c, func([]StudioImageOutput) *StudioLibraryStatus {
		calls++
		return &StudioLibraryStatus{Status: "queued", GenerationID: "image_atlas"}
	})
	result, err := svc.ForwardImages(context.Background(), c, account, parsed.Body, parsed, "")
	require.NoError(t, err)
	require.Zero(t, calls, "never collect before the handler's settlement gate")
	require.Empty(t, rec.Body.String())
	require.Len(t, result.StudioImageOutputs, 2)
	require.Equal(t, "https://images.example/1.png", result.StudioImageOutputs[0].URL)
	require.Equal(t, http.MethodPost, upstream.lastReq.Method)
}

func TestStudioAssetImageRawAPIKeyStripsOptInAfterHeaderOverrides(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/edits", nil)
	c.Request.Header.Set("X-Studio-Library", "save")
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{
		"header_override_enabled": true, "header_overrides": map[string]any{"X-Studio-Library": "save"},
	}}
	request, err := (&OpenAIGatewayService{cfg: &config.Config{}}).buildOpenAIImagesRequest(context.Background(), c, account, []byte(`{}`), "application/json", "fixture", openAIImagesEditsEndpoint)
	require.NoError(t, err)
	require.Empty(t, request.Header.Get("X-Studio-Library"))
}

type studioImageBillingRepository struct {
	UsageBillingRepository
	StudioAssetRepository
	assets   []*StudioAsset
	commands []*UsageBillingCommand
	fail     bool
}

func (r *studioImageBillingRepository) UpsertStudioAsset(_ context.Context, asset *StudioAsset) error {
	if r.fail {
		return ErrStudioAssetUnavailable
	}
	copy := *asset
	r.assets = append(r.assets, &copy)
	return nil
}

func (r *studioImageBillingRepository) Apply(_ context.Context, command *UsageBillingCommand) (*UsageBillingApplyResult, error) {
	copy := *command
	r.commands = append(r.commands, &copy)
	return &UsageBillingApplyResult{Applied: false}, nil
}

type studioImageCaptureUpstream struct {
	HTTPUpstream
	body  []byte
	calls int
}

func (u *studioImageCaptureUpstream) Do(_ *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.calls++
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"X-Request-Id": {"fixture_request"}}, Body: io.NopCloser(bytes.NewReader(u.body))}, nil
}

func TestStudioAssetImageStorageFailuresDoNotChangeSupplierOrBillingCommands(t *testing.T) {
	png := studioCapturePNG(t, 3)
	response, err := json.Marshal(map[string]any{"data": []any{
		map[string]string{"b64_json": base64.StdEncoding.EncodeToString(png)},
		map[string]string{"url": "https://images.example/second.png"},
	}})
	require.NoError(t, err)
	var baselineCommand *UsageBillingCommand
	for _, scenario := range []string{"available", "storage_unavailable", "database_unavailable", "non_studio"} {
		t.Run(scenario, func(t *testing.T) {
			repo := &studioImageBillingRepository{fail: scenario == "database_unavailable"}
			uploader := NewImageResultUploader(&studioArchiveObjects{}, "images/", 1024*1024, nil)
			library := NewStudioAssetService(repo, func() (*ImageResultUploader, bool) { return uploader, scenario != "storage_unavailable" })
			upstream := &studioImageCaptureUpstream{body: response}
			svc := newOpenAIRecordUsageServiceForTest(nil, nil, nil, nil)
			svc.httpUpstream, svc.usageBillingRepo = upstream, repo
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			body := []byte(`{"model":"gpt-image-2","prompt":"original prompt","n":2,"size":"1024x1024"}`)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", bytes.NewReader(body))
			c.Request.Header.Set("Content-Type", "application/json")
			if scenario != "non_studio" {
				c.Request.Header.Set("X-Studio-Library", "save")
			}
			price := 0.1
			key := &APIKey{ID: 9, UserID: 7, GroupID: i64p(11), Group: &Group{ID: 11, RateMultiplier: 1, ImageCountBilling: true, ImagePrice1K: &price}}
			c.Set("api_key", key)
			SetStudioImageCollector(c, func(outputs []StudioImageOutput) *StudioLibraryStatus {
				status := &StudioLibraryStatus{Status: "queued", GenerationID: "image_server"}
				for i, output := range outputs {
					if err := library.CollectImage(context.Background(), StudioAssetOwner{UserID: 7, APIKeyID: 9}, "image_server", i, "gpt-image-2", "original prompt", nil, output); err != nil {
						status.Status, status.Code = "warning", "studio_asset_unavailable"
					}
				}
				return status
			})
			parsed, err := svc.ParseOpenAIImagesRequest(c, body)
			require.NoError(t, err)
			account := &Account{ID: 8, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "fixture"}}
			result, err := svc.ForwardImages(context.Background(), c, account, body, parsed, "")
			require.NoError(t, err)
			require.NoError(t, svc.RecordUsage(context.Background(), &OpenAIRecordUsageInput{
				Result: result, APIKey: key, User: &User{ID: 7}, Account: account, RequestPayloadHash: HashUsageRequestPayload(body),
			}))
			require.Equal(t, 1, upstream.calls)
			require.Len(t, repo.commands, 1)
			if baselineCommand == nil {
				baselineCommand = repo.commands[0]
			} else {
				require.Equal(t, baselineCommand, repo.commands[0], "library must not alter the billing command")
			}
			require.Len(t, result.StudioImageOutputs, 2)
			require.Contains(t, rec.Body.String(), "https://images.example/second.png")
			switch scenario {
			case "available":
				require.Len(t, repo.assets, 2)
				require.Contains(t, rec.Body.String(), `"status":"queued"`)
			case "storage_unavailable":
				require.Len(t, repo.assets, 2, "retain retry sources durably even during storage outage")
				require.Contains(t, rec.Body.String(), `"status":"warning"`)
			case "database_unavailable":
				require.Empty(t, repo.assets)
				require.Contains(t, rec.Body.String(), `"status":"warning"`)
			case "non_studio":
				require.Empty(t, repo.assets)
				require.NotContains(t, rec.Body.String(), `"library"`)
			}
		})
	}
}
