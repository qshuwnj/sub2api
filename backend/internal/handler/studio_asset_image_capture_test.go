package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type studioCaptureRepository struct {
	service.UsageBillingRepository
	service.StudioAssetRepository
	assets   []*service.StudioAsset
	fail     bool
	deadline time.Duration
}

func (r *studioCaptureRepository) UpsertStudioAsset(ctx context.Context, asset *service.StudioAsset) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if deadline, ok := ctx.Deadline(); ok {
		r.deadline = time.Until(deadline)
	}
	if r.fail {
		return errors.New("database unavailable")
	}
	copy := *asset
	r.assets = append(r.assets, &copy)
	return nil
}

func studioCaptureHandler(repo *studioCaptureRepository) *OpenAIGatewayHandler {
	gateway := service.NewOpenAIGatewayService(nil, nil, repo, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	// Nil storage still persists URL retry sources but reports an independent warning.
	gateway.SetStudioAssetStorage(nil)
	return &OpenAIGatewayHandler{gatewayService: gateway}
}

func studioCaptureContext() *gin.Context {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
	c.Request.Header.Set("X-Studio-Library", "save")
	c.Request.Header.Set("X-Generation-ID", "client-cannot-select-this")
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{ID: 9, UserID: 7})
	return c
}

type studioCaptureObjectStore struct {
	saves int
}

func (s *studioCaptureObjectStore) Save(context.Context, string, string, []byte) (string, error) {
	s.saves++
	return "https://storage.example/private", nil
}
func (*studioCaptureObjectStore) OpenStudioAsset(context.Context, string, string) (*service.VideoResultContent, error) {
	return &service.VideoResultContent{Body: io.NopCloser(bytes.NewReader(nil))}, nil
}
func (*studioCaptureObjectStore) StudioAssetURL(context.Context, string, time.Duration) (string, error) {
	return "https://storage.example/private", nil
}

func TestStudioAssetImageCaptureQueuedOnlyAfterDurableSources(t *testing.T) {
	repo := &studioCaptureRepository{}
	h := studioCaptureHandler(repo)
	store := &studioCaptureObjectStore{}
	uploader := service.NewImageResultUploader(store, "images/", 1024*1024, nil)
	h.gatewayService.SetStudioAssetStorage(func() (*service.ImageResultUploader, bool) { return uploader, true })
	c := studioCaptureContext()
	h.prepareStudioImages(c, &service.OpenAIImagesRequest{Model: "model", Prompt: "prompt"}, nil)
	status := service.CompleteStudioImages(c, []service.StudioImageOutput{{URL: "https://images.example/one.png"}})
	require.Equal(t, "queued", status.Status)
	require.Empty(t, status.Code)
	require.Len(t, repo.assets, 1)
	require.Equal(t, "pending", repo.assets[0].State)
	require.Zero(t, store.saves, "delivery only persists the outbox; archival stays in the worker")
}

func TestStudioAssetImageCapturePersistsDetachedOwnedSourcesOnce(t *testing.T) {
	repo := &studioCaptureRepository{}
	h := studioCaptureHandler(repo)
	c := studioCaptureContext()
	ctx, cancel := context.WithCancel(c.Request.Context())
	c.Request = c.Request.WithContext(ctx)
	cancel()
	parsed := &service.OpenAIImagesRequest{Model: "original-model", Prompt: "original prompt", Size: "768x1024"}
	h.prepareStudioImages(c, parsed, json.RawMessage(`{"aspect_ratio":"3:4","seed":9007199254740993,"width":768,"private":{"token":"secret"}}`))
	outputs := []service.StudioImageOutput{{URL: "https://images.example/one.png"}, {URL: "https://images.example/two.png"}}
	status := service.CompleteStudioImages(c, outputs)
	require.Equal(t, "warning", status.Status, "unavailable storage must not report saved/queued")
	require.Equal(t, "studio_asset_unavailable", status.Code)
	require.NotEmpty(t, status.GenerationID)
	require.NotEqual(t, "client-cannot-select-this", status.GenerationID)
	require.Len(t, repo.assets, 2)
	for i, asset := range repo.assets {
		require.Equal(t, service.StudioAssetOwner{UserID: 7, APIKeyID: 9}, asset.Owner)
		require.Equal(t, status.GenerationID, asset.GenerationID)
		require.Equal(t, i, asset.OutputIndex)
		require.Equal(t, "original-model", asset.Model)
		require.Equal(t, "original prompt", asset.Prompt)
		require.JSONEq(t, `{"aspect_ratio":"3:4","seed":9007199254740993,"width":768}`, string(asset.Metadata))
	}
	require.Greater(t, repo.deadline, time.Duration(0))
	require.LessOrEqual(t, repo.deadline, 10*time.Second)
	service.CompleteStudioImages(c, outputs)
	require.Len(t, repo.assets, 2, "nested completion must not recollect")
}

func TestStudioAssetImageCaptureNonStudioAndMissingOwnerNeverPersist(t *testing.T) {
	for _, invalid := range []string{"no_opt_in", "missing_key", "invalid_key"} {
		c := studioCaptureContext()
		switch invalid {
		case "no_opt_in":
			c.Request.Header.Del("X-Studio-Library")
		case "missing_key":
			delete(c.Keys, string(middleware2.ContextKeyAPIKey))
		case "invalid_key":
			c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{ID: 9})
		}
		repo := &studioCaptureRepository{}
		h := studioCaptureHandler(repo)
		h.prepareStudioImages(c, &service.OpenAIImagesRequest{Model: "model", Prompt: "prompt"}, nil)
		service.CompleteStudioImages(c, []service.StudioImageOutput{{URL: "https://images.example/one.png"}})
		require.Empty(t, repo.assets, invalid)
	}
}

func TestStudioAssetImageCaptureDatabaseFailureIndependentWarning(t *testing.T) {
	repo := &studioCaptureRepository{fail: true}
	h := studioCaptureHandler(repo)
	c := studioCaptureContext()
	h.prepareStudioImages(c, &service.OpenAIImagesRequest{Model: "model", Prompt: "prompt"}, nil)
	status := service.CompleteStudioImages(c, []service.StudioImageOutput{{URL: "https://images.example/one.png"}})
	require.Equal(t, "warning", status.Status)
	require.Equal(t, "studio_asset_unavailable", status.Code)
	require.Empty(t, repo.assets)
	require.False(t, c.Writer.Written(), "collector must never replace media response")
}

func TestStudioAssetImageAsyncStableTaskIdentity(t *testing.T) {
	c := studioCaptureContext()
	repo := &studioCaptureRepository{}
	h := studioCaptureHandler(repo)
	tasks := service.NewImageTaskServiceWithUploader(&asyncImageMemoryStore{tasks: map[string]*service.ImageTaskRecord{}}, nil, time.Hour, time.Minute)
	task, err := tasks.Create(context.Background(), service.ImageTaskOwner{UserID: 7, APIKeyID: 9})
	require.NoError(t, err)
	async := &AsyncImageHandler{tasks: tasks, openAI: h, execute: func(_ string, taskCtx *gin.Context) {
		h.prepareStudioImages(taskCtx, &service.OpenAIImagesRequest{Model: "model", Prompt: "prompt"}, nil)
		status := service.CompleteStudioImages(taskCtx, []service.StudioImageOutput{{URL: "https://images.example/one.png"}})
		service.CompleteStudioImages(taskCtx, []service.StudioImageOutput{{URL: "https://images.example/one.png"}})
		taskCtx.JSON(http.StatusOK, gin.H{"data": []gin.H{{"url": "https://images.example/one.png"}}, "library": status})
	}}
	taskCtx, recorder, cancel := newAsyncImageContext(c, []byte(`{"model":"model","prompt":"prompt"}`), time.Minute)
	async.run(task.ID, service.PlatformOpenAI, taskCtx, recorder, cancel)
	require.Len(t, repo.assets, 1)
	require.Equal(t, task.ID, repo.assets[0].GenerationID)
	completed, err := tasks.Get(context.Background(), service.ImageTaskOwner{UserID: 7, APIKeyID: 9}, task.ID)
	require.NoError(t, err)
	require.Contains(t, string(completed.Result), `"generation_id":"`+task.ID+`"`)
}
