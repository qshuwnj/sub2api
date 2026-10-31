package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/shopspring/decimal"
	"golang.org/x/sync/singleflight"
)

type videoSupplierPrice struct {
	Price     decimal.Decimal
	Estimated bool
}

var errVideoCalculatorUnavailable = fmt.Errorf("%w: supplier price evidence is temporarily unavailable", ErrVideoQuoteUnavailable)

type videoPriceCacheEntry struct {
	value   videoSupplierPrice
	err     error
	expires time.Time
}

// This client only calls the fixed public calculator. No credentials, prompts,
// user media, account base URLs, or generation endpoints enter this boundary.
type videoPriceCalculator struct {
	client       *http.Client
	mu           sync.Mutex
	cache        map[string]videoPriceCacheEntry
	group        singleflight.Group
	slots        chan struct{}
	backoffUntil time.Time
}

func newVideoPriceCalculator(client *http.Client) *videoPriceCalculator {
	if client == nil {
		client = &http.Client{}
	}
	copy := *client
	copy.Timeout = 8 * time.Second
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &videoPriceCalculator{client: &copy, cache: make(map[string]videoPriceCacheEntry), slots: make(chan struct{}, 2)}
}

func (c *videoPriceCalculator) lookup(ctx context.Context, req VideoQuoteRequest) (videoSupplierPrice, error) {
	if c == nil {
		return videoSupplierPrice{}, ErrVideoSettingsQuoteRequired
	}
	req, err := req.normalized()
	if err != nil {
		return videoSupplierPrice{}, err
	}
	revision, err := VideoContractRevision(req.Model)
	if err != nil {
		return videoSupplierPrice{}, err
	}
	key, err := VideoTariffKey(req)
	if err != nil {
		return videoSupplierPrice{}, err
	}
	digest := sha256.Sum256([]byte(revision + key))
	key = hex.EncodeToString(digest[:])
	c.mu.Lock()
	cached, ok := c.cache[key]
	c.mu.Unlock()
	if ok && time.Now().Before(cached.expires) {
		return cached.value, cached.err
	}
	result := c.group.DoChan(key, func() (any, error) {
		c.mu.Lock()
		cached, ok := c.cache[key]
		backoff := time.Now().Before(c.backoffUntil)
		c.mu.Unlock()
		if ok && time.Now().Before(cached.expires) {
			return cached.value, cached.err
		}
		if backoff {
			return videoSupplierPrice{}, errVideoCalculatorUnavailable
		}
		select {
		case c.slots <- struct{}{}:
			defer func() { <-c.slots }()
		default:
			return videoSupplierPrice{}, errVideoCalculatorUnavailable
		}
		// A cancelled UI request must not cancel shared evidence for other callers.
		callCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		value, err := c.fetch(callCtx, req)
		ttl := 20 * time.Minute
		if err != nil {
			ttl = 30 * time.Second
		}
		c.mu.Lock()
		if len(c.cache) >= 2048 {
			for k, cell := range c.cache {
				if time.Now().After(cell.expires) {
					delete(c.cache, k)
				}
			}
			if len(c.cache) >= 2048 {
				for k := range c.cache {
					delete(c.cache, k)
					break
				}
			}
		}
		c.cache[key] = videoPriceCacheEntry{value: value, err: err, expires: time.Now().Add(ttl)}
		c.mu.Unlock()
		return value, err
	})
	select {
	case <-ctx.Done():
		return videoSupplierPrice{}, ctx.Err()
	case reply := <-result:
		if reply.Err != nil {
			return videoSupplierPrice{}, reply.Err
		}
		return reply.Val.(videoSupplierPrice), nil
	}
}

func (c *videoPriceCalculator) fetch(ctx context.Context, selection VideoQuoteRequest) (videoSupplierPrice, error) {
	payload := make(map[string]any, len(selection.Controls)+1)
	payload["model"] = selection.Model
	for field, value := range selection.Controls {
		payload[field] = value
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return videoSupplierPrice{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, VideoCalculatorURL, bytes.NewReader(raw))
	if err != nil {
		return videoSupplierPrice{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.client.Do(request)
	if err != nil {
		return videoSupplierPrice{}, errVideoCalculatorUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusTooManyRequests || response.StatusCode == http.StatusServiceUnavailable {
		c.mu.Lock()
		c.backoffUntil = time.Now().Add(30 * time.Second)
		c.mu.Unlock()
	}
	if response.StatusCode != http.StatusOK {
		return videoSupplierPrice{}, errVideoCalculatorUnavailable
	}
	raw, err = io.ReadAll(io.LimitReader(response.Body, (64<<10)+1))
	if err != nil || len(raw) > 64<<10 {
		return videoSupplierPrice{}, errVideoCalculatorUnavailable
	}
	var result struct {
		Code int `json:"code"`
		Data struct {
			Price     string          `json:"price"`
			Estimated json.RawMessage `json:"estimated"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &result) != nil || result.Code != 200 {
		return videoSupplierPrice{}, errVideoCalculatorUnavailable
	}
	if _, err := VideoRetailPrice(result.Data.Price); err != nil {
		return videoSupplierPrice{}, errVideoCalculatorUnavailable
	}
	estimated := false
	if len(result.Data.Estimated) != 0 {
		if !bytes.Equal(result.Data.Estimated, []byte("true")) && !bytes.Equal(result.Data.Estimated, []byte("false")) {
			return videoSupplierPrice{}, errVideoCalculatorUnavailable
		}
		estimated = bytes.Equal(result.Data.Estimated, []byte("true"))
	}
	price, _ := decimal.NewFromString(result.Data.Price)
	// Reference-media duration/content is deliberately absent from public lookup.
	// A calculator response without those facts is not an exact supplier invoice.
	contextEstimated := selection.Mode == "r2v"
	if contract := atlasMediaModel(selection.Model, "video"); contract != nil && selection.Mode == "i2v" {
		ratio, _ := selection.Controls["ratio"].(string)
		if ratio == "" {
			ratio, _ = selection.Controls["aspect_ratio"].(string)
		}
		contextEstimated = ratio == "auto" || ratio == "adaptive" ||
			(ratio == "" && selection.Model != "kwaivgi/kling-video-o1/image-to-video")
	}
	return videoSupplierPrice{Price: price, Estimated: estimated || contextEstimated}, nil
}

func videoDefaultSettings(req VideoQuoteRequest) (VideoQuoteRequest, error) {
	c := atlasMediaModel(req.Model, "video")
	if c == nil {
		return VideoQuoteRequest{}, ErrVideoSettingsQuoteRequired
	}
	base := VideoQuoteRequest{Model: req.Model, Mode: req.Mode, Controls: make(map[string]any)}
	for field, value := range req.Controls {
		if videoNonPriceField(c, field) {
			base.Controls[field] = value
		} else if property := c.Input.Properties[field]; property != nil && property.Default != nil {
			base.Controls[field] = property.Default
		}
	}
	return base.normalized()
}

func (s *OpenAIGatewayService) videoSettingsTariff(ctx context.Context, key *APIKey, req VideoQuoteRequest) (base, total, revision, multiplier string, estimated bool, err error) {
	if s.videoCalculator == nil {
		err = ErrVideoSettingsQuoteRequired
		return
	}
	defaults, e := videoDefaultSettings(req)
	if e != nil {
		err = e
		return
	}
	// Existing retail for the same model, ratio, duration and resolution is the
	// authority. Only its supplier-cost ratio changes; no other model is selected.
	baseline, _, baselineRevision, rateText, _, e := s.videoTariff(ctx, key, defaults)
	if e != nil {
		err = e
		return
	}
	retail, e := decimal.NewFromString(baseline)
	if e != nil || !retail.IsPositive() {
		err = ErrVideoSettingsQuoteRequired
		return
	}
	oldCost, e := s.videoCalculator.lookup(ctx, defaults)
	if e != nil {
		err = e
		return
	}
	newCost, e := s.videoCalculator.lookup(ctx, req)
	if e != nil {
		err = e
		return
	}
	rate, _ := decimal.NewFromString(rateText)
	baseAmount := retail.Mul(newCost.Price).DivRound(oldCost.Price, 8)
	totalAmount := baseAmount.Mul(rate).Round(8)
	if baseAmount.GreaterThanOrEqual(decimal.NewFromInt(1e10)) || totalAmount.GreaterThanOrEqual(decimal.NewFromInt(1e10)) ||
		(retail.IsPositive() && baseAmount.IsZero()) || (baseAmount.IsPositive() && rate.IsPositive() && totalAmount.IsZero()) {
		err = fmt.Errorf("video total outside ledger range")
		return
	}
	base, total, multiplier = baseAmount.StringFixed(8), totalAmount.StringFixed(8), rateText
	estimated = oldCost.Estimated || newCost.Estimated
	label, _ := VideoTariffKey(req)
	raw, _ := videoCanonical([]any{"calculator-settings-retail-ratio-v1", label, baselineRevision,
		oldCost.Price.String(), newCost.Price.String(), base, total, multiplier, estimated})
	hash := sha256.Sum256(raw)
	revision = hex.EncodeToString(hash[:])
	return
}
