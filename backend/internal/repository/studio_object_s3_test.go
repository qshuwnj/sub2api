package repository

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// PublicBaseURL must not bypass private signing, and reads must address S3 directly without fetching signatures.
func TestStudioObjectPrivateSigningAndDirectRangeRead(t *testing.T) {
	gets := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gets++
		require.Equal(t, "/studio/studio-results/16/20/digest/0", r.URL.Path)
		require.Empty(t, r.URL.Query().Get("X-Amz-Signature"))
		require.Equal(t, "bytes=0-3", r.Header.Get("Range"))
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Content-Range", "bytes 0-3/10")
		w.WriteHeader(206)
		_, _ = w.Write([]byte{1, 2, 3, 4})
	}))
	defer server.Close()
	client, err := newS3Client(context.Background(), s3ClientParams{Endpoint: server.URL, Region: "auto", AccessKeyID: "synthetic", SecretAccessKey: "synthetic", ForcePathStyle: true})
	require.NoError(t, err)
	store := &S3ImageStorage{client: client, bucket: "studio", publicBaseURL: "https://public.example"}
	key := "studio-results/16/20/digest/0"
	for _, tc := range []struct {
		expiry time.Duration
		want   string
	}{{time.Hour, "900"}, {0, "900"}, {time.Minute, "60"}} {
		link, err := store.StudioAssetURL(context.Background(), key, tc.expiry)
		require.NoError(t, err)
		u, err := url.Parse(link)
		require.NoError(t, err)
		require.Equal(t, tc.want, u.Query().Get("X-Amz-Expires"))
		require.NotEmpty(t, u.Query().Get("X-Amz-Signature"))
		require.NotEqual(t, "public.example", u.Host)
	}
	content, err := store.OpenStudioAsset(context.Background(), key, "bytes=0-3")
	require.NoError(t, err)
	defer content.Body.Close()
	data, err := io.ReadAll(content.Body)
	require.NoError(t, err)
	require.Equal(t, []byte{1, 2, 3, 4}, data)
	require.Equal(t, 206, content.Status)
	require.Equal(t, "bytes 0-3/10", content.ContentRange)
	require.Equal(t, 1, gets)
}
