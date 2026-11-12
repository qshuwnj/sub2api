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

func TestAtlasReferenceImagesSignedContracts(t *testing.T) {
	raw, err := os.ReadFile("testdata/atlas_reference_images_catalog.json")
	require.NoError(t, err)
	var fixture struct {
		Models []string `json:"models"`
		Cells  []struct {
			VideoQuoteRequest
			SaleMicrocredits  int64   `json:"saleMicrocredits"`
			Price             float64 `json:"price"`
			Source            string  `json:"source"`
			CheckedAt         string  `json:"checkedAt"`
			SupplierEstimated bool    `json:"supplierEstimated"`
		} `json:"cells"`
	}
	require.NoError(t, json.Unmarshal(raw, &fixture))
	require.Len(t, fixture.Models, 12)
	require.Len(t, fixture.Cells, 57)
	for _, cell := range fixture.Cells {
		t.Run(cell.Model+"/"+cell.Controls["resolution"].(string), func(t *testing.T) {
			contract := atlasMediaModel(cell.Model, "video")
			require.NotNil(t, contract)
			label, err := VideoTariffKey(cell.VideoQuoteRequest)
			require.NoError(t, err)
			revision, err := VideoContractRevision(cell.Model)
			require.NoError(t, err)
			amount := float64(cell.SaleMicrocredits) / 1e7
			price, err := json.Marshal(cell.Price)
			require.NoError(t, err)
			card := ChannelModelPricing{
				Models: []string{cell.Model}, Platform: PlatformOpenAI, BillingMode: BillingModeVideo,
				Intervals: []PricingInterval{{TierLabel: label, PerRequestPrice: &amount}},
				VideoTariff: &VideoTariffMetadata{Version: 1, ContractRevision: revision,
					Cells: map[string]VideoTariffProvenance{label: {
						Source: cell.Source, CheckedAt: cell.CheckedAt, UpstreamPrice: string(price),
						SupplierEstimated: cell.SupplierEstimated, Policy: VideoRetailPolicy,
					}}},
			}
			require.NoError(t, validateVideoTariffCard(card))
			gateway, key := videoFixture(t)
			gateway.httpUpstream = nil
			key.Group.ModelPricing = []ChannelModelPricing{card}
			now := time.Now()
			quote, err := gateway.QuoteVideo(context.Background(), key, cell.VideoQuoteRequest, now)
			require.NoError(t, err)
			require.True(t, quote.Enrolled)
			require.Equal(t, "native_tariff", quote.PriceSource)
			body := map[string]any{"model": cell.Model, "prompt": "Offline reference contract fixture"}
			for field, value := range cell.Controls {
				body[field] = value
			}
			field := ""
			for _, candidate := range []string{"refers", "reference_images", "image_urls"} {
				if contract.MediaFields[candidate].Array {
					field = candidate
					break
				}
			}
			require.NotEmpty(t, field)
			if field == "refers" {
				body[field] = []any{
					map[string]any{"url": "https://assets.example/first.png", "type": "image"},
					map[string]any{"url": "https://assets.example/second.png", "type": "image"},
				}
			} else {
				body[field] = []any{"https://assets.example/first.png", "https://assets.example/second.png"}
			}
			serialize := func(value map[string]any) (GrokMediaRequestInfo, []byte, error) {
				data, err := json.Marshal(value)
				require.NoError(t, err)
				return SerializeAtlasCloudVideoRequest(data)
			}
			info, native, err := serialize(body)
			require.NoError(t, err)
			require.Len(t, info.InputImageURLs, 2)
			snapshot, err := gateway.AcceptVideoQuote(context.Background(), key, native, quote.Token, now)
			require.NoError(t, err)
			require.Equal(t, quote.TotalPrice, snapshot.TotalPrice())
			// Every check is local: no supplier transport is configured.
			var changed map[string]any
			require.NoError(t, json.Unmarshal(native, &changed))
			delete(changed, field)
			_, _, err = serialize(changed)
			require.Error(t, err, "empty reference modes must fail")
			require.NoError(t, json.Unmarshal(native, &changed))
			if field == "refers" {
				changed[field] = []any{map[string]any{"url": "https://assets.example/input.mp4", "type": "video"}}
			} else {
				changed["reference_videos"] = []any{"https://assets.example/input.mp4"}
			}
			_, _, err = serialize(changed)
			require.Error(t, err, "input-duration billing must not use output-only fixed tariffs")
			require.NoError(t, json.Unmarshal(native, &changed))
			if field == "refers" {
				changed[field] = []any{map[string]any{"url": "https://127.0.0.1/private.png", "type": "image"}}
			} else {
				changed[field] = []any{"https://127.0.0.1/private.png"}
			}
			_, _, err = serialize(changed)
			require.Error(t, err, "private input URLs must fail before submission")
		})
	}
}
