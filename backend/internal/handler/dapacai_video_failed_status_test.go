//go:build unit

package handler

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestDapacaiFailedStatusSurvivesBillingAdmissionFailure(t *testing.T) {
	for _, scenario := range []string{"failed", "queued", "foreign key", "foreign user", "foreign task", "transport error", "malformed", "identity mismatch", "unsettled completed", "new generation"} {
		t.Run(scenario, func(t *testing.T) {
			repo := &dapacaiBillingRepo{}
			h, slots, bindings, upstream := newGrokMediaSlotHandlerWithBilling(t, false, false, repo, service.PlatformOpenAI, "https://api.dapacai.com")
			const task = "dapacai:task_owned"
			groupID := int64(24)
			require.NoError(t, h.gatewayService.BindGrokMediaVideoRequestAccount(context.Background(), &groupID, task, 10, 20, 1))
			cache := newHandlerInflightCache(0)
			cfg := &config.Config{RunMode: config.RunModeStandard}
			cfg.Billing.InflightReservation.Enabled = true
			cfg.Billing.InflightReservation.FailClosedOnUnpriced = true
			billing := service.NewBillingCacheService(cache, nil, nil, nil, nil, nil, cfg, nil)
			t.Cleanup(billing.Stop)
			h.billingCacheService = billing
			c, w := grokMediaSlotContext(context.Background(), scenario == "new generation")
			key, _ := middleware.GetAPIKeyFromContext(c)
			key.Group.Platform = service.PlatformOpenAI
			key.Group.VideoModelPrices = map[string]map[string]float64{"seedance-2.0": {"native": 0.1}}
			require.ErrorIs(t, billing.CheckBillingEligibility(c.Request.Context(), key.User, key, key.Group, nil, ""), service.ErrInsufficientBalance)
			c.Params = gin.Params{{Key: "request_id", Value: task}}
			switch scenario {
			case "foreign key":
				key.ID++
			case "foreign user":
				key.UserID++
				key.User.ID++
				c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: key.UserID, Concurrency: 5})
			case "foreign task":
				c.Params[0].Value = "dapacai:task_other"
			}
			upstream.call = func(req *http.Request, _ int64) (*http.Response, error) {
				require.Equal(t, http.MethodGet, req.Method, "polling must never create a new job")
				require.Equal(t, "/v1/videos/task_owned", req.URL.Path)
				if scenario == "transport error" {
					return nil, errors.New("unavailable")
				}
				body := `{"id":"task_owned","status":"failed","error":{"message":"task failed"}}`
				switch scenario {
				case "queued":
					body = `{"id":"task_owned","status":"queued"}`
				case "malformed":
					body = `not json`
				case "identity mismatch":
					body = `{"id":"task_other","status":"failed"}`
				case "unsettled completed":
					body = `{"id":"task_owned","status":"completed"}`
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
			}
			if scenario == "new generation" {
				c.Request.Body = io.NopCloser(strings.NewReader(`{"model":"seedance-2.0","prompt":"test","duration":5,"ratio":"16:9"}`))
				h.DapacaiVideoGeneration(c)
			} else {
				h.DapacaiVideoStatus(c)
			}
			switch scenario {
			case "failed", "queued":
				require.Equal(t, 200, w.Code, w.Body.String())
				require.Contains(t, w.Body.String(), `"id":"`+task+`"`)
				require.Contains(t, w.Body.String(), `"status":"`+scenario+`"`)
				require.Equal(t, 1, upstream.calls)
			case "foreign key", "foreign user", "foreign task":
				require.Equal(t, 404, w.Code, w.Body.String())
				require.Zero(t, upstream.calls)
			case "new generation":
				require.Equal(t, 403, w.Code, w.Body.String())
				require.Zero(t, upstream.calls)
			default:
				require.GreaterOrEqual(t, w.Code, 500, w.Body.String())
				require.NotContains(t, w.Body.String(), `"status":"failed"`)
				require.NotContains(t, w.Body.String(), `"status":"completed"`)
			}
			require.Empty(t, repo.commands, "failed/uncertain polls must never charge")
			require.Empty(t, bindings.billed)
			require.Empty(t, cache.res, "status queries must not reserve a new charge")
			slots.assertReleased(t)
		})
	}
}
