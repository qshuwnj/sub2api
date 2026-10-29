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

	"github.com/Wei-Shaw/sub2api/internal/config"
	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func atlasVideoContext(t *testing.T, create, priced bool) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	c, w := grokMediaSlotContext(context.Background(), create)
	key, _ := middleware.GetAPIKeyFromContext(c)
	key.Group.Platform = service.PlatformOpenAI
	if priced {
		key.Group.VideoModelPrices = map[string]map[string]float64{
			"minimax/h3-developer/text-to-video": {"4k-sr": 0.4},
		}
	}
	body := ""
	if create {
		body = `{"model":"minimax/h3-developer/text-to-video","prompt":"waves","duration":12,"resolution":"4k-sr"}`
	}
	c.Request = httptest.NewRequest(c.Request.Method, "/v1/videos/generations", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{{Key: "request_id", Value: "atlascloud:task-atlas"}}
	return c, w
}

func TestAtlasCloudVideoHandlerLifecycleAndOwnership(t *testing.T) {
	h, slots, bindings, upstream := newGrokMediaSlotHandler(t, false, false, service.PlatformOpenAI, "https://api.atlascloud.ai/api/v1")
	var owner int64
	upstream.call = func(req *http.Request, id int64) (*http.Response, error) {
		if req.Method == http.MethodPost {
			owner = id
			require.Equal(t, "https://api.atlascloud.ai/api/v1/model/generateVideo", req.URL.String())
		} else {
			require.Equal(t, owner, id)
			require.Equal(t, "https://api.atlascloud.ai/api/v1/model/prediction/task-atlas", req.URL.String())
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{"code":200,"data":{"id":"task-atlas","status":"processing"}}`))}, nil
	}
	c, w := atlasVideoContext(t, true, false)
	h.AtlasCloudVideoGeneration(c)
	require.Equal(t, 400, w.Code, w.Body.String())
	require.Zero(t, upstream.calls)
	slots.assertReleased(t)
	c, w = atlasVideoContext(t, true, true)
	h.AtlasCloudVideoGeneration(c)
	require.Equal(t, 200, w.Code, w.Body.String())
	require.Positive(t, owner)
	require.Len(t, bindings.pending, 1)
	slots.assertReleased(t)
	c, w = atlasVideoContext(t, false, true)
	h.AtlasCloudVideoStatus(c)
	require.Equal(t, 200, w.Code, w.Body.String())
	slots.assertReleased(t)
	for _, other := range []string{"user", "key", "group", "task", "provider"} {
		c, w = atlasVideoContext(t, false, true)
		key, _ := middleware.GetAPIKeyFromContext(c)
		switch other {
		case "user":
			c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 11, Concurrency: 5})
		case "key":
			key.ID = 21
		case "group":
			group := int64(25)
			key.GroupID = &group
		case "task":
			c.Params = gin.Params{{Key: "request_id", Value: "atlascloud:other"}}
		case "provider":
			c.Params = gin.Params{{Key: "request_id", Value: "seedance:task-atlas"}}
		}
		before := upstream.calls
		h.AtlasCloudVideoStatus(c)
		require.Equal(t, 404, w.Code, w.Body.String())
		require.Equal(t, before, upstream.calls)
		slots.assertReleased(t)
	}
	c, _ = atlasVideoContext(t, false, true)
	key, _ := middleware.GetAPIKeyFromContext(c)
	subject, _ := middleware.GetAuthSubjectFromContext(c)
	result := &service.OpenAIForwardResult{ResponseID: "atlascloud:task-atlas", VideoCount: 1}
	for i := range 20 {
		billed := prepareGrokVideoCompletionBilling(context.Background(), h, zap.NewNop(), key, subject, result.ResponseID, result)
		if i == 0 {
			require.NotNil(t, billed)
			require.Equal(t, "minimax/h3-developer/text-to-video", billed.BillingModel)
			require.Equal(t, "4k-sr", billed.VideoResolution)
			require.Equal(t, 12, billed.VideoDurationSeconds)
		} else {
			require.Nil(t, billed)
		}
	}
	require.Len(t, bindings.billed, 1)
}

func TestAtlasCloudImageHandlerMissingPriceNeverCallsUpstream(t *testing.T) {
	for _, model := range []string{"openai/gpt-image-2.5-flare/text-to-image", "xai/grok-imagine-image-2.0-developer/text-to-image", "openai/gpt-image-2/text-to-image",
		"qwen/qwen-image-2.0-pro/text-to-image", "openai/gpt-image-1-mini/text-to-image", "google/nano-banana-2/text-to-image"} {
		h, slots, _, upstream := newGrokMediaSlotHandler(t, false, false, service.PlatformOpenAI, "https://api.atlascloud.ai/api/v1")
		c, w := atlasVideoContext(t, true, false)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"`+model+`","prompt":"draw"}`))
		c.Request.Header.Set("Content-Type", "application/json")
		h.Images(c)
		require.Equal(t, 400, w.Code, w.Body.String())
		require.Contains(t, w.Body.String(), "pricing_not_configured")
		require.Zero(t, upstream.calls)
		slots.assertReleased(t)
	}
}

func TestAtlasCloudVideoBillingStateFailureNeverReleasesOutput(t *testing.T) {
	for _, scenario := range []string{"ownership", "snapshot", "create claim", "poll claim", "settlement marker", "claim in progress"} {
		t.Run(scenario, func(t *testing.T) {
			h, slots, bindings, upstream := newGrokMediaSlotHandler(t, false, false, service.PlatformOpenAI, "https://api.atlascloud.ai/api/v1")
			done := false
			upstream.call = func(*http.Request, int64) (*http.Response, error) {
				raw := `{"data":{"id":"task-atlas","status":"processing"}}`
				if done {
					raw = `{"data":{"id":"task-atlas","status":"completed","outputs":["https://cdn.example/paid.mp4"]}}`
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(raw))}, nil
			}
			create := scenario != "poll claim" && scenario != "settlement marker" && scenario != "claim in progress"
			if !create {
				c, w := atlasVideoContext(t, true, true)
				h.AtlasCloudVideoGeneration(c)
				require.Equal(t, 200, w.Code)
			}
			done = true
			switch scenario {
			case "ownership":
				bindings.bindErr = errors.New("cache unavailable")
			case "snapshot":
				bindings.pendingErr = errors.New("cache unavailable")
			case "settlement marker":
				bindings.pendingErr = errors.New("cache unavailable")
			case "claim in progress":
				c, _ := atlasVideoContext(t, false, true)
				key, _ := middleware.GetAPIKeyFromContext(c)
				claimed, err := h.gatewayService.ClaimGrokVideoBilling(context.Background(), "atlascloud:task-atlas", 10, key.ID)
				require.NoError(t, err)
				require.True(t, claimed)
			default:
				bindings.claimErr = errors.New("cache unavailable")
			}
			c, w := atlasVideoContext(t, create, true)
			if create {
				h.AtlasCloudVideoGeneration(c)
			} else {
				h.AtlasCloudVideoStatus(c)
			}
			require.Equal(t, 503, w.Code, "upstream calls=%d body=%s", upstream.calls, w.Body.String())
			require.NotContains(t, w.Body.String(), "paid.mp4")
			require.Contains(t, w.Body.String(), "billing_state_unavailable")
			slots.assertReleased(t)
		})
	}
}

func TestAtlasCloudVideoCompletedCreateAndRepeatedPollShareSettlement(t *testing.T) {
	h, slots, bindings, upstream := newGrokMediaSlotHandler(t, false, false, service.PlatformOpenAI, "https://api.atlascloud.ai/api/v1")
	upstream.call = func(*http.Request, int64) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{"data":{"id":"task-atlas","status":"completed","outputs":["https://cdn.example/paid.mp4"]}}`))}, nil
	}
	for i := 0; i < 3; i++ {
		c, w := atlasVideoContext(t, i == 0, true)
		if i > 0 {
			key, _ := middleware.GetAPIKeyFromContext(c)
			key.Group.VideoModelPrices = nil // A paid video survives a later price removal.
		}
		if i == 0 {
			h.AtlasCloudVideoGeneration(c)
		} else {
			h.AtlasCloudVideoStatus(c)
		}
		require.Equal(t, 200, w.Code, "iteration=%d upstream calls=%d body=%s", i, upstream.calls, w.Body.String())
		require.Contains(t, w.Body.String(), "paid.mp4")
		pending, err := h.gatewayService.LoadGrokVideoPendingBilling(context.Background(), "atlascloud:task-atlas", 10, 20)
		require.NoError(t, err)
		require.True(t, pending.Settled)
		require.Len(t, bindings.billed, 1)
		slots.assertReleased(t)
	}
}

func TestAtlasCloudCompletionCreateAndPollUseSameBillingFingerprint(t *testing.T) {
	body := []byte(`{"model":"minimax/h3-developer/text-to-video","prompt":"waves"}`)
	create := &service.UsageBillingCommand{RequestID: "video-job", APIKeyID: 20,
		RequestPayloadHash: service.HashUsageRequestPayload(grokMediaCompletionPayload(body, "atlascloud:job"))}
	poll := *create
	poll.RequestPayloadHash = service.HashUsageRequestPayload(grokMediaCompletionPayload(nil, "atlascloud:job"))
	create.Normalize()
	poll.Normalize()
	require.Equal(t, create.RequestFingerprint, poll.RequestFingerprint)
	require.Equal(t, body, grokMediaCompletionPayload(body, "native-job"), "preserve native behavior")
}

type atlasCloudBillingRepo struct {
	service.UsageBillingRepository
	commands  []*service.UsageBillingCommand
	fail      error
	committed bool
}

func (r *atlasCloudBillingRepo) Apply(_ context.Context, cmd *service.UsageBillingCommand) (*service.UsageBillingApplyResult, error) {
	cmd.Normalize()
	copy := *cmd
	r.commands = append(r.commands, &copy)
	if r.fail != nil {
		return nil, r.fail
	}
	if len(r.commands) > 1 && r.commands[0].RequestFingerprint != cmd.RequestFingerprint {
		return nil, service.ErrUsageBillingRequestConflict
	}
	r.committed = true
	// The real repository returns Applied=false for a previously committed charge.
	return &service.UsageBillingApplyResult{Applied: false}, nil
}

func (r *atlasCloudBillingRepo) HasCommittedUsageBilling(_ context.Context, requestID string, keyID int64) (bool, error) {
	if requestID != "grok-video:atlascloud:task-atlas" || keyID != 20 {
		return false, errors.New("wrong billing identity")
	}
	return r.committed, nil
}

func TestAtlasCloudVideoMarkerFailureRecoversAfterPriceChange(t *testing.T) {
	repo := &atlasCloudBillingRepo{}
	h, slots, bindings, upstream := newGrokMediaSlotHandlerWithBilling(t, false, false, repo, service.PlatformOpenAI, "https://api.atlascloud.ai/api/v1")
	upstream.call = func(*http.Request, int64) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{"data":{"id":"task-atlas","status":"processing"}}`))}, nil
	}
	c, w := atlasVideoContext(t, true, true)
	h.AtlasCloudVideoGeneration(c)
	require.Equal(t, 200, w.Code)
	var pending service.GrokVideoPendingBilling
	require.NoError(t, json.Unmarshal(bindings.pending["10:20:atlascloud:task-atlas"], &pending))
	require.False(t, pending.Settled)
	repo.committed = true // Billing committed; the pending marker write failed.
	upstream.call = func(*http.Request, int64) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{"data":{"id":"task-atlas","status":"completed","outputs":["https://cdn.example/paid.mp4"]}}`))}, nil
	}
	c, w = atlasVideoContext(t, false, false)
	h.AtlasCloudVideoStatus(c)
	require.Equal(t, 200, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "paid.mp4")
	require.NoError(t, json.Unmarshal(bindings.pending["10:20:atlascloud:task-atlas"], &pending))
	require.True(t, pending.Settled)
	require.Empty(t, repo.commands, "never recalculate or double bill an existing receipt")
	slots.assertReleased(t)
}

func TestAtlasCloudCompletionRecordUsageDurableRetryAndFailure(t *testing.T) {
	repo := &atlasCloudBillingRepo{}
	h, _, bindings, _ := newGrokMediaSlotHandlerWithBilling(t, false, false, repo, service.PlatformOpenAI, "https://api.atlascloud.ai/api/v1")
	h.cfg.RunMode = config.RunModeStandard
	record := func(create bool) error {
		c, _ := atlasVideoContext(t, create, true)
		key, _ := middleware.GetAPIKeyFromContext(c)
		key.Group.RateMultiplier = 1
		subject, _ := middleware.GetAuthSubjectFromContext(c)
		body := []byte(nil)
		if create {
			body = []byte(`{"model":"minimax/h3-developer/text-to-video","prompt":"waves"}`)
		}
		return recordGrokMediaUsage(c, h, zap.NewNop(), key, subject, nil,
			&service.Account{ID: 1, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey},
			&service.OpenAIForwardResult{Model: "minimax/h3-developer/text-to-video", ResponseID: "atlascloud:task-atlas",
				VideoCount: 1, VideoResolution: "4k-sr", VideoDurationSeconds: 12},
			"minimax/h3-developer/text-to-video", body, "atlascloud:task-atlas")
	}
	require.NoError(t, record(true))
	require.NoError(t, record(false))
	require.Len(t, repo.commands, 2)
	require.Equal(t, repo.commands[0].RequestID, repo.commands[1].RequestID)
	require.Equal(t, repo.commands[0].RequestFingerprint, repo.commands[1].RequestFingerprint)
	require.InDelta(t, 4.8, repo.commands[0].BalanceCost, 0.00001)
	claimed, err := h.gatewayService.ClaimGrokVideoBilling(context.Background(), "atlascloud:task-atlas", 10, 20)
	require.NoError(t, err)
	require.True(t, claimed)
	repo.fail = errors.New("durable settlement unavailable")
	require.ErrorIs(t, record(false), repo.fail)
	require.Empty(t, bindings.billed, "failed settlement must release its claim")
}
