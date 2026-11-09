//go:build unit

package service

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDapacaiPlus8RefreshPreservesExactControlsAndReferenceLimits(t *testing.T) {
	model := "seedance-2.5-plus-8"
	p := dapacaiVideoModel(model)
	require.NotNil(t, p)
	require.Equal(t, 30, p.images)
	require.Equal(t, 10, p.videos)
	require.Equal(t, 10, p.audios)
	for _, seconds := range []int{4, 17, 30} {
		for _, ratio := range p.ratios {
			body, err := json.Marshal(map[string]any{
				"model": model, "prompt": "test", "duration": seconds,
				"ratio": ratio, "resolution": "720p",
			})
			require.NoError(t, err)
			info, err := ParseDapacaiVideoRequest(body)
			require.NoError(t, err)
			require.Equal(t, model, info.Model)
			require.Equal(t, seconds, info.DurationSeconds)
			require.Equal(t, ratio, info.AspectRatio)
		}
	}
	for _, kind := range []VideoReferenceKind{VideoReferenceImage, VideoReferenceVideo, VideoReferenceAudio} {
		_, allowed := videoReferenceLimit("dapacai:"+model, kind)
		require.True(t, allowed)
	}
	for _, kind := range []string{"image", "video", "audio"} {
		maximum := map[string]int{"image": 30, "video": 10, "audio": 10}[kind]
		for _, count := range []int{maximum, maximum + 1} {
			media := make([]map[string]string, count)
			for i := range media {
				media[i] = map[string]string{"type": kind, "url": "https://assets.example/reference"}
			}
			body, err := json.Marshal(map[string]any{
				"model": model, "prompt": "test", "duration": 17, "ratio": "auto", "resolution": "720p",
				"input": map[string]any{"prompt": "test", "media": media},
			})
			require.NoError(t, err)
			_, err = ParseDapacaiVideoRequest(body)
			if count == maximum {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		}
	}
}

func TestDapacaiMiniMaxRefreshIsTextOnlyAndCaseSafeForBilling(t *testing.T) {
	const model = "minimax-h3"
	for _, seconds := range []int{1, 8, 15} {
		for _, ratio := range []string{"16:9", "9:16", "1:1"} {
			body, err := json.Marshal(map[string]any{
				"model": model, "prompt": "test", "duration": seconds, "ratio": ratio, "resolution": "768P",
			})
			require.NoError(t, err)
			info, err := ParseDapacaiVideoRequest(body)
			require.NoError(t, err)
			require.Equal(t, model, info.Model)
			require.Equal(t, seconds, info.DurationSeconds)
			tier, valid := LookupVideoBillingResolutionForModel(model, info.Resolution)
			require.True(t, valid)
			require.Equal(t, "768p", tier)
		}
	}
	_, err := VideoReferenceFamilyForModel(model)
	require.Error(t, err)
	_, err = VideoFrameFamilyForModel(model)
	require.Error(t, err)
	for _, kind := range []VideoReferenceKind{VideoReferenceImage, VideoReferenceVideo, VideoReferenceAudio} {
		_, allowed := videoReferenceLimit("dapacai:"+model, kind)
		require.False(t, allowed)
	}
	for _, extra := range []map[string]any{
		{"images": []string{"https://assets.example/image.png"}},
		{"duration": 16}, {"resolution": "720p"},
	} {
		var payload map[string]any
		require.NoError(t, json.Unmarshal(dapacaiTestBody(model), &payload))
		for field, value := range extra {
			payload[field] = value
		}
		body, err := json.Marshal(payload)
		require.NoError(t, err)
		_, err = ParseDapacaiVideoRequest(body)
		require.Error(t, err)
	}
	s, key := dapacaiPreviewFixture(t)
	for _, mode := range []string{"image", "references", "frames"} {
		_, err := s.PreviewDapacaiVideoPrice(context.Background(), key, VideoQuoteRequest{
			Model: model, Mode: mode, Controls: map[string]any{"duration": 8, "ratio": "16:9", "resolution": "768P"},
		}, time.Now())
		require.ErrorContains(t, err, "unsupported Dapacai video mode")
	}
	_, err = s.PreviewDapacaiVideoPrice(context.Background(), key, VideoQuoteRequest{
		Model: model, Mode: "text", Controls: map[string]any{"duration": 8, "ratio": "16:9", "resolution": "768P"},
	}, time.Now())
	require.ErrorContains(t, err, "price card is not configured", "do not guess conflicting supplier billing")
	price := 0.25 // Synthetic USD/s to verify case normalization, not a supplier price.
	key.Group.ModelPricing = []ChannelModelPricing{{
		Platform: PlatformOpenAI, Models: []string{model}, BillingMode: BillingModeVideo, PerRequestPrice: &price,
	}}
	q, err := s.PreviewDapacaiVideoPrice(context.Background(), key, VideoQuoteRequest{
		Model: model, Mode: "text", Controls: map[string]any{"duration": 8, "ratio": "16:9", "resolution": "768P"},
	}, time.Now())
	require.NoError(t, err)
	require.Equal(t, "2.40000000", q.TotalPrice)
	require.InDelta(t, 2.4, calculateDapacaiVideoCost(key, model, "768P", 1, 8, 1.2).ActualCost, 1e-12)
}

func TestDapacaiPlus8FixedRequestQuoteIsNotMultipliedBySecondsOrFX(t *testing.T) {
	s, key := dapacaiPreviewFixture(t)
	price := 5.3
	key.Group.ModelPricing = []ChannelModelPricing{{
		Platform: PlatformOpenAI, Models: []string{"seedance-2.5-plus-8"},
		BillingMode: BillingModePerRequest, PerRequestPrice: &price,
	}}
	for _, duration := range []int{4, 17, 30} {
		for _, mode := range []string{"text", "image", "references"} {
			q, err := s.PreviewDapacaiVideoPrice(context.Background(), key, VideoQuoteRequest{
				Model: "seedance-2.5-plus-8", Mode: mode,
				Controls: map[string]any{"duration": duration, "ratio": "auto", "resolution": "720p"},
			}, time.Now())
			require.NoError(t, err)
			require.Equal(t, "6.36000000", q.TotalPrice)
			require.Equal(t, "request", q.BillingUnit)
			require.Equal(t, 1, q.UnitCount)
			require.InDelta(t, 6.36, calculateDapacaiVideoCost(key, "seedance-2.5-plus-8", "720p", 1, duration, 1.2).ActualCost, 1e-12)
		}
	}
}

func TestDapacaiPlus8RejectsPerSecondTariffsBeforeGeneration(t *testing.T) {
	s, key := dapacaiPreviewFixture(t)
	const model = "seedance-2.5-plus-8"
	price := 5.3
	for _, legacy := range []bool{false, true} {
		key.Group.ModelPricing = nil
		key.Group.VideoModelPrices = nil
		if legacy {
			key.Group.VideoModelPrices = map[string]map[string]float64{model: {"720p": price}}
		} else {
			key.Group.ModelPricing = []ChannelModelPricing{{
				Platform: PlatformOpenAI, Models: []string{model},
				BillingMode: BillingModeVideo, PerRequestPrice: &price,
			}}
		}
		_, _, valid := dapacaiVideoCardPrice(key, model, "720p")
		require.False(t, valid)
		_, err := s.PreviewDapacaiVideoPrice(context.Background(), key, VideoQuoteRequest{
			Model: model, Mode: "text",
			Controls: map[string]any{"duration": 30, "ratio": "16:9", "resolution": "720p"},
		}, time.Now())
		require.ErrorContains(t, err, "price card is not configured")
	}
}
