//go:build unit

package service

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func grokCountRequest(t *testing.T, model, counts string) *OpenAIImagesRequest {
	t.Helper()
	input := map[string]any{
		"model": model, "prompt": "Non-billable regression fixture", "response_format": "url",
		"quality": "medium", "resolution": "1k", "aspect_ratio": "3:4",
	}
	for field, reference := range atlasMediaModel(model, "image").MediaFields {
		if reference.Array {
			input[field] = []string{"https://assets.example/reference.png"}
		} else {
			input[field] = "https://assets.example/reference.png"
		}
	}
	var overrides map[string]any
	require.NoError(t, json.Unmarshal([]byte(`{`+counts+`}`), &overrides))
	for field, value := range overrides {
		input[field] = value
	}
	body, err := json.Marshal(input)
	require.NoError(t, err)
	parsed := &OpenAIImagesRequest{N: 1, Body: body}
	require.NoError(t, parseOpenAIImagesJSONRequest(body, parsed))
	return parsed
}

func TestAtlasGrokImageCountAliases(t *testing.T) {
	for _, model := range []string{
		atlasCloudGrokImageModel,
		"xai/grok-imagine-image-2.0/text-to-image",
		"xai/grok-imagine-image-2.0-developer/edit",
		"xai/grok-imagine-image-2.0/edit",
	} {
		for _, counts := range []string{`"n":1`, `"num_images":1`, `"n":1,"num_images":1`} {
			t.Run(model+"/"+counts, func(t *testing.T) {
				payload, err := buildAtlasCloudImagePayload(grokCountRequest(t, model, counts), model)
				require.NoError(t, err)
				require.Equal(t, int64(1), gjson.GetBytes(payload, "num_images").Int())
				require.False(t, gjson.GetBytes(payload, "n").Exists())
			})
		}
	}
}

func TestAtlasGrokImageCountQuoteAdmission(t *testing.T) {
	s, key := miniQuoteFixture()
	req := ImageQuoteRequest{
		Model: atlasCloudGrokImageModel, Quality: "medium",
		Resolution: "1k", AspectRatio: "3:4", Count: 4,
	}
	key.Group.RateMultiplier = 1
	key.Group.ModelPricing[0].Models = []string{req.Model}
	key.Group.ModelPricing[0].Intervals = []PricingInterval{{
		TierLabel: "quality:medium:" + imageNativeControlKey(req), PerRequestPrice: quoteFloat(0.21),
	}}
	now := time.Now()
	quote, err := s.QuoteImageQuality(context.Background(), key, req, now)
	require.NoError(t, err)
	require.Equal(t, "0.84000000", quote.Total)
	for _, counts := range []string{`"n":1`, `"num_images":1`, `"n":1,"num_images":1`} {
		parsed := grokCountRequest(t, req.Model, counts)
		for index := 0; index < req.Count; index++ {
			snapshot, err := s.AcceptImageQualityQuote(context.Background(), key, parsed, quote.Token, index, now)
			require.NoError(t, err, counts)
			require.NoError(t, snapshot.validateUpstream(parsed, req.Model))
			require.Equal(t, "0.21000000", snapshot.Receipt().Charged)
		}
	}
	for _, counts := range []string{
		`"n":1,"num_images":2`, `"n":2,"num_images":1`, `"n":1.5`,
		`"num_images":0`, `"num_images":-1`, `"num_images":1.5`,
		`"num_images":"1"`, `"num_images":true`, `"num_images":null`,
		`"n":1,"seed":123`, `"n":1,"enable_base64_output":true`,
		`"n":1,"resolution":"4k"`, `"n":1,"aspect_ratio":"7:5"`,
	} {
		parsed := grokCountRequest(t, req.Model, counts)
		_, err := s.AcceptImageQualityQuote(context.Background(), key, parsed, quote.Token, 0, now)
		require.Error(t, err, counts)
		_, err = buildAtlasCloudImagePayload(parsed, req.Model)
		require.Error(t, err, counts)
	}
}

func TestAtlasGrokImageQuoteGenerationControlsAgree(t *testing.T) {
	contract := atlasMediaModel(atlasCloudGrokImageModel, "image")
	for _, quality := range contract.Input.Properties["quality"].Enum {
		for _, resolution := range contract.Input.Properties["resolution"].Enum {
			for _, ratio := range contract.Input.Properties["aspect_ratio"].Enum {
				req := ImageQuoteRequest{
					Model: atlasCloudGrokImageModel, Quality: quality.(string),
					Resolution: resolution.(string), AspectRatio: ratio.(string), Count: 1,
				}
				normalized, err := req.normalized()
				require.NoError(t, err)
				body, err := json.Marshal(map[string]any{
					"model": req.Model, "prompt": "Non-billable regression fixture",
					"quality": req.Quality, "resolution": req.Resolution,
					"aspect_ratio": req.AspectRatio, "n": 1, "num_images": 1,
				})
				require.NoError(t, err)
				payload, err := buildAtlasCloudImagePayload(&OpenAIImagesRequest{
					Model: req.Model, Prompt: "Non-billable regression fixture",
					Quality: req.Quality, N: 1, Body: body,
				}, req.Model)
				require.NoError(t, err)
				require.Equal(t, normalized.Quality, gjson.GetBytes(payload, "quality").String())
				require.Equal(t, normalized.Resolution, gjson.GetBytes(payload, "resolution").String())
				require.Equal(t, normalized.AspectRatio, gjson.GetBytes(payload, "aspect_ratio").String())
			}
		}
	}
}
