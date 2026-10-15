package service

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/tidwall/gjson"
)

func isAtlasCloudNewImageModel(model string) bool {
	return model == atlasCloudFlareImageModel || model == atlasCloudGrokImageModel
}

func IsAtlasCloudPricedImageModel(model string) bool {
	return isAtlasCloudNewImageModel(model) || model == atlasCloudGPTImageModel
}

// Admission uses configured customer prices, never a guessed catalog fallback.
func (s *OpenAIGatewayService) ValidateAtlasCloudImagePricing(ctx context.Context, key *APIKey, parsed *OpenAIImagesRequest, upstreamModel string) error {
	if parsed == nil || (!IsAtlasCloudPricedImageModel(parsed.Model) && !IsAtlasCloudPricedImageModel(upstreamModel)) {
		return nil
	}
	if parsed.Model != upstreamModel {
		return fmt.Errorf("AtlasCloud images require the exact upstream model ID")
	}
	payload, err := buildAtlasCloudImagePayload(parsed, upstreamModel)
	if err != nil {
		return err
	}
	_, tier := atlasCloudImageSize(upstreamModel, parsed.Size)
	if upstreamModel == atlasCloudGrokImageModel {
		tier = ImageBillingSize1K
		if gjson.GetBytes(payload, "resolution").String() == "2k" {
			tier = ImageBillingSize2K
		}
	}
	if key == nil || key.Group == nil {
		return fmt.Errorf("AtlasCloud image pricing is not configured")
	}
	resolved := s.resolveOpenAIChannelPricing(ctx, upstreamModel, key)
	if resolved != nil && resolved.Mode != BillingModeImage && resolved.Mode != BillingModePerRequest {
		return fmt.Errorf("AtlasCloud images require image or per-request pricing")
	}
	if price := key.Group.GetImagePrice(tier); price != nil &&
		*price >= 0 && !math.IsNaN(*price) && !math.IsInf(*price, 0) {
		return nil
	}
	if atlasCloudConfiguredImageTierPrice(resolved, tier) != nil {
		return nil
	}
	return fmt.Errorf("configure an AtlasCloud image price for the requested size before generation")
}

func validAtlasCloudImagePrice(price *float64) bool {
	return price != nil && *price >= 0 && !math.IsNaN(*price) && !math.IsInf(*price, 0)
}

func atlasCloudConfiguredImageTierPrice(resolved *ResolvedPricing, tier string) *float64 {
	if resolved == nil || (resolved.Mode != BillingModeImage && resolved.Mode != BillingModePerRequest) {
		return nil
	}
	for _, entry := range resolved.RequestTiers {
		if entry.TierLabel == tier && validAtlasCloudImagePrice(entry.PerRequestPrice) {
			return entry.PerRequestPrice
		}
	}
	if resolved.channelPricing != nil && validAtlasCloudImagePrice(resolved.channelPricing.PerRequestPrice) {
		return resolved.channelPricing.PerRequestPrice
	}
	return nil
}

func atlasCloudFlareSize(size string) (string, string) {
	size = strings.ReplaceAll(strings.ToLower(strings.TrimSpace(size)), "*", "x")
	if size == "" || size == "auto" {
		return "1024x1024", ImageBillingSize1K
	}
	parts := strings.Split(size, "x")
	if len(parts) != 2 {
		return "", ""
	}
	w, ew := strconv.Atoi(parts[0])
	h, eh := strconv.Atoi(parts[1])
	if ew != nil || eh != nil || w < 16 || h < 16 || w > 3840 || h > 3840 ||
		w%16 != 0 || h%16 != 0 || w > h*3 || h > w*3 || w*h > 3840*2160 {
		return "", ""
	}
	return size, normalizeOpenAIImageSizeTier(size)
}

func atlasCloudOption(body []byte, key, fallback string, values ...string) (string, error) {
	value := gjson.GetBytes(body, key)
	if !value.Exists() {
		return fallback, nil
	}
	if value.Type != gjson.String {
		return "", fmt.Errorf("%s must be a string", key)
	}
	for _, allowed := range values {
		if value.String() == allowed {
			return allowed, nil
		}
	}
	return "", fmt.Errorf("unsupported %s", key)
}

func atlasCloudCheckFields(body []byte, fields ...string) error {
	if len(body) == 0 {
		return nil
	}
	if !gjson.ValidBytes(body) || !gjson.ParseBytes(body).IsObject() {
		return fmt.Errorf("request body must be a JSON object")
	}
	allowed := make(map[string]bool, len(fields))
	for _, field := range fields {
		allowed[field] = true
	}
	var invalid string
	gjson.ParseBytes(body).ForEach(func(key, value gjson.Result) bool {
		if !allowed[key.String()] {
			invalid = key.String()
			return false
		}
		return true
	})
	if invalid != "" {
		return fmt.Errorf("unsupported AtlasCloud option: %s", invalid)
	}
	return nil
}

func buildAtlasCloudNewImagePayload(parsed *OpenAIImagesRequest, model string) ([]byte, error) {
	fields := []string{"model", "prompt", "n", "stream", "size", "quality", "response_format"}
	if model == atlasCloudFlareImageModel {
		fields = append(fields, "background", "output_format")
	} else {
		fields = append(fields, "aspect_ratio", "resolution", "enable_base64_output")
	}
	if err := atlasCloudCheckFields(parsed.Body, fields...); err != nil {
		return nil, err
	}
	for _, field := range []string{"model", "prompt", "size", "quality", "response_format", "background", "output_format"} {
		if v := gjson.GetBytes(parsed.Body, field); v.Exists() && v.Type != gjson.String {
			return nil, fmt.Errorf("%s must be a string", field)
		}
	}
	if n := gjson.GetBytes(parsed.Body, "n"); n.Exists() && (n.Type != gjson.Number || n.Float() != 1) {
		return nil, fmt.Errorf("AtlasCloud image adapter currently requires n=1")
	}
	payload := map[string]any{"model": model, "prompt": parsed.Prompt}
	quality := parsed.Quality
	if quality == "" {
		quality = "medium"
	}
	if model == atlasCloudFlareImageModel {
		if utf8.RuneCountInString(parsed.Prompt) > 32000 {
			return nil, fmt.Errorf("prompt exceeds 32000 characters")
		}
		switch quality {
		case "auto", "low", "medium", "high", "xhigh", "max":
		default:
			return nil, fmt.Errorf("unsupported Flare quality")
		}
		size, _ := atlasCloudFlareSize(parsed.Size)
		if size == "" {
			return nil, fmt.Errorf("unsupported Flare size")
		}
		format := parsed.OutputFormat
		if format == "" {
			format = "png"
		}
		if format != "png" && format != "jpeg" && format != "webp" {
			return nil, fmt.Errorf("unsupported Flare output_format")
		}
		background := parsed.Background
		if background == "" {
			background = "auto"
		}
		if background != "auto" && background != "opaque" && background != "transparent" {
			return nil, fmt.Errorf("unsupported Flare background")
		}
		if background == "transparent" && format == "jpeg" {
			return nil, fmt.Errorf("transparent background requires png or webp")
		}
		payload["size"], payload["quality"], payload["output_format"] = size, quality, format
		payload["background"], payload["n"] = background, 1
	} else {
		if utf8.RuneCountInString(parsed.Prompt) > 8000 {
			return nil, fmt.Errorf("prompt exceeds 8000 characters")
		}
		if quality != "low" && quality != "medium" {
			return nil, fmt.Errorf("Grok image quality must be low or medium")
		}
		if parsed.Size != "" && parsed.Size != "auto" {
			return nil, fmt.Errorf("Grok images use aspect_ratio and resolution instead of size")
		}
		ratio, err := atlasCloudOption(parsed.Body, "aspect_ratio", "1:1",
			"1:1", "3:4", "4:3", "9:16", "16:9", "2:3", "3:2", "9:19.5", "19.5:9", "9:20", "20:9", "1:2", "2:1")
		if err != nil {
			return nil, err
		}
		resolution, err := atlasCloudOption(parsed.Body, "resolution", "1k", "1k", "2k")
		if err != nil {
			return nil, err
		}
		if flag := gjson.GetBytes(parsed.Body, "enable_base64_output"); flag.Exists() && flag.Type != gjson.False {
			return nil, fmt.Errorf("enable_base64_output must be false")
		}
		payload["aspect_ratio"], payload["resolution"], payload["quality"] = ratio, resolution, quality
		payload["num_images"], payload["enable_base64_output"] = 1, false
	}
	return json.Marshal(payload)
}
