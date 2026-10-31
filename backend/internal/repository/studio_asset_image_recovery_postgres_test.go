package repository

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

type studioPartialImageRepo struct {
	*usageBillingRepository
	failSecond bool
	writes     []int
	billing    int
}

func (r *studioPartialImageRepo) UpsertStudioAsset(ctx context.Context, a *service.StudioAsset) error {
	r.writes = append(r.writes, a.OutputIndex)
	if a.OutputIndex == 1 && r.failSecond {
		return errors.New("synthetic second-output database failure")
	}
	return r.usageBillingRepository.UpsertStudioAsset(ctx, a)
}

func (r *studioPartialImageRepo) Apply(context.Context, *service.UsageBillingCommand) (*service.UsageBillingApplyResult, error) {
	r.billing++
	return nil, errors.New("library must not settle usage")
}

type studioRecoveryTaskStore struct{ task *service.ImageTaskRecord }

func (s *studioRecoveryTaskStore) Save(_ context.Context, task *service.ImageTaskRecord, _ time.Duration) error {
	copy := *task
	s.task = &copy
	return nil
}

func (s *studioRecoveryTaskStore) Get(_ context.Context, id string) (*service.ImageTaskRecord, error) {
	if s.task == nil || s.task.ID != id {
		return nil, service.ErrImageTaskNotFound
	}
	copy := *s.task
	return &copy, nil
}

type studioRecoveryObjects struct{ uploads map[string]int }

func (s *studioRecoveryObjects) Save(_ context.Context, key, _ string, _ []byte) (string, error) {
	s.uploads[key]++
	return key, nil
}
func (*studioRecoveryObjects) OpenStudioAsset(context.Context, string, string) (*service.VideoResultContent, error) {
	panic("reconcile must not read objects")
}
func (*studioRecoveryObjects) StudioAssetURL(context.Context, string, time.Duration) (string, error) {
	panic("reconcile must not sign objects")
}

func studioRecoveryImageTask(t *testing.T) (*service.ImageTaskService, *studioRecoveryTaskStore, []byte) {
	t.Helper()
	var data bytes.Buffer
	require.NoError(t, png.Encode(&data, image.NewRGBA(image.Rect(0, 0, 2, 2))))
	store := &studioRecoveryTaskStore{}
	tasks := service.NewImageTaskServiceWithUploader(store, nil, time.Hour, time.Minute)
	task, err := tasks.Create(context.Background(), service.ImageTaskOwner{UserID: 16, APIKeyID: 20})
	require.NoError(t, err)
	result, err := json.Marshal(map[string]any{"data": []any{
		map[string]string{"b64_json": base64.StdEncoding.EncodeToString(data.Bytes())},
		map[string]string{"b64_json": base64.StdEncoding.EncodeToString(data.Bytes())},
	}})
	require.NoError(t, err)
	require.NoError(t, tasks.Complete(context.Background(), task.ID, 200, result))
	return tasks, store, data.Bytes()
}

// Break caught: a persisted output zero (including a tombstone) must not skip
// owned live task verification or conceal a missing output one.
func TestStudioAssetPostgresImageRecoveryPartialOutputs(t *testing.T) {
	for _, variant := range []string{"pending first", "ready first", "leased first", "deleted first", "deleted sibling", "terminal first"} {
		t.Run(variant, func(t *testing.T) {
			base, ctx := studioHistoryDB(t)
			repo := &studioPartialImageRepo{usageBillingRepository: base, failSecond: true}
			objects := &studioRecoveryObjects{uploads: map[string]int{}}
			uploader := service.NewImageResultUploader(objects, "images/", 1<<20, nil)
			library := service.NewStudioAssetService(repo, func() (*service.ImageResultUploader, bool) { return uploader, true })
			tasks, store, data := studioRecoveryImageTask(t)
			library.SetOwnedImageTasks(tasks)
			owner := service.StudioAssetOwner{UserID: 16, APIKeyID: 20}
			id := store.task.ID
			require.NoError(t, library.CollectImage(ctx, owner, id, 0, "original-model", "original prompt",
				json.RawMessage(`{"aspect_ratio":"3:4"}`), service.StudioImageOutput{Bytes: data}))
			require.ErrorIs(t, library.CollectImage(ctx, owner, id, 1, "original-model", "original prompt", nil,
				service.StudioImageOutput{Bytes: data}), service.ErrStudioAssetUnavailable)
			first, err := base.FindStudioAssetGeneration(ctx, owner, id)
			require.NoError(t, err)
			switch variant {
			case "ready first":
				require.NoError(t, library.ProcessPending(ctx))
			case "leased first":
				claimed, err := repo.ClaimStudioAssets(ctx, 1)
				require.NoError(t, err)
				require.Len(t, claimed, 1)
			case "deleted first":
				require.NoError(t, repo.DeleteStudioAsset(ctx, owner, first.ID))
			case "deleted sibling":
				repo.failSecond = false
				require.NoError(t, library.CollectImage(ctx, owner, id, 1, "original-model", "original prompt", nil,
					service.StudioImageOutput{Bytes: data}))
				_, err = base.db.Exec(`UPDATE studio_assets SET deleted_at=NOW(),source_bytes=NULL WHERE output_index=1;
					DELETE FROM studio_assets WHERE output_index=0`)
				require.NoError(t, err)
			case "terminal first":
				_, err = base.db.Exec(`UPDATE studio_assets SET state='unavailable',last_error_code='invalid_image' WHERE id=$1`, first.ID)
				require.NoError(t, err)
			}
			var before string
			require.NoError(t, base.db.QueryRow(`SELECT json_agg(a ORDER BY output_index)::text FROM studio_assets a`).Scan(&before))
			uploadsBefore := len(objects.uploads)
			repo.writes = nil
			if variant != "deleted sibling" {
				result, err := library.Reconcile(ctx, owner, []string{id})
				require.NoError(t, err)
				require.Equal(t, &service.StudioAssetReconcileResult{Unrecoverable: 1}, result, "a sibling persistence failure must remain retryable, not already saved")
				require.Equal(t, []int{1}, repo.writes, "never rewrite an existing output while recovering another")
				repo.failSecond = false
				repo.writes = nil
			}
			result, err := library.Reconcile(ctx, owner, []string{id})
			require.NoError(t, err)
			require.Equal(t, &service.StudioAssetReconcileResult{Queued: 1}, result)
			missing := 1
			if variant == "deleted sibling" {
				missing = 0
			}
			require.Equal(t, []int{missing}, repo.writes)
			var after string
			require.NoError(t, base.db.QueryRow(`SELECT json_agg(a ORDER BY output_index)::text FROM studio_assets a WHERE output_index<>$1`, missing).Scan(&after))
			require.JSONEq(t, before, after, "ready bytes, leases, metadata, terminal state and per-output tombstones stay exactly unchanged")
			repo.writes = nil
			result, err = library.Reconcile(ctx, owner, []string{id, id})
			require.NoError(t, err)
			require.Equal(t, &service.StudioAssetReconcileResult{AlreadySaved: 1}, result)
			require.Empty(t, repo.writes, "a repeated recovery must not upsert any existing output")
			require.Len(t, objects.uploads, uploadsBefore, "reconcile is DB-only, even with ready objects")
			// Archive only newly missing/pending outputs, never ready/deleted/leased
			// or terminal rows, then prove retries cause no duplicate uploads.
			for i := 0; i < 3; i++ {
				require.NoError(t, library.ProcessPending(ctx))
			}
			for key, count := range objects.uploads {
				require.Equal(t, 1, count, "one upload per live output: "+key)
			}
			var count int
			require.NoError(t, base.db.QueryRow(`SELECT count(*) FROM studio_assets`).Scan(&count))
			require.Equal(t, 2, count)
			require.NoError(t, base.db.QueryRow(`SELECT count(*) FROM usage_logs`).Scan(&count))
			require.Zero(t, count)
			require.NoError(t, base.db.QueryRow(`SELECT count(*) FROM usage_billing_dedup`).Scan(&count))
			require.Zero(t, count)
			require.Zero(t, repo.billing, "no settlement command or receipt")
		})
	}
}

func TestStudioAssetPostgresImageRecoveryPartialRequiresLiveOwnedSuccess(t *testing.T) {
	for _, variant := range []string{"wrong user", "wrong key", "expired", "failed", "HTTP failure", "missing task"} {
		t.Run(variant, func(t *testing.T) {
			base, ctx := studioHistoryDB(t)
			repo := &studioPartialImageRepo{usageBillingRepository: base}
			objects := &studioRecoveryObjects{uploads: map[string]int{}}
			uploader := service.NewImageResultUploader(objects, "images/", 1<<20, nil)
			library := service.NewStudioAssetService(repo, func() (*service.ImageResultUploader, bool) { return uploader, true })
			tasks, store, data := studioRecoveryImageTask(t)
			library.SetOwnedImageTasks(tasks)
			owner := service.StudioAssetOwner{UserID: 16, APIKeyID: 20}
			id := store.task.ID
			require.NoError(t, library.CollectImage(ctx, owner, id, 0, "original", "original", nil, service.StudioImageOutput{Bytes: data}))
			switch variant {
			case "wrong user":
				store.task.UserID = 17
			case "wrong key":
				store.task.APIKeyID = 21
			case "expired":
				store.task.ExpiresAt = time.Now().Add(-time.Minute).Unix()
			case "failed":
				store.task.Status = service.ImageTaskStatusFailed
			case "HTTP failure":
				store.task.HTTPStatus = 500
			case "missing task":
				store.task = nil
			}
			repo.writes = nil
			result, err := library.Reconcile(ctx, owner, []string{id})
			require.NoError(t, err)
			require.Equal(t, &service.StudioAssetReconcileResult{Unrecoverable: 1}, result)
			require.Empty(t, repo.writes, "existing row zero is not task ownership/success proof")
			require.Empty(t, objects.uploads)
			require.Zero(t, repo.billing)
			var count int
			require.NoError(t, base.db.QueryRow(`SELECT count(*) FROM studio_assets`).Scan(&count))
			require.Equal(t, 1, count)
		})
	}
}

func TestStudioAssetPostgresImageRecoveryKeepsOriginalOutputIndex(t *testing.T) {
	base, ctx := studioHistoryDB(t)
	objects := &studioRecoveryObjects{uploads: map[string]int{}}
	uploader := service.NewImageResultUploader(objects, "images/", 1<<20, nil)
	library := service.NewStudioAssetService(base, func() (*service.ImageResultUploader, bool) { return uploader, true })
	tasks, store, data := studioRecoveryImageTask(t)
	store.task.Result = json.RawMessage(`{"data":[{},{"b64_json":"` + base64.StdEncoding.EncodeToString(data) + `"}]}`)
	library.SetOwnedImageTasks(tasks)
	owner := service.StudioAssetOwner{UserID: 16, APIKeyID: 20}
	result, err := library.Reconcile(ctx, owner, []string{store.task.ID})
	require.NoError(t, err)
	require.Equal(t, &service.StudioAssetReconcileResult{Queued: 1}, result)
	page, err := base.ListStudioAssets(ctx, owner, service.StudioAssetQuery{})
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	require.Equal(t, 1, page.Items[0].OutputIndex, "filtering absent output sources must not shift original indices")
	require.Empty(t, objects.uploads)
}
