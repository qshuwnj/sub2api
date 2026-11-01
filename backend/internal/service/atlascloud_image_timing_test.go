//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestAtlasCloudImageTimingSeparatesStagesWithoutLoggingMedia(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	ctx := logger.IntoContext(context.Background(), zap.New(core).With(zap.String("request_id", "timing-request")))
	upstream := &atlasImageUpstream{replies: []string{
		`{"data":{"id":"original-task","status":"processing"}}`,
		`{"data":{"id":"original-task","status":"completed","outputs":["https://cdn.example/result.png?sig=result-secret"]}}`,
	}}
	svc, c, _, account, _ := atlasImageFixture(t, upstream)
	body := []byte(`{"model":"openai/gpt-image-2-developer/edit","prompt":"prompt-secret","n":1,"size":"2048x1152","quality":"medium","images":["https://private.example/reference.png?token=reference-secret"]}`)
	c.Request = httptest.NewRequest(http.MethodPost, openAIImagesGenerationsEndpoint, bytes.NewReader(body)).WithContext(ctx)
	parsed, err := svc.ParseOpenAIImagesRequest(c, body)
	require.NoError(t, err)
	account.Credentials["model_mapping"] = map[string]any{parsed.Model: parsed.Model}
	result, err := svc.ForwardImages(ctx, c, account, body, parsed, "")
	require.NoError(t, err)
	require.Equal(t, parsed.Model, result.UpstreamModel)
	require.Equal(t, []string{"POST", "GET"}, upstream.methods)

	events := logs.FilterMessage("image_generation.stage_completed").All()
	require.NotEmpty(t, events, "each boundary must be timed, not just the final HTTP response")
	var stages []string
	for _, event := range events {
		fields := event.ContextMap()
		stages = append(stages, fields["stage"].(string))
		require.GreaterOrEqual(t, fields["stage_ms"], int64(0))
		require.Equal(t, "timing-request", fields["request_id"])
	}
	require.Equal(t, []string{"preflight", "reference_preparation", "submission", "upstream_wait", "response_preparation"}, stages)
	summaries := logs.FilterMessage("image_generation.finished").All()
	require.Len(t, summaries, 1)
	fields := summaries[0].ContextMap()
	require.Equal(t, "returned", fields["outcome"])
	require.Equal(t, int64(1), fields["submission_count"])
	require.Equal(t, int64(1), fields["poll_count"])
	require.Equal(t, int64(0), fields["reference_upload_count"])
	require.Greater(t, fields["total_ms"], int64(0))
	states := logs.FilterMessage("image_generation.upstream_status").All()
	require.Len(t, states, 2)
	require.Equal(t, "processing", states[0].ContextMap()["upstream_status"])
	require.Equal(t, "completed", states[1].ContextMap()["upstream_status"])
	for _, event := range logs.All() {
		raw, err := json.Marshal(event.ContextMap())
		require.NoError(t, err)
		for _, secret := range []string{"prompt-secret", "reference-secret", "result-secret", "upstream-only", "customer-only", "private.example", "cdn.example"} {
			require.NotContains(t, string(raw), secret)
		}
	}
}

func TestAtlasCloudImageTimingUploadAndFailureAreDistinct(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	ctx := logger.IntoContext(context.Background(), zap.New(core))
	upstream := &atlasImageUpstream{replies: []string{
		`{"code":200,"data":{"download_url":"https://storage.atlascloud.ai/reference.png"}}`,
		`{"data":{"id":"original-task","status":"unknown-secret-status","error":"upstream-only"}}`,
	}}
	svc, c, _, account, _ := atlasImageFixture(t, upstream)
	body := []byte(`{"model":"openai/gpt-image-1-mini/edit","prompt":"private-prompt","images":["data:image/png;base64,iVBORw0KGgo="],"n":1}`)
	c.Request = httptest.NewRequest(http.MethodPost, openAIImagesGenerationsEndpoint, bytes.NewReader(body)).WithContext(ctx)
	parsed, err := svc.ParseOpenAIImagesRequest(c, body)
	require.NoError(t, err)
	account.Credentials["model_mapping"] = map[string]any{parsed.Model: parsed.Model}
	_, err = svc.ForwardImages(ctx, c, account, body, parsed, "")
	require.Error(t, err)
	require.Len(t, upstream.methods, 2, "upload once, generate once, never resubmit")
	summaries := logs.FilterMessage("image_generation.finished").All()
	require.Len(t, summaries, 1)
	fields := summaries[0].ContextMap()
	require.Equal(t, "not_completed", fields["outcome"])
	require.Equal(t, int64(1), fields["reference_upload_count"])
	require.Equal(t, int64(1), fields["submission_count"])
	require.Equal(t, int64(0), fields["poll_count"])
	states := logs.FilterMessage("image_generation.upstream_status").All()
	require.Len(t, states, 1)
	require.Equal(t, "unknown", states[0].ContextMap()["upstream_status"])
	for _, event := range logs.All() {
		raw, err := json.Marshal(event.ContextMap())
		require.NoError(t, err)
		for _, secret := range []string{"private-prompt", "unknown-secret-status", "upstream-only", "iVBORw0KGgo", "storage.atlascloud.ai/reference"} {
			require.NotContains(t, string(raw), secret)
		}
	}
}

type delayedAtlasImageUpstream struct {
	*atlasImageUpstream
}

func (u *delayedAtlasImageUpstream) Do(req *http.Request, proxy string, id int64, concurrency int) (*http.Response, error) {
	if req.Method == http.MethodPost {
		timer := time.NewTimer(time.Second)
		defer timer.Stop()
		select {
		case <-req.Context().Done():
			return nil, req.Context().Err()
		case <-timer.C:
		}
	}
	return u.atlasImageUpstream.Do(req, proxy, id, concurrency)
}

func TestAtlasCloudImagePollingDoesNotAddFullDelayAfterSlowRequest(t *testing.T) {
	upstream := &atlasImageUpstream{replies: []string{
		`{"data":{"id":"original-task","status":"processing"}}`,
		`{"data":{"id":"original-task","status":"completed","outputs":["https://cdn.example/result.png"]}}`,
	}}
	svc, c, _, account, parsed := atlasImageFixture(t, upstream)
	svc.httpUpstream = &delayedAtlasImageUpstream{upstream}
	ctx, cancel := context.WithTimeout(context.Background(), 2800*time.Millisecond)
	defer cancel()
	result, err := svc.ForwardImages(ctx, c, account, parsed.Body, parsed, "")
	require.NoError(t, err, "a one-second HTTP request must not be followed by another full two-second sleep")
	require.Equal(t, "original-task", result.RequestID)
	require.Equal(t, []string{"POST", "GET"}, upstream.methods)
}
