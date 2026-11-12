//go:build unit

package service

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type wanKlingVideoCell struct {
	VideoQuoteRequest
	SaleMicrocredits int64   `json:"saleMicrocredits"`
	Price            float64 `json:"price"`
	CheckedAt        string  `json:"checkedAt"`
	Source           string  `json:"source"`
}

func wanKlingVideoCatalog(t *testing.T) ([]wanKlingVideoCell, map[string]ChannelModelPricing) {
	t.Helper()
	raw, err := os.ReadFile("testdata/atlas_video_wan_kling_catalog.json")
	require.NoError(t, err)
	var fixture struct {
		Models []string            `json:"models"`
		Cells  []wanKlingVideoCell `json:"cells"`
	}
	require.NoError(t, json.Unmarshal(raw, &fixture))
	require.Len(t, fixture.Models, 8)
	require.Len(t, fixture.Cells, 40)
	cards := map[string]ChannelModelPricing{}
	for _, cell := range fixture.Cells {
		contract := atlasMediaModel(cell.Model, "video")
		require.NotNil(t, contract, cell.Model)
		require.True(t, videoSchemaSupported(videoRawContracts[cell.Model]["input"]))
		key, err := VideoTariffKey(cell.VideoQuoteRequest)
		require.NoError(t, err, cell.Model)
		revision, err := VideoContractRevision(cell.Model)
		require.NoError(t, err)
		price := float64(cell.SaleMicrocredits) / 1e7
		card := cards[cell.Model]
		if card.VideoTariff == nil {
			card = ChannelModelPricing{
				Models: []string{cell.Model}, Platform: PlatformOpenAI, BillingMode: BillingModeVideo,
				VideoTariff: &VideoTariffMetadata{Version: 1, ContractRevision: revision,
					Cells: map[string]VideoTariffProvenance{}},
			}
		}
		require.NotContains(t, card.VideoTariff.Cells, key)
		card.Intervals = append(card.Intervals, PricingInterval{TierLabel: key, PerRequestPrice: &price})
		rawPrice, err := json.Marshal(cell.Price)
		require.NoError(t, err)
		card.VideoTariff.Cells[key] = VideoTariffProvenance{
			Source: cell.Source, CheckedAt: cell.CheckedAt, UpstreamPrice: string(rawPrice), Policy: VideoRetailPolicy,
		}
		cards[cell.Model] = card
	}
	require.Len(t, cards, 8)
	for _, card := range cards {
		require.NoError(t, validateVideoTariffCard(card))
	}
	return fixture.Cells, cards
}

func TestAtlasWanKlingVideoSignedQuoteAndNativePayload(t *testing.T) {
	cells, cards := wanKlingVideoCatalog(t)
	seen := map[string]bool{}
	for _, cell := range cells {
		t.Run(cell.Model+"/"+cell.Controls["resolution"].(string), func(t *testing.T) {
			service, key := videoFixture(t)
			service.httpUpstream = nil
			key.Group.ModelPricing = []ChannelModelPricing{cards[cell.Model]}
			now := time.Now()
			quote, err := service.QuoteVideo(context.Background(), key, cell.VideoQuoteRequest, now)
			require.NoError(t, err)
			require.True(t, quote.Enrolled)
			require.Equal(t, "native_tariff", quote.PriceSource)
			var total float64
			require.NoError(t, json.Unmarshal([]byte(quote.TotalPrice), &total))
			require.Equal(t, cell.SaleMicrocredits, int64(total*1e7+0.5))
			body := map[string]any{"model": cell.Model, "prompt": "Synthetic offline protocol fixture"}
			for field, value := range cell.Controls {
				body[field] = value
			}
			if cell.Mode == "i2v" {
				body["image"] = "https://assets.example/first-frame.png"
			}
			raw, err := json.Marshal(body)
			require.NoError(t, err)
			info, native, err := SerializeAtlasCloudVideoRequest(raw)
			require.NoError(t, err)
			require.Equal(t, cell.Model, info.Model)
			require.EqualValues(t, cell.Controls["duration"], info.DurationSeconds)
			var serialized map[string]any
			require.NoError(t, json.Unmarshal(native, &serialized))
			require.Equal(t, body, serialized, "native quote controls must not be renamed or dropped")
			snapshot, err := service.AcceptVideoQuote(context.Background(), key, native, quote.Token, now)
			require.NoError(t, err)
			require.Equal(t, quote.TotalPrice, snapshot.TotalPrice())

			body["duration"] = float64(6)
			mutated, err := json.Marshal(body)
			require.NoError(t, err)
			_, err = service.AcceptVideoQuote(context.Background(), key, mutated, quote.Token, now)
			require.Error(t, err, "changed duration must not reuse a signed price")
			unpriced := cell.VideoQuoteRequest
			require.NoError(t, json.Unmarshal(native, &serialized))
			unpriced.Controls = map[string]any{}
			for field, value := range cell.Controls {
				unpriced.Controls[field] = value
			}
			unpriced.Controls["duration"] = float64(6)
			_, err = service.QuoteVideo(context.Background(), key, unpriced, now)
			require.ErrorContains(t, err, "exact video tariff cell", "no fallback for an unconfigured duration")

			if !seen[cell.Model] {
				// Exercise the real serializer/transport against an in-memory fake only.
				upstream := &atlasImageUpstream{replies: []string{
					`{"code":200,"data":{"id":"offline-video-fixture","status":"processing"}}`,
				}}
				gateway, _, _, account, _ := atlasImageFixture(t, upstream)
				account.Credentials["model_mapping"] = map[string]any{cell.Model: cell.Model}
				result, err := gateway.requestQuotedAtlasVideo(context.Background(), account, "", native)
				require.NoError(t, err)
				require.Equal(t, "atlascloud:offline-video-fixture", result.TaskID)
				require.Equal(t, []string{http.MethodPost}, upstream.methods)
				require.Equal(t, []string{"https://api.atlascloud.ai/api/v1/model/generateVideo"}, upstream.urls)
				require.JSONEq(t, string(native), string(upstream.bodies[0]))
				seen[cell.Model] = true
			}
		})
	}
	require.Len(t, seen, 8)
	t.Log("Verified 40 priced specifications and 8 native transports; all supplier responses were in-memory fakes")
}

func TestAtlasWanKlingVideoRejectsUnsafeOrUnquotedInputs(t *testing.T) {
	cells, _ := wanKlingVideoCatalog(t)
	for _, cell := range cells {
		body := map[string]any{"model": cell.Model, "prompt": "Synthetic offline validation"}
		for key, value := range cell.Controls {
			body[key] = value
		}
		if cell.Mode == "i2v" {
			body["image"] = "https://assets.example/reference.png"
		}
		for _, mutate := range []func(map[string]any){
			func(b map[string]any) { b["duration"] = -1 },
			func(b map[string]any) { b["duration"] = 5.5 },
			func(b map[string]any) { b["resolution"] = "unsupported" },
			func(b map[string]any) { b["unverified_control"] = true },
		} {
			raw, err := json.Marshal(body)
			require.NoError(t, err)
			var changed map[string]any
			require.NoError(t, json.Unmarshal(raw, &changed))
			mutate(changed)
			raw, err = json.Marshal(changed)
			require.NoError(t, err)
			_, _, err = SerializeAtlasCloudVideoRequest(raw)
			require.Error(t, err, cell.Model)
		}
		if cell.Mode != "i2v" {
			continue
		}
		for _, image := range []string{"", "http://127.0.0.1/image.png", "https://10.0.0.1/image.png"} {
			body["image"] = image
			raw, err := json.Marshal(body)
			require.NoError(t, err)
			_, _, err = SerializeAtlasCloudVideoRequest(raw)
			require.Error(t, err, cell.Model)
		}
		delete(body, "image")
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		_, _, err = SerializeAtlasCloudVideoRequest(raw)
		require.Error(t, err, "first frame must be present for image-to-video")
	}
}
