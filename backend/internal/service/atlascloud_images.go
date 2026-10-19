package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const (
	atlasCloudGPTImageModel   = "openai/gpt-image-2/text-to-image"
	atlasCloudFlareImageModel = "openai/gpt-image-2.5-flare/text-to-image"
	atlasCloudGrokImageModel  = "xai/grok-imagine-image-2.0-developer/text-to-image"
	atlasCloudImagesPath      = "/model/generateImage"
	atlasCloudPollPath        = "/model/prediction/"
	atlasCloudPollDelay       = 2 * time.Second
	atlasCloudTimeout         = 180 * time.Second
	atlasCloudBodyLimit       = 4 << 20
)

func isAtlasCloudImagesBaseURL(base string) bool {
	parsed, err := url.Parse(strings.TrimSpace(base))
	return err == nil && strings.EqualFold(parsed.Hostname(), "api.atlascloud.ai")
}

func atlasCloudImagePreset(size string) (string, string) {
	size = strings.ReplaceAll(strings.ToLower(strings.TrimSpace(size)), "x", "*")
	switch size {
	case "", "auto":
		return "2048*2048", ImageBillingSize2K
	case "2048*2048", "2304*1728", "1728*2304", "2848*1600", "1600*2848", "2496*1664", "1664*2496", "3136*1344":
		return size, ImageBillingSize2K
	case "4096*4096", "4704*3520", "3520*4704", "5504*3040", "3040*5504", "4992*3328", "3328*4992", "6240*2656":
		return size, ImageBillingSize4K
	default:
		return "", ""
	}
}

func atlasCloudImageSize(model, size string) (string, string) {
	if model == atlasCloudFlareImageModel {
		return atlasCloudFlareSize(size)
	}
	if model == atlasCloudGrokImageModel {
		return "", ""
	}
	if model != atlasCloudGPTImageModel {
		if model != "bytedance/seedream-v4.5" {
			return atlasMediaImageSize(model, size)
		}
		return atlasCloudImagePreset(size)
	}
	size = strings.ReplaceAll(strings.ToLower(strings.TrimSpace(size)), "*", "x")
	switch size {
	case "", "auto":
		size = "1024x1024"
	case "1024x1024", "1024x768", "768x1024", "1024x1536", "1536x1024",
		"2048x2048", "2048x1152", "1152x2048", "2560x1088", "1088x2560",
		"2880x2160", "2160x2880", "3840x2160", "2160x3840":
	default:
		return "", ""
	}
	return size, normalizeOpenAIImageSizeTier(size)
}

func buildAtlasCloudImagePayload(parsed *OpenAIImagesRequest, model string) ([]byte, error) {
	if parsed != nil && model != "bytedance/seedream-v4.5" && model != atlasCloudGPTImageModel && !isAtlasCloudNewImageModel(model) {
		if parsed.Multipart || parsed.Stream || parsed.N != 1 {
			return nil, fmt.Errorf("AtlasCloud media requires JSON, n=1 and stream=false")
		}
		payload, err := buildAtlasMediaContractPayload(parsed.Body, atlasMediaModel(model, "image"), true)
		if err != nil {
			return nil, err
		}
		return json.Marshal(payload)
	}
	if parsed == nil || (model != "bytedance/seedream-v4.5" && model != atlasCloudGPTImageModel && !isAtlasCloudNewImageModel(model)) {
		return nil, fmt.Errorf("unsupported AtlasCloud text-to-image model mapping")
	}
	if parsed.IsEdits() || parsed.Multipart || len(parsed.InputImageURLs) > 0 || parsed.HasMask {
		return nil, fmt.Errorf("AtlasCloud image edits are not supported")
	}
	if parsed.N != 1 || parsed.Stream {
		return nil, fmt.Errorf("AtlasCloud requires n=1 and stream=false")
	}
	if strings.TrimSpace(parsed.Prompt) == "" {
		return nil, fmt.Errorf("prompt is required")
	}
	if parsed.ResponseFormat != "" && parsed.ResponseFormat != "url" {
		return nil, fmt.Errorf("AtlasCloud currently requires response_format=url")
	}
	if (parsed.Background != "" && model != atlasCloudFlareImageModel) || parsed.OutputCompression != nil ||
		parsed.PartialImages != nil || parsed.Style != "" || parsed.InputFidelity != "" || parsed.Moderation != "" {
		return nil, fmt.Errorf("AtlasCloud does not support these OpenAI image options")
	}
	for _, field := range []string{"seed", "image", "images", "image_url", "image_urls", "images_list", "mask"} {
		if value := gjson.GetBytes(parsed.Body, field); value.Exists() && value.Type != gjson.Null {
			return nil, fmt.Errorf("AtlasCloud does not support the %s option in text-to-image mode", field)
		}
	}
	if model == atlasCloudFlareImageModel || model == atlasCloudGrokImageModel {
		return buildAtlasCloudNewImagePayload(parsed, model)
	}
	size, _ := atlasCloudImageSize(model, parsed.Size)
	if size == "" {
		return nil, fmt.Errorf("unsupported AtlasCloud image size; select a documented model preset")
	}
	payload := map[string]any{"model": model, "prompt": parsed.Prompt, "size": size}
	if model == atlasCloudGPTImageModel {
		quality := parsed.Quality
		switch quality {
		case "", "auto":
			quality = "medium"
		case "low", "medium", "high":
		default:
			return nil, fmt.Errorf("AtlasCloud GPT Image 2 quality must be low, medium, high, or auto")
		}
		outputFormat := parsed.OutputFormat
		switch outputFormat {
		case "":
			outputFormat = "jpeg"
		case "jpeg", "png":
		default:
			return nil, fmt.Errorf("AtlasCloud GPT Image 2 output_format must be jpeg or png")
		}
		for _, field := range []string{"enable_sync_mode", "enable_base64_output"} {
			if value := gjson.GetBytes(parsed.Body, field); value.Exists() && value.Type != gjson.False {
				return nil, fmt.Errorf("AtlasCloud requires %s=false", field)
			}
		}
		payload["quality"] = quality
		payload["output_format"] = outputFormat
		payload["enable_sync_mode"] = false
		payload["enable_base64_output"] = false
	} else {
		if parsed.Quality != "" && parsed.Quality != "standard" && parsed.Quality != "auto" {
			return nil, fmt.Errorf("AtlasCloud quality is selected through a 2K or 4K size preset")
		}
		if parsed.OutputFormat != "" {
			return nil, fmt.Errorf("AtlasCloud Seedream does not support output_format")
		}
	}
	return json.Marshal(payload)
}

func atlasCloudValue(raw []byte, field string) gjson.Result {
	if value := gjson.GetBytes(raw, "data."+field); value.Exists() {
		return value
	}
	return gjson.GetBytes(raw, field)
}

func atlasCloudOutputURLs(raw []byte) []string {
	outputs := make([]string, 0)
	for _, item := range atlasCloudValue(raw, "outputs").Array() {
		value := item.String()
		if item.IsObject() {
			value = item.Get("url").String()
		}
		parsed, err := url.Parse(strings.TrimSpace(value))
		if err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil {
			outputs = append(outputs, parsed.String())
		}
	}
	return outputs
}

func atlasCloudImagesError(c *gin.Context, status int, message string) (*OpenAIForwardResult, error) {
	errorType := "upstream_error"
	if status == http.StatusBadRequest {
		errorType = "invalid_request_error"
	}
	err := &OpenAIImagesUpstreamError{StatusCode: status, ErrorType: errorType, Message: message}
	// The Images handler assumes typed upstream errors have already been written.
	writeOpenAIImagesUpstreamErrorResponse(c, err)
	return nil, err
}

func (s *OpenAIGatewayService) forwardAtlasCloudImages(
	ctx context.Context, c *gin.Context, account *Account, parsed *OpenAIImagesRequest, upstreamModel string,
) (*OpenAIForwardResult, error) {
	started := time.Now()
	payload, err := buildAtlasCloudImagePayload(parsed, upstreamModel)
	if err != nil {
		return atlasCloudImagesError(c, http.StatusBadRequest, err.Error())
	}
	base, err := s.validateUpstreamBaseURL(account.GetOpenAIBaseURL())
	if err != nil {
		return atlasCloudImagesError(c, http.StatusBadGateway, "Invalid AtlasCloud account base URL")
	}
	baseURL, err := url.Parse(base)
	if err != nil || baseURL.Scheme != "https" || baseURL.Host != "api.atlascloud.ai" ||
		baseURL.User != nil || baseURL.RawQuery != "" || baseURL.Fragment != "" ||
		(baseURL.Path != "" && baseURL.Path != "/" && strings.TrimRight(baseURL.Path, "/") != "/api/v1") {
		return atlasCloudImagesError(c, http.StatusBadGateway, "Use https://api.atlascloud.ai/api/v1 for this account")
	}
	base = "https://api.atlascloud.ai/api/v1"
	token, _, err := s.GetAccessToken(ctx, account)
	if err != nil {
		return atlasCloudImagesError(c, http.StatusBadGateway, "AtlasCloud account credentials are unavailable")
	}
	// Finish accounting for an accepted paid job even if the browser disconnects.
	// Preserve any caller deadline and impose a hard bound on the whole job.
	deadline := started.Add(atlasCloudTimeout)
	if earlier, ok := ctx.Deadline(); ok && earlier.Before(deadline) {
		deadline = earlier
	}
	ctx, cancel := context.WithDeadline(context.WithoutCancel(ctx), deadline)
	defer cancel()
	payload, err = s.prepareAtlasCloudImageReferences(ctx, account, token, upstreamModel, payload)
	if err != nil {
		return atlasCloudImagesError(c, http.StatusBadRequest, err.Error())
	}
	method, target, body := http.MethodPost, base+atlasCloudImagesPath, payload
	predictionID := ""
	for {
		raw, statusCode, requestErr := s.doAtlasCloudImagesRequest(ctx, account, method, target, token, body)
		if requestErr != nil {
			if ctx.Err() != nil {
				return atlasCloudImagesError(c, http.StatusGatewayTimeout, "AtlasCloud image generation timed out; do not automatically resubmit")
			}
			return atlasCloudImagesError(c, http.StatusBadGateway, "AtlasCloud request failed; do not automatically resubmit")
		}
		if statusCode < 200 || statusCode >= 300 {
			status := http.StatusBadGateway
			if statusCode == http.StatusTooManyRequests {
				status = statusCode
			}
			return atlasCloudImagesError(c, status, "AtlasCloud rejected the image request")
		}
		if !gjson.ValidBytes(raw) {
			return atlasCloudImagesError(c, http.StatusBadGateway, "AtlasCloud returned an invalid response")
		}
		if predictionID == "" {
			predictionID = strings.TrimSpace(atlasCloudValue(raw, "id").String())
		}
		status := strings.ToLower(strings.TrimSpace(atlasCloudValue(raw, "status").String()))
		switch status {
		case "completed", "succeeded", "success":
			outputs := atlasCloudOutputURLs(raw)
			if len(outputs) == 0 {
				return atlasCloudImagesError(c, http.StatusBadGateway, "AtlasCloud returned no image output")
			}
			images := make([]map[string]string, len(outputs))
			for i, output := range outputs {
				images[i] = map[string]string{"url": output}
			}
			responseBody, _ := json.Marshal(map[string]any{"id": predictionID, "created": time.Now().Unix(), "data": images})
			synthetic := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
				Body: io.NopCloser(bytes.NewReader(responseBody))}
			size, tier := atlasCloudRequestedImageSize(parsed, upstreamModel)
			if upstreamModel == atlasCloudGrokImageModel {
				tier = ImageBillingSize1K
				if gjson.GetBytes(payload, "resolution").String() == "2k" {
					tier = ImageBillingSize2K
				}
			}
			if IsAtlasCloudPricedImageModel(upstreamModel) {
				return &OpenAIForwardResult{
					RequestID: predictionID, UpstreamModel: upstreamModel, Model: parsed.Model,
					ImageCount: len(images), ImageSize: tier, ImageInputSize: strings.ReplaceAll(size, "*", "x"),
					UpstreamEndpoint: "/api/v1" + atlasCloudImagesPath,
					ResponseHeaders:  synthetic.Header.Clone(), Duration: time.Since(started),
					AtlasCloudImageResponse: responseBody,
				}, nil
			}
			responseRequest := parsed
			if upstreamModel == atlasCloudGPTImageModel || upstreamModel == atlasCloudFlareImageModel || upstreamModel == atlasCloudGrokImageModel {
				// Keep GPT Image 2 URL-only even when the account enables base64 backfill.
				urlRequest := *parsed
				urlRequest.ResponseFormat = "url"
				responseRequest = &urlRequest
			}
			usage, count, sizes, err := s.handleOpenAIImagesNonStreamingResponse(ctx, synthetic, c, account, responseRequest)
			if err != nil {
				return nil, err
			}
			return &OpenAIForwardResult{
				RequestID: predictionID, UpstreamModel: upstreamModel, Model: parsed.Model, Usage: usage,
				ImageCount: count, ImageSize: tier, ImageInputSize: strings.ReplaceAll(size, "*", "x"),
				ImageOutputSizes: sizes, UpstreamEndpoint: "/api/v1" + atlasCloudImagesPath,
				ResponseHeaders: synthetic.Header.Clone(), Duration: time.Since(started),
			}, nil
		case "created", "pending", "queued", "starting", "processing", "running":
			if predictionID == "" {
				return atlasCloudImagesError(c, http.StatusBadGateway, "AtlasCloud returned no prediction ID")
			}
		default:
			return atlasCloudImagesError(c, http.StatusBadGateway, "AtlasCloud image generation failed or returned an unsupported status")
		}
		timer := time.NewTimer(atlasCloudPollDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return atlasCloudImagesError(c, http.StatusGatewayTimeout, "AtlasCloud image generation timed out; do not automatically resubmit")
		case <-timer.C:
		}
		method, target, body = http.MethodGet, base+atlasCloudPollPath+url.PathEscape(predictionID), nil
	}
}

func (s *OpenAIGatewayService) doAtlasCloudImagesRequest(
	ctx context.Context, account *Account, method, target, token string, body []byte, contentTypes ...string,
) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		contentType := "application/json"
		if len(contentTypes) > 0 {
			contentType = contentTypes[0]
		}
		req.Header.Set("Content-Type", contentType)
	}
	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	resp, err := s.httpUpstream.Do(req, proxyURL, account.ID, account.Concurrency)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, atlasCloudBodyLimit+1))
	if err == nil && len(raw) > atlasCloudBodyLimit {
		err = fmt.Errorf("AtlasCloud response exceeds size limit")
	}
	return raw, resp.StatusCode, err
}
