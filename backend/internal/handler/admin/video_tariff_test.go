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
