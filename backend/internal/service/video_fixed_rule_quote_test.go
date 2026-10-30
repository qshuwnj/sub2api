package service

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

func fixedVideoBody(t *testing.T, req VideoQuoteRequest) []byte {
	t.Helper()
	payload := map[string]any{"model": req.Model, "prompt": "An offline price-lock regression scene"}
	for field, value := range req.Controls {
		payload[field] = value
	}
	if req.Mode == "i2v" || req.Mode == "r2v" {
		contract := atlasMediaModel(req.Model, "video")
		for _, field := range []string{"image", "image_url", "images", "image_urls", "reference_images"} {
			if media, ok := contract.MediaFields[field]; ok {
				if media.Array {
					payload[field] = []string{"https://assets.example/input.png"}
				} else {
					payload[field] = "https://assets.example/input.png"
				}
				break
			}
		}
	}
	body, err := json.Marshal(payload)
	require.NoError(t, err)
	return body
}

func TestVideoFixedRuleQuoteStudioControlMatrix(t *testing.T) {
	raw, err := os.ReadFile("testdata/atlas_video_quote_matrix.json")
	require.NoError(t, err)
	var models []struct {
		Model    string `json:"model"`
		Mode     string `json:"mode"`
		Variants []struct {
			Body      map[string]any `json:"body"`
			Controls  map[string]any `json:"controls"`
			Durations []int          `json:"durations"`
		} `json:"variants"`
	}
	require.NoError(t, json.Unmarshal(raw, &models))
	require.Len(t, models, 91)
	combinations, inputDerivedModels := 0, 0
	for _, model := range models {
		t.Run(model.Model, func(t *testing.T) {
			s, key := videoFixture(t)
			key.Group.ModelPricing = nil
			derived := false
			for _, variant := range model.Variants {
				resolution, _ := variant.Controls["resolution"].(string)
				tier := atlasMediaVideoResolutionOrDefault(model.Model, resolution)
				// Exercise exact model/tier precedence over a different flat rate.
				flat := 0.7
				key.Group.VideoPrice480P, key.Group.VideoPrice720P, key.Group.VideoPrice1080P = &flat, &flat, &flat
				priceModel := CanonicalGrokImagineVideoPriceFamily(model.Model)
				if priceModel == "" {
					priceModel = model.Model
				}
				key.Group.VideoModelPrices = map[string]map[string]float64{priceModel: {tier: 0.2}}
				key.Group.RateMultiplier = 1.5
				for _, seconds := range variant.Durations {
					var duration any = float64(seconds)
					if _, nativeString := variant.Controls["duration"].(string); nativeString {
						duration = fmt.Sprint(seconds)
					}
					variant.Controls["duration"], variant.Body["duration"] = duration, duration
					req := VideoQuoteRequest{Model: model.Model, Mode: model.Mode, Controls: variant.Controls}
					now := time.Now()
					quote, err := s.QuoteVideo(context.Background(), key, req, now)
					require.NoError(t, err, "%s %v", model.Model, req.Controls)
					require.True(t, quote.Enrolled)
					require.False(t, quote.EstimateOnly)
					require.Equal(t, "configured_rule", quote.PriceSource)
					require.True(t, quote.SupplierEstimated, "fixed customer price is not an exact supplier invoice")
					want := decimal.NewFromInt(int64(seconds)).Mul(decimal.RequireFromString("0.3")).StringFixed(8)
					require.Equal(t, want, quote.TotalPrice)
					body, err := json.Marshal(variant.Body)
					require.NoError(t, err)
					snapshot, err := s.AcceptVideoQuote(context.Background(), key, body, quote.Token, now)
					require.NoError(t, err)
					require.Equal(t, want, snapshot.TotalPrice())
					require.Equal(t, req.Model, snapshot.Selection().Model, "never switch the exact selected model")
					combinations++
					if model.Mode == "i2v" &&
						(variant.Controls["ratio"] == "adaptive" || variant.Controls["aspect_ratio"] == "auto" ||
							variant.Controls["ratio"] == nil && variant.Controls["aspect_ratio"] == nil) {
						derived = true
					}
				}
			}
			if derived {
				inputDerivedModels++
			}
		})
	}
	require.Equal(t, 15950, combinations)
	// The 28 blocked default I2V entries plus five other models which expose an
	// optional automatic ratio must all retain their native framing controls.
	require.Equal(t, 33, inputDerivedModels)
}

func TestVideoFixedRuleQuoteLocksConfiguredCustomerPrice(t *testing.T) {
	s, key := videoFixture(t)
	key.Group.ModelPricing = nil
	price := 0.25
	key.Group.VideoPrice720P = &price
	key.Group.RateMultiplier = 1.5
	now := time.Now().UTC()

	quote, err := s.QuoteVideo(context.Background(), key, videoTestRequest(), now)
	require.NoError(t, err)
	require.True(t, quote.Enrolled, "a configured rule must enter durable quoted billing")
	require.False(t, quote.EstimateOnly)
	require.NotEmpty(t, quote.Token)
	require.Equal(t, "3.00000000", quote.TotalPrice, "0.25 per second * 8 seconds * 1.5")
	wire, err := json.Marshal(quote)
	require.NoError(t, err)
	var response map[string]any
	require.NoError(t, json.Unmarshal(wire, &response))
	require.Equal(t, "configured_rule", response["price_source"])

	body := fixedVideoBody(t, videoTestRequest())
	snapshot, err := s.AcceptVideoQuote(context.Background(), key, body, quote.Token, now)
	require.NoError(t, err)
	require.Equal(t, "2.00000000", snapshot.BasePrice())
	require.Equal(t, "3.00000000", snapshot.TotalPrice())

	price = 0.5
	_, err = s.AcceptVideoQuote(context.Background(), key, body, quote.Token, now)
	require.Error(t, err, "an unaccepted quote must be refreshed after a configured price edit")
	require.Equal(t, "3.00000000", snapshot.TotalPrice(), "an accepted price never changes")
}

func TestVideoFixedRuleQuoteSupportsInputDerivedAndAdaptiveRatios(t *testing.T) {
	for _, raw := range []string{
		`{"model":"kwaivgi/kling-v2.5-turbo-pro/image-to-video","mode":"i2v","controls":{"duration":5}}`,
		`{"model":"vidu/q1/image-to-video","mode":"i2v","controls":{"movement_amplitude":"auto","generate_audio":true,"bgm":true,"resolution":"1080p","duration":5}}`,
		`{"model":"bytedance/seedance-2.5/image-to-video","mode":"i2v","controls":{"generate_audio":true,"watermark":false,"return_last_frame":false,"output_format":"mp4","draft":false,"ratio":"adaptive","resolution":"720p","duration":5}}`,
		`{"model":"minimax/h3/image-to-video","mode":"i2v","controls":{"prompt_expansion":false,"ratio":"adaptive","resolution":"480P","duration":5}}`,
	} {
		var req VideoQuoteRequest
		require.NoError(t, json.Unmarshal([]byte(raw), &req))
		t.Run(req.Model, func(t *testing.T) {
			s, key := videoFixture(t)
			key.Group.ModelPricing = nil
			tier, _ := req.Controls["resolution"].(string)
			tier = atlasMediaVideoResolutionOrDefault(req.Model, tier)
			key.Group.VideoModelPrices = map[string]map[string]float64{req.Model: {tier: 0.2}}
			now := time.Now().UTC()
			quote, err := s.QuoteVideo(context.Background(), key, req, now)
			require.NoError(t, err, "input-derived framing is not an unsupported model")
			require.True(t, quote.Enrolled)
			require.NotEmpty(t, quote.Token)
			require.Equal(t, "1.00000000", quote.TotalPrice)
			require.Equal(t, req.Controls, quote.Controls, "do not replace adaptive framing or invent a ratio")
			snapshot, err := s.AcceptVideoQuote(context.Background(), key, fixedVideoBody(t, req), quote.Token, now)
			require.NoError(t, err)
			require.Equal(t, "1.00000000", snapshot.TotalPrice())
			changed := snapshot.Selection()
			changed.Controls["duration"] = float64(10)
			_, err = s.AcceptVideoQuote(context.Background(), key, fixedVideoBody(t, changed), quote.Token, now)
			require.Error(t, err, "a different native duration cannot reuse the fixed quote")
		})
	}
}

func TestVideoFixedRuleQuoteFailsClosedWithoutDurableBillingOrValidPrice(t *testing.T) {
	for _, name := range []string{"billing not ready", "invalid configured price"} {
		t.Run(name, func(t *testing.T) {
			s, key := videoFixture(t)
			key.Group.ModelPricing = nil
			if name == "billing not ready" {
				s.usageBillingRepo = nil
			} else {
				price := -1.0
				key.Group.VideoPrice720P = &price
			}
			quote, err := s.QuoteVideo(context.Background(), key, videoTestRequest(), time.Now())
			require.Error(t, err)
			require.Nil(t, quote, "never downgrade to a usable unsigned estimate")
		})
	}
}

func TestVideoFixedRuleQuoteRetainsNativePriceCardPriority(t *testing.T) {
	s, key := videoFixture(t)
	price := 0.01
	key.Group.VideoPrice720P = &price
	now := time.Now()
	quote, err := s.QuoteVideo(context.Background(), key, videoTestRequest(), now)
	require.NoError(t, err)
	require.Equal(t, "13.33171200", quote.TotalPrice)

	req := videoTestRequest()
	req.Controls["duration"] = float64(4)
	_, err = s.QuoteVideo(context.Background(), key, req, now)
	require.ErrorContains(t, err, "exact video tariff cell", "missing native cells must not fall back to generic rules")
}
