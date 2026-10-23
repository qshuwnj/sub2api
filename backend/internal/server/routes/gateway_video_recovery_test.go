package routes

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	servermiddleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type videoRecoveryRouteKeys struct {
	*keyBillingRouteAPIKeyRepo
}

func (*videoRecoveryRouteKeys) UpdateLastUsed(context.Context, int64, time.Time) error { return nil }

type videoRecoveryRouteRepo struct {
	service.UsageBillingRepository
	service.VideoAttemptRepository
	attempt  *service.VideoAttempt
	reads    int
	scenario string
}

func (*videoRecoveryRouteRepo) VideoQuotedBillingReady() bool { return true }
func (r *videoRecoveryRouteRepo) GetVideoAttempt(_ context.Context, owner service.VideoAttemptOwner, id string) (*service.VideoAttempt, error) {
	r.reads++
	if r.scenario == "lookup failure" {
		return nil, errors.New("synthetic failure")
	}
	if r.attempt == nil || id != r.attempt.ID || owner.UserID != r.attempt.UserID || owner.APIKeyID != r.attempt.APIKeyID ||
		r.scenario == "disappeared" && r.reads > 1 {
		return nil, service.ErrVideoAttemptNotFound
	}
	copy := *r.attempt
	return &copy, nil
}

func TestGatewayVideoAcceptedRecoveryPrecedesNewAdmissionMiddleware(t *testing.T) {
	const body = `{"model":"google/veo3.1/text-to-video","prompt":"private","aspect_ratio":"16:9","duration":8,"generate_audio":false,"resolution":"720p"}`
	for _, prefix := range []string{"/v1", ""} {
		for _, scenario := range []string{"owned", "owned balance", "owned expired", "body mismatch", "model mismatch", "foreign", "new", "new balance", "disabled", "disappeared", "lookup failure", "legacy model", "get attempt", "get balance"} {
			t.Run(prefix+"/"+scenario, func(t *testing.T) {
				gin.SetMode(gin.TestMode)
				_, native, err := service.SerializeAtlasCloudVideoRequest([]byte(body))
				require.NoError(t, err)
				repo := &videoRecoveryRouteRepo{scenario: scenario, attempt: &service.VideoAttempt{
					ID: "attempt", QuoteID: "quote", UserID: 7, APIKeyID: 100, GroupID: 42, AccountID: 30,
					RequestFingerprint: service.HashUsageRequestPayload(native),
					Selection:          service.VideoQuoteRequest{Model: "google/veo3.1/text-to-video"},
					State:              service.VideoAttemptCompleted, FinancialState: service.VideoFinancialCaptured,
					TotalPrice: "13.33171200", UpstreamTaskID: "atlascloud:job", ResultURLs: []string{"https://cdn.example/result.mp4"},
					Receipt: &service.VideoChargeReceipt{RequestID: "video-quote:quote", Amount: "13.33171200", Currency: "USD", UsageLogID: 3},
				}}
				// The group is now composite and its allowlist no longer permits
				// the accepted model. Recovery must never route/map it again.
				group := &service.Group{ID: 42, Status: service.StatusActive, Hydrated: true, Platform: service.PlatformComposite,
					ModelAllowlist: service.GroupModelAllowlist{Enabled: true, Models: []string{"other-model"}}}
				key := &service.APIKey{ID: 100, UserID: 7, Key: "client-key", Status: service.StatusAPIKeyQuotaExhausted,
					Group: group, GroupID: &group.ID, Quota: 1, QuotaUsed: 1,
					User: &service.User{ID: 7, Status: service.StatusActive, Balance: 0}}
				if scenario == "foreign" {
					key.ID = 101
				}
				if scenario == "disabled" {
					key.Status = service.StatusDisabled
				}
				if scenario == "owned balance" || scenario == "new balance" || scenario == "get balance" {
					key.Status, key.Quota, key.QuotaUsed = service.StatusActive, 0, 0
				}
				if scenario == "owned expired" {
					expired := time.Now().Add(-time.Hour)
					key.Status, key.ExpiresAt = service.StatusAPIKeyExpired, &expired
				}
				cfg := &config.Config{RunMode: config.RunModeStandard}
				cfg.Gateway.MaxBodySize = 4096
				keys := service.NewAPIKeyService(&videoRecoveryRouteKeys{&keyBillingRouteAPIKeyRepo{apiKey: key}}, nil, nil, nil, nil, nil, cfg)
				gateway := service.NewOpenAIGatewayService(nil, nil, repo, nil, nil, nil, nil, cfg, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
				keys.SetVideoAttemptRecoveryLookup(gateway.GetQuotedVideo)
				h := handler.NewOpenAIGatewayHandler(gateway, nil, nil, keys, nil, nil, nil, nil, cfg)
				router := gin.New()
				RegisterGatewayRoutes(router, &handler.Handlers{Gateway: &handler.GatewayHandler{}, OpenAIGateway: h, AsyncImage: handler.NewAsyncImageHandler(nil, nil)},
					servermiddleware.NewAPIKeyAuthMiddleware(keys, nil, cfg), keys, nil, nil, nil, nil, cfg)
				submit, id, method, path := body, "attempt", http.MethodPost, prefix+"/videos/generations"
				switch scenario {
				case "body mismatch":
					submit = strings.Replace(submit, "private", "different", 1)
				case "model mismatch":
					submit = strings.Replace(submit, "text-to-video", "image-to-video", 1)
				case "legacy model":
					submit = `{"model":"grok-imagine-video","prompt":"private"}`
				case "new", "new balance":
					id = "new-attempt"
				case "get attempt", "get balance":
					method, path, submit = http.MethodGet, prefix+"/videos/attempts/attempt", ""
				}
				req := httptest.NewRequest(method, path, strings.NewReader(submit))
				req.Header.Set("X-API-Key", "client-key")
				req.Header.Set("Idempotency-Key", id)
				req.Header.Set("X-Video-Quote", "expired")
				w := httptest.NewRecorder()
				router.ServeHTTP(w, req)
				switch scenario {
				case "owned", "owned balance", "owned expired", "get attempt", "get balance":
					require.Equal(t, 200, w.Code, w.Body.String())
					require.Contains(t, w.Body.String(), "result.mp4")
				case "body mismatch":
					require.Equal(t, 409, w.Code, w.Body.String())
				case "disappeared":
					require.Equal(t, 404, w.Code, w.Body.String())
				case "lookup failure":
					require.Equal(t, 503, w.Code, w.Body.String())
				case "disabled":
					require.Equal(t, 401, w.Code, w.Body.String())
				case "new balance":
					require.Equal(t, 403, w.Code, w.Body.String())
				default:
					require.Equal(t, 429, w.Code, w.Body.String())
				}
				// No account repository or HTTPUpstream is installed. Entering
				// generation or legacy billing would fail this real route test.
			})
		}
	}
}
