//go:build unit

package service

import (
	"context"
	"math"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestDapacaiUSDPricingUsesOneMarkupAndNoAtlasFX(t *testing.T) {
	upstream := 0.1
	key := &APIKey{Group: &Group{
		RateMultiplier: 9, ImageRateIndependent: true, ImageRateMultiplier: 1.2,
		VideoRateIndependent: true, VideoRateMultiplier: 1.2, ImagePrice1K: &upstream,
		ModelPricing: []ChannelModelPricing{{
			Platform: PlatformOpenAI, Models: []string{"seedance-2.0"},
			BillingMode: BillingModePerRequest, PerRequestPrice: &upstream,
		}},
	}}
	image := (&BillingService{}).CalculateImageCost("gpt-image-2.5-cdn", "1K", 1,
		imagePriceConfigFromAPIKey(key), resolveImageRateMultiplier(key, 9))
	require.InDelta(t, 0.12, image.ActualCost, 1e-12)
	for _, duration := range []int{5, 10, 15} {
		require.True(t, HasDapacaiVideoPrice(key, "seedance-2.0", "native"))
		cost := (&OpenAIGatewayService{}).calculateOpenAIVideoCost(context.Background(),
			"seedance-2.0", key, &OpenAIForwardResult{
				VideoCount: 1, VideoDurationSeconds: duration, VideoResolution: "native",
			}, resolveVideoRateMultiplier(key, 9))
		require.InDelta(t, 0.1, cost.TotalCost, 1e-12)
		require.InDelta(t, 0.12, cost.ActualCost, 1e-12, "fixed job price must not multiply duration")
	}
	key.Group.ModelPricing[0].BillingMode = BillingModeVideo
	cost := calculateDapacaiVideoCost(key, "seedance-2.0", "native", 2, 15, 1.2)
	require.InDelta(t, 3, cost.TotalCost, 1e-12)
	require.InDelta(t, 3.6, cost.ActualCost, 1e-12)
}

func TestDapacaiPriceCardAdmissionRejectsGuessesAndInvalidOverrides(t *testing.T) {
	price := 0.39
	for _, scenario := range []string{"valid", "zero", "wildcard", "wrong platform", "missing tier", "nan", "negative", "disabled", "wrong tier", "time multiplier"} {
		t.Run(scenario, func(t *testing.T) {
			p := price
			card := ChannelModelPricing{Platform: PlatformOpenAI, Models: []string{"seedance-2.0"},
				BillingMode: BillingModePerRequest, PerRequestPrice: &p}
			key := &APIKey{Group: &Group{VideoPrice480P: &price}}
			model, tier := "seedance-2.0", "native"
			switch scenario {
			case "zero":
				p = 0
			case "wildcard":
				card.Models = []string{"seedance-*"}
			case "wrong platform":
				card.Platform = PlatformAnthropic
			case "missing tier":
				card.Intervals = []PricingInterval{{TierLabel: "720p", PerRequestPrice: &p}}
			case "nan":
				p = math.NaN()
			case "negative":
				p = -1
			case "disabled":
				model, card.Models = "minimax-h3", []string{"minimax-h3"}
			case "wrong tier":
				tier = "1080p"
			case "time multiplier":
				card.TimePricing = &ChannelTimePricing{}
			}
			key.Group.ModelPricing = []ChannelModelPricing{card}
			require.Equal(t, scenario == "valid" || scenario == "zero", HasDapacaiVideoPrice(key, model, tier))
		})
	}
}

func TestDapacaiUSDPricingDoesNotChangeAtlasTariffs(t *testing.T) {
	billing := &BillingService{}
	model := "google/gemini-omni-1.1-flash/text-to-video"
	cost := billing.CalculateVideoCost(model, "720p", 1, 5, nil, 1)
	require.InDelta(t, 0.099*5*8.33232, cost.ActualCost, 1e-10)
}

func TestDapacaiInflightReservationUsesSameFixedJobAndMultiplier(t *testing.T) {
	gid := int64(72)
	price := 1.3
	key := &APIKey{User: &User{ID: 1}, GroupID: &gid, Group: &Group{
		ID: gid, Platform: PlatformOpenAI, RateMultiplier: 1.2,
		VideoRateIndependent: true, VideoRateMultiplier: 1.2,
		ModelPricing: []ChannelModelPricing{{
			Platform: PlatformOpenAI, Models: []string{"seedance-2.0"},
			BillingMode: BillingModePerRequest, PerRequestPrice: &price,
		}},
	}}
	svc := newInflightEstimateGateway(t, nil)
	for _, duration := range []int{5, 10, 15} {
		cost, priced := svc.EstimateInflightReservation(context.Background(), key, InflightEstimateRequest{
			Model: "seedance-2.0", Kind: InflightEstimateVideo, Units: 1,
			VideoResolution: "native", VideoDurationSeconds: duration,
		})
		require.True(t, priced)
		require.InDelta(t, 1.56, cost, 1e-12)
	}
}

func TestDapacaiPresetPriceCardsValidateAndKeepTokenUnits(t *testing.T) {
	image, videoJob, videoSecond := 0.1, 1.3, 0.41
	input, output, cache := 0.000005, 0.00003, 0.0000005
	cards, err := normalizeGroupModelPricing(PlatformOpenAI, []ChannelModelPricing{
		{Platform: PlatformOpenAI, Models: []string{"gpt-image-2.5-cdn"},
			BillingMode: BillingModeImage, PerRequestPrice: &image},
		{Platform: PlatformOpenAI, Models: []string{"seedance-2.0"},
			BillingMode: BillingModePerRequest, PerRequestPrice: &videoJob},
		{Platform: PlatformOpenAI, Models: []string{"seedance-2.5-plus-7"},
			BillingMode: BillingModeVideo, PerRequestPrice: &videoSecond},
		{Platform: PlatformOpenAI, Models: []string{"gpt-5.6-sol"},
			BillingMode: BillingModeToken, InputPrice: &input, OutputPrice: &output, CacheReadPrice: &cache},
	})
	require.NoError(t, err)
	group := &Group{ID: 72, Platform: PlatformOpenAI, RateMultiplier: 1.2, ModelPricing: cards}
	billing := NewBillingService(&config.Config{}, nil)
	cost, err := billing.CalculateCostUnified(CostInput{
		Ctx: context.Background(), Group: group, Model: "gpt-5.6-sol", RateMultiplier: 1.2,
		Tokens:   UsageTokens{InputTokens: 1_000_000, OutputTokens: 1_000_000},
		Resolver: NewModelPricingResolver(nil, billing),
	})
	require.NoError(t, err)
	require.InDelta(t, 35, cost.TotalCost, 1e-10)
	require.InDelta(t, 42, cost.ActualCost, 1e-10)
}
