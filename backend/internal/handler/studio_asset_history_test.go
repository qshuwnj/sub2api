package handler

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

type studioImageHistoryRepository struct{ studioCaptureRepository }

func (*studioImageHistoryRepository) ReconcileStudioAssets(_ context.Context, _ service.StudioAssetOwner, ids []string) (*service.StudioAssetReconcileResult, error) {
	return &service.StudioAssetReconcileResult{Unrecoverable: len(ids)}, nil
}

func TestStudioAssetHistoryRecoversOnlyOwnedLiveImageTaskResults(t *testing.T) {
	for _, scenario := range []string{"owned live", "owned live storage disabled", "database failure", "foreign key", "expired", "failed", "URL alone"} {
		t.Run(scenario, func(t *testing.T) {
			repo := &studioImageHistoryRepository{}
			gateway := service.NewOpenAIGatewayService(nil, nil, repo, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
			store := &studioCaptureObjectStore{}
			uploader := service.NewImageResultUploader(store, "images/", 1024*1024, nil)
			gateway.SetStudioAssetStorage(func() (*service.ImageResultUploader, bool) { return uploader, true })
			if scenario == "owned live storage disabled" {
				gateway.SetStudioAssetStorage(nil)
			}
			if scenario == "database failure" {
				repo.fail = true
			}
			h := &OpenAIGatewayHandler{gatewayService: gateway}
			mem := &asyncImageMemoryStore{tasks: map[string]*service.ImageTaskRecord{}}
			tasks := service.NewImageTaskServiceWithUploader(mem, nil, time.Hour, time.Minute)
			task, err := tasks.Create(context.Background(), service.ImageTaskOwner{UserID: 7, APIKeyID: 9})
			require.NoError(t, err)
			require.NoError(t, tasks.Complete(context.Background(), task.ID, 200, []byte(`{"data":[{"url":"https://images.example/owned.png"}]}`)))
			completed := time.Now().Add(-10 * time.Minute).Unix()
			mem.tasks[task.ID].CompletedAt = &completed
			NewAsyncImageHandler(tasks, h)
			id := task.ID
			switch scenario {
			case "foreign key":
				mem.tasks[task.ID].APIKeyID = 10
			case "expired":
				mem.tasks[task.ID].ExpiresAt = time.Now().Add(-time.Minute).Unix()
			case "failed":
				mem.tasks[task.ID].Status = service.ImageTaskStatusFailed
			case "URL alone":
				id = "https://images.example/owned.png"
			}
			result, err := gateway.StudioAssets().Reconcile(context.Background(), service.StudioAssetOwner{UserID: 7, APIKeyID: 9}, []string{id})
			if scenario == "URL alone" {
				require.ErrorIs(t, err, service.ErrStudioAssetInvalidRequest)
				require.Empty(t, repo.assets)
				return
			}
			require.NoError(t, err)
			if scenario == "owned live" || scenario == "owned live storage disabled" {
				require.Equal(t, 1, result.Queued)
				require.Len(t, repo.assets, 1)
				require.Equal(t, task.ID, repo.assets[0].GenerationID)
				require.Equal(t, "https://images.example/owned.png", repo.assets[0].SourceURL)
				require.Empty(t, repo.assets[0].Model)
				require.Empty(t, repo.assets[0].Prompt)
				require.Equal(t, completed, repo.assets[0].GeneratedAt.Unix(), "history must retain actual generation time rather than recovery time")
			} else {
				require.Equal(t, 1, result.Unrecoverable)
				require.Empty(t, repo.assets)
			}
			require.Zero(t, store.saves)
		})
	}
}
