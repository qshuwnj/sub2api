//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestAtlasCloudNewImageModels(t *testing.T) {
	for _, model := range []string{
		"openai/gpt-image-2.5-flare/text-to-image",
		"xai/grok-imagine-image-2.0-developer/text-to-image",
	} {
		t.Run(model, func(t *testing.T) {
			require.NoError(t, validateCompatibleImagesModel(model))
			require.Equal(t, OpenAIImagesCapabilityAPIKey,
				(&OpenAIImagesRequest{}).RequiredCapabilityForModel(model))
			payload, err := buildAtlasCloudImagePayload(&OpenAIImagesRequest{Prompt: "draw", N: 1}, model)
			require.NoError(t, err)
			require.Equal(t, model, gjson.GetBytes(payload, "model").String())
		})
	}
}

func TestAtlasCloudNewImagesRequireExplicitPricing(t *testing.T) {
	svc := &OpenAIGatewayService{}
	req := &OpenAIImagesRequest{Model: "xai/grok-imagine-image-2.0-developer/text-to-image", Prompt: "draw", N: 1,
		Body: []byte(`{"resolution":"2k"}`)}
	key := &APIKey{Group: &Group{}}
	require.Error(t, svc.ValidateAtlasCloudImagePricing(context.Background(), key, req, req.Model))
	price := 0.25
	key.Group.ImagePrice2K = &price
	require.NoError(t, svc.ValidateAtlasCloudImagePricing(context.Background(), key, req, req.Model))
	require.Error(t, svc.ValidateAtlasCloudImagePricing(context.Background(), key, req, atlasCloudGPTImageModel))
}

func TestAtlasCloudAllImagesRequireExplicitPricing(t *testing.T) {
	req := &OpenAIImagesRequest{Model: atlasCloudGPTImageModel, Prompt: "draw", N: 1}
	require.Error(t, (&OpenAIGatewayService{}).ValidateAtlasCloudImagePricing(context.Background(), &APIKey{Group: &Group{}}, req, req.Model))
}

func TestAtlasCloudImagesRejectMissingTierButAllowExplicitZero(t *testing.T) {
	billing := NewBillingService(nil, nil)
	svc := &OpenAIGatewayService{billingService: billing, resolver: NewModelPricingResolver(nil, billing)}
	model := atlasCloudGrokImageModel
	price := 0.25
	key := &APIKey{Group: &Group{ModelPricing: []ChannelModelPricing{{
		Models: []string{model}, BillingMode: BillingModeImage,
		Intervals: []PricingInterval{{TierLabel: "1K", PerRequestPrice: &price}},
	}}}}
	req := &OpenAIImagesRequest{Model: model, Prompt: "draw", N: 1, Body: []byte(`{"resolution":"2k"}`)}
	require.Error(t, svc.ValidateAtlasCloudImagePricing(context.Background(), key, req, model))
	zero := 0.0
	key.Group.ModelPricing[0].Intervals = append(key.Group.ModelPricing[0].Intervals, PricingInterval{TierLabel: "2K", PerRequestPrice: &zero})
	key.Group.ModelPricing[0].PerRequestPrice = &price
	require.NoError(t, svc.ValidateAtlasCloudImagePricing(context.Background(), key, req, model))
	cost := svc.calculateOpenAIImageCost(context.Background(), model, key, &OpenAIForwardResult{ImageCount: 1, ImageSize: ImageBillingSize2K}, 1)
	require.Zero(t, cost.TotalCost, "explicit free tier must not fall through to another tier")
}

func TestAtlasCloudNewImagesRejectMalformedOptions(t *testing.T) {
	for _, body := range []string{`{"n":1.5}`, `{"prompt":123}`, `{"quality":42}`, `{"seed":1}`, `{"num_images":4}`} {
		_, err := buildAtlasCloudImagePayload(&OpenAIImagesRequest{
			Prompt: "draw", N: 1, Body: []byte(body),
		}, "xai/grok-imagine-image-2.0-developer/text-to-image")
		require.Error(t, err, body)
	}
}

func TestAtlasCloudFlareImageOptions(t *testing.T) {
	payload, err := buildAtlasCloudImagePayload(&OpenAIImagesRequest{
		Prompt: "draw", N: 1, Size: "1024x1280", Quality: "xhigh",
		Background: "transparent", OutputFormat: "webp",
	}, "openai/gpt-image-2.5-flare/text-to-image")
	require.NoError(t, err)
	require.Equal(t, "1024x1280", gjson.GetBytes(payload, "size").String())
	require.Equal(t, "xhigh", gjson.GetBytes(payload, "quality").String())
	require.Equal(t, "transparent", gjson.GetBytes(payload, "background").String())
	require.Equal(t, "webp", gjson.GetBytes(payload, "output_format").String())
	require.False(t, gjson.GetBytes(payload, "enable_sync_mode").Exists())
}

func TestAtlasCloudGrokImageOptions(t *testing.T) {
	payload, err := buildAtlasCloudImagePayload(&OpenAIImagesRequest{
		Prompt: "draw", N: 1, Quality: "low",
		Body: []byte(`{"aspect_ratio":"16:9","resolution":"2k"}`),
	}, "xai/grok-imagine-image-2.0-developer/text-to-image")
	require.NoError(t, err)
	require.Equal(t, "16:9", gjson.GetBytes(payload, "aspect_ratio").String())
	require.Equal(t, "2k", gjson.GetBytes(payload, "resolution").String())
	require.Equal(t, int64(1), gjson.GetBytes(payload, "num_images").Int())
	require.False(t, gjson.GetBytes(payload, "size").Exists())
}

func TestAtlasCloudNewImagesForwardURLAndBillingTier(t *testing.T) {
	for _, tc := range []struct{ model, options, tier string }{
		{"openai/gpt-image-2.5-flare/text-to-image", `"size":"1024x1280","quality":"xhigh","background":"transparent","output_format":"webp"`, ImageBillingSize2K},
		{"xai/grok-imagine-image-2.0-developer/text-to-image", `"resolution":"2k","aspect_ratio":"16:9"`, ImageBillingSize2K},
	} {
		t.Run(tc.model, func(t *testing.T) {
			upstream := &atlasImageUpstream{replies: []string{`{"code":200,"data":{"id":"ready","status":"completed","outputs":["https://cdn.example/output.png"]}}`}}
			body := []byte(`{"model":"` + tc.model + `","prompt":"draw",` + tc.options + `}`)
			svc, c, w, account, parsed := atlasGPTImageFixture(t, upstream, body)
			account.Extra = map[string]any{AccountExtraImagesURLToB64JSON: true}
			require.Equal(t, tc.tier, parsed.SizeTier)
			result, err := svc.ForwardImages(context.Background(), c, account, body, parsed, "")
			require.NoError(t, err)
			require.Equal(t, tc.tier, result.ImageSize)
			require.Equal(t, tc.model, result.UpstreamModel)
			require.Equal(t, 1, result.ImageCount)
			require.Empty(t, w.Body.String(), "withhold image URL until settlement")
			require.Equal(t, "https://cdn.example/output.png", gjson.GetBytes(result.AtlasCloudImageResponse, "data.0.url").String())
			require.NotContains(t, string(result.AtlasCloudImageResponse), "upstream-only")
			require.Len(t, upstream.methods, 1)
		})
	}
}
