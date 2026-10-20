package service

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

const miniQualityModel = "openai/gpt-image-1-mini"

type imageQualityContextKey struct{}

func WithImageQualityPricing(ctx context.Context, snapshot *ImageQualityPriceSnapshot) context.Context {
	return context.WithValue(ctx, imageQualityContextKey{}, snapshot)
}

func (s *ImageQualityPriceSnapshot) validateUpstream(parsed *OpenAIImagesRequest, upstream string) error {
	if parsed == nil || upstream != s.claims.Request.Model || parsed.Model != upstream {
		return fmt.Errorf("quoted images require the exact selected upstream model")
	}
	req, err := (ImageQuoteRequest{Model: parsed.Model, Quality: parsed.Quality, Size: parsed.Size, Count: s.claims.Request.Count}).normalized()
	if err != nil || req != s.claims.Request || parsed.N != 1 || parsed.Stream || parsed.Multipart {
		return fmt.Errorf("upstream controls differ from the accepted image quote")
	}
	_, err = buildAtlasCloudImagePayload(parsed, upstream)
	return err
}

func miniQualityEndpoint(model string) bool {
	switch model {
	case miniQualityModel + "/text-to-image", miniQualityModel + "/edit":
		return true
	}
	return false
}

func imageQualityEndpoint(model string) bool {
	return strings.HasPrefix(model, "openai/") && atlasMediaModel(model, "image") != nil
}

func imageQualityCanonicalModel(model string) string {
	return strings.TrimSuffix(strings.TrimSuffix(model, "/text-to-image"), "/edit")
}

type ImageQuoteRequest struct {
	Model   string `json:"model"`
	Quality string `json:"quality"`
	Size    string `json:"size"`
	Count   int    `json:"count"`
}

func (r ImageQuoteRequest) normalized() (ImageQuoteRequest, error) {
	if !imageQualityEndpoint(r.Model) {
		return r, fmt.Errorf("fixed quality quotes are not supported for this model")
	}
	properties := atlasMediaModel(r.Model, "image").Input.Properties
	if r.Quality == "" {
		r.Quality, _ = properties["quality"].defaultString()
		if properties["quality"] == nil {
			r.Quality = "none"
		}
	}
	if r.Size == "" {
		r.Size, _ = properties["size"].defaultString()
	}
	if properties["quality"] == nil && r.Quality != "none" {
		return r, fmt.Errorf("unsupported image quality")
	}
	if properties["quality"] != nil && validateAtlasMediaValue(r.Quality, properties["quality"], "quality", true) != nil {
		return r, fmt.Errorf("unsupported image quality")
	}
	if properties["size"] == nil || validateAtlasMediaValue(r.Size, properties["size"], "size", true) != nil {
		return r, fmt.Errorf("unsupported image size")
	}
	if r.Count < 1 || r.Count > 4 {
		return r, fmt.Errorf("image count must be between 1 and 4")
	}
	return r, nil
}

type ImageQualityQuote struct {
	Enrolled bool `json:"enrolled"`
	ImageQuoteRequest
	ID        string    `json:"quote_id,omitempty"`
	Token     string    `json:"quote_token,omitempty"`
	Revision  string    `json:"tariff_revision,omitempty"`
	Currency  string    `json:"currency,omitempty"`
	UnitPrice string    `json:"unit_price,omitempty"`
	Total     string    `json:"total,omitempty"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
}

type imageQuoteClaims struct {
	ID         string            `json:"id"`
	KeyID      int64             `json:"key"`
	UserID     int64             `json:"user"`
	GroupID    int64             `json:"group"`
	Request    ImageQuoteRequest `json:"request"`
	Revision   string            `json:"revision"`
	BasePrice  string            `json:"base"`
	UnitPrice  string            `json:"unit"`
	Multiplier float64           `json:"multiplier"`
	Expires    int64             `json:"expires"`
}

// Snapshot is only constructed after server-side verification; never deserialize it from a client.
type ImageQualityPriceSnapshot struct {
	claims  imageQuoteClaims
	index   int
	attempt *imageQualityAttempt
}

type ImageChargeReceipt struct {
	QuoteID  string `json:"quote_id"`
	Child    int    `json:"child_index"`
	Model    string `json:"model"`
	Quality  string `json:"quality"`
	Currency string `json:"currency"`
	Charged  string `json:"charged"`
	Status   string `json:"status"`
	Revision string `json:"tariff_revision"`
}

func (s *ImageQualityPriceSnapshot) Receipt() ImageChargeReceipt {
	return ImageChargeReceipt{s.claims.ID, s.index, imageQualityCanonicalModel(s.claims.Request.Model), s.claims.Request.Quality,
		"USD", s.claims.UnitPrice, "settled", s.claims.Revision}
}

func (s *ImageQualityPriceSnapshot) RequestID() string {
	return "image-quote:" + s.claims.ID + ":" + strconv.Itoa(s.index)
}

func (s *ImageQualityPriceSnapshot) Amount() float64 {
	v, _ := decimal.RequireFromString(s.claims.UnitPrice).Float64()
	return v
}

func (s *ImageQualityPriceSnapshot) cost(count int) *CostBreakdown {
	base, _ := decimal.RequireFromString(s.claims.BasePrice).Mul(decimal.NewFromInt(int64(count))).Float64()
	actual, _ := decimal.RequireFromString(s.claims.UnitPrice).Mul(decimal.NewFromInt(int64(count))).Float64()
	return &CostBreakdown{ImageOutputCost: base, TotalCost: base, ActualCost: actual, BillingMode: string(BillingModeImage)}
}

func (s *ImageQualityPriceSnapshot) validateResult(result *OpenAIForwardResult) error {
	if result == nil || result.ImageCount != 1 || result.Model != s.claims.Request.Model ||
		(result.UpstreamModel != "" && result.UpstreamModel != s.claims.Request.Model) {
		return fmt.Errorf("quoted image result does not match accepted model and count")
	}
	return nil
}

func imageQualityCard(key *APIKey, model string) (*ChannelModelPricing, error) {
	if key == nil || key.Group == nil {
		return nil, nil
	}
	var found *ChannelModelPricing
	for i := range key.Group.ModelPricing {
		card := &key.Group.ModelPricing[i]
		enrolled := false
		for _, iv := range card.Intervals {
			enrolled = enrolled || strings.HasPrefix(iv.TierLabel, "quality:")
		}
		if !enrolled {
			continue
		}
		matches := false
		for _, candidate := range card.Models {
			// Preserve the original Mini shared generation/edit card. New endpoints
			// are exact matches because editing may have a different upstream price.
			matches = matches || candidate == model || (miniQualityEndpoint(model) && miniQualityEndpoint(candidate))
		}
		if !matches {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("image endpoint has overlapping quality price cards")
		}
		if err := validateImageQualityCard(*card); err != nil {
			return nil, err
		}
		found = card
	}
	return found, nil
}

func imageQualityTier(label string) (quality, size string, valid bool) {
	parts := strings.Split(label, ":")
	if (len(parts) != 2 && len(parts) != 4) || parts[0] != "quality" || parts[1] == "" {
		return "", "", false
	}
	if len(parts) == 4 {
		if parts[2] != "size" || parts[3] == "" {
			return "", "", false
		}
		size = parts[3]
	}
	return parts[1], size, true
}

func validateImageQualityCard(card ChannelModelPricing) error {
	if card.BillingMode != BillingModeImage || card.Platform != PlatformOpenAI || card.PerRequestPrice != nil ||
		len(card.ReasoningEffortMultipliers) != 0 || card.TimePricing != nil {
		return fmt.Errorf("image quality tariffs require image billing without fallback prices or extra multipliers")
	}
	if len(card.Models) == 0 {
		return fmt.Errorf("an image quality price card requires an exact model")
	}
	for _, model := range card.Models {
		if !imageQualityEndpoint(model) || imageQualityCanonicalModel(model) != imageQualityCanonicalModel(card.Models[0]) {
			return fmt.Errorf("an image quality price card cannot include other model families or wildcards")
		}
	}
	seen := map[string]bool{}
	var sized bool
	for i, iv := range card.Intervals {
		quality, size, valid := imageQualityTier(iv.TierLabel)
		if !valid {
			return fmt.Errorf("image quality tariff contains an unsupported size or quality")
		}
		for _, model := range card.Models {
			if size == "" && !miniQualityEndpoint(model) {
				return fmt.Errorf("non-Mini image tariffs require an exact native size")
			}
			if _, err := (ImageQuoteRequest{Model: model, Quality: quality, Size: size, Count: 1}).normalized(); err != nil {
				return fmt.Errorf("image quality tariff contains an unsupported size or quality: %w", err)
			}
		}
		if i == 0 {
			sized = size != ""
		} else if sized != (size != "") {
			return fmt.Errorf("image quality tariffs cannot mix flat and size-specific prices")
		}
		if seen[iv.TierLabel] || !validAtlasCloudImagePrice(iv.PerRequestPrice) {
			return fmt.Errorf("image quality tariff must have unique, explicitly configured finite prices")
		}
		if decimal.NewFromFloat(*iv.PerRequestPrice).Exponent() < -UsageBillingMonetaryScale {
			return fmt.Errorf("image tariffs support at most eight decimal places")
		}
		if *iv.PerRequestPrice >= 1e12 {
			return fmt.Errorf("image tariff exceeds the ledger amount limit")
		}
		seen[iv.TierLabel] = true
	}
	return nil
}

func (s *OpenAIGatewayService) ImageQualityPricingEnrolled(key *APIKey, model string) (bool, error) {
	if !imageQualityEndpoint(model) {
		return false, nil
	}
	card, err := imageQualityCard(key, model)
	return card != nil, err
}

// Always reload pilot tariff configuration at admission; cached auth snapshots may predate an admin edit.
func (s *OpenAIGatewayService) RefreshImageQualityKey(ctx context.Context, key *APIKey, model string) (*APIKey, error) {
	if !imageQualityEndpoint(model) || key == nil || key.GroupID == nil || s.channelService == nil || s.channelService.groupRepo == nil {
		return key, nil
	}
	group, err := s.channelService.groupRepo.GetByIDLite(ctx, *key.GroupID)
	if err != nil || group == nil {
		return nil, fmt.Errorf("image tariff configuration is unavailable")
	}
	fresh := *key
	fresh.Group = group
	return &fresh, nil
}

func (s *OpenAIGatewayService) ImageQualitySettlementReady() bool {
	return s.usageBillingRepo != nil && s.billingService != nil
}

func (s *OpenAIGatewayService) imageQualityTariff(ctx context.Context, key *APIKey, req ImageQuoteRequest) (base, unit, revision string, multiplier float64, err error) {
	card, err := imageQualityCard(key, req.Model)
	if err != nil || card == nil {
		return "", "", "", 0, fmt.Errorf("image quality tariff is not configured")
	}
	var price *float64
	label := "quality:" + req.Quality
	if len(card.Intervals) > 0 {
		if _, size, _ := imageQualityTier(card.Intervals[0].TierLabel); size != "" {
			label += ":size:" + req.Size
		}
	}
	for _, iv := range card.Intervals {
		if iv.TierLabel == label {
			price = iv.PerRequestPrice
		}
	}
	if !validAtlasCloudImagePrice(price) {
		return "", "", "", 0, fmt.Errorf("configure the selected image size and quality price before generation")
	}
	rate := key.Group.RateMultiplier
	if s.userGroupRateResolver != nil {
		rate = s.ResolveUserGroupRateMultiplier(ctx, key.UserID, key.Group.ID, rate)
	}
	_, multiplier = computePeakAwareMultipliers(key, rate, time.Now())
	if multiplier < 0 || math.IsNaN(multiplier) || math.IsInf(multiplier, 0) {
		return "", "", "", 0, fmt.Errorf("invalid customer image multiplier")
	}
	baseAmount := decimal.NewFromFloat(*price)
	unitAmount := baseAmount.Mul(decimal.NewFromFloat(multiplier)).Round(8)
	ledgerAmount, _ := unitAmount.Float64()
	limit := decimal.NewFromInt(1e12)
	if unitAmount.GreaterThanOrEqual(limit) ||
		unitAmount.Mul(decimal.NewFromInt(int64(req.Count))).GreaterThanOrEqual(limit) ||
		(baseAmount.IsPositive() && multiplier > 0 && unitAmount.IsZero()) ||
		!decimal.NewFromFloat(ledgerAmount).Round(8).Equal(unitAmount) {
		return "", "", "", 0, fmt.Errorf("image price is outside the ledger amount precision or range")
	}
	base = baseAmount.StringFixed(8)
	unit = unitAmount.StringFixed(8)
	// Size-specific cards bind their exact cell; legacy flat-quality cards stay compatible.
	tier := strings.TrimPrefix(label, "quality:")
	hash := sha256.Sum256([]byte(req.Model + "|" + tier + "|" + base + "|" + strconv.FormatFloat(multiplier, 'g', -1, 64)))
	revision = hex.EncodeToString(hash[:])
	return base, unit, revision, multiplier, nil
}

func (s *OpenAIGatewayService) quoteMAC(payload string) ([]byte, error) {
	if s.cfg == nil || len(s.cfg.JWT.Secret) < 32 {
		return nil, fmt.Errorf("image quote signing is unavailable")
	}
	mac := hmac.New(sha256.New, []byte(s.cfg.JWT.Secret))
	mac.Write([]byte("image-quality-quote-v1\x00"))
	mac.Write([]byte(payload))
	return mac.Sum(nil), nil
}

func (s *OpenAIGatewayService) QuoteImageQuality(ctx context.Context, key *APIKey, req ImageQuoteRequest, now time.Time) (*ImageQualityQuote, error) {
	key, err := s.RefreshImageQualityKey(ctx, key, req.Model)
	if err != nil {
		return nil, err
	}
	enrolled, err := s.ImageQualityPricingEnrolled(key, req.Model)
	if err != nil {
		return nil, err
	}
	if !enrolled {
		return &ImageQualityQuote{Enrolled: false}, nil
	}
	req, err = req.normalized()
	if err != nil {
		return nil, err
	}
	base, unit, revision, rate, err := s.imageQualityTariff(ctx, key, req)
	if err != nil {
		return nil, err
	}
	expires := now.Add(5 * time.Minute).Truncate(time.Second)
	claims := imageQuoteClaims{uuid.NewString(), key.ID, key.UserID, key.Group.ID, req, revision, base, unit, rate, expires.Unix()}
	body, err := json.Marshal(claims)
	if err != nil {
		return nil, err
	}
	payload := base64.RawURLEncoding.EncodeToString(body)
	mac, err := s.quoteMAC(payload)
	if err != nil {
		return nil, err
	}
	total := decimal.RequireFromString(unit).Mul(decimal.NewFromInt(int64(req.Count))).StringFixed(8)
	return &ImageQualityQuote{Enrolled: true, ImageQuoteRequest: req, ID: claims.ID,
		Token: payload + "." + base64.RawURLEncoding.EncodeToString(mac), Revision: revision,
		Currency: "USD", UnitPrice: unit, Total: total, ExpiresAt: expires}, nil
}

func (s *OpenAIGatewayService) AcceptImageQualityQuote(ctx context.Context, key *APIKey, parsed *OpenAIImagesRequest, token string, index int, now time.Time) (*ImageQualityPriceSnapshot, error) {
	if len(token) > 4096 {
		return nil, fmt.Errorf("invalid image quote")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return nil, fmt.Errorf("refresh and confirm an image price quote before generation")
	}
	want, err := s.quoteMAC(parts[0])
	if err != nil {
		return nil, err
	}
	got, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || !hmac.Equal(want, got) {
		return nil, fmt.Errorf("invalid image quote signature")
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[0])
	var claims imageQuoteClaims
	if err != nil || json.Unmarshal(body, &claims) != nil || key == nil || key.Group == nil || parsed == nil ||
		claims.KeyID != key.ID || claims.UserID != key.UserID || claims.GroupID != key.Group.ID || !now.Before(time.Unix(claims.Expires, 0)) {
		return nil, fmt.Errorf("image quote expired or belongs to a different caller; refresh and confirm the price")
	}
	req, err := (ImageQuoteRequest{Model: parsed.Model, Quality: parsed.Quality, Size: parsed.Size, Count: claims.Request.Count}).normalized()
	if err != nil || req != claims.Request || parsed.N != 1 || parsed.Stream || parsed.Multipart || index < 0 || index >= req.Count {
		return nil, fmt.Errorf("image request differs from the quoted selection")
	}
	_, _, revision, _, err := s.imageQualityTariff(ctx, key, req)
	if err != nil || revision != claims.Revision {
		return nil, fmt.Errorf("image tariff changed; refresh and confirm the new price")
	}
	return &ImageQualityPriceSnapshot{claims: claims, index: index}, nil
}

// Never reclaim or retry a submitted child: a provider timeout is an unknown outcome.
func (s *ImageQualityPriceSnapshot) claim(ctx context.Context, repo IdempotencyRepository, bodyHash string, now time.Time) (int64, error) {
	if repo == nil {
		return 0, ErrIdempotencyStoreUnavail
	}
	if _, ok := repo.(ImageQualityClaimStore); !ok {
		return 0, ErrIdempotencyStoreUnavail
	}
	hash := sha256.Sum256([]byte(s.RequestID()))
	expiry := now.Add(24 * time.Hour)
	outcome := ImageQualityOutcome{QuoteID: s.claims.ID, Child: s.index, Status: "accepted", BillingStatus: "not_charged"}
	auditBody, err := s.auditBody(outcome, nil)
	if err != nil {
		return 0, err
	}
	record := &IdempotencyRecord{Scope: "image-quality-quote-v1", IdempotencyKeyHash: hex.EncodeToString(hash[:]),
		RequestFingerprint: bodyHash, Status: IdempotencyStatusProcessing, LockedUntil: &expiry, ExpiresAt: expiry,
		ResponseBody: &auditBody}
	owner, err := repo.CreateProcessing(ctx, record)
	if err != nil {
		return 0, ErrIdempotencyStoreUnavail
	}
	if !owner {
		return 0, ErrIdempotencyInProgress
	}
	if record.ID == 0 {
		return 0, ErrIdempotencyStoreUnavail
	}
	s.attempt = &imageQualityAttempt{repo: repo, id: record.ID, outcome: outcome}
	return record.ID, nil
}

func (s *ImageQualityPriceSnapshot) Claim(ctx context.Context, bodyHash string) (int64, error) {
	coordinator := DefaultIdempotencyCoordinator()
	if coordinator == nil {
		return 0, ErrIdempotencyStoreUnavail
	}
	return s.claim(ctx, coordinator.repo, bodyHash, time.Now())
}
