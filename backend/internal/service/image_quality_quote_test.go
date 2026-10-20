//go:build unit

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/stretchr/testify/require"
)

func miniQuoteFixture() (*OpenAIGatewayService, *APIKey) {
	prices := []float64{0.02, 0.04, 0.08} // Test tariffs, not recommended selling prices.
	g := &Group{ID: 7, RateMultiplier: 1.5}
	g.ModelPricing = []ChannelModelPricing{{Platform: PlatformOpenAI,
		Models: []string{"openai/gpt-image-1-mini/text-to-image"}, BillingMode: BillingModeImage}}
	for i, q := range []string{"low", "medium", "high"} {
		g.ModelPricing[0].Intervals = append(g.ModelPricing[0].Intervals,
			PricingInterval{TierLabel: "quality:" + q, PerRequestPrice: &prices[i]})
	}
	s := &OpenAIGatewayService{cfg: &config.Config{JWT: config.JWTConfig{Secret: strings.Repeat("test", 12)}}}
	return s, &APIKey{ID: 11, UserID: 3, User: &User{ID: 3}, GroupID: &g.ID, Group: g}
}

func TestImageQualityQuoteRatioInvariant(t *testing.T) {
	s, key := miniQuoteFixture()
	for _, model := range []string{"openai/gpt-image-1-mini/text-to-image", "openai/gpt-image-1-mini/edit"} {
		for i, quality := range []string{"low", "medium", "high"} {
			for _, size := range []string{"1024x1024", "1024x1536", "1536x1024"} {
				q, err := s.QuoteImageQuality(context.Background(), key, ImageQuoteRequest{Model: model, Quality: quality, Size: size, Count: 3}, time.Now())
				require.NoError(t, err)
				require.True(t, q.Enrolled)
				require.Equal(t, []string{"0.03000000", "0.06000000", "0.12000000"}[i], q.UnitPrice)
				require.Equal(t, []string{"0.09000000", "0.18000000", "0.36000000"}[i], q.Total)
				require.Equal(t, "USD", q.Currency)
			}
		}
	}
}

func TestImageQualityQuoteRejectsInvalidOrMissingTariffs(t *testing.T) {
	for _, price := range []*float64{nil, quoteFloat(-1), quoteFloat(math.NaN()), quoteFloat(math.Inf(1))} {
		s, key := miniQuoteFixture()
		key.Group.ModelPricing[0].Intervals[1].PerRequestPrice = price
		_, err := s.QuoteImageQuality(context.Background(), key, ImageQuoteRequest{Model: "openai/gpt-image-1-mini/text-to-image", Quality: "medium", Count: 1}, time.Now())
		require.Error(t, err)
	}
	s, key := miniQuoteFixture()
	key.Group.ModelPricing[0].Intervals[1].PerRequestPrice = quoteFloat(0)
	q, err := s.QuoteImageQuality(context.Background(), key, ImageQuoteRequest{Model: "openai/gpt-image-1-mini/text-to-image", Count: 1}, time.Now())
	require.NoError(t, err)
	require.Equal(t, "0.00000000", q.Total)
	key.Group.ModelPricing = nil
	q, err = s.QuoteImageQuality(context.Background(), key, ImageQuoteRequest{Model: "openai/gpt-image-1-mini/text-to-image", Count: 1}, time.Now())
	require.NoError(t, err)
	require.False(t, q.Enrolled, "no quality enrollment must preserve legacy models")
}

func quoteFloat(v float64) *float64 { return &v }

func TestImageQualityQuoteBindingAndSettlement(t *testing.T) {
	s, key := miniQuoteFixture()
	now := time.Now()
	req := ImageQuoteRequest{Model: "openai/gpt-image-1-mini/text-to-image", Quality: "medium", Size: "1024x1536", Count: 2}
	q, err := s.QuoteImageQuality(context.Background(), key, req, now)
	require.NoError(t, err)
	parsed := &OpenAIImagesRequest{Model: req.Model, Quality: req.Quality, Size: req.Size, N: 1}
	snapshot, err := s.AcceptImageQualityQuote(context.Background(), key, parsed, q.Token, 1, now)
	require.NoError(t, err)
	require.Equal(t, "0.06000000", snapshot.Receipt().Charged)
	require.Equal(t, "settled", snapshot.Receipt().Status)
	require.Equal(t, 0.06, snapshot.cost(1).ActualCost)
	require.Equal(t, 0.04, snapshot.cost(1).TotalCost)
	require.Equal(t, "image", snapshot.cost(1).BillingMode)
	require.Error(t, snapshot.validateResult(&OpenAIForwardResult{ImageCount: 2, Model: req.Model}))
	require.NoError(t, snapshot.validateResult(&OpenAIForwardResult{ImageCount: 1, Model: req.Model, UpstreamModel: req.Model}))
	for _, mutate := range []func(){
		func() { key.ID++ },
		func() { key.UserID++ },
		func() { key.Group.ID++ },
		func() { parsed.Size = "1024x1024" },
		func() { parsed.Quality = "high" },
		func() { parsed.Model = "openai/gpt-image-1-mini/edit" },
	} {
		s, key = miniQuoteFixture()
		parsed = &OpenAIImagesRequest{Model: req.Model, Quality: req.Quality, Size: req.Size, N: 1}
		mutate()
		_, err := s.AcceptImageQualityQuote(context.Background(), key, parsed, q.Token, 1, now)
		require.Error(t, err)
	}
	s, key = miniQuoteFixture()
	parsed = &OpenAIImagesRequest{Model: req.Model, Quality: req.Quality, Size: req.Size, N: 1}
	_, err = s.AcceptImageQualityQuote(context.Background(), key, parsed, q.Token, 2, now)
	require.Error(t, err)
	_, err = s.AcceptImageQualityQuote(context.Background(), key, parsed, q.Token+"x", 0, now)
	require.Error(t, err)
	_, err = s.AcceptImageQualityQuote(context.Background(), key, parsed, q.Token, 0, now.Add(6*time.Minute))
	require.Error(t, err)
	key.Group.ModelPricing[0].Intervals[1].PerRequestPrice = quoteFloat(9)
	_, err = s.AcceptImageQualityQuote(context.Background(), key, parsed, q.Token, 0, now)
	require.Error(t, err, "price edits invalidate unaccepted quotes")
	require.Equal(t, 0.06, snapshot.cost(1).ActualCost, "accepted snapshot remains locked")
}

func TestImageQualityQuoteClaimOnlyOnce(t *testing.T) {
	s, key := miniQuoteFixture()
	now := time.Now()
	q, err := s.QuoteImageQuality(context.Background(), key, ImageQuoteRequest{Model: "openai/gpt-image-1-mini/text-to-image", Count: 1}, now)
	require.NoError(t, err)
	snapshot, err := s.AcceptImageQualityQuote(context.Background(), key, &OpenAIImagesRequest{Model: q.Model, N: 1}, q.Token, 0, now)
	require.NoError(t, err)
	repo := newInMemoryIdempotencyRepo()
	var winners atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := snapshot.claim(context.Background(), repo, "body-hash", now); err == nil {
				winners.Add(1)
			}
		}()
	}
	wg.Wait()
	require.EqualValues(t, 1, winners.Load())
	record, err := repo.GetByScopeAndKeyHash(context.Background(), "image-quality-quote-v1", HashIdempotencyKey(snapshot.RequestID()))
	require.NoError(t, err)
	require.NotNil(t, record.ResponseBody, "persist the accepted tariff before any supplier request")
	require.Contains(t, *record.ResponseBody, `"unit":"0.06000000"`)
	require.Contains(t, *record.ResponseBody, q.Revision)
	_, err = snapshot.claim(context.Background(), nil, "body-hash", now)
	require.Error(t, err)
}

func TestImageQualityQuoteSettlementCannotFallBackToSizeOrTokens(t *testing.T) {
	s, key := miniQuoteFixture()
	now := time.Now()
	q, err := s.QuoteImageQuality(context.Background(), key, ImageQuoteRequest{Model: "openai/gpt-image-1-mini/text-to-image", Count: 1}, now)
	require.NoError(t, err)
	snapshot, err := s.AcceptImageQualityQuote(context.Background(), key, &OpenAIImagesRequest{Model: q.Model, N: 1}, q.Token, 0, now)
	require.NoError(t, err)
	result := &OpenAIForwardResult{Model: q.Model, ImageCount: 1, ImageQualityPricing: snapshot}
	key.Group.ModelPricing[0].Intervals[1].PerRequestPrice = quoteFloat(99)
	cost, err := s.calculateOpenAIRecordUsageCost(context.Background(), result, key, []string{q.Model}, 8, 9, 1, 1, UsageTokens{}, "", nil, false, now)
	require.NoError(t, err)
	require.Equal(t, 0.06, cost.ActualCost)
	result.ImageQualityPricing = nil
	_, err = s.calculateOpenAIRecordUsageCost(context.Background(), result, key, []string{q.Model}, 8, 9, 1, 1, UsageTokens{}, "", nil, false, now)
	require.Error(t, err)
}

func TestImageQualityQuoteStrictReservation(t *testing.T) {
	ctx := context.Background()
	cache := &fixedChargeCacheStub{balance: 1}
	cfg := &config.Config{}
	cfg.Billing.InflightReservation.Enabled = true
	svc := NewBillingCacheService(cache, &imageQuoteBalanceRepo{cache: cache}, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(svc.Stop)
	user, key := &User{ID: 1}, &APIKey{ID: 2, Quota: 0.9}
	first, err := svc.ReserveQuotedImageCharge(ctx, user, key, nil, nil, 0.6)
	require.NoError(t, err)
	require.NotNil(t, first)
	_, err = svc.ReserveQuotedImageCharge(ctx, user, key, nil, nil, 0.5)
	require.Error(t, err, "inflight costs must consume balance/key quota headroom")
	first.HandlerDone()
	require.Empty(t, cache.reserved["user:1"])
	require.Empty(t, cache.reserved["apikey:2"])
	_, err = svc.ReserveQuotedImageCharge(ctx, user, key, nil, nil, 1.1)
	require.Error(t, err)
	cfg.Billing.InflightReservation.Enabled = false
	_, err = svc.ReserveQuotedImageCharge(ctx, user, key, nil, nil, 0.1)
	require.Error(t, err, "quoted charges must not silently use fail-open reservation")
	svc.cache = nil
	_, err = svc.ReserveQuotedImageCharge(ctx, user, key, nil, nil, 0.1)
	require.Error(t, err)
}

type imageQuoteBalanceRepo struct {
	UserRepository
	cache BillingCache
}

func (r *imageQuoteBalanceRepo) GetByID(ctx context.Context, id int64) (*User, error) {
	balance, err := r.cache.GetUserBalance(ctx, id)
	return &User{ID: id, Balance: balance}, err
}

func TestImageQualityQuoteAdmissionBypassesStaleBalanceCache(t *testing.T) {
	cache := &fixedChargeCacheStub{balance: 1}
	authoritative := &fixedChargeCacheStub{balance: 0.4}
	cfg := &config.Config{}
	cfg.Billing.InflightReservation.Enabled = true
	svc := NewBillingCacheService(cache, &imageQuoteBalanceRepo{cache: authoritative}, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(svc.Stop)
	r, err := svc.ReserveQuotedImageCharge(context.Background(), &User{ID: 8}, nil, nil, nil, 0.6)
	if r != nil {
		r.HandlerDone()
	}
	require.ErrorIs(t, err, ErrInsufficientBalance, "a stale higher Redis balance cannot authorize a new fixed-price job")
	require.Empty(t, cache.reserved["user:8"])
}

// A thread-safe fake of the cache's atomic reservation boundary.
type concurrentImageQuoteCache struct {
	fixedChargeCacheStub
	mu sync.Mutex
}

func (s *concurrentImageQuoteCache) ReserveInflightBalance(ctx context.Context, userID int64, id string, amount, balance float64, ttl time.Duration) (bool, float64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fixedChargeCacheStub.ReserveInflightBalance(ctx, userID, id, amount, balance, ttl)
}
func (s *concurrentImageQuoteCache) ReleaseInflightBalance(ctx context.Context, userID int64, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fixedChargeCacheStub.ReleaseInflightBalance(ctx, userID, id)
}
func (s *concurrentImageQuoteCache) ReserveInflightScope(ctx context.Context, scope, id string, amount float64, ttl time.Duration) (float64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fixedChargeCacheStub.ReserveInflightScope(ctx, scope, id, amount, ttl)
}
func (s *concurrentImageQuoteCache) ReleaseInflightScope(ctx context.Context, scope, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fixedChargeCacheStub.ReleaseInflightScope(ctx, scope, id)
}

func (s *fixedChargeCacheStub) RegisterInflightBalance(_ context.Context, userID int64, id string, amount float64, _ time.Duration) (float64, error) {
	return s.reserve(fmt.Sprintf("user:%d", userID), id, amount), nil
}
func (s *concurrentImageQuoteCache) RegisterInflightBalance(ctx context.Context, userID int64, id string, amount float64, ttl time.Duration) (float64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fixedChargeCacheStub.RegisterInflightBalance(ctx, userID, id, amount, ttl)
}

type interleavedImageBalanceCache struct {
	concurrentImageQuoteCache
	firstRead atomic.Bool
	read      chan struct{}
	resume    chan struct{}
}

func (s *interleavedImageBalanceCache) GetUserBalance(context.Context, int64) (float64, error) {
	s.mu.Lock()
	balance := s.balance
	s.mu.Unlock()
	if s.firstRead.CompareAndSwap(false, true) {
		close(s.read)
		<-s.resume
	}
	return balance, nil
}

func TestImageQualityQuoteBalanceSettlementReleaseInterleaving(t *testing.T) {
	cache := &interleavedImageBalanceCache{read: make(chan struct{}), resume: make(chan struct{})}
	cache.balance = 1
	cfg := &config.Config{}
	cfg.Billing.InflightReservation.Enabled = true
	svc := NewBillingCacheService(cache, &imageQuoteBalanceRepo{cache: cache}, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(svc.Stop)
	var held *InflightReservation
	var heldErr error
	done := make(chan struct{})
	go func() {
		held, heldErr = svc.ReserveQuotedImageCharge(context.Background(), &User{ID: 8}, nil, nil, nil, 0.6)
		close(done)
	}()
	<-cache.read
	fast, fastErr := svc.ReserveQuotedImageCharge(context.Background(), &User{ID: 8}, nil, nil, nil, 0.6)
	if fastErr == nil {
		cache.mu.Lock()
		cache.balance -= 0.6 // Settlement updates balance BEFORE releasing its reservation.
		cache.mu.Unlock()
		fast.HandlerDone()
	}
	close(cache.resume)
	<-done
	winners := 0
	if fastErr == nil {
		winners++
	}
	if heldErr == nil {
		winners++
		held.HandlerDone()
	}
	require.Equal(t, 1, winners, "a stale pre-settlement read must not admit a second paid job after release")
	require.Empty(t, cache.reserved["user:8"])
}

func TestImageQualityQuoteConcurrentReservationHeadroom(t *testing.T) {
	for _, scope := range []string{"balance", "api key quota"} {
		t.Run(scope, func(t *testing.T) {
			cache := &concurrentImageQuoteCache{fixedChargeCacheStub: fixedChargeCacheStub{balance: 1}}
			key := &APIKey{ID: 9}
			if scope == "api key quota" {
				cache.balance, key.Quota = 100, 1
			}
			cfg := &config.Config{}
			cfg.Billing.InflightReservation.Enabled = true
			svc := NewBillingCacheService(cache, &imageQuoteBalanceRepo{cache: cache}, nil, nil, nil, nil, cfg, nil)
			t.Cleanup(svc.Stop)
			accepted := make(chan *InflightReservation, 24)
			var wg sync.WaitGroup
			for i := 0; i < 24; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					r, err := svc.ReserveQuotedImageCharge(context.Background(), &User{ID: 8}, key, nil, nil, 0.6)
					if err == nil {
						accepted <- r
					}
				}()
			}
			wg.Wait()
			close(accepted)
			require.Len(t, accepted, 1, "concurrent distinct requests must not overdraw shared headroom")
			for r := range accepted {
				r.HandlerDone()
			}
			require.Empty(t, cache.reserved["user:8"])
			require.Empty(t, cache.reserved["apikey:9"])
		})
	}
}

func TestImageQualityQuoteAdminValidation(t *testing.T) {
	for _, name := range []string{"duplicate", "mixed model", "wildcard", "two cards", "nonfinite", "precision", "fallback"} {
		t.Run(name, func(t *testing.T) {
			_, key := miniQuoteFixture()
			cards := key.Group.ModelPricing
			switch name {
			case "duplicate":
				cards[0].Intervals = append(cards[0].Intervals, cards[0].Intervals[0])
			case "mixed model":
				cards[0].Models = append(cards[0].Models, "another-model")
			case "wildcard":
				cards[0].Models = []string{"openai/gpt-image-1-mini/*"}
			case "two cards":
				cards = append(cards, cards[0].Clone())
				cards[1].Models = []string{"openai/gpt-image-1-mini/edit"}
			case "nonfinite":
				cards[0].Intervals[0].PerRequestPrice = quoteFloat(math.Inf(1))
			case "precision":
				cards[0].Intervals[0].PerRequestPrice = quoteFloat(0.123456789)
			case "fallback":
				cards[0].PerRequestPrice = quoteFloat(0)
			}
			_, err := normalizeGroupModelPricing(PlatformOpenAI, cards)
			require.Error(t, err, "invalid quality card must fail before persistence")
		})
	}
	_, key := miniQuoteFixture()
	normalized, err := normalizeGroupModelPricing(PlatformOpenAI, key.Group.ModelPricing)
	require.NoError(t, err)
	require.Len(t, normalized[0].Intervals, 3)
}

func TestImageQualityQuoteLedgerUsesLockedPriceAndChildIdentity(t *testing.T) {
	s, key := miniQuoteFixture()
	now := time.Now()
	q, err := s.QuoteImageQuality(context.Background(), key, ImageQuoteRequest{Model: "openai/gpt-image-1-mini/text-to-image", Count: 2}, now)
	require.NoError(t, err)
	snapshot, err := s.AcceptImageQualityQuote(context.Background(), key, &OpenAIImagesRequest{Model: q.Model, N: 1}, q.Token, 1, now)
	require.NoError(t, err)
	usageRepo := &openAIRecordUsageLogRepoStub{inserted: true}
	billingRepo := &openAIRecordUsageBillingRepoStub{}
	recorder := newOpenAIRecordUsageServiceWithBillingRepoForTest(usageRepo, billingRepo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{}, nil)
	// The accepted price wins even after every live pricing field changes.
	key.Group.RateMultiplier = 10
	key.Group.ModelPricing[0].Intervals[1].PerRequestPrice = quoteFloat(99)
	ctx := context.WithValue(context.Background(), ctxkey.ClientRequestID, "shared-client-id-for-all-children")
	err = recorder.RecordUsage(ctx, &OpenAIRecordUsageInput{
		APIKey: key, User: key.User, Account: &Account{ID: 30, Type: AccountTypeAPIKey, RateMultiplier: quoteFloat(7)},
		Result: &OpenAIForwardResult{Model: q.Model, UpstreamModel: q.Model, RequestID: "atlas-prediction-locked-tariff",
			ImageCount: 1, ImageQualityPricing: snapshot},
	})
	require.NoError(t, err)
	require.Equal(t, snapshot.RequestID(), billingRepo.lastCmd.RequestID, "client IDs must not collapse quoted child charges")
	require.Equal(t, 0.06, billingRepo.lastCmd.BalanceCost)
	require.Equal(t, 0.06, usageRepo.lastLog.ActualCost)
	require.Equal(t, 1.5, usageRepo.lastLog.RateMultiplier)
	require.Equal(t, 0.04, usageRepo.lastLog.TotalCost)
	require.NotNil(t, usageRepo.lastLog.UpstreamRequestID)
	require.Equal(t, "atlas-prediction-locked-tariff", *usageRepo.lastLog.UpstreamRequestID)
}

func TestImageQualityQuoteRejectsMoneyOverflowAndImplicitFreeRounding(t *testing.T) {
	for _, tc := range []struct{ price, rate float64 }{{1e12, 1}, {1, math.MaxFloat64}, {0.00000001, 0.001}, {123456789.12345678, 1.5}} {
		s, key := miniQuoteFixture()
		key.Group.ModelPricing[0].Intervals[1].PerRequestPrice = quoteFloat(tc.price)
		key.Group.RateMultiplier = tc.rate
		_, err := s.QuoteImageQuality(context.Background(), key, ImageQuoteRequest{Model: "openai/gpt-image-1-mini/text-to-image", Count: 4}, time.Now())
		require.Error(t, err)
	}
}

func TestImageQualityQuoteForwardGuardAndAcceptedSnapshot(t *testing.T) {
	for _, mode := range []string{"missing quote", "changed controls", "withdrawn card after acceptance"} {
		t.Run(mode, func(t *testing.T) {
			upstream := &atlasImageUpstream{replies: []string{
				`{"data":{"id":"pred-mini","status":"completed","outputs":["https://storage.atlascloud.ai/test.png"]}}`,
			}}
			body := []byte(`{"model":"openai/gpt-image-1-mini/text-to-image","prompt":"synthetic draw","size":"1024x1024","quality":"medium"}`)
			svc, c, _, account, parsed := atlasGPTImageFixture(t, upstream, body)
			signer, key := miniQuoteFixture()
			c.Set("api_key", key)
			ctx := context.Background()
			if mode != "missing quote" {
				q, err := signer.QuoteImageQuality(ctx, key, ImageQuoteRequest{Model: parsed.Model, Count: 1}, time.Now())
				require.NoError(t, err)
				snapshot, err := signer.AcceptImageQualityQuote(ctx, key, parsed, q.Token, 0, time.Now())
				require.NoError(t, err)
				_, err = snapshot.claim(ctx, newInMemoryIdempotencyRepo(), "test-body", time.Now())
				require.NoError(t, err)
				ctx = WithImageQualityPricing(ctx, snapshot)
				c.Request = c.Request.WithContext(ctx)
				if mode == "changed controls" {
					parsed.Quality = "high"
				} else {
					key.Group.ModelPricing = nil
				}
			}
			_, err := svc.ForwardImages(ctx, c, account, body, parsed, "")
			if mode == "withdrawn card after acceptance" {
				require.NoError(t, err)
				require.Equal(t, []string{http.MethodPost}, upstream.methods)
			} else {
				require.Error(t, err)
				require.Empty(t, upstream.methods, "admission failure must precede supplier submission")
			}
		})
	}
}

func (r *inMemoryIdempotencyRepo) UpdateImageQualityClaim(_ context.Context, id int64, body string, expiresAt time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, record := range r.data {
		if record.ID == id && record.Scope == "image-quality-quote-v1" && record.Status == IdempotencyStatusProcessing {
			record.ResponseBody, record.ExpiresAt = &body, expiresAt
			return nil
		}
	}
	return fmt.Errorf("image claim is unavailable")
}

func TestImageQualityQuoteDurableUpstreamOutcomes(t *testing.T) {
	for _, tc := range []struct{ status, want string }{
		{"processing", "unknown"}, {"failed", "failed"}, {"completed", "generated"},
	} {
		t.Run(tc.status, func(t *testing.T) {
			upstream := &atlasImageUpstream{replies: []string{fmt.Sprintf(
				`{"data":{"id":"pred-auditable","status":%q,"outputs":["https://storage.atlascloud.ai/test.png"]}}`, tc.status)}}
			body := []byte(`{"model":"openai/gpt-image-1-mini/text-to-image","prompt":"synthetic draw","size":"1024x1024","quality":"medium"}`)
			svc, c, _, account, parsed := atlasGPTImageFixture(t, upstream, body)
			signer, key := miniQuoteFixture()
			q, err := signer.QuoteImageQuality(context.Background(), key, ImageQuoteRequest{Model: parsed.Model, Count: 1}, time.Now())
			require.NoError(t, err)
			snapshot, err := signer.AcceptImageQualityQuote(context.Background(), key, parsed, q.Token, 0, time.Now())
			require.NoError(t, err)
			repo := newInMemoryIdempotencyRepo()
			_, err = snapshot.claim(context.Background(), repo, "test-body", time.Now())
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(WithImageQualityPricing(context.Background(), snapshot), 80*time.Millisecond)
			defer cancel()
			c.Request = c.Request.WithContext(ctx)
			c.Set("api_key", key)
			_, err = svc.ForwardImages(ctx, c, account, body, parsed, "")
			if tc.want == "generated" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			record, err := repo.GetByScopeAndKeyHash(context.Background(), "image-quality-quote-v1", HashIdempotencyKey(snapshot.RequestID()))
			require.NoError(t, err)
			var audit struct {
				Outcome struct {
					Status   string `json:"status"`
					Upstream string `json:"upstream_request_id"`
					Billing  string `json:"billing_status"`
				} `json:"outcome"`
			}
			require.NoError(t, json.Unmarshal([]byte(*record.ResponseBody), &audit))
			require.Equal(t, "pred-auditable", audit.Outcome.Upstream)
			require.Equal(t, tc.want, audit.Outcome.Status)
			require.Equal(t, "not_charged", audit.Outcome.Billing)
			_, err = snapshot.claim(context.Background(), repo, "test-body", time.Now())
			require.Error(t, err, "failed and unknown jobs must never be resubmitted")
		})
	}
}

type imageQuoteAuditFailureRepo struct {
	*inMemoryIdempotencyRepo
	failState   string
	failReceipt bool
	states      []string
}

func (r *imageQuoteAuditFailureRepo) UpdateImageQualityClaim(ctx context.Context, id int64, body string, expiry time.Time) error {
	var audit struct {
		Outcome ImageQualityOutcome `json:"outcome"`
	}
	if err := json.Unmarshal([]byte(body), &audit); err != nil {
		return err
	}
	r.states = append(r.states, audit.Outcome.Status)
	if audit.Outcome.Status == r.failState {
		return fmt.Errorf("synthetic image audit failure")
	}
	return r.inMemoryIdempotencyRepo.UpdateImageQualityClaim(ctx, id, body, expiry)
}

func (r *imageQuoteAuditFailureRepo) MarkSucceeded(ctx context.Context, id int64, status int, body string, expiry time.Time) error {
	if r.failReceipt {
		return fmt.Errorf("synthetic receipt write failure")
	}
	return r.inMemoryIdempotencyRepo.MarkSucceeded(ctx, id, status, body, expiry)
}

func TestImageQualityQuoteAuditFailureNeverResubmits(t *testing.T) {
	for _, state := range []string{"submitting", "pending"} {
		t.Run(state, func(t *testing.T) {
			upstream := &atlasImageUpstream{replies: []string{`{"data":{"id":"pred-kept","status":"processing"}}`}}
			body := []byte(`{"model":"openai/gpt-image-1-mini/text-to-image","prompt":"synthetic draw","quality":"medium"}`)
			svc, c, _, account, parsed := atlasGPTImageFixture(t, upstream, body)
			signer, key := miniQuoteFixture()
			q, err := signer.QuoteImageQuality(context.Background(), key, ImageQuoteRequest{Model: parsed.Model, Count: 1}, time.Now())
			require.NoError(t, err)
			snapshot, err := signer.AcceptImageQualityQuote(context.Background(), key, parsed, q.Token, 0, time.Now())
			require.NoError(t, err)
			repo := &imageQuoteAuditFailureRepo{inMemoryIdempotencyRepo: newInMemoryIdempotencyRepo(), failState: state}
			_, err = snapshot.claim(context.Background(), repo, "test-body", time.Now())
			require.NoError(t, err)
			ctx := WithImageQualityPricing(context.Background(), snapshot)
			c.Request = c.Request.WithContext(ctx)
			c.Set("api_key", key)
			_, err = svc.ForwardImages(ctx, c, account, body, parsed, "")
			require.Error(t, err)
			require.NoError(t, snapshot.MarkForwardFailure(ctx))
			if state == "submitting" {
				require.Empty(t, upstream.methods, "fail closed before supplier POST if submission audit cannot be written")
				require.Equal(t, "not_submitted", snapshot.Outcome().Status)
			} else {
				require.Equal(t, []string{http.MethodPost}, upstream.methods, "never poll or resubmit when the accepted ID cannot initially be persisted")
				require.Equal(t, "unknown", snapshot.Outcome().Status)
				record, err := repo.GetByScopeAndKeyHash(ctx, "image-quality-quote-v1", HashIdempotencyKey(snapshot.RequestID()))
				require.NoError(t, err)
				require.Contains(t, *record.ResponseBody, "pred-kept")
			}
			require.Equal(t, "not_charged", snapshot.Outcome().BillingStatus)
			_, err = snapshot.claim(ctx, repo, "test-body", time.Now())
			require.Error(t, err)
		})
	}
}

func TestImageQualityQuoteSettlementAuditFailureStates(t *testing.T) {
	svc, key := miniQuoteFixture()
	ctx := context.Background()
	q, err := svc.QuoteImageQuality(ctx, key, ImageQuoteRequest{Model: "openai/gpt-image-1-mini/text-to-image", Count: 1}, time.Now())
	require.NoError(t, err)
	snapshot, err := svc.AcceptImageQualityQuote(ctx, key, &OpenAIImagesRequest{Model: q.Model, N: 1}, q.Token, 0, time.Now())
	require.NoError(t, err)
	repo := &imageQuoteAuditFailureRepo{inMemoryIdempotencyRepo: newInMemoryIdempotencyRepo()}
	id, err := snapshot.claim(ctx, repo, "test-body", time.Now())
	require.NoError(t, err)
	require.NoError(t, snapshot.markOutcome(ctx, "generated", "pred-reconcile"))
	repo.failState = "settlement_pending"
	require.Error(t, snapshot.MarkSettlementPending(ctx))
	require.Equal(t, "not_charged", snapshot.Outcome().BillingStatus, "failed pre-settlement audit must not imply a ledger operation occurred")
	repo.failState = ""
	require.NoError(t, snapshot.MarkSettlementPending(ctx))
	require.NoError(t, snapshot.MarkSettlementUnknown(ctx))
	require.Equal(t, "unresolved", snapshot.Outcome().BillingStatus, "a billing error is not proof that no transaction committed")
	require.Equal(t, "pred-reconcile", snapshot.Outcome().UpstreamRequestID)
	repo.failReceipt = true
	require.Error(t, snapshot.MarkSettled(ctx, id, "pred-reconcile"))
	require.NoError(t, snapshot.MarkReceiptUnavailable(ctx))
	require.Equal(t, "settled", snapshot.Outcome().BillingStatus, "confirmed ledger success remains charged even when receipt persistence fails")
	require.Equal(t, "receipt_unavailable", snapshot.Outcome().Status)
	require.Equal(t, "0.06000000", snapshot.Receipt().Charged)
	record, err := repo.GetByScopeAndKeyHash(ctx, "image-quality-quote-v1", HashIdempotencyKey(snapshot.RequestID()))
	require.NoError(t, err)
	require.Contains(t, *record.ResponseBody, `"billing_status":"settled"`)
	require.Contains(t, *record.ResponseBody, "pred-reconcile")
	_, err = snapshot.claim(ctx, repo, "test-body", time.Now())
	require.Error(t, err, "an audit or settlement error never grants another supplier submission")
}
