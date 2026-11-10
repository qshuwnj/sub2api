package service

import (
	"math"
	"slices"
)

// Dapacai fixed ModelPrice cards are per request, even when the supplier's
// usage schema includes duration. Only explicit duration expressions use USD/s.
func dapacaiVideoCardPrice(key *APIKey, model, resolution string) (float64, BillingMode, bool) {
	if key == nil || key.Group == nil {
		return 0, "", false
	}
	profile := dapacaiVideoModel(model)
	tier, validTier := LookupVideoBillingResolutionForModel(model, resolution)
	if profile == nil || profile.disabled || !validTier ||
		(len(profile.resolutions) == 0 && tier != "native") {
		return 0, "", false
	}
	for _, card := range key.Group.ModelPricing {
		// No wildcard, provider aliases, inherited channel or Atlas cards.
		if card.Platform != PlatformOpenAI || !slices.Contains(card.Models, model) {
			continue
		}
		if card.BillingMode != BillingModePerRequest && card.BillingMode != BillingModeVideo {
			return 0, "", false
		}
		if (model == "seedance-2.5-plus-8" || model == "seedance-2.0-1") && card.BillingMode != BillingModePerRequest {
			return 0, "", false
		}
		price := card.PerRequestPrice
		if len(card.Intervals) > 0 {
			price = nil
			for _, interval := range card.Intervals {
				if interval.TierLabel == tier {
					price = interval.PerRequestPrice
					break
				}
			}
		}
		if price == nil || *price < 0 || math.IsNaN(*price) || math.IsInf(*price, 0) ||
			card.TimePricing != nil || len(card.ReasoningEffortMultipliers) != 0 {
			return 0, "", false
		}
		return *price, card.BillingMode, true
	}
	// Confirmed fixed-job products must never inherit legacy USD/s fallback.
	if model == "seedance-2.5-plus-8" || model == "seedance-2.0-1" {
		return 0, "", false
	}
	// Preserve explicitly entered per-second prices from the initial adapter.
	price := LookupVideoModelPrice(key.Group.VideoModelPrices, model, tier)
	if price == nil || *price < 0 || math.IsNaN(*price) || math.IsInf(*price, 0) {
		return 0, "", false
	}
	return *price, BillingModeVideo, true
}

func calculateDapacaiVideoCost(key *APIKey, model, resolution string, count, seconds int, multiplier float64) *CostBreakdown {
	price, mode, valid := dapacaiVideoCardPrice(key, model, resolution)
	if !valid || count <= 0 {
		return &CostBreakdown{BillingMode: string(BillingModeVideo)}
	}
	units := count
	if mode == BillingModeVideo {
		units *= NormalizeVideoBillingDurationForModel(model, seconds)
	}
	if multiplier < 0 || math.IsNaN(multiplier) || math.IsInf(multiplier, 0) {
		multiplier = 0
	}
	total := price * float64(units)
	return &CostBreakdown{
		TotalCost: total, ActualCost: total * multiplier,
		BillingMode: string(BillingModeVideo),
	}
}
