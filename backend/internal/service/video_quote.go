package service

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

const (
	VideoCalculatorURL = "https://api.atlascloud.ai/api/v1/model/calculate"
	VideoRetailPolicy  = "fixed-customer-price-platform-variance-v1"
	videoQuoteDomain   = "video-native-quote-v1"
)

var ErrVideoQuoteUnavailable = errors.New("video quoted billing or fresh admission is unavailable")
var ErrVideoSettingsQuoteRequired = errors.New("video settings require an exact configured quote")

func videoSettingLocked(field string) bool {
	switch field {
	case "multi_shot", "shot_type", "multi_prompt", "elements", "omni_reference_task_type", "draft", "return_last_frame":
		return true
	}
	return false
}

func videoNonPriceField(c *atlasMediaContract, field string) bool {
	switch field {
	case "model", "prompt", "negative_prompt", "ratio", "aspect_ratio", "duration", "resolution":
		return true
	}
	_, media := c.MediaFields[field]
	return media
}

func videoLegacySettings(c *atlasMediaContract, controls map[string]any) error {
	for field, value := range controls {
		if videoNonPriceField(c, field) {
			continue
		}
		property := c.Input.Properties[field]
		if property == nil || property.Default == nil || !reflectVideoEqual(value, property.Default) {
			return ErrVideoSettingsQuoteRequired
		}
	}
	return nil
}

// ValidateLegacyVideoSettings prevents changed cost controls from falling
// through to model/resolution-only legacy billing without an exact quote.
func ValidateLegacyVideoSettings(body []byte) error {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return err
	}
	model, _ := payload["model"].(string)
	if c := atlasMediaModel(model, "video"); c != nil {
		return videoLegacySettings(c, payload)
	}
	return nil
}

// VideoTariffMetadata is stored only in group model_pricing JSON. Cells are
// keyed by the exact Studio video:v1 tariff key, never by duration or resolution.
type VideoTariffMetadata struct {
	Version          int                              `json:"version"`
	ContractRevision string                           `json:"contract_revision"`
	Cells            map[string]VideoTariffProvenance `json:"cells"`
}
type VideoTariffProvenance struct {
	Source            string `json:"source"`
	CheckedAt         string `json:"checked_at"`
	UpstreamPrice     string `json:"upstream_price"`
	SupplierEstimated bool   `json:"supplier_estimated"`
	Policy            string `json:"policy"`
	ExplicitFree      bool   `json:"explicit_free,omitempty"`
}
type VideoQuoteRequest struct {
	Model    string         `json:"model"`
	Mode     string         `json:"mode"`
	Controls map[string]any `json:"controls"`
}
type VideoQuote struct {
	Enrolled     bool `json:"enrolled"`
	EstimateOnly bool `json:"estimate_only,omitempty"`
	VideoQuoteRequest
	ID                string    `json:"quote_id,omitempty"`
	Token             string    `json:"quote_token,omitempty"`
	TariffRevision    string    `json:"tariff_revision,omitempty"`
	ContractRevision  string    `json:"contract_revision,omitempty"`
	ExpiresAt         time.Time `json:"expires_at,omitempty"`
	Currency          string    `json:"currency,omitempty"`
	BillingUnit       string    `json:"billing_unit,omitempty"`
	UnitCount         int       `json:"unit_count,omitempty"`
	TotalPrice        string    `json:"total_price,omitempty"`
	PriceSource       string    `json:"price_source,omitempty"`
	SupplierEstimated bool      `json:"supplier_estimated"`
}
type videoQuoteClaims struct {
	ID                string            `json:"id"`
	KeyID             int64             `json:"key"`
	UserID            int64             `json:"user"`
	GroupID           int64             `json:"group"`
	Selection         VideoQuoteRequest `json:"selection"`
	BasePrice         string            `json:"base_price"`
	TotalPrice        string            `json:"total_price"`
	Multiplier        string            `json:"multiplier"`
	TariffRevision    string            `json:"tariff_revision"`
	ContractRevision  string            `json:"contract_revision"`
	SupplierEstimated bool              `json:"supplier_estimated"`
	Expires           int64             `json:"expires"`
}

// A snapshot can only be constructed by signature verification and fresh
// admission. Durable acceptance/reservation belongs to the attempt repository.
type VideoQuoteSnapshot struct{ claims videoQuoteClaims }

func (v *VideoQuoteSnapshot) QuoteID() string          { return v.claims.ID }
func (v *VideoQuoteSnapshot) APIKeyID() int64          { return v.claims.KeyID }
func (v *VideoQuoteSnapshot) UserID() int64            { return v.claims.UserID }
func (v *VideoQuoteSnapshot) GroupID() int64           { return v.claims.GroupID }
func (v *VideoQuoteSnapshot) TotalPrice() string       { return v.claims.TotalPrice }
func (v *VideoQuoteSnapshot) BasePrice() string        { return v.claims.BasePrice }
func (v *VideoQuoteSnapshot) Multiplier() string       { return v.claims.Multiplier }
func (v *VideoQuoteSnapshot) TariffRevision() string   { return v.claims.TariffRevision }
func (v *VideoQuoteSnapshot) ContractRevision() string { return v.claims.ContractRevision }
func (v *VideoQuoteSnapshot) SupplierEstimated() bool  { return v.claims.SupplierEstimated }
func (v *VideoQuoteSnapshot) ExpiresAt() time.Time     { return time.Unix(v.claims.Expires, 0).UTC() }
func (v *VideoQuoteSnapshot) RequestID() string        { return "video-quote:" + v.claims.ID }
func (v *VideoQuoteSnapshot) Selection() VideoQuoteRequest {
	out := v.claims.Selection
	raw, _ := json.Marshal(out.Controls)
	out.Controls = nil
	_ = json.Unmarshal(raw, &out.Controls)
	return out
}

func videoCanonical(value any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte{'\n'}), nil
}

var videoRawContracts = func() map[string]map[string]any {
	var entries []map[string]any
	if err := json.Unmarshal(atlasMediaSchemaJSON, &entries); err != nil {
		panic(err)
	}
	result := map[string]map[string]any{}
	for _, e := range entries {
		if id, ok := e["id"].(string); ok {
			result[id] = e
		}
	}
	return result
}()

// Matches collect-atlas-video-tariffs.mjs revision(entry): source and annotations
// are not executable contract data and do not change this revision.
func VideoContractRevision(model string) (string, error) {
	e := videoRawContracts[model]
	if atlasMediaModel(model, "video") == nil || e == nil {
		return "", fmt.Errorf("unsupported native video model")
	}
	var sanitize func(any) any
	sanitize = func(value any) any {
		switch v := value.(type) {
		case map[string]any:
			out := map[string]any{}
			for k, child := range v {
				if slices.Contains([]string{"description", "title", "examples", "example", "deprecated", "step"}, k) || strings.HasPrefix(k, "x-") {
					continue
				}
				out[k] = sanitize(child)
			}
			return out
		case []any:
			out := make([]any, len(v))
			for i, child := range v {
				out[i] = sanitize(child)
			}
			return out
		default:
			return v
		}
	}
	input := sanitize(e["input"]).(map[string]any)
	properties := input["properties"].(map[string]any)
	properties["model"] = map[string]any{"type": "string", "const": model}
	if prompt, ok := properties["prompt"].(map[string]any); ok {
		delete(prompt, "default")
	}
	raw, err := videoCanonical(map[string]any{"id": model, "mode": e["mode"], "input": input, "mediaFields": e["mediaFields"]})
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:]), nil
}

// Reject unimplemented schema keywords instead of silently discarding them.
func videoSchemaSupported(value any) bool {
	schema, ok := value.(map[string]any)
	if !ok {
		return false
	}
	for k, v := range schema {
		switch k {
		case "type":
			kind, ok := v.(string)
			if !ok || !slices.Contains([]string{"string", "number", "integer", "boolean", "array", "object"}, kind) {
				return false
			}
		case "required", "enum", "const", "default", "minimum", "maximum", "minLength", "maxLength", "minItems", "maxItems", "pattern", "description", "title", "examples", "example", "deprecated", "disabled":
		case "format":
			if v != "uri" {
				return false
			}
		case "additionalProperties":
			if _, ok := v.(bool); !ok {
				return false
			}
		case "properties":
			props, ok := v.(map[string]any)
			if !ok {
				return false
			}
			for _, child := range props {
				if !videoSchemaSupported(child) {
					return false
				}
			}
		case "items", "if", "then", "else":
			if !videoSchemaSupported(v) {
				return false
			}
		case "allOf":
			children, ok := v.([]any)
			if !ok {
				return false
			}
			for _, child := range children {
				if !videoSchemaSupported(child) {
					return false
				}
			}
		default:
			if strings.HasPrefix(k, "x-") {
				continue
			}
			return false
		}
	}
	return true
}

func (req VideoQuoteRequest) normalized() (VideoQuoteRequest, error) {
	c := atlasMediaModel(req.Model, "video")
	if c == nil || req.Mode != c.Mode || (req.Mode != "t2v" && req.Mode != "i2v" && req.Mode != "r2v") || req.Controls == nil ||
		!videoSchemaSupported(videoRawContracts[req.Model]["input"]) {
		return req, fmt.Errorf("unsupported native video contract or mode")
	}
	// JSON-normalize numeric types while retaining the native string/number distinction.
	raw, err := videoCanonical(req.Controls)
	if err != nil {
		return req, err
	}
	var controls map[string]any
	if json.Unmarshal(raw, &controls) != nil {
		return req, fmt.Errorf("invalid controls")
	}
	p := c.Input.Properties
	ratio := ""
	if p["ratio"] != nil {
		ratio = "ratio"
	} else if p["aspect_ratio"] != nil {
		ratio = "aspect_ratio"
	}
	expected := map[string]bool{"duration": true}
	optional := map[string]bool{}
	for _, field := range []string{ratio, "resolution"} {
		if field != "" && p[field] != nil {
			expected[field] = true
		}
	}
	for field, property := range p {
		if field == "model" || field == "prompt" || field == "negative_prompt" || field == ratio || field == "duration" || field == "resolution" {
			continue
		}
		if _, media := c.MediaFields[field]; media {
			continue
		}
		if property.Default != nil {
			expected[field] = true
			if videoSettingLocked(field) && !reflectVideoEqual(controls[field], property.Default) {
				return req, fmt.Errorf("video control %s requires a structured workflow", field)
			}
		} else if !videoSettingLocked(field) &&
			(property.Type == "string" || property.Type == "boolean" || property.Type == "integer" || property.Type == "number") {
			optional[field] = true
		}
	}
	for field := range expected {
		if _, exists := controls[field]; !exists {
			return req, fmt.Errorf("incomplete video control %s", field)
		}
	}
	for field := range controls {
		if !expected[field] && !optional[field] {
			return req, fmt.Errorf("unsupported video control %s", field)
		}
	}
	for _, field := range []string{ratio, "resolution"} {
		if field == "" || p[field] == nil {
			continue
		}
		v := controls[field]
		if text, ok := v.(string); ok && (strings.EqualFold(text, "auto") || strings.EqualFold(text, "adaptive")) && field != ratio {
			return req, fmt.Errorf("automatic video controls are not fixed-price")
		}
		if len(p[field].Enum) == 0 && p[field].Default == nil {
			return req, fmt.Errorf("unbounded video control %s", field)
		}
		if len(p[field].Enum) == 0 && !reflectVideoEqual(v, p[field].Default) {
			return req, fmt.Errorf("unsupported fixed video %s", field)
		}
	}
	duration := controls["duration"]
	var seconds decimal.Decimal
	switch d := duration.(type) {
	case float64:
		seconds = decimal.NewFromFloat(d)
	case string:
		seconds, err = decimal.NewFromString(d)
	default:
		return req, fmt.Errorf("invalid native video duration")
	}
	if err != nil || !seconds.IsPositive() || !seconds.Equal(seconds.Truncate(0)) || seconds.GreaterThan(decimal.NewFromInt(120)) {
		return req, fmt.Errorf("a fixed positive duration is required")
	}
	// No-reference validation placeholders exist only in memory.
	body := map[string]any{"model": req.Model, "prompt": "Native price validation"}
	for k, v := range controls {
		body[k] = v
	}
	if req.Mode == "i2v" || req.Mode == "r2v" {
		found := false
		fields := []string{"image", "image_url", "images", "image_urls", "reference_images"}
		if req.Mode == "r2v" {
			fields = []string{"reference_images", "reference_videos"}
		}
		for _, field := range fields {
			if ref, ok := c.MediaFields[field]; ok {
				if ref.Array {
					body[field] = []any{"https://assets.example/price-schema-check.png"}
				} else {
					body[field] = "https://assets.example/price-schema-check.png"
				}
				found = true
				break
			}
		}
		if !found {
			return req, fmt.Errorf("unverified input reference")
		}
	}
	if err := validateAtlasMediaValue(body, c.Input, "video", true); err != nil {
		return req, err
	}
	req.Controls = controls
	return req, nil
}
func reflectVideoEqual(a, b any) bool {
	x, _ := videoCanonical(a)
	y, _ := videoCanonical(b)
	return bytes.Equal(x, y)
}

func VideoTariffKey(req VideoQuoteRequest) (string, error) {
	req, err := req.normalized()
	if err != nil {
		return "", err
	}
	raw, err := videoCanonical([]any{req.Model, req.Mode, req.Controls})
	if err != nil {
		return "", err
	}
	return "video:v1:" + base64.RawURLEncoding.EncodeToString(raw), nil
}
func videoSelectionFromKey(key string) (VideoQuoteRequest, error) {
	var req VideoQuoteRequest
	if !strings.HasPrefix(key, "video:v1:") {
		return req, fmt.Errorf("invalid video tariff namespace")
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(strings.TrimPrefix(key, "video:v1:"))
	var tuple []json.RawMessage
	if err != nil || json.Unmarshal(raw, &tuple) != nil || len(tuple) != 3 {
		return req, fmt.Errorf("invalid video tariff key")
	}
	if json.Unmarshal(tuple[0], &req.Model) != nil || json.Unmarshal(tuple[1], &req.Mode) != nil || json.Unmarshal(tuple[2], &req.Controls) != nil {
		return req, fmt.Errorf("invalid video tariff tuple")
	}
	normalized, err := VideoTariffKey(req)
	if err != nil || normalized != key {
		return req, fmt.Errorf("noncanonical or unsupported video tariff")
	}
	return req, nil
}

var videoSupplierPricePattern = regexp.MustCompile(`^(?:0|[1-9]\d{0,11})(?:\.\d{1,18})?$`)

func VideoRetailPrice(upstream string) (string, error) {
	if !videoSupplierPricePattern.MatchString(upstream) {
		return "", fmt.Errorf("invalid supplier price")
	}
	amount, err := decimal.NewFromString(upstream)
	if err != nil || !amount.IsPositive() {
		return "", fmt.Errorf("positive supplier price required")
	}
	retail := amount.Mul(decimal.RequireFromString("8.33232")).Round(8)
	if !retail.IsPositive() || retail.GreaterThanOrEqual(decimal.NewFromInt(1e10)) {
		return "", fmt.Errorf("retail price outside ledger range")
	}
	return retail.StringFixed(8), nil
}

func videoCardEnrolled(card ChannelModelPricing) bool {
	if card.VideoTariff != nil {
		return true
	}
	for _, iv := range card.Intervals {
		if strings.HasPrefix(iv.TierLabel, "video:") {
			return true
		}
	}
	return false
}

func videoIntervalPrice(iv PricingInterval) (decimal.Decimal, error) {
	if iv.PerRequestPrice == nil || math.IsNaN(*iv.PerRequestPrice) || math.IsInf(*iv.PerRequestPrice, 0) {
		return decimal.Zero, fmt.Errorf("video prices require finite nonnegative amounts with at most eight decimals")
	}
	price := decimal.NewFromFloat(*iv.PerRequestPrice)
	if iv.perRequestPriceDecimal != "" && iv.perRequestPriceSource == iv.PerRequestPrice {
		exact, err := decimal.NewFromString(iv.perRequestPriceDecimal)
		if err != nil {
			return decimal.Zero, fmt.Errorf("invalid native video retail decimal")
		}
		// Copies retain the original pointer. Replacements invalidate captured
		// precision, even when their binary64 value equals the source projection;
		// different in-place values are detected by this value check.
		original, _ := exact.Float64()
		if original == *iv.PerRequestPrice {
			price = exact
		}
	}
	if price.IsNegative() || price.GreaterThanOrEqual(decimal.NewFromInt(1e12)) || price.Exponent() < -8 {
		return decimal.Zero, fmt.Errorf("video prices require finite nonnegative amounts with at most eight decimals")
	}
	return price, nil
}

func validateVideoTariffCard(card ChannelModelPricing) error {
	if card.Platform != PlatformOpenAI || card.BillingMode != BillingModeVideo || len(card.Models) != 1 || card.VideoTariff == nil ||
		card.VideoTariff.Version != 1 || len(card.Intervals) == 0 || card.TimePricing != nil || len(card.ReasoningEffortMultipliers) != 0 ||
		card.PerRequestPrice != nil || card.InputPrice != nil || card.OutputPrice != nil || card.CacheWritePrice != nil || card.CacheWrite1hPrice != nil ||
		card.CacheReadPrice != nil || card.ImageInputPrice != nil || card.ImageOutputPrice != nil || card.FastMultiplier != nil || card.FlexMultiplier != nil {
		return fmt.Errorf("native video tariffs require one exact OpenAI model, versioned provenance and per-job cells without fallback prices")
	}
	revision, err := VideoContractRevision(card.Models[0])
	if err != nil || revision != card.VideoTariff.ContractRevision {
		return fmt.Errorf("video contract metadata mismatch")
	}
	if len(card.VideoTariff.Cells) != len(card.Intervals) {
		return fmt.Errorf("video provenance must match every unique tariff cell")
	}
	seen := map[string]bool{}
	for _, iv := range card.Intervals {
		req, err := videoSelectionFromKey(iv.TierLabel)
		if err != nil || req.Model != card.Models[0] || seen[iv.TierLabel] {
			return fmt.Errorf("duplicate or invalid native video cell")
		}
		seen[iv.TierLabel] = true
		if iv.MinTokens != 0 || iv.MaxTokens != nil || iv.InputPrice != nil || iv.OutputPrice != nil || iv.CacheWritePrice != nil || iv.CacheWrite1hPrice != nil || iv.CacheReadPrice != nil ||
			iv.InputMultiplier != nil || iv.OutputMultiplier != nil || iv.CacheWriteMultiplier != nil || iv.CacheReadMultiplier != nil {
			return fmt.Errorf("video cells contain only complete per-job prices")
		}
		price, err := videoIntervalPrice(iv)
		if err != nil {
			return err
		}
		if price.GreaterThanOrEqual(decimal.NewFromInt(1e10)) {
			return ErrVideoAttemptMoney
		}
		provenance, ok := card.VideoTariff.Cells[iv.TierLabel]
		if !ok || provenance.Source != VideoCalculatorURL || provenance.Policy != VideoRetailPolicy {
			return fmt.Errorf("unverified video provenance")
		}
		if _, err := time.Parse(time.RFC3339Nano, provenance.CheckedAt); err != nil {
			return fmt.Errorf("invalid video evidence timestamp")
		}
		if _, err := VideoRetailPrice(provenance.UpstreamPrice); err != nil {
			return err
		}
		if price.IsZero() != provenance.ExplicitFree {
			return fmt.Errorf("zero video price requires explicit free tariff intent")
		}
	}
	return nil
}
func (s *OpenAIGatewayService) VideoPricingEnrolled(key *APIKey, model string) (bool, error) {
	if key == nil || key.Group == nil {
		return false, nil
	}
	found := false
	for _, c := range key.Group.ModelPricing {
		if !videoCardEnrolled(c) {
			continue
		}
		// Stale tariffs for other models must not disable this model. Keep
		// matching wildcard/multi-model and unscoped malformed cards fail-closed.
		matches := len(c.Models) == 0
		for _, pattern := range c.Models {
			if groupAllowlistPatternMatches(strings.TrimSpace(pattern), model) {
				matches = true
			}
		}
		if !matches {
			continue
		}
		if err := validateVideoTariffCard(c); err != nil {
			return false, err
		}
		if c.Models[0] != model {
			continue
		}
		if found {
			return false, fmt.Errorf("overlapping native video cards")
		}
		found = true
	}
	return found, nil
}

type videoQuotedBillingReadiness interface{ VideoQuotedBillingReady() bool }

func (s *OpenAIGatewayService) VideoQuotedBillingReady() bool {
	if s == nil || s.usageBillingRepo == nil {
		return false
	}
	ready, ok := s.usageBillingRepo.(videoQuotedBillingReadiness)
	return ok && ready.VideoQuotedBillingReady()
}

// RefreshVideoQuoteKey never trusts a cached key, user, group, channel or account.
// Only DB-backed GetByID capability is required from the existing key loader.
func (s *OpenAIGatewayService) RefreshVideoQuoteKey(ctx context.Context, key *APIKey, model string, now time.Time) (*APIKey, error) {
	if s == nil || key == nil || key.ID <= 0 || key.UserID <= 0 || key.GroupID == nil || s.billingCacheService == nil || s.userRepo == nil || s.channelService == nil || s.channelService.groupRepo == nil || s.accountRepo == nil {
		return nil, ErrVideoQuoteUnavailable
	}
	reader, ok := s.billingCacheService.apiKeyRateLimitLoader.(interface {
		GetByID(context.Context, int64) (*APIKey, error)
	})
	if !ok {
		return nil, ErrVideoQuoteUnavailable
	}
	fresh, err := reader.GetByID(ctx, key.ID)
	if err != nil || fresh == nil || fresh.ID != key.ID || fresh.UserID != key.UserID || fresh.GroupID == nil || *fresh.GroupID != *key.GroupID || !fresh.IsActive() ||
		fresh.ExpiresAt != nil && !now.Before(*fresh.ExpiresAt) || fresh.IsQuotaExhausted() {
		return nil, fmt.Errorf("video API key is inactive, expired, reassigned or exhausted")
	}
	user, err := s.userRepo.GetByID(ctx, fresh.UserID)
	if err != nil || user == nil || user.ID != fresh.UserID || !user.IsActive() || user.DeletedAt != nil {
		return nil, fmt.Errorf("video user is unavailable")
	}
	group, err := s.channelService.groupRepo.GetByIDLite(ctx, *fresh.GroupID)
	if err != nil || group == nil || group.ID != *fresh.GroupID || !group.IsActive() || !user.CanBindGroup(group.ID, group.IsExclusive) ||
		(group.Platform != PlatformOpenAI && group.Platform != PlatformComposite) {
		return nil, fmt.Errorf("video group is unavailable")
	}
	if !GroupAllowsImageGeneration(group) {
		return nil, fmt.Errorf("video generation permission denied")
	}
	if group.ModelAllowlistEnabled() && !group.ModelAllowlist.Allows(model) {
		return nil, fmt.Errorf("video model not allowed for this group")
	}
	// Read the channel directly, avoiding a stale permissive restriction cache.
	if s.channelService.repo == nil {
		return nil, ErrVideoQuoteUnavailable
	}
	channelID, err := s.channelService.repo.GetChannelIDByGroupID(ctx, group.ID)
	if err != nil {
		return nil, ErrVideoQuoteUnavailable
	}
	if channelID > 0 {
		ch, err := s.channelService.repo.GetByID(ctx, channelID)
		if err != nil || ch == nil || !ch.IsActive() {
			return nil, ErrVideoQuoteUnavailable
		}
		cache := populateChannelCache([]Channel{*ch}, map[int64]string{group.ID: group.Platform})
		lookup := &channelLookup{cache: cache, channel: ch, platform: PlatformOpenAI}
		if checkRestricted(lookup, group.ID, model) || resolveMapping(lookup, group.ID, model).MappedModel != model {
			return nil, fmt.Errorf("video channel restricts or remaps the exact model")
		}
	}
	accounts, err := s.accountRepo.ListSchedulableByGroupIDAndPlatform(ctx, group.ID, PlatformOpenAI)
	if err != nil {
		return nil, ErrVideoQuoteUnavailable
	}
	eligible := false
	scheduler := &defaultOpenAIAccountScheduler{service: s}
	for i := range accounts {
		a := &accounts[i]
		if a.ID <= 0 || !slices.Contains(a.GroupIDs, group.ID) || strings.TrimSpace(a.GetCredential("api_key")) == "" ||
			!a.IsSchedulable() || !a.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityAtlasCloudVideo) || !a.IsModelSupported(model) || a.GetMappedModel(model) != model ||
			group.RequireOAuthOnly || group.RequirePrivacySet && !a.IsPrivacySet() {
			continue
		}
		if !scheduler.isAccountRequestCompatible(WithOpenAIProfitControlSuppressed(ctx), a, OpenAIAccountScheduleRequest{
			RequestedModel: model, Platform: PlatformOpenAI, RequiredCapability: OpenAIEndpointCapabilityAtlasCloudVideo, RequirePrivacySet: group.RequirePrivacySet,
		}) {
			continue
		}
		eligible = true
		break
	}
	if !eligible {
		return nil, fmt.Errorf("no exact capable AtlasCloud video account available")
	}
	copy := *fresh
	copy.User = user
	copy.Group = group
	return &copy, nil
}

func (s *OpenAIGatewayService) videoTariff(ctx context.Context, key *APIKey, req VideoQuoteRequest) (base, total, revision, multiplier string, estimated bool, err error) {
	label, e := VideoTariffKey(req)
	if e != nil {
		err = e
		return
	}
	enrolled, e := s.VideoPricingEnrolled(key, req.Model)
	if e != nil {
		err = e
		return
	}
	if !enrolled {
		if videoLegacySettings(atlasMediaModel(req.Model, "video"), req.Controls) != nil {
			return s.videoSettingsTariff(ctx, key, req)
		}
		return s.videoConfiguredRuleTariff(ctx, key, req)
	}
	var card *ChannelModelPricing
	for i := range key.Group.ModelPricing {
		if len(key.Group.ModelPricing[i].Models) == 1 && key.Group.ModelPricing[i].Models[0] == req.Model && videoCardEnrolled(key.Group.ModelPricing[i]) {
			card = &key.Group.ModelPricing[i]
		}
	}
	var selected *PricingInterval
	for _, iv := range card.Intervals {
		if iv.TierLabel == label {
			cell := iv
			selected = &cell
		}
	}
	if selected == nil {
		if videoLegacySettings(atlasMediaModel(req.Model, "video"), req.Controls) != nil {
			return s.videoSettingsTariff(ctx, key, req)
		}
		err = fmt.Errorf("exact video tariff cell is not configured")
		return
	}
	rate, e := s.videoCustomerRate(ctx, key)
	if e != nil {
		err = e
		return
	}
	baseAmount, e := videoIntervalPrice(*selected)
	if e != nil {
		err = e
		return
	}
	totalAmount := baseAmount.Mul(decimal.NewFromFloat(rate)).Round(8)
	if baseAmount.GreaterThanOrEqual(decimal.NewFromInt(1e10)) || totalAmount.GreaterThanOrEqual(decimal.NewFromInt(1e10)) || (baseAmount.IsPositive() && rate > 0 && totalAmount.IsZero()) {
		err = fmt.Errorf("video total outside ledger range")
		return
	}
	base, total, multiplier = baseAmount.StringFixed(8), totalAmount.StringFixed(8), decimal.NewFromFloat(rate).String()
	metadata := card.VideoTariff.Cells[label]
	estimated = metadata.SupplierEstimated
	raw, _ := videoCanonical([]any{label, base, total, multiplier, card.VideoTariff.ContractRevision, metadata})
	hash := sha256.Sum256(raw)
	revision = hex.EncodeToString(hash[:])
	return
}

// Existing per-second customer prices can be locked without claiming that they
// are verified supplier invoices. Non-default cost settings still need a native
// tariff cell or verified calculator ratio; native cards never fall back to
// unrelated per-second rules.
func (s *OpenAIGatewayService) videoConfiguredRuleTariff(ctx context.Context, key *APIKey, req VideoQuoteRequest) (base, total, revision, multiplier string, estimated bool, err error) {
	if s == nil || s.billingService == nil || key == nil || key.Group == nil {
		err = ErrVideoQuoteUnavailable
		return
	}
	if err = videoLegacySettings(atlasMediaModel(req.Model, "video"), req.Controls); err != nil {
		return
	}
	resolution, _ := req.Controls["resolution"].(string)
	resolution = atlasMediaVideoResolutionOrDefault(req.Model, resolution)
	if !HasAtlasCloudVideoPrice(key, req.Model, resolution) {
		err = fmt.Errorf("video tariff not configured")
		return
	}
	unit := s.billingService.getVideoUnitPrice(req.Model, resolution, videoPriceConfigFromAPIKey(key))
	duration, e := decimal.NewFromString(fmt.Sprint(req.Controls["duration"]))
	if e != nil || !duration.IsPositive() || !duration.Equal(duration.Truncate(0)) || duration.GreaterThan(decimal.NewFromInt(120)) {
		err = fmt.Errorf("invalid native video duration")
		return
	}
	rate, e := s.videoCustomerRate(ctx, key)
	if e != nil {
		err = e
		return
	}
	baseAmount := decimal.NewFromFloat(unit).Mul(duration).Round(8)
	totalAmount := baseAmount.Mul(decimal.NewFromFloat(rate)).Round(8)
	if baseAmount.GreaterThanOrEqual(decimal.NewFromInt(1e10)) || totalAmount.GreaterThanOrEqual(decimal.NewFromInt(1e10)) ||
		(unit > 0 && baseAmount.IsZero()) || (baseAmount.IsPositive() && rate > 0 && totalAmount.IsZero()) {
		err = fmt.Errorf("video total outside ledger range")
		return
	}
	base, total, multiplier = baseAmount.StringFixed(8), totalAmount.StringFixed(8), decimal.NewFromFloat(rate).String()
	label, e := VideoTariffKey(req)
	if e != nil {
		err = e
		return
	}
	contract, e := VideoContractRevision(req.Model)
	if e != nil {
		err = e
		return
	}
	// Bind the source and unrounded unit as well as the exact native controls,
	// so even a price edit below display precision invalidates an unused quote.
	raw, _ := videoCanonical([]any{"configured-rule-v1", label, resolution, decimal.NewFromFloat(unit).String(), base, total, multiplier, contract})
	hash := sha256.Sum256(raw)
	revision = hex.EncodeToString(hash[:])
	estimated = true
	if supplier, e := s.videoCalculator.lookup(ctx, req); e == nil {
		estimated = supplier.Estimated
	}
	return
}

func (s *OpenAIGatewayService) videoCustomerRate(ctx context.Context, key *APIKey) (float64, error) {
	if key == nil || key.Group == nil {
		return 0, ErrVideoQuoteUnavailable
	}
	rate := key.Group.RateMultiplier
	// Bypass the rate cache, including its fail-open default, at financial admission.
	if s.userGroupRateResolver != nil && s.userGroupRateResolver.repo != nil {
		override, err := s.userGroupRateResolver.repo.GetByUserAndGroup(ctx, key.UserID, key.Group.ID)
		if err != nil {
			return 0, ErrVideoQuoteUnavailable
		}
		if override != nil {
			rate = *override
		}
	}
	if key.Group.VideoRateIndependent {
		rate = key.Group.VideoRateMultiplier
	}
	if math.IsNaN(rate) || math.IsInf(rate, 0) || rate < 0 {
		return 0, fmt.Errorf("invalid customer video multiplier")
	}
	return rate, nil
}

func (s *OpenAIGatewayService) videoLegacyEstimate(ctx context.Context, key *APIKey, req VideoQuoteRequest) (string, error) {
	if c := atlasMediaModel(req.Model, "video"); c != nil {
		if err := videoLegacySettings(c, req.Controls); err != nil {
			return "", err
		}
	}
	if s == nil || s.billingService == nil || key == nil || key.Group == nil {
		return "", ErrVideoQuoteUnavailable
	}
	resolution, _ := req.Controls["resolution"].(string)
	resolution = atlasMediaVideoResolutionOrDefault(req.Model, resolution)
	if !HasAtlasCloudVideoPrice(key, req.Model, resolution) {
		return "", fmt.Errorf("video tariff not configured")
	}
	durationValue := req.Controls["duration"]
	var duration decimal.Decimal
	switch value := durationValue.(type) {
	case float64:
		duration = decimal.NewFromFloat(value)
	case string:
		var err error
		duration, err = decimal.NewFromString(value)
		if err != nil {
			return "", fmt.Errorf("invalid native video duration")
		}
	default:
		return "", fmt.Errorf("invalid native video duration")
	}
	if !duration.IsPositive() || !duration.Equal(duration.Truncate(0)) {
		return "", fmt.Errorf("invalid native video duration")
	}
	rate, err := s.videoCustomerRate(ctx, key)
	if err != nil {
		return "", err
	}
	cost := s.billingService.CalculateVideoCost(req.Model, resolution, 1, int(duration.IntPart()), videoPriceConfigFromAPIKey(key), rate)
	if cost == nil || math.IsNaN(cost.ActualCost) || math.IsInf(cost.ActualCost, 0) || cost.ActualCost <= 0 {
		return "", fmt.Errorf("video tariff not configured")
	}
	return decimal.NewFromFloat(cost.ActualCost).Round(8).StringFixed(8), nil
}

func (s *OpenAIGatewayService) videoQuoteMAC(payload string) ([]byte, error) {
	if s.cfg == nil || len(s.cfg.JWT.Secret) < 32 {
		return nil, ErrVideoQuoteUnavailable
	}
	mac := hmac.New(sha256.New, []byte(s.cfg.JWT.Secret))
	_, _ = mac.Write([]byte(videoQuoteDomain + "\x00"))
	_, _ = mac.Write([]byte(payload))
	return mac.Sum(nil), nil
}
func (s *OpenAIGatewayService) QuoteVideo(ctx context.Context, key *APIKey, req VideoQuoteRequest, now time.Time) (*VideoQuote, error) {
	var err error
	key, err = s.RefreshVideoQuoteKey(ctx, key, req.Model, now)
	if err != nil {
		return nil, err
	}
	enrolled, err := s.VideoPricingEnrolled(key, req.Model)
	if err != nil {
		return nil, err
	}
	req, err = req.normalized()
	if err != nil {
		return nil, err
	}
	if !s.VideoQuotedBillingReady() {
		return nil, ErrVideoQuoteUnavailable
	}
	base, total, revision, multiplier, estimated, err := s.videoTariff(ctx, key, req)
	if err != nil {
		return nil, err
	}
	contract, _ := VideoContractRevision(req.Model)
	expires := now.Add(5 * time.Minute).UTC().Truncate(time.Second)
	claims := videoQuoteClaims{ID: uuid.NewString(), KeyID: key.ID, UserID: key.UserID, GroupID: key.Group.ID, Selection: req, BasePrice: base, TotalPrice: total, Multiplier: multiplier, TariffRevision: revision, ContractRevision: contract, SupplierEstimated: estimated, Expires: expires.Unix()}
	raw, err := videoCanonical(claims)
	if err != nil {
		return nil, err
	}
	payload := base64.RawURLEncoding.EncodeToString(raw)
	mac, err := s.videoQuoteMAC(payload)
	if err != nil {
		return nil, err
	}
	source := "configured_rule"
	if enrolled {
		source = "native_tariff"
	}
	if videoLegacySettings(atlasMediaModel(req.Model, "video"), req.Controls) != nil {
		source = "calculator_settings"
		if enrolled {
			label, _ := VideoTariffKey(req)
			for _, card := range key.Group.ModelPricing {
				if len(card.Models) == 1 && card.Models[0] == req.Model && card.VideoTariff != nil {
					if _, exact := card.VideoTariff.Cells[label]; exact {
						source = "native_tariff"
					}
				}
			}
		}
	}
	return &VideoQuote{Enrolled: true, VideoQuoteRequest: req, ID: claims.ID, Token: payload + "." + base64.RawURLEncoding.EncodeToString(mac), TariffRevision: revision, ContractRevision: contract, ExpiresAt: expires, Currency: "USD", BillingUnit: "job", UnitCount: 1, TotalPrice: total, PriceSource: source, SupplierEstimated: estimated}, nil
}

// AcceptVideoQuote verifies the actual native serialized payload. It does not
// reserve, consume a quote or submit; callers must durably accept before POST.
func (s *OpenAIGatewayService) AcceptVideoQuote(ctx context.Context, key *APIKey, body []byte, token string, now time.Time) (*VideoQuoteSnapshot, error) {
	if len(token) > 32768 {
		return nil, fmt.Errorf("invalid video quote")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return nil, fmt.Errorf("confirm a video price quote before generation")
	}
	want, err := s.videoQuoteMAC(parts[0])
	if err != nil {
		return nil, err
	}
	mac, err := base64.RawURLEncoding.Strict().DecodeString(parts[1])
	if err != nil || !hmac.Equal(mac, want) {
		return nil, fmt.Errorf("invalid video quote signature")
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(parts[0])
	var claims videoQuoteClaims
	if err != nil || json.Unmarshal(raw, &claims) != nil || claims.ID == "" || key == nil || key.GroupID == nil ||
		claims.KeyID != key.ID || claims.UserID != key.UserID || claims.GroupID != *key.GroupID || !now.Before(time.Unix(claims.Expires, 0)) {
		return nil, fmt.Errorf("video quote expired or belongs to a different caller")
	}
	if !s.VideoQuotedBillingReady() {
		return nil, ErrVideoQuoteUnavailable
	}
	key, err = s.RefreshVideoQuoteKey(ctx, key, claims.Selection.Model, now)
	if err != nil {
		return nil, err
	}
	c := atlasMediaModel(claims.Selection.Model, "video")
	info, serialized, err := buildAtlasCloudVideoPayload(body)
	if err != nil {
		return nil, err
	}
	var payload map[string]any
	if c == nil || info.Model != claims.Selection.Model || json.Unmarshal(serialized, &payload) != nil {
		return nil, fmt.Errorf("native video serialization differs from the quoted model")
	}
	actual := VideoQuoteRequest{Model: claims.Selection.Model, Mode: c.Mode, Controls: map[string]any{}}
	for field, value := range payload {
		if field == "model" || field == "prompt" || field == "negative_prompt" {
			continue
		}
		if _, media := c.MediaFields[field]; media {
			continue
		}
		actual.Controls[field] = value
	}
	actualKey, err := VideoTariffKey(actual)
	quotedKey, qerr := VideoTariffKey(claims.Selection)
	if err != nil || qerr != nil || actualKey != quotedKey {
		return nil, fmt.Errorf("native video body differs from quoted controls")
	}
	base, total, revision, multiplier, estimated, err := s.videoTariff(ctx, key, actual)
	contract, _ := VideoContractRevision(actual.Model)
	if err != nil || base != claims.BasePrice || total != claims.TotalPrice || revision != claims.TariffRevision || multiplier != claims.Multiplier || estimated != claims.SupplierEstimated || contract != claims.ContractRevision {
		return nil, fmt.Errorf("video tariff changed; refresh and confirm the new price")
	}
	return &VideoQuoteSnapshot{claims: claims}, nil
}
