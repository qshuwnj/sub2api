package service

import (
	"context"
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func dapacaiPreviewFixture(t *testing.T) (*OpenAIGatewayService, *APIKey) {
	s, key := videoFixture(t)
	key.Group.RateMultiplier = 9 // Must be replaced, not multiplied, by independent media rates.
	key.Group.ImageRateIndependent, key.Group.ImageRateMultiplier = true, 1.2
	key.Group.VideoRateIndependent, key.Group.VideoRateMultiplier = true, 1.2
	image, job, second := 0.1, 1.3, 0.41
	key.Group.ModelPricing = []ChannelModelPricing{
		{Platform: PlatformOpenAI, Models: []string{"gpt-image-2.5-cdn"}, BillingMode: BillingModeImage, PerRequestPrice: &image},
		{Platform: PlatformOpenAI, Models: []string{"seedance-2.0", "seedance-2.5-plus", "seedance-2.5-plus-6"}, BillingMode: BillingModePerRequest, PerRequestPrice: &job},
		{Platform: PlatformOpenAI, Models: []string{"seedance-2.5-plus-7"}, BillingMode: BillingModeVideo, PerRequestPrice: &second},
	}
	s.accountRepo.(*videoQuoteAccounts).accounts[0].Credentials["base_url"] = "https://api.dapacai.com"
	return s, key
}

func TestDapacaiPricePreviewMatchesSingleMarkupAndNativeBillingUnits(t *testing.T) {
	s, key := dapacaiPreviewFixture(t)
	now := time.Now()
	for _, duration := range []int{5, 10, 15} {
		req := VideoQuoteRequest{Model: "seedance-2.0", Mode: "text", Controls: map[string]any{"duration": duration, "ratio": "16:9"}}
		q, err := s.PreviewDapacaiVideoPrice(context.Background(), key, req, now)
		require.NoError(t, err)
		require.Equal(t, "1.56000000", q.TotalPrice)
		require.Equal(t, "request", q.BillingUnit)
		require.Equal(t, 1, q.UnitCount)
		require.True(t, q.PricePreview)
		require.WithinDuration(t, now.Add(time.Minute), q.ExpiresAt, time.Second)
		cost := calculateDapacaiVideoCost(key, req.Model, "native", 1, duration, 1.2)
		require.InDelta(t, 1.56, cost.ActualCost, 1e-12)
		raw, err := json.Marshal(q)
		require.NoError(t, err)
		require.NotContains(t, string(raw), "quote_token")
	}
	for _, duration := range []int{4, 15, 30} {
		for _, mode := range []string{"text", "image", "references"} {
			req := VideoQuoteRequest{Model: "seedance-2.5-plus-7", Mode: mode,
				Controls: map[string]any{"duration": duration, "ratio": "16:9", "resolution": "720p"}}
			q, err := s.PreviewDapacaiVideoPrice(context.Background(), key, req, now)
			require.NoError(t, err)
			require.Equal(t, "second", q.BillingUnit)
			require.Equal(t, duration, q.UnitCount)
			require.Equal(t, "0.49200000", q.UnitPrice)
			cost := calculateDapacaiVideoCost(key, req.Model, "720p", 1, duration, 1.2)
			value, err := json.Number(q.TotalPrice).Float64()
			require.NoError(t, err)
			require.InDelta(t, cost.ActualCost, value, 1e-12)
			if duration == 15 {
				require.Equal(t, "7.38000000", q.TotalPrice)
			}
		}
	}
	for _, mode := range []string{"text", "image", "references"} {
		req := VideoQuoteRequest{Model: "seedance-2.5-plus-6", Mode: mode,
			Controls: map[string]any{"duration": 30, "ratio": "16:9", "resolution": "480p"}}
		q, err := s.PreviewDapacaiVideoPrice(context.Background(), key, req, now)
		require.NoError(t, err)
		require.Equal(t, "request", q.BillingUnit)
		require.Equal(t, 1, q.UnitCount)
		require.Equal(t, "1.56000000", q.TotalPrice)
	}
	for _, size := range []string{"1024x1024", "1536x1024", "1024x1536"} {
		req := ImageQuoteRequest{Model: "gpt-image-2.5-cdn", Quality: "none", Size: size, Count: 1}
		q, err := s.PreviewDapacaiImagePrice(context.Background(), key, req, now)
		require.NoError(t, err)
		require.Equal(t, "image", q.BillingUnit)
		require.Equal(t, "0.12000000", q.TotalPrice)
	}
	req := ImageQuoteRequest{Model: "gpt-image-2.5-cdn", Quality: "none", Size: "1024x1024", Count: 4}
	q, err := s.PreviewDapacaiImagePrice(context.Background(), key, req, now)
	require.NoError(t, err)
	require.Equal(t, "0.48000000", q.TotalPrice, "Studio batch means four separate n=1 supplier requests")
}

func TestDapacaiPricePreviewEveryModelUsesItsOwnCardAndExplicitFrames(t *testing.T) {
	for index, model := range []string{"seedance-2.0", "seedance-2.0-plus", "seedance-2.5-plus", "seedance-2.5-plus-1", "seedance-2.5-plus-2", "seedance-2.5-plus-3", "seedance-2.5-plus-4", "seedance-2.5-plus-5", "seedance-2.5-plus-6", "seedance-2.5-plus-7", "wan-3.0"} {
		t.Run(model, func(t *testing.T) {
			s, key := dapacaiPreviewFixture(t)
			price := float64(index + 1) // Synthetic, distinct prices detect a different model's card.
			key.Group.ModelPricing = []ChannelModelPricing{{
				Platform: PlatformOpenAI, Models: []string{model}, BillingMode: BillingModePerRequest, PerRequestPrice: &price,
			}}
			profile := dapacaiVideoModel(model)
			controls := map[string]any{"duration": profile.durations[0], "ratio": profile.ratios[0]}
			if len(profile.resolutions) > 0 {
				controls["resolution"] = profile.resolutions[0]
			}
			for _, mode := range []string{"text", "image", "references", "frames"} {
				req := VideoQuoteRequest{Model: model, Mode: mode, Controls: controls}
				q, err := s.PreviewDapacaiVideoPrice(context.Background(), key, req, time.Now())
				if mode == "frames" && model != "seedance-2.0" || mode == "text" && model == "seedance-2.5-plus" {
					require.ErrorContains(t, err, "unsupported Dapacai video mode")
					continue
				}
				require.NoError(t, err)
				require.Equal(t, req, q.Selection)
				value, err := json.Number(q.TotalPrice).Float64()
				require.NoError(t, err)
				require.InDelta(t, price*1.2, value, 1e-12)
			}
		})
	}
}

func TestDapacaiPricePreviewRejectsContentPricesAndUnsupportedControls(t *testing.T) {
	for _, field := range []string{"prompt", "input", "images", "image_url", "total_price", "speed", "quality"} {
		s, key := dapacaiPreviewFixture(t)
		_, err := s.PreviewDapacaiVideoPrice(context.Background(), key, VideoQuoteRequest{
			Model: "seedance-2.0", Mode: "text", Controls: map[string]any{"duration": 5, "ratio": "16:9", field: "secret"},
		}, time.Now())
		require.Error(t, err, field)
	}
	for _, controls := range []map[string]any{
		{"duration": 30, "ratio": "16:9"}, {"duration": "5", "ratio": "16:9"},
		{"duration": 5.5, "ratio": "16:9"}, {"duration": 5, "ratio": "bad"},
		{"duration": 5, "ratio": "16:9", "resolution": "1080p"},
	} {
		s, key := dapacaiPreviewFixture(t)
		_, err := s.PreviewDapacaiVideoPrice(context.Background(), key,
			VideoQuoteRequest{Model: "seedance-2.0", Mode: "text", Controls: controls}, time.Now())
		require.Error(t, err)
	}
	s, key := dapacaiPreviewFixture(t)
	_, err := s.PreviewDapacaiVideoPrice(context.Background(), key, VideoQuoteRequest{
		Model: "seedance-2.5-plus", Mode: "text", Controls: map[string]any{"duration": 10, "ratio": "16:9", "resolution": "480p"},
	}, time.Now())
	require.ErrorContains(t, err, "unsupported Dapacai video mode")
	for _, req := range []ImageQuoteRequest{
		{Model: "gpt-image-2.5-cdn", Quality: "high", Size: "1024x1024", Count: 1},
		{Model: "gpt-image-2.5-cdn", Quality: "none", Size: "2048x2048", Count: 1},
		{Model: "gpt-image-2.5-cdn", Quality: "none", Size: "1024x1024", Count: 5},
	} {
		_, err := s.PreviewDapacaiImagePrice(context.Background(), key, req, time.Now())
		require.Error(t, err)
	}
}

func TestDapacaiPricePreviewUsesFreshGroupAndRejectsOldAccounts(t *testing.T) {
	for _, scenario := range []string{"missing card", "old account", "wrong binding", "inactive key", "disallowed", "invalid multiplier"} {
		t.Run(scenario, func(t *testing.T) {
			s, key := dapacaiPreviewFixture(t)
			switch scenario {
			case "missing card":
				fresh := *key.Group
				fresh.ModelPricing = nil
				s.channelService.groupRepo.(*videoQuoteGroups).group = &fresh
			case "old account":
				s.accountRepo.(*videoQuoteAccounts).accounts[0].Credentials["base_url"] = "https://api.atlascloud.ai"
			case "wrong binding":
				s.accountRepo.(*videoQuoteAccounts).accounts[0].GroupIDs = []int64{999}
			case "inactive key":
				fresh := *key
				fresh.Status = "disabled"
				s.billingCacheService.apiKeyRateLimitLoader.(*videoQuoteKeys).key = &fresh
			case "disallowed":
				key.Group.AllowImageGeneration = false
			case "invalid multiplier":
				key.Group.VideoRateMultiplier = math.NaN()
			}
			_, err := s.PreviewDapacaiVideoPrice(context.Background(), key, VideoQuoteRequest{
				Model: "seedance-2.0", Mode: "text", Controls: map[string]any{"duration": 5, "ratio": "16:9"},
			}, time.Now())
			require.Error(t, err)
		})
	}
}
