package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestImageDeliveryTimingSeparatesDownloadStorageAndPersistence(t *testing.T) {
	for _, failure := range []string{"", "download", "storage", "persist"} {
		t.Run(failure, func(t *testing.T) {
			core, logs := observer.New(zap.InfoLevel)
			ctx := logger.IntoContext(context.Background(), zap.New(core))
			objectStore := &fakeImageStorage{}
			if failure == "storage" {
				objectStore.err = errors.New("private-storage-secret")
			}
			downloads := 0
			client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				downloads++
				if failure == "download" {
					return nil, errors.New("private-download-secret")
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"image/png"}},
					Body: io.NopCloser(bytes.NewReader(pngBytes)), Request: req}, nil
			})}
			store := &imageTaskMemoryStore{}
			svc := NewImageTaskServiceWithUploader(store, NewImageResultUploader(objectStore, "images/", 0, client), time.Hour, time.Minute)
			owner := ImageTaskOwner{UserID: 1, APIKeyID: 2}
			task, err := svc.Create(ctx, owner)
			require.NoError(t, err)
			if failure == "persist" {
				store.saveErr = errors.New("private-redis-secret")
			}
			err = svc.Complete(ctx, task.ID, http.StatusOK, json.RawMessage(`{"data":[{"url":"https://cdn.example/image.png?sig=private-result-secret"}]}`))
			if failure == "persist" {
				require.ErrorIs(t, err, ErrImageTaskUnavailable)
			} else {
				require.NoError(t, err)
			}
			if failure == "" {
				require.Equal(t, ImageTaskStatusCompleted, store.task.Status)
				require.Equal(t, pngBytes, objectStore.saved[0].data, "timing must not recompress or alter the image")
			}
			require.Equal(t, 1, downloads, "delivery timing must not add downloads or regenerate")
			summaries := logs.FilterMessage("image_task.delivery_finished").All()
			require.Len(t, summaries, 1)
			fields := summaries[0].ContextMap()
			wantStatus := "completed"
			if failure != "" {
				wantStatus = "failed"
			}
			require.Equal(t, wantStatus, fields["status"])
			require.GreaterOrEqual(t, fields["offload_ms"], int64(0))
			require.GreaterOrEqual(t, fields["persist_ms"], int64(0))
			downloadsLog := logs.FilterMessage("image_task.result_download").All()
			require.Len(t, downloadsLog, 1)
			require.Equal(t, failure != "download", downloadsLog[0].ContextMap()["success"])
			uploadsLog := logs.FilterMessage("image_task.result_upload").All()
			if failure == "download" {
				require.Empty(t, uploadsLog)
			} else {
				require.Len(t, uploadsLog, 1)
				require.Equal(t, failure != "storage", uploadsLog[0].ContextMap()["success"])
			}
			for _, entry := range logs.All() {
				encoded, err := json.Marshal(entry.ContextMap())
				require.NoError(t, err)
				for _, secret := range []string{"private-result-secret", "private-download-secret", "private-storage-secret", "private-redis-secret", "cdn.example", string(pngBytes)} {
					require.NotContains(t, string(encoded), secret)
				}
			}
		})
	}
}
