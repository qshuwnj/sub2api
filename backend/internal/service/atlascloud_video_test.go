//go:build unit

package service

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type atlasCloudClaimTTLCache struct {
	GatewayCache
	ttl time.Duration
}

func (c *atlasCloudClaimTTLCache) ClaimGrokVideoBilled(_ context.Context, _ string, ttl time.Duration) (bool, error) {
	c.ttl = ttl
	return true, nil
}

func TestAtlasCloudVideoBillingLeaseAllowsCrashRecovery(t *testing.T) {
	cache := &atlasCloudClaimTTLCache{}
	svc := &OpenAIGatewayService{cache: cache}
	_, err := svc.ClaimGrokVideoBilling(context.Background(), "atlascloud:job", 1, 2)
	require.NoError(t, err)
	require.Equal(t, 2*time.Minute, cache.ttl)
	_, err = svc.ClaimGrokVideoBilling(context.Background(), "native-job", 1, 2)
	require.NoError(t, err)
	require.Equal(t, 48*time.Hour, cache.ttl, "keep non-AtlasCloud claims unchanged")
}

func TestAtlasCloudVideoDefaults(t *testing.T) {
	for _, tc := range []struct {
		model, resolution, ratio string
		duration                 int
	}{
		{"google/gemini-omni-1.1-flash/text-to-video", "720p", "16:9", 10},
		{"minimax/h3-developer/text-to-video", "768p", "1:1", 8},
		{"xai/grok-imagine-video/text-to-video", "720p", "16:9", 8},
	} {
		t.Run(tc.model, func(t *testing.T) {
			info, err := ParseAtlasCloudVideoRequest([]byte(fmt.Sprintf(`{"model":%q,"prompt":"draw"}`, tc.model)))
			require.NoError(t, err)
			require.Equal(t, tc.resolution, info.Resolution)
			require.Equal(t, tc.ratio, info.AspectRatio)
			require.Equal(t, tc.duration, info.DurationSeconds)
		})
	}
}

func TestAtlasCloudVideoValidation(t *testing.T) {
	for _, body := range []string{
		`{}`, `{"model":"other","prompt":"draw"}`,
		`{"model":"xai/grok-imagine-video/text-to-video","prompt":"draw","duration":16}`,
		`{"model":"xai/grok-imagine-video/text-to-video","prompt":"draw","duration":1.5}`,
		`{"model":"xai/grok-imagine-video/text-to-video","prompt":"draw","resolution":"4k"}`,
		`{"model":"minimax/h3-developer/text-to-video","prompt":"draw","resolution":"720p"}`,
		`{"model":"google/gemini-omni-1.1-flash/text-to-video","prompt":"draw","duration":2}`,
		`{"model":"google/gemini-omni-1.1-flash/text-to-video","prompt":"draw","thinking_level":"invalid"}`,
		`{"model":"xai/grok-imagine-video/text-to-video","prompt":"draw","image":"https://example.com/i.png"}`,
		`{"model":"xai/grok-imagine-video/text-to-video","prompt":123}`,
	} {
		_, err := ParseAtlasCloudVideoRequest([]byte(body))
		require.Error(t, err, body)
	}
}

func TestAtlasCloudVideoForwardAndPoll(t *testing.T) {
	upstream := &atlasImageUpstream{replies: []string{
		`{"code":200,"data":{"id":"job-123","status":"processing"}}`,
		`{"code":200,"data":{"id":"job-123","status":"completed","outputs":["https://cdn.example/video.mp4"]}}`,
	}}
	svc, c, _, account, _ := atlasImageFixture(t, upstream)
	body := []byte(`{"model":"minimax/h3-developer/text-to-video","prompt":"draw","resolution":"1440p-sr","ratio":"9:16","duration":12,"prompt_expansion":true}`)
	result, err := svc.ForwardAtlasCloudVideo(context.Background(), c, account, AtlasCloudEndpointVideoCreate, "", body)
	require.NoError(t, err)
	require.Equal(t, "atlascloud:job-123", result.ResponseID)
	require.Equal(t, "1440p-sr", result.VideoResolution)
	require.Equal(t, 12, result.VideoDurationSeconds)
	require.Equal(t, "https://api.atlascloud.ai/api/v1/model/generateVideo", upstream.urls[0])
	require.Equal(t, "Bearer upstream-only", upstream.auth[0])
	require.Equal(t, "1440p-sr", gjson.GetBytes(upstream.bodies[0], "resolution").String())
	result, err = svc.ForwardAtlasCloudVideo(context.Background(), c, account, AtlasCloudEndpointVideoStatus, "atlascloud:job-123", nil)
	require.NoError(t, err)
	require.Equal(t, 1, result.VideoCount)
	require.Equal(t, "https://api.atlascloud.ai/api/v1/model/prediction/job-123", upstream.urls[1])
}

func TestAtlasCloudVideoDefersCompletedResponseUntilSettlement(t *testing.T) {
	upstream := &atlasImageUpstream{replies: []string{`{"data":{"id":"task-atlas","status":"completed","outputs":["https://cdn.example/paid.mp4"]}}`}}
	svc, c, w, account, _ := atlasImageFixture(t, upstream)
	result, err := svc.ForwardAtlasCloudVideo(context.Background(), c, account, AtlasCloudEndpointVideoCreate, "",
		[]byte(`{"model":"minimax/h3-developer/text-to-video","prompt":"waves","duration":12,"resolution":"4k-sr"}`))
	require.NoError(t, err)
	require.Equal(t, 1, result.VideoCount)
	require.NotNil(t, result.AtlasCloudVideoResponse)
	require.Empty(t, w.Body.String(), "forwarding must not expose paid output before settlement")
}

func TestAtlasCloudVideoPricingPreservesNativeTiers(t *testing.T) {
	flat := 0.01
	group := &Group{VideoPrice480P: &flat, VideoModelPrices: map[string]map[string]float64{
		"minimax/h3-developer/text-to-video":         {"768P": 0.2, "1440p-sr": 0.3, "4k-sr": 0.4},
		"google/gemini-omni-1.1-flash/text-to-video": {"360p": 0.1, "4k": 0.5},
	}}
	group.VideoModelPrices = NormalizeVideoModelPrices(group.VideoModelPrices)
	require.Equal(t, 0.4, *group.GetVideoPriceForModel("minimax/h3-developer/text-to-video", "4k-sr"))
	require.Nil(t, group.GetVideoPriceForModel("xai/grok-imagine-video/text-to-video", "720p"))
	cost := (&BillingService{}).CalculateVideoCost("minimax/h3-developer/text-to-video", "1440p-sr", 1, 12, group.VideoPriceConfig(), 1)
	require.InDelta(t, 3.6, cost.TotalCost, 1e-9)
}

func TestAtlasCloudVideoReservationUsesVideoMultiplier(t *testing.T) {
	model := "minimax/h3-developer/text-to-video"
	key := &APIKey{Group: &Group{VideoRateIndependent: true, VideoRateMultiplier: 3,
		VideoModelPrices: map[string]map[string]float64{model: {"4k-sr": 0.4}}}}
	cost := (inflightEstimateDeps{billing: &BillingService{}}).estimateOne(context.Background(), key, model,
		InflightEstimateRequest{Kind: InflightEstimateVideo, Units: 1, VideoResolution: "4k-sr", VideoDurationSeconds: 8}, 1, 1)
	require.InDelta(t, 9.6, cost, 1e-9)
}

func TestAtlasCloudVideoWrongAccountsAndUnsafeTasksNeverCallUpstream(t *testing.T) {
	for _, mutation := range []string{"oauth", "grok", "wrong-host", "http", "query", "path", "port"} {
		t.Run(mutation, func(t *testing.T) {
			upstream := &atlasImageUpstream{}
			svc, c, _, account, _ := atlasImageFixture(t, upstream)
			switch mutation {
			case "oauth":
				account.Type = AccountTypeOAuth
			case "grok":
				account.Platform = PlatformGrok
			case "wrong-host":
				account.Credentials["base_url"] = "https://api.atlascloud.ai.example/api/v1"
			case "http":
				account.Credentials["base_url"] = "http://api.atlascloud.ai/api/v1"
			case "query":
				account.Credentials["base_url"] = "https://api.atlascloud.ai/api/v1?extra=1"
			case "path":
				account.Credentials["base_url"] = "https://api.atlascloud.ai/api/v1/responses"
			case "port":
				account.Credentials["base_url"] = "https://api.atlascloud.ai:444/api/v1"
			}
			_, err := svc.ForwardAtlasCloudVideo(context.Background(), c, account, AtlasCloudEndpointVideoCreate, "",
				[]byte(`{"model":"xai/grok-imagine-video/text-to-video","prompt":"draw"}`))
			require.Error(t, err)
			require.Empty(t, upstream.methods)
		})
	}
	for _, id := range []string{"task", "seedance:task", "atlascloud:../keys", "atlascloud:%2Fkeys", "atlascloud:..", "atlascloud:"} {
		upstream := &atlasImageUpstream{}
		svc, c, _, account, _ := atlasImageFixture(t, upstream)
		_, err := svc.ForwardAtlasCloudVideo(context.Background(), c, account, AtlasCloudEndpointVideoStatus, id, nil)
		require.Error(t, err, id)
		require.Empty(t, upstream.methods)
	}
}

func TestAtlasCloudVideoNeverLeaksErrorsOrFollowsPollURL(t *testing.T) {
	for _, raw := range []string{
		`{"code":500,"data":{"id":"job","status":"processing"},"message":"SECRET_KEY"}`,
		`{"data":{"id":"job","status":"unknown","error":"SECRET_KEY"}}`,
		`{"data":{"id":"job","status":"completed","outputs":["http://private.local/video"]}}`,
	} {
		upstream := &atlasImageUpstream{replies: []string{raw}}
		svc, c, w, account, _ := atlasImageFixture(t, upstream)
		_, err := svc.ForwardAtlasCloudVideo(context.Background(), c, account, AtlasCloudEndpointVideoCreate, "",
			[]byte(`{"model":"xai/grok-imagine-video/text-to-video","prompt":"draw"}`))
		require.Error(t, err)
		require.NotContains(t, err.Error(), "SECRET_KEY")
		require.NotContains(t, w.Body.String(), "SECRET_KEY")
		require.Len(t, upstream.methods, 1)
	}
	upstream := &atlasImageUpstream{replies: []string{
		`{"data":{"id":"job","status":"processing","urls":{"get":"https://other.example/keys"}}}`,
	}}
	svc, c, _, account, _ := atlasImageFixture(t, upstream)
	_, err := svc.ForwardAtlasCloudVideo(context.Background(), c, account, AtlasCloudEndpointVideoStatus, "atlascloud:job", nil)
	require.NoError(t, err)
	require.Equal(t, "https://api.atlascloud.ai/api/v1/model/prediction/job", upstream.urls[0])
}
