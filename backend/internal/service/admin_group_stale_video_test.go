package service

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestGroupPricingUpdatePreservesUnchangedStaleVideoWithoutEnablingQuotes(t *testing.T) {
	svc, key := videoFixture(t)
	stale := key.Group.ModelPricing[0].Clone()
	stale.VideoTariff.ContractRevision = strings.Repeat("a", 64)
	_, err := normalizeGroupModelPricing(PlatformOpenAI, []ChannelModelPricing{stale})
	require.Error(t, err, "new stale cards must remain rejected")
	got, err := normalizeGroupModelPricing(PlatformOpenAI, []ChannelModelPricing{stale}, []ChannelModelPricing{stale})
	require.NoError(t, err, "unrelated repairs must not require deleting an unchanged stale card")
	require.Equal(t, stale.VideoTariff, got[0].VideoTariff)
	require.Error(t, validateVideoTariffCard(got[0]), "stored stale cards must remain unusable at quote time")
	key.Group.ModelPricing = got
	_, err = svc.QuoteVideo(t.Context(), key, videoTestRequest(), time.Now())
	require.Error(t, err)
	edited := stale.Clone()
	edited.Intervals[0].PerRequestPrice = floatPtr(1)
	_, err = normalizeGroupModelPricing(PlatformOpenAI, []ChannelModelPricing{edited}, []ChannelModelPricing{stale})
	require.Error(t, err, "stale retail changes must not be grandfathered")
	metadataEdit := stale.Clone()
	metadataEdit.VideoTariff.ContractRevision = strings.Repeat("b", 64)
	_, err = normalizeGroupModelPricing(PlatformOpenAI, []ChannelModelPricing{metadataEdit}, []ChannelModelPricing{stale})
	require.Error(t, err, "invalid metadata changes must not be grandfathered")
	_, err = normalizeGroupModelPricing(PlatformOpenAI, []ChannelModelPricing{stale, stale}, []ChannelModelPricing{stale})
	require.Error(t, err, "one stored stale card must not authorize duplicate cards")
}
