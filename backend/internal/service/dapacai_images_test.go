//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestDapacaiCDNImagesForwardExactModelWithoutOldAccountFallback(t *testing.T) {
	for _, scenario := range []string{"accepted", "exact model card", "old account", "missing price", "reference", "extra option"} {
		t.Run(scenario, func(t *testing.T) {
			body := `{"model":"gpt-image-2.5-cdn","prompt":"test","n":1,"size":"1024x1024","response_format":"url"}`
			if scenario == "reference" {
				body = strings.TrimSuffix(body, "}") + `,"image_url":"https://assets.example/image.png"}`
			}
			if scenario == "extra option" {
				body = strings.TrimSuffix(body, "}") + `,"quality":"high"}`
			}
			c, recorder := grokMediaContentTestContext(http.MethodPost, "https://gateway.example/v1/images/generations",
				map[string]string{"Content-Type": "application/json"})
			price := 0.1
			key := &APIKey{ID: 20, UserID: 10, Group: &Group{ImagePrice1K: &price}}
			if scenario == "missing price" {
				key.Group.ImagePrice1K = nil
			}
			c.Set("api_key", key)
			upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200,
				Header: http.Header{"Content-Type": {"application/json"}},
				Body:   io.NopCloser(strings.NewReader(`{"data":[{"url":"https://assets.example/generated.png"}]}`))}}
			svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
			if scenario == "exact model card" {
				key.Group.ImagePrice1K = nil
				key.Group.ImageRateIndependent, key.Group.ImageRateMultiplier = true, 1.2
				key.Group.ModelPricing = []ChannelModelPricing{{
					Platform: PlatformOpenAI, Models: []string{"gpt-image-2.5-cdn"},
					BillingMode: BillingModeImage, PerRequestPrice: &price,
				}}
				svc.billingService = NewBillingService(svc.cfg, nil)
				svc.resolver = NewModelPricingResolver(nil, svc.billingService)
			}
			parsed, err := svc.ParseOpenAIImagesRequest(c, []byte(body))
			require.NoError(t, err)
			require.Equal(t, OpenAIImagesCapabilityAPIKey, parsed.RequiredCapability)
			account := dapacaiTestAccount()
			if scenario == "old account" {
				account.Credentials["base_url"] = "https://api.atlascloud.ai"
			}
			result, err := svc.ForwardImages(context.Background(), c, account, []byte(body), parsed, "")
			if scenario != "accepted" && scenario != "exact model card" {
				require.Error(t, err)
				require.Equal(t, 400, recorder.Code, recorder.Body.String())
				require.Empty(t, upstream.requests)
				return
			}
			require.NoError(t, err)
			require.Equal(t, "gpt-image-2.5-cdn", result.UpstreamModel)
			require.Equal(t, 1, result.ImageCount)
			require.Len(t, upstream.requests, 1)
			require.Equal(t, "https://api.dapacai.com/v1/images/generations", upstream.lastReq.URL.String())
			require.Equal(t, "Bearer test-upstream-key", upstream.lastReq.Header.Get("Authorization"))
			require.JSONEq(t, body, string(upstream.lastBody))
			require.Contains(t, recorder.Body.String(), "generated.png")
			if scenario == "exact model card" {
				cost := svc.calculateOpenAIImageCost(context.Background(), "gpt-image-2.5-cdn", key,
					result, resolveImageRateMultiplier(key, 9))
				require.InDelta(t, 0.12, cost.ActualCost, 1e-12)
				require.Nil(t, key.Group.ImagePrice1K, "model card does not alter other image model defaults")
			}
		})
	}
}
