//go:build unit

package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

type quotedHandlerRepository struct {
	service.UsageBillingRepository
	service.VideoAttemptRepository
	attempt   *service.VideoAttempt
	err       error
	acceptErr error
	reserves  int
	captures  int
	notReady  bool
}

func (r *quotedHandlerRepository) VideoQuotedBillingReady() bool { return !r.notReady }
func (r *quotedHandlerRepository) GetVideoAttempt(_ context.Context, owner service.VideoAttemptOwner, id string) (*service.VideoAttempt, error) {
	if r.err != nil {
		return nil, r.err
	}
	if r.attempt == nil || r.attempt.ID != id || r.attempt.UserID != owner.UserID || r.attempt.APIKeyID != owner.APIKeyID {
		return nil, service.ErrVideoAttemptNotFound
	}
	copy := *r.attempt
	return &copy, nil
}
func (r *quotedHandlerRepository) GetVideoAttemptByTask(ctx context.Context, owner service.VideoAttemptOwner, task string) (*service.VideoAttempt, error) {
	if r.err != nil {
		return nil, r.err
	}
	if r.attempt == nil || r.attempt.UpstreamTaskID != task {
		return nil, service.ErrVideoAttemptNotFound
	}
	return r.GetVideoAttempt(ctx, owner, r.attempt.ID)
}
func (r *quotedHandlerRepository) AcceptVideoAttempt(_ context.Context, command *service.VideoAttemptAcceptCommand) (*service.VideoAttemptAcceptResult, error) {
	if r.acceptErr != nil {
		return nil, r.acceptErr
	}
	copy := *command.Attempt
	copy.State, copy.FinancialState = service.VideoAttemptAccepted, service.VideoFinancialReserved
	r.attempt, r.reserves = &copy, r.reserves+1
	return &service.VideoAttemptAcceptResult{Attempt: &copy, Created: true}, nil
}
func (r *quotedHandlerRepository) UpdateVideoAttempt(_ context.Context, _ service.VideoAttemptOwner, _ string, update service.VideoAttemptUpdate) (*service.VideoAttempt, error) {
	r.attempt.State = update.State
	r.attempt.UpstreamTaskID = update.UpstreamTaskID
	return r.attempt, nil
}
func (r *quotedHandlerRepository) CaptureVideoAttempt(_ context.Context, _ service.VideoAttemptOwner, _ string, urls []string) (*service.VideoAttemptSettlementResult, error) {
	r.captures++
	r.attempt.State, r.attempt.FinancialState = service.VideoAttemptCompleted, service.VideoFinancialCaptured
	r.attempt.ResultURLs = urls
	r.attempt.Receipt = &service.VideoChargeReceipt{RequestID: r.attempt.RequestID(), Amount: r.attempt.TotalPrice, Currency: "USD", UsageLogID: 1}
	return &service.VideoAttemptSettlementResult{Attempt: r.attempt, Applied: true}, nil
}

const quotedHandlerBody = `{"model":"google/veo3.1/text-to-video","prompt":"private","aspect_ratio":"16:9","duration":8,"generate_audio":false,"resolution":"720p"}`

func quotedHandlerAttempt(t *testing.T) *service.VideoAttempt {
	t.Helper()
	_, native, err := service.SerializeAtlasCloudVideoRequest([]byte(quotedHandlerBody))
	require.NoError(t, err)
	return &service.VideoAttempt{
		ID: "owned-attempt", QuoteID: "quote", UserID: 10, APIKeyID: 20, GroupID: 24, AccountID: 1,
		RequestFingerprint: service.HashUsageRequestPayload(native),
		Selection:          service.VideoQuoteRequest{Model: "google/veo3.1/text-to-video"},
		TotalPrice:         "13.33171200", AccountCost: "1.60000000", BasePrice: "13.33171200",
		State: service.VideoAttemptCompleted, FinancialState: service.VideoFinancialCaptured,
		UpstreamTaskID: "atlascloud:task-atlas", ResultURLs: []string{"https://cdn.example/result.mp4"},
		Receipt: &service.VideoChargeReceipt{RequestID: "video-quote:quote", Amount: "13.33171200", Currency: "USD", UsageLogID: 1},
	}
}

func TestQuotedVideoHandlerRecoveryBeforeTariffAndBodyConflict(t *testing.T) {
	for _, scenario := range []string{"recover", "body conflict", "foreign key", "lookup failure", "unsent"} {
		t.Run(scenario, func(t *testing.T) {
			repo := &quotedHandlerRepository{attempt: quotedHandlerAttempt(t)}
			h, slots, _, upstream := newGrokMediaSlotHandlerWithBilling(t, false, false, repo, service.PlatformOpenAI, "https://api.atlascloud.ai")
			c, w := atlasVideoContext(t, true, false)
			body := quotedHandlerBody
			switch scenario {
			case "body conflict":
				body = strings.Replace(body, "private", "different", 1)
			case "foreign key":
				key, _ := middleware.GetAPIKeyFromContext(c)
				key.ID = 99
			case "lookup failure":
				repo.err = errors.New("synthetic storage failure")
			case "unsent":
				repo.attempt.State, repo.attempt.FinancialState = service.VideoAttemptAccepted, service.VideoFinancialReserved
				repo.attempt.UpstreamTaskID, repo.attempt.ResultURLs, repo.attempt.Receipt = "", nil, nil
			}
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos/generations", strings.NewReader(body))
			c.Request.Header.Set("Idempotency-Key", "owned-attempt")
			c.Request.Header.Set("X-Video-Quote", "expired-or-no-longer-enrolled")
			h.AtlasCloudVideoGeneration(c)
			switch scenario {
			case "recover":
				require.Equal(t, 200, w.Code, w.Body.String())
				require.Contains(t, w.Body.String(), `"amount":"13.33171200"`)
				require.Contains(t, w.Body.String(), "result.mp4")
			case "unsent":
				require.Equal(t, 202, w.Code, w.Body.String())
				require.Contains(t, w.Body.String(), `"status":"accepted"`)
				require.NotContains(t, w.Body.String(), "result.mp4")
			case "body conflict":
				require.Equal(t, 409, w.Code, w.Body.String())
			case "lookup failure":
				require.Equal(t, 503, w.Code, w.Body.String())
			default:
				require.Equal(t, 400, w.Code, w.Body.String())
			}
			require.Zero(t, upstream.calls, "recovery, conflict or failed admission cannot POST")
			for _, private := range []string{"private", "AccountID", "account_id", "request_fingerprint", "AccountCost", "1.60000000", "synthetic storage"} {
				require.NotContains(t, w.Body.String(), private)
			}
			slots.assertReleased(t)
		})
	}
}

func TestQuotedVideoHandlerBodyLimitAndPanicBoundary(t *testing.T) {
	t.Run("body limit", func(t *testing.T) {
		h, slots, _, upstream := newGrokMediaSlotHandlerWithBilling(t, false, false, &quotedHandlerRepository{}, service.PlatformOpenAI, "https://api.atlascloud.ai")
		c, w := atlasVideoContext(t, true, false)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos/generations", strings.NewReader(quotedHandlerBody))
		c.Request.Body = http.MaxBytesReader(w, c.Request.Body, 8)
		h.AtlasCloudVideoGeneration(c)
		require.Equal(t, 413, w.Code, w.Body.String())
		require.Zero(t, upstream.calls)
		slots.assertReleased(t)
	})
	t.Run("panic remains contained", func(t *testing.T) {
		c, w := atlasVideoContext(t, true, false)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos/generations", strings.NewReader(quotedHandlerBody))
		c.Request.Header.Set("Idempotency-Key", "attempt")
		h := &OpenAIGatewayHandler{}
		require.NotPanics(t, func() { h.AtlasCloudVideoGeneration(c) })
		require.Equal(t, 502, w.Code, w.Body.String(), "preserve the established non-streaming panic response")
	})
}

func TestQuotedVideoHandlerOwnedResourceAndNativeStatus(t *testing.T) {
	for _, scenario := range []string{"attempt", "native", "foreign", "ambiguous"} {
		t.Run(scenario, func(t *testing.T) {
			repo := &quotedHandlerRepository{attempt: quotedHandlerAttempt(t)}
			h, slots, _, upstream := newGrokMediaSlotHandlerWithBilling(t, false, false, repo, service.PlatformOpenAI, "https://api.atlascloud.ai")
			c, w := atlasVideoContext(t, false, false)
			c.Params = append(c.Params, struct{ Key, Value string }{"attempt_id", "owned-attempt"})
			if scenario == "foreign" {
				key, _ := middleware.GetAPIKeyFromContext(c)
				key.ID = 99
			}
			if scenario == "ambiguous" {
				repo.err = service.ErrVideoAttemptConflict
			}
			if scenario == "native" || scenario == "ambiguous" {
				h.AtlasCloudVideoStatus(c)
			} else {
				h.VideoAttempt(c)
			}
			if scenario == "foreign" {
				require.Equal(t, 404, w.Code, w.Body.String())
			} else if scenario == "ambiguous" {
				require.Equal(t, 409, w.Code, w.Body.String())
			} else {
				require.Equal(t, 200, w.Code, w.Body.String())
				require.Contains(t, w.Body.String(), `"attempt_id":"owned-attempt"`)
				require.Contains(t, w.Body.String(), "result.mp4")
				require.Equal(t, "no-store", w.Header().Get("Cache-Control"))
			}
			require.Zero(t, upstream.calls, "native durable status must not enter legacy pending billing")
			slots.assertReleased(t)
		})
	}
}

func TestQuotedVideoOptionalDiscoveryFailureBoundary(t *testing.T) {
	for _, scenario := range []string{"nil gateway", "legacy capability absent", "durable not ready", "durable read failed", "durable conflict"} {
		t.Run(scenario, func(t *testing.T) {
			var repo service.UsageBillingRepository = &struct{ service.UsageBillingRepository }{}
			if scenario == "durable read failed" {
				repo = &quotedHandlerRepository{err: service.ErrVideoAttemptUnavailable}
			}
			if scenario == "durable conflict" {
				repo = &quotedHandlerRepository{err: service.ErrVideoAttemptConflict}
			}
			if scenario == "durable not ready" {
				repo = &quotedHandlerRepository{notReady: true}
			}
			h, slots, _, upstream := newGrokMediaSlotHandlerWithBilling(t, false, false, repo, service.PlatformOpenAI, "https://api.atlascloud.ai")
			if scenario == "nil gateway" {
				h.gatewayService = nil
			}
			c, w := atlasVideoContext(t, false, false)
			c.Params = append(c.Params, struct{ Key, Value string }{"request_id", "atlascloud:task"})
			var handled bool
			require.NotPanics(t, func() { handled = h.QuotedVideoStatus(c) })
			switch scenario {
			case "durable not ready", "durable read failed":
				require.True(t, handled, "real durable lookup failures must terminate before legacy billing")
				require.Equal(t, 503, w.Code, w.Body.String())
			case "durable conflict":
				require.True(t, handled, "ambiguous owned identities must never fall through")
				require.Equal(t, 409, w.Code, w.Body.String())
			default:
				require.False(t, handled, "absent optional capabilities preserve the legacy route")
				c.Params = append(c.Params, struct{ Key, Value string }{"attempt_id", "owned-attempt"})
				require.NotPanics(t, func() { h.VideoAttempt(c) })
				require.Equal(t, 503, w.Code, w.Body.String(), "new durable resources fail closed when unavailable")
			}
			require.Zero(t, upstream.calls)
			slots.assertReleased(t)
		})
	}
}

func TestQuotedVideoHandlerEnrolledMissingQuoteCannotFallback(t *testing.T) {
	h, slots, _, upstream := newGrokMediaSlotHandler(t, false, false, service.PlatformOpenAI, "https://api.atlascloud.ai")
	c, w := atlasVideoContext(t, true, true)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos/generations", strings.NewReader(quotedHandlerBody))
	key, _ := middleware.GetAPIKeyFromContext(c)
	revision, err := service.VideoContractRevision("google/veo3.1/text-to-video")
	require.NoError(t, err)
	input := service.VideoQuoteRequest{Model: "google/veo3.1/text-to-video", Mode: "t2v",
		Controls: map[string]any{"aspect_ratio": "16:9", "duration": 8, "generate_audio": false, "resolution": "720p"}}
	cell, err := service.VideoTariffKey(input)
	require.NoError(t, err)
	price := 13.331712
	key.Group.ModelPricing = []service.ChannelModelPricing{{Platform: service.PlatformOpenAI, Models: []string{input.Model}, BillingMode: service.BillingModeVideo,
		Intervals: []service.PricingInterval{{TierLabel: cell, PerRequestPrice: &price}},
		VideoTariff: &service.VideoTariffMetadata{Version: 1, ContractRevision: revision, Cells: map[string]service.VideoTariffProvenance{
			cell: {Source: service.VideoCalculatorURL, CheckedAt: "2026-10-22T00:00:00Z", UpstreamPrice: "1.60", Policy: service.VideoRetailPolicy},
		}}}}
	h.AtlasCloudVideoGeneration(c)
	require.Equal(t, 400, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "video_quote_required")
	require.Zero(t, upstream.calls)
	slots.assertReleased(t)
}

func TestQuotedVideoHandlerLegacySettingsCannotBypassQuote(t *testing.T) {
	for _, changed := range []bool{false, true} {
		h, slots, _, upstream := newGrokMediaSlotHandler(t, false, false, service.PlatformOpenAI, "https://api.atlascloud.ai")
		c, w := atlasVideoContext(t, true, true)
		key, _ := middleware.GetAPIKeyFromContext(c)
		key.Group.ModelPricing = nil
		body := quotedHandlerBody
		if changed {
			body = strings.Replace(body, `"generate_audio":false`, `"generate_audio":true`, 1)
		}
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos/generations", strings.NewReader(body))
		handled := h.tryQuotedAtlasVideo(c)
		require.Equal(t, changed, handled)
		if changed {
			require.Equal(t, 400, w.Code, w.Body.String())
			require.Contains(t, w.Body.String(), "video_settings_quote_required")
		}
		require.Zero(t, upstream.calls)
		slots.assertReleased(t)
	}
}

type quotedAdmissionKeys struct {
	service.APIKeyRepository
	key *service.APIKey
}

func (r *quotedAdmissionKeys) GetByID(context.Context, int64) (*service.APIKey, error) {
	return r.key, nil
}

type quotedAdmissionUsers struct {
	service.UserRepository
	user *service.User
}

func (r *quotedAdmissionUsers) GetByID(context.Context, int64) (*service.User, error) {
	return r.user, nil
}

type quotedAdmissionGroups struct {
	service.GroupRepository
	group *service.Group
}

func (r *quotedAdmissionGroups) GetByIDLite(context.Context, int64) (*service.Group, error) {
	return r.group, nil
}

type quotedAdmissionChannels struct{ service.ChannelRepository }

func (*quotedAdmissionChannels) GetChannelIDByGroupID(context.Context, int64) (int64, error) {
	return 0, nil
}
func (*quotedAdmissionChannels) ListAll(context.Context) ([]service.Channel, error) { return nil, nil }

func TestQuotedVideoHandlerFreshAdmissionAndImmediateCapture(t *testing.T) {
	for _, scenario := range []string{"processing", "completed", "body mismatch", "platform quota", "configured rule"} {
		t.Run(scenario, func(t *testing.T) {
			repo := &quotedHandlerRepository{}
			h, slots, _, upstream := newGrokMediaSlotHandlerWithBilling(t, false, false, repo, service.PlatformOpenAI, "https://api.atlascloud.ai")
			c, w := atlasVideoContext(t, true, false)
			key, _ := middleware.GetAPIKeyFromContext(c)
			key.Status, key.Group.Status, key.User.Status = service.StatusActive, service.StatusActive, service.StatusActive
			key.User.Balance, key.Group.RateMultiplier = 100, 1
			input := service.VideoQuoteRequest{Model: "google/veo3.1/text-to-video", Mode: "t2v",
				Controls: map[string]any{"aspect_ratio": "16:9", "duration": 8, "generate_audio": false, "resolution": "720p"}}
			cell, err := service.VideoTariffKey(input)
			require.NoError(t, err)
			revision, err := service.VideoContractRevision(input.Model)
			require.NoError(t, err)
			price := 13.331712
			key.Group.ModelPricing = []service.ChannelModelPricing{{Platform: service.PlatformOpenAI, Models: []string{input.Model}, BillingMode: service.BillingModeVideo,
				Intervals: []service.PricingInterval{{TierLabel: cell, PerRequestPrice: &price}},
				VideoTariff: &service.VideoTariffMetadata{Version: 1, ContractRevision: revision, Cells: map[string]service.VideoTariffProvenance{
					cell: {Source: service.VideoCalculatorURL, CheckedAt: "2026-10-22T00:00:00Z", UpstreamPrice: "1.60", Policy: service.VideoRetailPolicy},
				}}}}
			wantPrice := "13.33171200"
			if scenario == "configured rule" {
				key.Group.ModelPricing = nil
				perSecond := 0.25
				key.Group.VideoPrice720P = &perSecond
				wantPrice = "2.00000000"
			}
			cfg := &config.Config{RunMode: config.RunModeSimple, JWT: config.JWTConfig{Secret: strings.Repeat("test", 12)}}
			cfg.Gateway.Scheduling.StickySessionWaitTimeout = 20 * time.Millisecond
			keyRepo := &quotedAdmissionKeys{key: key}
			users := &quotedAdmissionUsers{user: key.User}
			billing := service.NewBillingCacheService(nil, users, nil, keyRepo, nil, nil, cfg, nil)
			t.Cleanup(billing.Stop)
			channel := service.NewChannelService(&quotedAdmissionChannels{}, &quotedAdmissionGroups{group: key.Group}, nil, nil, nil)
			accounts := grokMediaSlotRepo{openAIImagesFailoverAccountRepo: openAIImagesFailoverAccountRepo{accounts: []service.Account{{
				ID: 1, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Concurrency: 50,
				GroupIDs: []int64{24}, Credentials: map[string]any{"api_key": "supplier-only", "base_url": "https://api.atlascloud.ai"},
			}}}}
			concurrency := h.concurrencyHelper.concurrencyService
			gateway := service.NewOpenAIGatewayService(accounts, nil, repo, users, nil, nil, nil, cfg, nil, concurrency, &service.BillingService{}, nil, billing, upstream, nil, nil, nil, nil, channel, nil, nil, nil)
			h.gatewayService, h.billingCacheService = gateway, billing
			quote, err := gateway.QuoteVideo(context.Background(), key, input, time.Now())
			require.NoError(t, err)
			if scenario == "platform quota" {
				repo.acceptErr = service.ErrVideoAttemptPlatformQuota
			}
			upstream.call = func(req *http.Request, accountID int64) (*http.Response, error) {
				require.Equal(t, 1, repo.reserves, "price must be locked before supplier POST")
				require.Equal(t, service.VideoAttemptSubmitting, repo.attempt.State)
				require.Equal(t, wantPrice, repo.attempt.TotalPrice)
				require.Empty(t, req.Header.Get("X-Video-Quote"))
				require.Empty(t, req.Header.Get("Idempotency-Key"))
				status := "processing"
				if scenario == "completed" || scenario == "configured rule" {
					status = "completed"
				}
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"data":{"id":"job","status":"` + status + `","outputs":["https://cdn.example/result.mp4"]}}`))}, nil
			}
			body := quotedHandlerBody
			if scenario == "body mismatch" {
				body = strings.Replace(body, `"duration":8`, `"duration":4`, 1)
			}
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos/generations", strings.NewReader(body))
			c.Request.Header.Set("Idempotency-Key", "new-attempt")
			c.Request.Header.Set("X-Video-Quote", quote.Token)
			h.AtlasCloudVideoGeneration(c)
			switch scenario {
			case "body mismatch":
				require.Equal(t, 400, w.Code, w.Body.String())
				require.Zero(t, repo.reserves)
				require.Zero(t, upstream.calls)
			case "platform quota":
				require.Equal(t, 503, w.Code, w.Body.String())
				require.Contains(t, w.Body.String(), "video_platform_quota_unsupported")
				require.Zero(t, repo.reserves)
				require.Zero(t, upstream.calls)
			default:
				want := 202
				if scenario == "completed" || scenario == "configured rule" {
					want = 200
					require.Equal(t, 1, repo.captures)
				}
				require.Equal(t, want, w.Code, w.Body.String())
				require.Equal(t, 1, upstream.calls)
				var response map[string]any
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
				wantStatus := scenario
				if scenario == "configured rule" {
					wantStatus = "completed"
					require.Equal(t, "configured_rule", quote.PriceSource)
					require.Contains(t, w.Body.String(), `"amount":"2.00000000"`)
					require.True(t, repo.attempt.SupplierEstimated)
				}
				require.Equal(t, wantStatus, response["status"])
				if scenario == "processing" {
					require.NotContains(t, response, "outputs")
					require.NotContains(t, response, "receipt")
				}
			}
			slots.assertReleased(t)
		})
	}
}
