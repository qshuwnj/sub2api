package admin

import (
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/handler/dto"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestVideoTariffGroupAdminMetadataRoundTrip(t *testing.T) {
	price := 13.331712
	provenance := &service.VideoTariffMetadata{Version: 1, ContractRevision: "contract-revision", Cells: map[string]service.VideoTariffProvenance{
		"exact-key": {Source: service.VideoCalculatorURL, CheckedAt: "2026-10-22T00:00:00Z", UpstreamPrice: "1.60", Policy: service.VideoRetailPolicy, SupplierEstimated: true},
	}}
	group := &service.Group{ModelPricing: []service.ChannelModelPricing{{Platform: service.PlatformOpenAI, Models: []string{"google/veo3.1/text-to-video"}, BillingMode: service.BillingModeVideo, VideoTariff: provenance,
		Intervals: []service.PricingInterval{{TierLabel: "exact-key", PerRequestPrice: &price}},
	}}}
	raw, err := json.Marshal(dto.GroupFromServiceAdmin(group))
	require.NoError(t, err)
	var request UpdateGroupRequest
	require.NoError(t, json.Unmarshal(raw, &request))
	require.NotNil(t, request.ModelPricing)
	require.Len(t, *request.ModelPricing, 1)
	require.Equal(t, provenance, (*request.ModelPricing)[0].VideoTariff)
	require.Equal(t, price, *(*request.ModelPricing)[0].Intervals[0].PerRequestPrice)
}

func TestVideoTariffGroupAdminRawDecimalBoundary(t *testing.T) {
	require.Error(t, json.Unmarshal([]byte(`{"model_pricing":[{"intervals":[{"tier_label":"video:v1:synthetic","per_request_price":0.100000000000000001}]}]}`), &UpdateGroupRequest{}))
	const price = "999999999999.12345678"
	var request UpdateGroupRequest
	require.NoError(t, json.Unmarshal([]byte(`{"model_pricing":[{"billing_mode":"video","intervals":[{"tier_label":"video:v1:synthetic","per_request_price":`+price+`}]}]}`), &request))
	group := &service.Group{ModelPricing: []service.ChannelModelPricing{(*request.ModelPricing)[0].Clone()}}
	raw, err := json.Marshal(dto.GroupFromServiceAdmin(group))
	require.NoError(t, err)
	require.Contains(t, string(raw), `"per_request_price":`+price)
	require.NoError(t, json.Unmarshal(raw, &request))
	again, err := json.Marshal(request.ModelPricing)
	require.NoError(t, err)
	require.Contains(t, string(again), `"per_request_price":`+price)
}

func TestVideoTariffLegacyPricingJSONCompatibility(t *testing.T) {
	for _, tc := range []struct{ label, mode string }{{"1K", "image"}, {"audio-job", "per_request"}} {
		raw := `{"billing_mode":"` + tc.mode + `","intervals":[{"tier_label":"` + tc.label + `","per_request_price":0.100000000000000001}]}`
		var request channelModelPricingRequest
		require.NoError(t, json.Unmarshal([]byte(raw), &request))
		cards := pricingRequestToService([]channelModelPricingRequest{request}, false)
		require.Equal(t, 0.1, *cards[0].Intervals[0].PerRequestPrice)
		response := pricingToResponse(&cards[0])
		got, err := json.Marshal(response)
		require.NoError(t, err)
		require.Contains(t, string(got), `"per_request_price":0.1`)
		var card service.ChannelModelPricing
		require.NoError(t, json.Unmarshal([]byte(raw), &card))
		got, err = json.Marshal(card.Clone())
		require.NoError(t, err)
		require.Contains(t, string(got), `"per_request_price":0.1`)
	}
}
