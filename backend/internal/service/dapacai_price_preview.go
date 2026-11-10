package service

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/shopspring/decimal"
)

// A preview is read-only current group pricing, not an Atlas signed price lock.
// It never reserves funds, creates a task or contacts a supplier.
type DapacaiPricePreview struct {
	PricePreview bool      `json:"price_preview"`
	Selection    any       `json:"selection"`
	Currency     string    `json:"currency"`
	BillingUnit  string    `json:"billing_unit"`
	UnitCount    int       `json:"unit_count"`
	UnitPrice    string    `json:"unit_price"`
	TotalPrice   string    `json:"total_price"`
	ExpiresAt    time.Time `json:"expires_at"`
}

func newDapacaiPricePreview(selection any, unit string, count int, base, rate float64, now time.Time) (*DapacaiPricePreview, error) {
	if count < 1 || base < 0 || rate < 0 || math.IsNaN(base) || math.IsInf(base, 0) || math.IsNaN(rate) || math.IsInf(rate, 0) {
		return nil, fmt.Errorf("invalid Dapacai price or multiplier")
	}
	price := decimal.NewFromFloat(base).Mul(decimal.NewFromFloat(rate))
	total := price.Mul(decimal.NewFromInt(int64(count)))
	if total.GreaterThanOrEqual(decimal.NewFromInt(1e10)) || (total.IsPositive() && total.Round(8).IsZero()) {
		return nil, fmt.Errorf("Dapacai price outside ledger range")
	}
	return &DapacaiPricePreview{
		PricePreview: true, Selection: selection, Currency: "USD", BillingUnit: unit, UnitCount: count,
		UnitPrice: price.StringFixed(8), TotalPrice: total.StringFixed(8),
		ExpiresAt: now.Add(time.Minute).UTC().Truncate(time.Second),
	}, nil
}

func (s *OpenAIGatewayService) PreviewDapacaiVideoPrice(ctx context.Context, key *APIKey, req VideoQuoteRequest, now time.Time) (*DapacaiPricePreview, error) {
	profile := dapacaiVideoModel(req.Model)
	if profile == nil || profile.disabled {
		return nil, fmt.Errorf("unsupported Dapacai video model")
	}
	if !slices.Contains([]string{"text", "image", "frames", "references"}, req.Mode) ||
		req.Mode == "text" && profile.requiresReference || req.Mode == "frames" && !profile.frames ||
		profile.textOnly && req.Mode != "text" {
		return nil, fmt.Errorf("unsupported Dapacai video mode")
	}
	var controls struct {
		Duration   int    `json:"duration"`
		Ratio      string `json:"ratio"`
		Resolution string `json:"resolution,omitempty"`
	}
	// Reject all content, caller prices and unsupported settings, not just a denylist.
	for field := range req.Controls {
		if field != "duration" && field != "ratio" && field != "resolution" {
			return nil, fmt.Errorf("Dapacai price controls contain unsupported fields")
		}
	}
	raw, err := json.Marshal(req.Controls)
	if err != nil || json.Unmarshal(raw, &controls) != nil ||
		!slices.Contains(profile.durations, controls.Duration) || !slices.Contains(profile.ratios, controls.Ratio) ||
		(len(profile.resolutions) == 0 && controls.Resolution != "") ||
		(len(profile.resolutions) > 0 && !slices.Contains(profile.resolutions, controls.Resolution)) {
		return nil, fmt.Errorf("unsupported Dapacai duration, ratio or resolution")
	}
	key, err = s.RefreshVideoQuoteKey(ctx, key, req.Model, now)
	if err != nil {
		return nil, err
	}
	tier := controls.Resolution
	if tier == "" {
		tier = "native"
	}
	price, mode, valid := dapacaiVideoCardPrice(key, req.Model, tier)
	if !valid {
		return nil, fmt.Errorf("Dapacai price card is not configured")
	}
	rate, err := s.videoCustomerRate(ctx, key)
	if err != nil {
		return nil, err
	}
	unit, count := "request", 1
	if mode == BillingModeVideo {
		unit, count = "second", controls.Duration
	}
	return newDapacaiPricePreview(req, unit, count, price, rate, now)
}

func (s *OpenAIGatewayService) PreviewDapacaiImagePrice(ctx context.Context, key *APIKey, req ImageQuoteRequest, now time.Time) (*DapacaiPricePreview, error) {
	if !IsDapacaiImageModel(req.Model) || req.Count < 1 || req.Count > 4 || req.Quality != "none" ||
		req.AspectRatio != "" || req.Resolution != "" ||
		!slices.Contains([]string{"1024x1024", "1536x1024", "1024x1536"}, req.Size) {
		return nil, fmt.Errorf("unsupported Dapacai image controls")
	}
	key, err := s.RefreshVideoQuoteKey(ctx, key, req.Model, now)
	if err != nil {
		return nil, err
	}
	var price *float64
	for _, card := range key.Group.ModelPricing {
		if card.Platform != PlatformOpenAI || !slices.Contains(card.Models, req.Model) {
			continue
		}
		if price != nil || (card.BillingMode != BillingModeImage && card.BillingMode != BillingModePerRequest) ||
			len(card.Intervals) != 0 || card.TimePricing != nil || len(card.ReasoningEffortMultipliers) != 0 {
			return nil, fmt.Errorf("unsupported or overlapping Dapacai image price cards")
		}
		price = card.PerRequestPrice
	}
	if price == nil {
		return nil, fmt.Errorf("Dapacai image price card is not configured")
	}
	rate := key.Group.RateMultiplier
	if s.userGroupRateResolver != nil && s.userGroupRateResolver.repo != nil {
		override, err := s.userGroupRateResolver.repo.GetByUserAndGroup(ctx, key.UserID, key.Group.ID)
		if err != nil {
			return nil, ErrVideoQuoteUnavailable
		}
		if override != nil {
			rate = *override
		}
	}
	if key.Group.ImageRateIndependent {
		rate = key.Group.ImageRateMultiplier
	}
	return newDapacaiPricePreview(req, "image", req.Count, *price, rate, now)
}
