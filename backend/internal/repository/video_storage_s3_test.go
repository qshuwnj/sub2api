package repository

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestVideoS3StreamsUploadSignsPrivateObjectAndReadsRanges(t *testing.T) {
	payload := []byte("synthetic video file bytes")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/studio/video-results/16/20/test.mp4", r.URL.Path)
		switch r.Method {
		case http.MethodPut:
			require.Equal(t, int64(len(payload)), r.ContentLength)
			require.Equal(t, "video/mp4", r.Header.Get("Content-Type"))
			got, err := io.ReadAll(r.Body)
			require.NoError(t, err)
			require.Equal(t, payload, got)
		case http.MethodGet:
			require.Equal(t, "bytes=0-3", r.Header.Get("Range"))
			w.Header().Set("Content-Type", "video/mp4")
			w.Header().Set("Content-Range", "bytes 0-3/26")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(payload[:4])
		default:
			t.Errorf("unexpected method %s", r.Method)
		}
	}))
	defer server.Close()
	client, err := newS3Client(context.Background(), s3ClientParams{
		Endpoint: server.URL, Region: "auto", AccessKeyID: "synthetic", SecretAccessKey: "synthetic", ForcePathStyle: true,
	})
	require.NoError(t, err)
	storage := &S3ImageStorage{client: client, bucket: "studio", publicBaseURL: "https://public.example"}
	key := "video-results/16/20/test.mp4"
	require.NoError(t, storage.SaveVideo(context.Background(), key, "video/mp4", bytes.NewReader(payload), int64(len(payload))))
	link, err := storage.VideoURL(context.Background(), key)
	require.NoError(t, err)
	u, err := url.Parse(link)
	require.NoError(t, err)
	require.Equal(t, "900", u.Query().Get("X-Amz-Expires"))
	require.NotEmpty(t, u.Query().Get("X-Amz-Signature"), "private video access must not depend on a public bucket")
	content, err := storage.OpenVideo(context.Background(), key, "bytes=0-3")
	require.NoError(t, err)
	defer content.Body.Close()
	require.Equal(t, http.StatusPartialContent, content.Status)
	require.Equal(t, "bytes 0-3/26", content.ContentRange)
	got, err := io.ReadAll(content.Body)
	require.NoError(t, err)
	require.Equal(t, payload[:4], got)
}
