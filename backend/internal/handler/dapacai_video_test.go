//go:build unit

package handler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

type dapacaiBillingRepo struct {
	atlasCloudBillingRepo
	fixedJob bool
}

func (r *dapacaiBillingRepo) FindVideoChargeReceipt(_ context.Context, owner service.VideoAttemptOwner, task string) (*service.VideoChargeReceipt, error) {
	if !r.committed || owner.UserID != 10 || owner.APIKeyID != 20 || task != "dapacai:task_owned" {
		return nil, service.ErrVideoResultNotFound
	}
	amount := "0.5000000000"
	if r.fixedJob {
		amount = "1.5600000000"
	}
	return &service.VideoChargeReceipt{RequestID: "grok-video:" + task, Amount: amount, Currency: "USD", UsageLogID: 42}, nil
}

func TestDapacaiCompletionRequiresCommittedReceiptAndBillsOnce(t *testing.T) {
	for _, fixedJob := range []bool{false, true} {
		for _, scenario := range []string{"settles once", "paid without pending", "unpaid without pending", "settlement failure"} {
			t.Run(fmt.Sprintf("%s/fixed=%t", scenario, fixedJob), func(t *testing.T) {
				repo := &dapacaiBillingRepo{fixedJob: fixedJob}
				h, slots, bindings, upstream := newGrokMediaSlotHandlerWithBilling(t, false, false, repo,
					service.PlatformOpenAI, "https://api.dapacai.com")
				upstream.call = func(req *http.Request, _ int64) (*http.Response, error) {
					status := "queued"
					if req.Method == http.MethodGet {
						status = "completed"
					}
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}},
						Body: io.NopCloser(strings.NewReader(`{"id":"task_owned","status":"` + status + `"}`))}, nil
				}
				run := func(create bool) (int, string) {
					core, logs := observer.New(zap.DebugLevel)
					c, w := grokMediaSlotContext(logger.IntoContext(context.Background(), zap.New(core)), create)
					key, _ := middleware.GetAPIKeyFromContext(c)
					key.Group.Platform, key.Group.RateMultiplier = service.PlatformOpenAI, 1
					key.Group.VideoModelPrices = map[string]map[string]float64{"seedance-2.0": {"native": 0.1}}
					if fixedJob {
						price := 1.3
						key.Group.VideoRateIndependent, key.Group.VideoRateMultiplier = true, 1.2
						key.Group.ModelPricing = []service.ChannelModelPricing{{
							Platform: service.PlatformOpenAI, Models: []string{"seedance-2.0"},
							BillingMode: service.BillingModePerRequest, PerRequestPrice: &price,
						}}
					}
					c.Params = gin.Params{{Key: "request_id", Value: "dapacai:task_owned"}}
					if create {
						c.Request.Body = io.NopCloser(strings.NewReader(`{"model":"seedance-2.0","prompt":"test","duration":5,"ratio":"16:9"}`))
						h.DapacaiVideoGeneration(c)
					} else {
						h.DapacaiVideoStatus(c)
					}
					if w.Code >= 400 {
						for _, entry := range logs.All() {
							t.Log(entry.Message, entry.ContextMap())
						}
					}
					return w.Code, w.Body.String()
				}
				code, body := run(true)
				require.Equal(t, 200, code, body)
				// Keep the fixture eligibility cache independent from the gateway's
				// real settlement mode; this mock has no user-balance database.
				billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil,
					&config.Config{RunMode: config.RunModeSimple}, nil)
				t.Cleanup(billing.Stop)
				h.billingCacheService = billing
				h.cfg.RunMode = config.RunModeStandard
				switch scenario {
				case "paid without pending":
					repo.committed, bindings.pending = true, nil
				case "unpaid without pending":
					bindings.pending = nil
				case "settlement failure":
					repo.fail = errors.New("mock settlement unavailable")
				}
				code, body = run(false)
				if scenario == "unpaid without pending" || scenario == "settlement failure" {
					require.Equal(t, 503, code, body)
					require.NotContains(t, body, `"status":"completed"`)
					require.Empty(t, bindings.billed, "failed settlement releases its claim")
				} else {
					require.Equal(t, 200, code, body)
					expected := 0.5
					if fixedJob {
						expected = 1.56
					}
					require.Contains(t, body, fmt.Sprintf(`"amount":"%.10f"`, expected))
					count := len(repo.commands)
					code, body = run(false)
					require.Equal(t, 200, code, body)
					require.Len(t, repo.commands, count, "paid GET must never bill again")
					if scenario == "settles once" {
						require.Len(t, repo.commands, 1)
						require.InDelta(t, expected, repo.commands[0].BalanceCost, 0.000001)
					}
				}
				slots.assertReleased(t)
			})
		}
	}
}

func TestDapacaiGenerationIsIsolatedAndRequiresItsOwnPrice(t *testing.T) {
	for _, scenario := range []string{"accepted", "no price", "old account", "upstream unavailable"} {
		t.Run(scenario, func(t *testing.T) {
			base := "https://api.dapacai.com"
			if scenario == "old account" {
				base = "https://api.atlascloud.ai"
			}
			h, slots, bindings, upstream := newGrokMediaSlotHandler(t, false, false, service.PlatformOpenAI, base)
			c, w := grokMediaSlotContext(context.Background(), true)
			key, ok := middleware.GetAPIKeyFromContext(c)
			require.True(t, ok)
			key.Group.Platform = service.PlatformOpenAI
			if scenario != "no price" {
				key.Group.VideoModelPrices = map[string]map[string]float64{"seedance-2.0": {"native": 0.1}}
			}
			c.Request = c.Request.Clone(c.Request.Context())
			c.Request.URL.Path = "/v1/videos"
			c.Request.Body = io.NopCloser(strings.NewReader(`{"model":"seedance-2.0","prompt":"test","duration":5,"ratio":"16:9"}`))
			upstream.call = func(req *http.Request, _ int64) (*http.Response, error) {
				require.Equal(t, "https://api.dapacai.com/v1/videos", req.URL.String())
				status, body := 200, `{"id":"task_owned","status":"queued"}`
				if scenario == "upstream unavailable" {
					status, body = 503, `{"error":{"message":"not available"}}`
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}},
					Body: io.NopCloser(strings.NewReader(body))}, nil
			}
			h.DapacaiVideoGeneration(c)
			slots.assertReleased(t)
			switch scenario {
			case "accepted":
				require.Equal(t, 200, w.Code, w.Body.String())
				require.Contains(t, w.Body.String(), "dapacai:task_owned")
				require.Equal(t, 1, upstream.calls)
				require.NotEmpty(t, bindings.pending)
			case "no price":
				require.Equal(t, 400, w.Code, w.Body.String())
				require.Contains(t, w.Body.String(), "pricing_not_configured")
				require.Zero(t, upstream.calls)
			case "old account":
				require.Equal(t, 503, w.Code, w.Body.String())
				require.Zero(t, upstream.calls)
			case "upstream unavailable":
				require.Equal(t, 503, w.Code, w.Body.String())
				require.Equal(t, 1, upstream.calls, "must never fail over a non-idempotent video submission")
			}
		})
	}
}
