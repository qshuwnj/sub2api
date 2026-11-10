//go:build unit

package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestDapacaiNewVideoNativeContractAndUploadLimits(t *testing.T) {
	const model = "seedance-2.0-1"
	p := dapacaiVideoModel(model)
	require.NotNil(t, p)
	require.Equal(t, 9, p.images)
	require.Equal(t, 3, p.audios)
	require.Zero(t, p.videos)
	for _, seconds := range []int{5, 9, 15} {
		for _, ratio := range []string{"16:9", "9:16", "1:1"} {
			body, err := json.Marshal(map[string]any{
				"model": model, "prompt": "test", "duration": seconds, "ratio": ratio, "resolution": "720p",
			})
			require.NoError(t, err)
			info, err := ParseDapacaiVideoRequest(body)
			require.NoError(t, err)
			require.Equal(t, model, info.Model)
			require.Equal(t, seconds, info.DurationSeconds)
		}
	}
	for kind, maximum := range map[string]int{"image": 9, "audio": 3, "video": 0, "first_frame": 0, "last_frame": 0} {
		for _, count := range []int{maximum, maximum + 1} {
			if count == 0 {
				continue
			}
			media := []map[string]string{}
			if kind == "audio" {
				media = append(media, map[string]string{"type": "image", "url": "https://assets.example/reference.png"})
			}
			for i := 0; i < count; i++ {
				media = append(media, map[string]string{"type": kind, "url": "https://assets.example/reference"})
			}
			body, err := json.Marshal(map[string]any{
				"model": model, "prompt": "test", "duration": 15, "ratio": "16:9", "resolution": "720p",
				"input": map[string]any{"prompt": "test", "media": media},
			})
			require.NoError(t, err)
			_, err = ParseDapacaiVideoRequest(body)
			if maximum > 0 && count == maximum {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		}
	}
	for kind, expected := range map[VideoReferenceKind]bool{VideoReferenceImage: true, VideoReferenceAudio: true, VideoReferenceVideo: false} {
		_, allowed := videoReferenceLimit("dapacai:"+model, kind)
		require.Equal(t, expected, allowed)
	}
	for _, fields := range []map[string]any{
		{"duration": 4}, {"duration": 16}, {"duration": 5.5}, {"resolution": "1080p"}, {"ratio": "21:9"},
	} {
		payload := map[string]any{"model": model, "prompt": "test", "duration": 15, "ratio": "16:9", "resolution": "720p"}
		for k, v := range fields {
			payload[k] = v
		}
		body, err := json.Marshal(payload)
		require.NoError(t, err)
		_, err = ParseDapacaiVideoRequest(body)
		require.Error(t, err)
	}
}

func TestDapacaiNewVideoPriceUsesExactRequestCardAndSingleMarkup(t *testing.T) {
	const model = "seedance-2.0-1"
	s, key := dapacaiPreviewFixture(t)
	price := 1.5
	for _, duration := range []int{5, 10, 15} {
		for _, mode := range []string{"text", "image", "references"} {
			req := VideoQuoteRequest{Model: model, Mode: mode,
				Controls: map[string]any{"duration": duration, "ratio": "16:9", "resolution": "720p"}}
			key.Group.ModelPricing = []ChannelModelPricing{{
				Platform: PlatformOpenAI, Models: []string{model}, BillingMode: BillingModePerRequest, PerRequestPrice: &price,
			}}
			q, err := s.PreviewDapacaiVideoPrice(context.Background(), key, req, time.Now())
			require.NoError(t, err)
			require.Equal(t, "1.80000000", q.TotalPrice)
			require.Equal(t, "request", q.BillingUnit)
			require.Equal(t, 1, q.UnitCount)
			require.InDelta(t, 1.8, calculateDapacaiVideoCost(key, model, "720p", 1, duration, 1.2).ActualCost, 1e-12)
			key.Group.ModelPricing[0].BillingMode = BillingModeVideo
			_, err = s.PreviewDapacaiVideoPrice(context.Background(), key, req, time.Now())
			require.ErrorContains(t, err, "price card is not configured")
			key.Group.ModelPricing = nil
			key.Group.VideoModelPrices = map[string]map[string]float64{model: {"720p": price}}
			_, _, valid := dapacaiVideoCardPrice(key, model, "720p")
			require.False(t, valid)
		}
	}
}

func TestDapacaiImage26PreviewUsesItsOwnCard(t *testing.T) {
	s, key := dapacaiPreviewFixture(t)
	price := 0.12
	key.Group.ModelPricing = append(key.Group.ModelPricing, ChannelModelPricing{
		Platform: PlatformOpenAI, Models: []string{"gpt-image-2.6"}, BillingMode: BillingModeImage, PerRequestPrice: &price,
	})
	for _, size := range []string{"1024x1024", "1536x1024", "1024x1536"} {
		q, err := s.PreviewDapacaiImagePrice(context.Background(), key, ImageQuoteRequest{
			Model: "gpt-image-2.6", Quality: "none", Size: size, Count: 1,
		}, time.Now())
		require.NoError(t, err)
		require.Equal(t, "0.14400000", q.TotalPrice)
		require.Equal(t, "image", q.BillingUnit)
	}
	key.Group.ModelPricing = key.Group.ModelPricing[:1] // CDN price must not match 2.6.
	_, err := s.PreviewDapacaiImagePrice(context.Background(), key, ImageQuoteRequest{
		Model: "gpt-image-2.6", Quality: "none", Size: "1024x1024", Count: 1,
	}, time.Now())
	require.Error(t, err)
}

func TestDapacaiImage26ForwardingAndReferenceFence(t *testing.T) {
	for _, scenario := range []string{"accepted", "old account", "reference", "extra option"} {
		t.Run(scenario, func(t *testing.T) {
			body := `{"model":"gpt-image-2.6","prompt":"test","n":1,"size":"1536x1024","response_format":"url"}`
			if scenario == "reference" {
				body = strings.TrimSuffix(body, "}") + `,"image_url":"https://assets.example/reference.png"}`
			}
			if scenario == "extra option" {
				body = strings.TrimSuffix(body, "}") + `,"quality":"high"}`
			}
			c, recorder := grokMediaContentTestContext(http.MethodPost, "https://gateway.example/v1/images/generations",
				map[string]string{"Content-Type": "application/json"})
			price := 0.12
			key := &APIKey{ID: 20, UserID: 10, Group: &Group{
				ImageRateIndependent: true, ImageRateMultiplier: 1.2,
				ModelPricing: []ChannelModelPricing{{
					Platform: PlatformOpenAI, Models: []string{"gpt-image-2.6"}, BillingMode: BillingModeImage, PerRequestPrice: &price,
				}},
			}}
			c.Set("api_key", key)
			upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200,
				Header: http.Header{"Content-Type": {"application/json"}},
				Body:   io.NopCloser(strings.NewReader(`{"data":[{"url":"https://assets.example/generated.png"}]}`)),
			}}
			svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
			svc.billingService = NewBillingService(svc.cfg, nil)
			svc.resolver = NewModelPricingResolver(nil, svc.billingService)
			parsed, err := svc.ParseOpenAIImagesRequest(c, []byte(body))
			require.NoError(t, err)
			require.Equal(t, OpenAIImagesCapabilityAPIKey, parsed.RequiredCapability)
			account := dapacaiTestAccount()
			if scenario == "old account" {
				account.Credentials["base_url"] = "https://api.atlascloud.ai"
			}
			result, err := svc.ForwardImages(context.Background(), c, account, []byte(body), parsed, "")
			if scenario != "accepted" {
				require.Error(t, err)
				require.Equal(t, 400, recorder.Code)
				require.Empty(t, upstream.requests)
				return
			}
			require.NoError(t, err)
			require.Equal(t, "gpt-image-2.6", result.UpstreamModel)
			require.Len(t, upstream.requests, 1)
			require.Equal(t, "https://api.dapacai.com/v1/images/generations", upstream.lastReq.URL.String())
			require.JSONEq(t, body, string(upstream.lastBody))
			cost := svc.calculateOpenAIImageCost(context.Background(), result.UpstreamModel, key, result, resolveImageRateMultiplier(key, 9))
			require.InDelta(t, 0.144, cost.ActualCost, 1e-12)
		})
	}
}
