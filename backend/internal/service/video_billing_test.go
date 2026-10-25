package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCanonicalGrokImagineVideoPriceFamily(t *testing.T) {
	t.Parallel()
	require.Equal(t, VideoPriceFamilyGrokImagineVideo, CanonicalGrokImagineVideoPriceFamily("grok-imagine-video"))
	require.Equal(t, VideoPriceFamilyGrokImagineVideo15, CanonicalGrokImagineVideoPriceFamily("grok-imagine-video-1.5"))
	require.Equal(t, VideoPriceFamilyGrokImagineVideo15, CanonicalGrokImagineVideoPriceFamily("grok-imagine-video-1.5-preview"))
	require.Equal(t, VideoPriceFamilyGrokImagineVideo15, CanonicalGrokImagineVideoPriceFamily("xai/grok-video-1.5"))
	require.Equal(t, "grok-imagine-video-2", CanonicalGrokImagineVideoPriceFamily("grok-imagine-video-2"))
	require.Equal(t, "grok-imagine-video-2", CanonicalGrokImagineVideoPriceFamily("xai/grok-imagine-video-2"))
}

func TestNormalizeAndLookupVideoModelPrices(t *testing.T) {
	t.Parallel()
	raw := map[string]map[string]float64{
		"grok-imagine-video-1.5-preview": {"480p": 0.08, "720p": 0.14},
		"grok-imagine-video":             {"480p": 0.05},
		"grok-imagine-video-2":           {"1080p": 0.4},
	}
	norm := NormalizeVideoModelPrices(raw)
	require.NotNil(t, norm)
	require.Contains(t, norm, VideoPriceFamilyGrokImagineVideo15)
	require.Contains(t, norm, VideoPriceFamilyGrokImagineVideo)
	require.Contains(t, norm, "grok-imagine-video-2")

	p15 := LookupVideoModelPrice(norm, "grok-imagine-video-1.5", "480p")
	require.NotNil(t, p15)
	require.InDelta(t, 0.08, *p15, 1e-9)

	pBase := LookupVideoModelPrice(norm, "grok-imagine-video", "480p")
	require.NotNil(t, pBase)
	require.InDelta(t, 0.05, *pBase, 1e-9)
	// A missing model-specific tier must fall back to the flat tier price,
	// rather than borrowing another model-specific resolution.
	require.Nil(t, LookupVideoModelPrice(norm, "grok-imagine-video", "720p"))

	p2 := LookupVideoModelPrice(norm, "grok-imagine-video-2", "1080p")
	require.NotNil(t, p2)
	require.InDelta(t, 0.4, *p2, 1e-9)

	// Unmatched model → nil (caller falls back to flat columns / defaults).
	require.Nil(t, LookupVideoModelPrice(norm, "unknown-model", "480p"))
}

func TestCalculateVideoCostUsesFlatGroupPriceForAtlasModel(t *testing.T) {
	price := 0.25
	cost := (&BillingService{}).CalculateVideoCost(
		"bytedance/seedance-2.5/text-to-video",
		"720p",
		1,
		8,
		&VideoPriceConfig{Price720P: &price},
		1,
	)
	require.InDelta(t, 2.0, cost.ActualCost, 1e-9)
}

func TestCalculateVideoCostUsesAtlasPublishedVideoDefaults(t *testing.T) {
	t.Parallel()
	service := &BillingService{}
	for _, tc := range []struct {
		name       string
		model      string
		resolution string
		want       float64
	}{
		{"seedance 2.5 native 480p", "bytedance/seedance-2.5/text-to-video", "480p", 1.1641254251328},
		{"seedance 2.5 1080p esr", "bytedance/seedance-2.5/text-to-video", "1080p-esr", 3.755376624},
		{"seedance 2.5 4k esr", "bytedance/seedance-2.5/reference-to-video", "4k-esr", 14.169943392},
		{"seedance 2.0 mini native 480p", "bytedance/seedance-2.0-mini/text-to-video", "480p", 0.3749304029184},
		{"seedance 2.0 mini 1080p sr", "bytedance/seedance-2.0-mini/image-to-video", "1080p-sr", 1.451490144},
		{"seedance 2.0 native 4k", "bytedance/seedance-2.0/text-to-video", "4k", 8.100681504},
		{"seedance 2.0 fast 1440p sr", "bytedance/seedance-2.0-fast/text-to-video", "1440p-sr", 4.12866456},
		{"gemini omni 720p", atlasCloudGeminiVideoModel, "720p", 0.82489968},
		{"gemini omni 4k", atlasCloudGeminiVideoModel, "4k", 2.32471728},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cost := service.CalculateVideoCost(tc.model, tc.resolution, 1, 1, nil, 1)
			require.InDelta(t, tc.want, cost.TotalCost, 1e-9)
			require.InDelta(t, tc.want, cost.ActualCost, 1e-9)
		})
	}
}

func TestCalculateVideoCostAppliesGroupMultiplierAfterAtlasRetailConversion(t *testing.T) {
	t.Parallel()
	cost := (&BillingService{}).CalculateVideoCost(
		"bytedance/seedance-2.5/text-to-video",
		"720p",
		1,
		1,
		nil,
		1.2,
	)
	require.InDelta(t, 2.50386216, cost.TotalCost, 1e-9)
	require.InDelta(t, 3.0046345919999995, cost.ActualCost, 1e-9)
}

func TestNormalizeVideoModelPricesDropsUnknownResolutions(t *testing.T) {
	t.Parallel()
	// "4k" and "1080i" are not billable tiers. Collapsing them into 480p would
	// charge a 480p request at the operator's high-resolution price.
	norm := NormalizeVideoModelPrices(map[string]map[string]float64{
		"grok-imagine-video": {"480p": 0.05, "4k": 0.50, "1080i": 0.30},
	})
	require.NotNil(t, norm)
	require.Equal(t, map[string]float64{VideoBillingResolution480P: 0.05}, norm[VideoPriceFamilyGrokImagineVideo])

	// A model whose tiers are all unrecognized contributes no family at all.
	require.Nil(t, NormalizeVideoModelPrices(map[string]map[string]float64{
		"grok-imagine-video": {"4k": 0.50},
	}))
}

func TestNormalizeVideoModelPricesIsDeterministicAcrossAliasConflicts(t *testing.T) {
	t.Parallel()
	// Both keys canonicalize onto grok-imagine-video-1.5 and disagree on 480p.
	// Whichever price wins, it must be the same one on every run — a Go map walk
	// would let two processes bill the same request differently.
	raw := map[string]map[string]float64{
		"grok-imagine-video-1.5":         {"480p": 0.08},
		"grok-imagine-video-1.5-preview": {"480p": 0.11},
		"grok-video-1.5":                 {"480p": 0.09},
	}
	first := NormalizeVideoModelPrices(raw)
	require.NotNil(t, first)
	for i := 0; i < 50; i++ {
		require.Equal(t, first, NormalizeVideoModelPrices(raw), "run %d diverged", i)
	}

	// Aliases within one model key normalize to the same tier deterministically too.
	aliased := map[string]map[string]float64{
		"grok-imagine-video": {"720": 0.12, "720p": 0.13, "hd": 0.14},
	}
	firstAliased := NormalizeVideoModelPrices(aliased)
	require.NotNil(t, firstAliased)
	for i := 0; i < 50; i++ {
		require.Equal(t, firstAliased, NormalizeVideoModelPrices(aliased), "run %d diverged", i)
	}
}

func TestLookupVideoBillingResolutionReportsUnknownTiers(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"480", "480p", "SD", "720", "hd", "1080", "full-hd", " fhd "} {
		normalized, ok := LookupVideoBillingResolution(in)
		require.True(t, ok, "input=%q", in)
		require.NotEmpty(t, normalized)
	}
	for _, in := range []string{"", "4k", "1080i", "2160p", "potato"} {
		normalized, ok := LookupVideoBillingResolution(in)
		require.False(t, ok, "input=%q", in)
		require.Empty(t, normalized)
	}
	// Runtime billing still needs a tier for unrecognized upstream values.
	require.Equal(t, VideoBillingResolution480P, NormalizeVideoBillingResolutionOrDefault("4k"))
	require.Equal(t, VideoBillingResolution1080P, NormalizeVideoBillingResolutionOrDefault("full_hd"))
}

func TestVideoModelPriceMissingTierFallsBackToFlatTierPrice(t *testing.T) {
	t.Parallel()
	flat720P := 0.7
	service := &BillingService{}

	result := service.CalculateVideoCost("grok-imagine-video", "720p", 1, 1, &VideoPriceConfig{
		Price720P: &flat720P,
		ModelPrices: map[string]map[string]float64{
			VideoPriceFamilyGrokImagineVideo: {VideoBillingResolution480P: 0.05},
		},
	}, 1)

	require.InDelta(t, flat720P, result.TotalCost, 1e-9)
}
