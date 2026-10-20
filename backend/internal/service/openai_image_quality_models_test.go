//go:build unit

package service

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

func TestImageQualityVerifiedCatalogQuotesForwardsAndSettles(t *testing.T) {
	var catalog struct {
		Cells []struct {
			Model, Size, Quality, Price, Upstream string
		}
	}
	raw, err := os.ReadFile("testdata/openai_image_price_catalog.json")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &catalog))
	require.Len(t, catalog.Cells, 558)
	cards := map[string]ChannelModelPricing{}
	for _, cell := range catalog.Cells {
		require.True(t, decimal.RequireFromString(cell.Price).Equal(decimal.RequireFromString(cell.Upstream).Mul(decimal.RequireFromString("1.20"))))
		card := cards[cell.Model]
		card.Models, card.Platform, card.BillingMode = []string{cell.Model}, PlatformOpenAI, BillingModeImage
		price, _ := decimal.RequireFromString(cell.Price).Float64()
		card.Intervals = append(card.Intervals, PricingInterval{TierLabel: "quality:" + cell.Quality + ":size:" + cell.Size, PerRequestPrice: quoteFloat(price)})
		cards[cell.Model] = card
	}
	// All endpoint tariffs must coexist in one group; Mini retains its shared legacy card.
	var groupCards []ChannelModelPricing
	for model, card := range cards {
		if model == miniQualityModel+"/edit" {
			continue
		}
		if model == miniQualityModel+"/text-to-image" {
			card.Models = append([]string(nil), card.Models...)
			card.Models = append(card.Models, miniQualityModel+"/edit")
		}
		groupCards = append(groupCards, card)
	}
	normalized, err := normalizeGroupModelPricing(PlatformOpenAI, groupCards)
	require.NoError(t, err)
	require.Len(t, normalized, 17)
	seen := map[string]bool{}
	for _, cell := range catalog.Cells {
		signer, key := miniQuoteFixture()
		key.Group.RateMultiplier = 1
		key.Group.ModelPricing = []ChannelModelPricing{cards[cell.Model]}
		key.Group.ModelPricing[0].Intervals = append([]PricingInterval(nil), cards[cell.Model].Intervals...)
		_, err := normalizeGroupModelPricing(PlatformOpenAI, key.Group.ModelPricing)
		require.NoError(t, err, cell.Model)
		req := ImageQuoteRequest{Model: cell.Model, Size: cell.Size, Quality: cell.Quality, Count: 4}
		quote, err := signer.QuoteImageQuality(context.Background(), key, req, time.Now())
		require.NoError(t, err)
		require.Equal(t, cell.Price, quote.UnitPrice)
		require.Equal(t, decimal.RequireFromString(cell.Price).Mul(decimal.NewFromInt(4)).StringFixed(8), quote.Total)
		if seen[cell.Model] {
			continue
		}
		seen[cell.Model] = true
		body := map[string]any{"model": cell.Model, "size": cell.Size, "prompt": "Synthetic contract verification", "n": 1}
		if cell.Quality != "none" {
			body["quality"] = cell.Quality
		}
		if strings.HasSuffix(cell.Model, "/edit") {
			field := "images"
			if atlasMediaContracts[cell.Model].Input.Properties["image"] != nil {
				field = "image"
			}
			body[field] = []string{"https://storage.atlascloud.ai/synthetic-reference.png"}
		}
		payload, err := json.Marshal(body)
		require.NoError(t, err)
		upstream := &atlasImageUpstream{replies: []string{`{"data":{"id":"pred-catalog","status":"completed","outputs":["https://storage.atlascloud.ai/synthetic-output.png"]}}`}}
		svc, c, _, account, parsed := atlasGPTImageFixture(t, upstream, payload)
		c.Set("api_key", key)
		snapshot, err := signer.AcceptImageQualityQuote(context.Background(), key, parsed, quote.Token, 0, time.Now())
		require.NoError(t, err, cell.Model)
		_, err = snapshot.claim(context.Background(), newInMemoryIdempotencyRepo(), "catalog-test-body", time.Now())
		require.NoError(t, err)
		ctx := WithImageQualityPricing(context.Background(), snapshot)
		c.Request = c.Request.WithContext(ctx)
		result, err := svc.ForwardImages(ctx, c, account, payload, parsed, "")
		require.NoError(t, err, cell.Model)
		require.Equal(t, []string{http.MethodPost}, upstream.methods)
		result.ImageQualityPricing = snapshot
		usage := &openAIRecordUsageLogRepoStub{inserted: true}
		billing := &openAIRecordUsageBillingRepoStub{}
		recorder := newOpenAIRecordUsageServiceWithBillingRepoForTest(usage, billing, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{}, nil)
		// An accepted quote must not change if the editable tariff changes later.
		key.Group.ModelPricing[0].Intervals[0].PerRequestPrice = quoteFloat(99)
		err = recorder.RecordUsage(ctx, &OpenAIRecordUsageInput{APIKey: key, User: key.User, Account: account, Result: result})
		require.NoError(t, err, cell.Model)
		require.InDelta(t, snapshot.Amount(), billing.lastCmd.BalanceCost, 1e-10)
		require.InDelta(t, snapshot.Amount(), usage.lastLog.ActualCost, 1e-10)
		require.Equal(t, snapshot.RequestID(), billing.lastCmd.RequestID)
	}
	require.Len(t, seen, 18)
}

func TestImageQualityAllOpenAIModelsUseNativeControls(t *testing.T) {
	count := 0
	for id, contract := range atlasMediaContracts {
		if contract.Media != "image" || !strings.HasPrefix(id, "openai/") {
			continue
		}
		count++
		s, key := miniQuoteFixture()
		key.Group.RateMultiplier = 1
		card := ChannelModelPricing{Platform: PlatformOpenAI, Models: []string{id}, BillingMode: BillingModeImage}
		qualities := []any{"none"}
		if field := contract.Input.Properties["quality"]; field != nil {
			qualities = field.Enum
		}
		for _, size := range contract.Input.Properties["size"].Enum {
			for _, quality := range qualities {
				card.Intervals = append(card.Intervals, PricingInterval{
					TierLabel:       "quality:" + quality.(string) + ":size:" + size.(string),
					PerRequestPrice: quoteFloat(0.01234), // Synthetic test tariff.
				})
			}
		}
		key.Group.ModelPricing = []ChannelModelPricing{card}
		_, err := normalizeGroupModelPricing(PlatformOpenAI, key.Group.ModelPricing)
		require.NoError(t, err, id)
		for _, size := range contract.Input.Properties["size"].Enum {
			for _, quality := range qualities {
				req := ImageQuoteRequest{Model: id, Size: size.(string), Quality: quality.(string), Count: 3}
				q, err := s.QuoteImageQuality(context.Background(), key, req, time.Now())
				require.NoError(t, err, id)
				require.True(t, q.Enrolled, id)
				require.Equal(t, "0.01234000", q.UnitPrice)
				require.Equal(t, "0.03702000", q.Total)
				nativeQuality := req.Quality
				if nativeQuality == "none" {
					nativeQuality = ""
				}
				parsed := &OpenAIImagesRequest{Model: id, Size: req.Size, Quality: nativeQuality, N: 1}
				snapshot, err := s.AcceptImageQualityQuote(context.Background(), key, parsed, q.Token, 2, time.Now())
				require.NoError(t, err, id)
				require.Equal(t, strings.TrimSuffix(strings.TrimSuffix(id, "/text-to-image"), "/edit"), snapshot.Receipt().Model)
				require.Equal(t, 0.01234, snapshot.cost(1).ActualCost)
			}
		}
	}
	require.Equal(t, 18, count)
}

func TestImageQualityEndpointPricesDoNotBleedAcrossModelsOrModes(t *testing.T) {
	s, key := miniQuoteFixture()
	key.Group.RateMultiplier = 1
	models := []string{
		"openai/gpt-image-2/text-to-image", "openai/gpt-image-2/edit",
		"openai/gpt-image-2.5-sunburst/text-to-image",
	}
	key.Group.ModelPricing = nil
	for i, model := range models {
		key.Group.ModelPricing = append(key.Group.ModelPricing, ChannelModelPricing{
			Platform: PlatformOpenAI, BillingMode: BillingModeImage, Models: []string{model},
			Intervals: []PricingInterval{{TierLabel: "quality:high:size:2048x2048", PerRequestPrice: quoteFloat(float64(i+1) / 100)}},
		})
	}
	_, err := normalizeGroupModelPricing(PlatformOpenAI, key.Group.ModelPricing)
	require.NoError(t, err)
	for i, model := range models {
		req := ImageQuoteRequest{Model: model, Size: "2048x2048", Quality: "high", Count: 1}
		q, err := s.QuoteImageQuality(context.Background(), key, req, time.Now())
		require.NoError(t, err)
		require.True(t, q.Enrolled)
		require.Equal(t, []string{"0.01000000", "0.02000000", "0.03000000"}[i], q.UnitPrice)
		parsed := &OpenAIImagesRequest{Model: model, Size: req.Size, Quality: req.Quality, N: 1}
		parsed.Model = models[(i+1)%len(models)]
		_, err = s.AcceptImageQualityQuote(context.Background(), key, parsed, q.Token, 0, time.Now())
		require.Error(t, err)
	}
	key.Group.ModelPricing = append(key.Group.ModelPricing, key.Group.ModelPricing[0])
	_, err = normalizeGroupModelPricing(PlatformOpenAI, key.Group.ModelPricing)
	require.Error(t, err, "overlapping model cards must not silently pick one price")
}

func TestImageQualityNativePriceCellsRejectInvalidOrUnverifiedCombinations(t *testing.T) {
	s, key := miniQuoteFixture()
	model := "openai/gpt-image-2.5-flare-developer/text-to-image"
	key.Group.ModelPricing = []ChannelModelPricing{{
		Platform: PlatformOpenAI, Models: []string{model}, BillingMode: BillingModeImage,
		Intervals: []PricingInterval{{TierLabel: "quality:none:size:1024x1024", PerRequestPrice: quoteFloat(0.036)}},
	}}
	for _, req := range []ImageQuoteRequest{
		{Model: model, Quality: "medium", Size: "1024x1024", Count: 1},
		{Model: model, Quality: "none", Size: "auto", Count: 1},
		{Model: model, Quality: "none", Size: "3840x2160", Count: 1},
		{Model: model, Quality: "none", Size: "9999x9999", Count: 1},
	} {
		_, err := s.QuoteImageQuality(context.Background(), key, req, time.Now())
		require.Error(t, err, req)
	}
	q, err := s.QuoteImageQuality(context.Background(), key, ImageQuoteRequest{Model: model, Count: 1}, time.Now())
	require.NoError(t, err)
	require.True(t, q.Enrolled)
	require.Equal(t, "none", q.Quality)
	require.Equal(t, "0.05400000", q.Total) // Existing 1.5x customer rate remains effective.
	key.Group.ModelPricing[0].Models = []string{"openai/gpt-image-2.5-flare/edit"}
	key.Group.ModelPricing[0].Intervals[0].TierLabel = "quality:high:size:1024x1024"
	_, err = normalizeGroupModelPricing(PlatformOpenAI, key.Group.ModelPricing)
	require.Error(t, err, "an edit endpoint cannot inherit an unsupported text-to-image size")
}
