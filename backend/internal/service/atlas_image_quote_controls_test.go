//go:build unit

package service

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAtlasImageNativeControlsQuoteAndAdmission(t *testing.T) {
	s, key := miniQuoteFixture()
	key.Group.RateMultiplier = 1
	model := "xai/grok-imagine-image-2.0/text-to-image"
	key.Group.ModelPricing[0].Models = []string{model}
	key.Group.ModelPricing[0].Intervals = []PricingInterval{{
		TierLabel: "quality:low:controls:WyIiLCIyayIsIjE2OjkiXQ", PerRequestPrice: quoteFloat(0.12345678),
	}}
	var request ImageQuoteRequest
	require.NoError(t, json.Unmarshal([]byte(`{"model":"`+model+`","quality":"low","size":"","resolution":"2k","aspect_ratio":"16:9","count":2}`), &request))
	q, err := s.QuoteImageQuality(context.Background(), key, request, time.Now())
	require.NoError(t, err)
	require.True(t, q.Enrolled)
	require.Equal(t, "0.24691356", q.Total)
	body := []byte(`{"model":"` + model + `","prompt":"Synthetic test","quality":"low","resolution":"2k","aspect_ratio":"16:9","n":1}`)
	parsed := &OpenAIImagesRequest{Model: model, Quality: "low", Prompt: "Synthetic test", N: 1, Body: body}
	snapshot, err := s.AcceptImageQualityQuote(context.Background(), key, parsed, q.Token, 0, time.Now())
	require.NoError(t, err)
	require.NoError(t, snapshot.validateUpstream(parsed, model))
	for _, changed := range []string{
		`{"model":"` + model + `","prompt":"Synthetic test","quality":"low","resolution":"1k","aspect_ratio":"16:9","n":1}`,
		`{"model":"` + model + `","prompt":"Synthetic test","quality":"low","resolution":"2k","aspect_ratio":"1:1","n":1}`,
	} {
		copy := *parsed
		copy.Body = []byte(changed)
		_, err := s.AcceptImageQualityQuote(context.Background(), key, &copy, q.Token, 0, time.Now())
		require.Error(t, err)
		require.Error(t, snapshot.validateUpstream(&copy, model))
	}
}

func TestAtlasImageQuoteRejectsUnquotedSearchSurcharges(t *testing.T) {
	s, key := miniQuoteFixture()
	model := "google/nano-banana-2.1/text-to-image"
	key.Group.ModelPricing[0].Models = []string{model}
	key.Group.ModelPricing[0].Intervals = []PricingInterval{{
		TierLabel: "quality:none:controls:WyIiLCIyayIsIjE2OjkiXQ", PerRequestPrice: quoteFloat(1),
	}}
	var req ImageQuoteRequest
	require.NoError(t, json.Unmarshal([]byte(`{"model":"`+model+`","resolution":"2k","aspect_ratio":"16:9","count":1}`), &req))
	q, err := s.QuoteImageQuality(context.Background(), key, req, time.Now())
	require.NoError(t, err)
	for _, field := range []string{"enable_web_search", "enable_image_search"} {
		body := `{"model":"` + model + `","prompt":"Test","resolution":"2k","aspect_ratio":"16:9","` + field + `":true}`
		parsed := &OpenAIImagesRequest{Model: model, Prompt: "Test", N: 1, Body: []byte(body)}
		_, err := s.AcceptImageQualityQuote(context.Background(), key, parsed, q.Token, 0, time.Now())
		require.Error(t, err, "extra upstream charges must not bypass a fixed output-only quote")
	}
}

func TestAtlasVerifiedNativeCatalogQuotesAndPayloads(t *testing.T) {
	raw, err := os.ReadFile("testdata/atlas_image_price_catalog.json")
	require.NoError(t, err)
	var catalog struct {
		Cells []struct {
			ImageQuoteRequest
			ControlKey string `json:"controlKey"`
			Price      string `json:"price"`
		} `json:"cells"`
	}
	require.NoError(t, json.Unmarshal(raw, &catalog))
	require.NotEmpty(t, catalog.Cells)
	cards := map[string]ChannelModelPricing{}
	for _, cell := range catalog.Cells {
		card := cards[cell.Model]
		card.Models, card.Platform, card.BillingMode = []string{cell.Model}, PlatformOpenAI, BillingModeImage
		var price float64
		require.NoError(t, json.Unmarshal([]byte(cell.Price), &price))
		card.Intervals = append(card.Intervals, PricingInterval{
			TierLabel: "quality:" + cell.Quality + ":" + cell.ControlKey, PerRequestPrice: &price,
		})
		cards[cell.Model] = card
	}
	var groupCards []ChannelModelPricing
	for _, card := range cards {
		groupCards = append(groupCards, card)
	}
	normalized, err := normalizeGroupModelPricing(PlatformOpenAI, groupCards)
	require.NoError(t, err, "all complete native tariff cards must coexist in one group")
	require.Len(t, normalized, 38)
	for _, cell := range catalog.Cells {
		t.Run(cell.Model+"/"+cell.ControlKey+"/"+cell.Quality, func(t *testing.T) {
			s, key := miniQuoteFixture()
			key.Group.RateMultiplier = 1
			key.Group.ModelPricing = []ChannelModelPricing{cards[cell.Model]}
			req := cell.ImageQuoteRequest
			req.Count = 1
			quote, err := s.QuoteImageQuality(context.Background(), key, req, time.Now())
			require.NoError(t, err)
			require.Equal(t, cell.Price, quote.Total)
			body := map[string]any{"model": req.Model, "prompt": "Non-billable regression fixture", "n": 1}
			for field, value := range map[string]string{"size": req.Size, "resolution": req.Resolution, "aspect_ratio": req.AspectRatio} {
				if value != "" {
					body[field] = value
				}
			}
			if req.Quality != "none" {
				body["quality"] = req.Quality
			}
			contract := atlasMediaModel(req.Model, "image")
			if contract.Mode == "i2i" {
				for field, ref := range contract.MediaFields {
					if ref.Array {
						body[field] = []string{"https://assets.example/reference.png"}
					} else {
						body[field] = "https://assets.example/reference.png"
					}
				}
			}
			encoded, err := json.Marshal(body)
			require.NoError(t, err)
			parsed := &OpenAIImagesRequest{Model: req.Model, Size: req.Size, Prompt: body["prompt"].(string), N: 1, Body: encoded}
			if req.Quality != "none" {
				parsed.Quality = req.Quality
			}
			_, err = buildAtlasCloudImagePayload(parsed, req.Model)
			require.NoError(t, err, "quoted controls must pass the actual upstream serializer")
			snapshot, err := s.AcceptImageQualityQuote(context.Background(), key, parsed, quote.Token, 0, time.Now())
			require.NoError(t, err)
			require.NoError(t, snapshot.validateUpstream(parsed, req.Model))
			require.Equal(t, cell.Price, snapshot.Receipt().Charged)
		})
	}
}

func TestAtlasImageNativeTariffRejectsForgedControls(t *testing.T) {
	s, key := miniQuoteFixture()
	key.Group.ModelPricing[0].Models = []string{"google/nano-banana-2/text-to-image"}
	for _, label := range []string{
		"quality:none:controls:not-base64",
		"quality:none:controls:WyIiLCI4ayIsIjE2OjkiXQ",
		"quality:high:controls:WyIiLCIyayIsIjE2OjkiXQ",
		"quality:none:size:1024x1024",
	} {
		key.Group.ModelPricing[0].Intervals = []PricingInterval{{TierLabel: label, PerRequestPrice: quoteFloat(1)}}
		_, err := s.ImageQualityPricingEnrolled(key, key.Group.ModelPricing[0].Models[0])
		require.Error(t, err, label)
	}
}
