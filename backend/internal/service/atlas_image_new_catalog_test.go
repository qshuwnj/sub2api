//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestAtlasNewCanvasImageCatalogAdmission(t *testing.T) {
	raw, err := os.ReadFile("testdata/atlas_image_new_catalog.json")
	require.NoError(t, err)
	var catalog struct {
		Cells []struct {
			ImageQuoteRequest
			Price string         `json:"price"`
			Body  map[string]any `json:"body"`
		} `json:"cells"`
	}
	require.NoError(t, json.Unmarshal(raw, &catalog))
	require.Len(t, catalog.Cells, 398)
	cards := map[string]ChannelModelPricing{}
	forwarded := map[string]bool{}
	for _, cell := range catalog.Cells {
		req, err := cell.ImageQuoteRequest.normalized()
		require.NoError(t, err, cell.Model)
		require.Equal(t, cell.ImageQuoteRequest, req)
		var price float64
		require.NoError(t, json.Unmarshal([]byte(cell.Price), &price))
		card := cards[cell.Model]
		card.Models, card.Platform, card.BillingMode = []string{cell.Model}, PlatformOpenAI, BillingModeImage
		card.Intervals = append(card.Intervals, PricingInterval{
			TierLabel: "quality:" + cell.Quality + ":" + imageNativeControlKey(req), PerRequestPrice: &price,
		})
		cards[cell.Model] = card
	}
	require.Len(t, cards, 18)
	var all []ChannelModelPricing
	for _, card := range cards {
		all = append(all, card)
	}
	_, err = normalizeGroupModelPricing(PlatformOpenAI, all)
	require.NoError(t, err)
	for _, cell := range catalog.Cells {
		t.Run(cell.Model+"/"+imageNativeControlKey(cell.ImageQuoteRequest)+"/"+cell.Quality, func(t *testing.T) {
			s, key := miniQuoteFixture()
			key.Group.RateMultiplier = 1
			key.Group.ModelPricing = []ChannelModelPricing{cards[cell.Model]}
			q, err := s.QuoteImageQuality(context.Background(), key, cell.ImageQuoteRequest, time.Now())
			require.NoError(t, err)
			require.True(t, q.Enrolled)
			expected, err := strconv.ParseFloat(cell.Price, 64)
			require.NoError(t, err)
			actual, err := strconv.ParseFloat(q.Total, 64)
			require.NoError(t, err)
			require.Equal(t, expected, actual)
			body := cell.Body
			body["prompt"], body["response_format"] = "Synthetic non-billable contract fixture", "url"
			contract := atlasMediaModel(cell.Model, "image")
			require.NotNil(t, contract)
			if _, exists := contract.Input.Properties["output_format"]; exists {
				body["output_format"] = "png"
			}
			for field := range contract.MediaFields {
				body[field] = []string{"https://assets.example/reference.png"}
			}
			raw, err := json.Marshal(body)
			require.NoError(t, err)
			parsed := &OpenAIImagesRequest{Model: cell.Model, Prompt: body["prompt"].(string), Size: cell.Size, N: 1, Body: raw}
			if cell.Quality != "none" {
				parsed.Quality = cell.Quality
			}
			_, err = buildAtlasCloudImagePayload(parsed, cell.Model)
			require.NoError(t, err)
			snapshot, err := s.AcceptImageQualityQuote(context.Background(), key, parsed, q.Token, 0, time.Now())
			require.NoError(t, err)
			require.NoError(t, snapshot.validateUpstream(parsed, cell.Model))
			if !forwarded[cell.Model] {
				upstream := &atlasImageUpstream{replies: []string{
					`{"code":200,"data":{"id":"offline-new-image","status":"completed","outputs":["https://storage.atlascloud.ai/test/output.png"]}}`,
				}}
				svc, c, w, account, _ := atlasImageFixture(t, upstream)
				c.Request = httptest.NewRequest(http.MethodPost, openAIImagesGenerationsEndpoint, bytes.NewReader(raw))
				c.Request.Header.Set("Content-Type", "application/json")
				native, err := svc.ParseOpenAIImagesRequest(c, raw)
				require.NoError(t, err)
				result, err := svc.forwardOpenAIImagesAPIKey(context.Background(), c, account, raw, native, "")
				require.NoError(t, err)
				require.Equal(t, 1, result.ImageCount)
				require.Len(t, upstream.bodies, 1)
				require.Equal(t, "https://api.atlascloud.ai/api/v1/model/generateImage", upstream.urls[0])
				require.Equal(t, cell.Model, gjson.GetBytes(upstream.bodies[0], "model").String())
				require.Empty(t, w.Body.String(), "settlement must precede exposing generated output")
				forwarded[cell.Model] = true
			}
			changed := *parsed
			body["size"] = "9999*9999"
			changed.Body, _ = json.Marshal(body)
			require.Error(t, snapshot.validateUpstream(&changed, cell.Model))
		})
	}
	require.Len(t, forwarded, 18)
}

func TestAtlasSunburstQuoteRejectsUnpricedGIF(t *testing.T) {
	model := "openai/gpt-image-2.6-sunburst/text-to-image"
	req := ImageQuoteRequest{Model: model, Quality: "low", Size: "1024x1024", Count: 1}
	s, key := miniQuoteFixture()
	key.Group.RateMultiplier = 1
	key.Group.ModelPricing[0] = ChannelModelPricing{
		Models: []string{model}, Platform: PlatformOpenAI, BillingMode: BillingModeImage,
		Intervals: []PricingInterval{{TierLabel: "quality:low:" + imageNativeControlKey(req), PerRequestPrice: quoteFloat(1)}},
	}
	quote, err := s.QuoteImageQuality(context.Background(), key, req, time.Now())
	require.NoError(t, err)
	body := []byte(`{"model":"` + model + `","prompt":"Test","quality":"low","size":"1024x1024","output_format":"gif","n":1}`)
	parsed := &OpenAIImagesRequest{Model: model, Quality: "low", Size: "1024x1024", Prompt: "Test", N: 1, Body: body}
	_, err = s.AcceptImageQualityQuote(context.Background(), key, parsed, quote.Token, 0, time.Now())
	require.Error(t, err, "GIF sprite sheets have no fixed single-image tariff")
}
