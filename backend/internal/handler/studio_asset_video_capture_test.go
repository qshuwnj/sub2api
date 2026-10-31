//go:build unit

package handler

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

type studioVideoHandlerRepository struct {
	quotedHandlerRepository
	service.StudioAssetRepository
	service.VideoResultRepository
	intake    *service.StudioVideoIntake
	video     *service.VideoResult
	failSave  bool
	failQueue bool
	deadline  time.Duration
	receipt   bool
}

func (r *studioVideoHandlerRepository) FindStudioNativeVideoReceipt(_ context.Context, owner service.VideoAttemptOwner, task string) (*service.VideoResult, error) {
	if !r.receipt || r.intake == nil || r.intake.Owner != (service.StudioAssetOwner{UserID: owner.UserID, APIKeyID: owner.APIKeyID}) ||
		r.intake.Resource != "task:"+task {
		return nil, service.ErrVideoResultNotFound
	}
	return &service.VideoResult{UserID: owner.UserID, APIKeyID: owner.APIKeyID, Resource: "task:" + task, TaskID: task, AccountID: r.intake.AccountID}, nil
}

func (r *studioVideoHandlerRepository) SaveStudioVideoIntake(ctx context.Context, in *service.StudioVideoIntake) error {
	requireDeadline, _ := ctx.Deadline()
	r.deadline = time.Until(requireDeadline)
	if r.failSave {
		return errors.New("private database failure")
	}
	copy := *in
	if r.intake == nil {
		r.intake = &copy
	} else {
		r.intake.Completed = r.intake.Completed || in.Completed
	}
	return nil
}

func (r *studioVideoHandlerRepository) GetStudioVideoIntake(_ context.Context, o service.StudioAssetOwner, resource string) (*service.StudioVideoIntake, error) {
	if r.intake == nil || r.intake.Owner != o || r.intake.Resource != resource {
		return nil, service.ErrStudioAssetNotFound
	}
	return r.intake, nil
}

func (*studioVideoHandlerRepository) ProcessStudioVideoIntakes(context.Context, int) error {
	return nil
}
func (*studioVideoHandlerRepository) ReconcileStudioAssets(context.Context, service.StudioAssetOwner, []string) (*service.StudioAssetReconcileResult, error) {
	return &service.StudioAssetReconcileResult{Queued: 1}, nil
}

func (r *studioVideoHandlerRepository) QueueVideoResult(_ context.Context, v *service.VideoResult) error {
	if r.failQueue {
		return service.ErrVideoResultUnavailable
	}
	copy := *v
	r.video = &copy
	return nil
}

func TestStudioAssetVideoNativeCompletionPreservesProofAcrossQueueFailure(t *testing.T) {
	for _, mismatch := range []bool{false, true} {
		repo := &studioVideoHandlerRepository{intake: &service.StudioVideoIntake{
			Owner: service.StudioAssetOwner{UserID: 10, APIKeyID: 20}, Resource: "task:task",
			AccountID: 1, Model: "original-model", Prompt: "original prompt"}, failQueue: true, receipt: true}
		h, _, _, _ := newGrokMediaSlotHandlerWithBilling(t, false, false, repo)
		h.gatewayService.SetStudioAssetStorage(nil)
		h.gatewayService.SetVideoResultStorage(nil)
		accountID := int64(1)
		if mismatch {
			accountID = 2
		}
		err := h.gatewayService.CompleteStudioNativeVideo(context.Background(), repo.intake.Owner, "task:task", "https://vidgen.x.ai/owned.mp4", accountID, true)
		require.Error(t, err)
		require.Equal(t, !mismatch, repo.intake.Completed, "owned success proof must survive an archive enqueue outage, never an account mismatch")
		require.Nil(t, repo.video)
	}
}

func TestStudioAssetVideoNativeProtectedResultQueuesNoCredentials(t *testing.T) {
	repo := &studioVideoHandlerRepository{intake: &service.StudioVideoIntake{
		Owner: service.StudioAssetOwner{UserID: 10, APIKeyID: 20}, Resource: "task:task",
		AccountID: 1, Model: "original-model", Prompt: "original prompt"}, receipt: true}
	h, _, _, upstream := newGrokMediaSlotHandlerWithBilling(t, false, false, repo)
	h.gatewayService.SetStudioAssetStorage(nil)
	h.gatewayService.SetVideoResultStorage(nil)
	require.NoError(t, h.gatewayService.CompleteStudioNativeVideo(context.Background(), repo.intake.Owner, "task:task", "", 1, true))
	require.True(t, repo.intake.Completed)
	require.NotNil(t, repo.video)
	require.Equal(t, "task:task", repo.video.Resource)
	require.Empty(t, repo.video.SourceURL, "authenticated content access must persist an owned task reference, not credentials or signed links")
	require.Zero(t, upstream.calls, "request collection queues only; archival belongs to the bounded worker")
}
func (r *studioVideoHandlerRepository) GetVideoResult(_ context.Context, o service.VideoAttemptOwner, resource string) (*service.VideoResult, error) {
	if r.video == nil || r.video.UserID != o.UserID || r.video.APIKeyID != o.APIKeyID || r.video.Resource != resource {
		return nil, service.ErrVideoResultNotFound
	}
	return r.video, nil
}

// A missing submission hook loses original prompt after caches and body expire.
func TestStudioAssetVideoSubmissionPersistsOriginalIntentBeforeResponse(t *testing.T) {
	for _, fail := range []bool{false, true} {
		repo := &studioVideoHandlerRepository{quotedHandlerRepository: quotedHandlerRepository{attempt: quotedHandlerAttempt(t)}, failSave: fail}
		h, slots, _, upstream := newGrokMediaSlotHandlerWithBilling(t, false, false, repo, service.PlatformOpenAI, "https://api.atlascloud.ai")
		h.gatewayService.SetStudioAssetStorage(nil)
		c, w := atlasVideoContext(t, true, false)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos/generations", strings.NewReader(quotedHandlerBody))
		c.Request.Header.Set("Idempotency-Key", "owned-attempt")
		c.Request.Header.Set("X-Studio-Library", "save")
		h.AtlasCloudVideoGeneration(c)
		require.Equal(t, 200, w.Code, w.Body.String())
		require.Contains(t, w.Body.String(), "result.mp4", "library errors must preserve the paid result")
		require.Contains(t, w.Body.String(), `"library":`)
		require.NotContains(t, w.Body.String(), "private database failure")
		if fail {
			require.Contains(t, w.Body.String(), `"status":"warning"`)
		} else {
			require.NotNil(t, repo.intake)
			require.Equal(t, service.StudioAssetOwner{UserID: 10, APIKeyID: 20}, repo.intake.Owner)
			require.Equal(t, "attempt:owned-attempt", repo.intake.Resource)
			require.Equal(t, "google/veo3.1/text-to-video", repo.intake.Model)
			require.Equal(t, "private", repo.intake.Prompt)
			require.JSONEq(t, `{"aspect_ratio":"16:9","duration":8,"resolution":"720p"}`, string(repo.intake.Metadata))
			require.Greater(t, repo.deadline, time.Duration(0))
			require.LessOrEqual(t, repo.deadline, 10*time.Second)
		}
		require.Zero(t, repo.reserves)
		require.Zero(t, repo.captures)
		require.Zero(t, upstream.calls)
		slots.assertReleased(t)
	}
}

func TestStudioAssetVideoNativeSubmissionUsesActualTaskIdentity(t *testing.T) {
	repo := &studioVideoHandlerRepository{}
	h, slots, _, upstream := newGrokMediaSlotHandlerWithBilling(t, false, false, repo)
	h.gatewayService.SetStudioAssetStorage(nil)
	h.gatewayService.SetVideoResultStorage(nil)
	c, w := grokMediaSlotContext(context.Background(), true)
	c.Request.Header.Set("X-Studio-Library", "save")
	h.GrokVideoGeneration(c)
	require.Equal(t, 200, w.Code, w.Body.String())
	require.NotNil(t, repo.intake)
	require.Equal(t, "task:task", repo.intake.Resource, "native IDs must not be fabricated AtlasCloud IDs")
	require.Equal(t, "grok-imagine-video", repo.intake.Model)
	require.Equal(t, "test", repo.intake.Prompt)
	require.False(t, repo.intake.Completed)
	require.Contains(t, w.Body.String(), `"library":`)
	require.Equal(t, 1, upstream.calls, "collection cannot submit a second generation")
	slots.assertReleased(t)

	upstream.call = func(req *http.Request, _ int64) (*http.Response, error) {
		require.Equal(t, http.MethodGet, req.Method)
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{"request_id":"task","status":"done","video":{"url":"https://vidgen.x.ai/video.mp4"}}`))}, nil
	}
	c, w = grokMediaSlotContext(context.Background(), false)
	h.GrokVideoStatus(c)
	require.Equal(t, 200, w.Code, w.Body.String())
	require.False(t, repo.intake.Completed, "supplier success without committed customer usage must not become library proof")
	require.Nil(t, repo.video)
	repo.receipt = true
	c, w = grokMediaSlotContext(context.Background(), false)
	h.GrokVideoStatus(c)
	require.Equal(t, 200, w.Code, w.Body.String())
	require.NotNil(t, repo.video)
	require.Equal(t, "task:task", repo.video.Resource)
	require.Equal(t, int64(10), repo.video.UserID)
	require.Equal(t, int64(20), repo.video.APIKeyID)
	require.Equal(t, "https://vidgen.x.ai/video.mp4", repo.video.SourceURL)
	require.Equal(t, 3, upstream.calls)
	require.Zero(t, repo.reserves)
	require.Zero(t, repo.captures)
	slots.assertReleased(t)
}

func TestStudioAssetVideoNativeStatusRejectsInvalidSuccessfulOutput(t *testing.T) {
	for _, scenario := range []struct {
		name, source string
		status       int
	}{
		{"unsupported URL", "https://attacker.example/unowned.mp4", 200},
		{"redirect", "https://vidgen.x.ai/owned.mp4", 302},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			repo := &studioVideoHandlerRepository{intake: &service.StudioVideoIntake{
				Owner: service.StudioAssetOwner{UserID: 10, APIKeyID: 20}, Resource: "task:task", AccountID: 1}}
			h, slots, _, upstream := newGrokMediaSlotHandlerWithBilling(t, false, false, repo)
			h.gatewayService.SetStudioAssetStorage(nil)
			h.gatewayService.SetVideoResultStorage(nil)
			upstream.call = func(*http.Request, int64) (*http.Response, error) {
				return &http.Response{StatusCode: scenario.status, Header: http.Header{"Content-Type": {"application/json"}},
					Body: io.NopCloser(strings.NewReader(`{"request_id":"task","status":"done","video":{"url":"` + scenario.source + `"}}`))}, nil
			}
			c, _ := grokMediaSlotContext(context.Background(), false)
			h.GrokVideoStatus(c)
			require.False(t, repo.intake.Completed, "only validated successful owned output can complete intake")
			require.Nil(t, repo.video)
			slots.assertReleased(t)
		})
	}
}
