package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func TestVideoReferenceDapacaiStorageFilenameUsesInspectedFormat(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var pngData, jpegData bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 600, 600))
	if err := png.Encode(&pngData, img); err != nil {
		t.Fatal(err)
	}
	if err := jpeg.Encode(&jpegData, img, nil); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, kind, mime, extension, probe string
		data                               []byte
	}{
		{"png", "image", "image/png", ".png", "", pngData.Bytes()},
		{"jpeg", "image", "image/jpeg", ".jpg", "", jpegData.Bytes()},
		{"mp3", "audio", "audio/mpeg", ".mp3", `{"streams":[{"codec_type":"audio","duration":"3"}],"format":{"format_name":"mp3"}}`, []byte("audio fixture")},
		{"m4a", "audio", "audio/mp4", ".m4a", `{"streams":[{"codec_type":"audio","duration":"3"}],"format":{"format_name":"m4a"}}`, []byte("audio fixture")},
		{"mp4", "video", "video/mp4", ".mp4", `{"streams":[{"codec_type":"video","width":600,"height":800,"duration":"3","r_frame_rate":"30/1"}],"format":{"format_name":"mp4"}}`, []byte("video fixture")},
		{"webm", "video", "video/webm", ".webm", `{"streams":[{"codec_type":"video","width":600,"height":800,"duration":"3","r_frame_rate":"30/1"}],"format":{"format_name":"webm"}}`, []byte("video fixture")},
	} {
		for _, purpose := range []string{"", "frame"} {
			if purpose == "frame" && tc.kind != "image" {
				continue
			}
			t.Run(tc.name+"/"+purpose, func(t *testing.T) {
				store := service.NewVideoReferenceAssetStore(time.Now)
				blobs := &testVideoReferenceBlobStore{}
				probe := service.NewVideoReferenceFFProbe(func(context.Context, ...string) ([]byte, error) {
					return []byte(tc.probe), nil
				})
				handler := NewVideoReferenceAssetHandler(service.NewVideoReferenceAssetService(store, probe), blobs)
				family := "dapacai:seedance-2.5-plus-8"
				if purpose == "frame" {
					family = "frame:seedance-2.5-plus-8"
				}
				// Original filenames are untrusted, including a misleading extension.
				body, contentType := multipartBody(t, tc.data, "misleading.txt", map[string]string{
					"model": "seedance-2.5-plus-8", "endpoint_family": family, "kind": tc.kind, "purpose": purpose,
				})
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/video-reference-assets", body)
				c.Request.Header.Set("Content-Type", contentType)
				c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{ID: 8, UserID: 7})
				c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 7})
				handler.Upload(c)
				if rec.Code != http.StatusCreated {
					t.Fatalf("upload failed: status=%d body=%s", rec.Code, rec.Body.String())
				}
				var response struct {
					Asset service.VideoReferenceAsset `json:"asset"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
					t.Fatal(err)
				}
				asset, err := store.GetOwned(response.Asset.ID, 7, 8)
				if err != nil {
					t.Fatal(err)
				}
				wantKey := "video-reference/7/" + asset.ID + tc.extension
				if blobs.saved != 1 || blobs.key != wantKey || asset.StorageKey != wantKey || blobs.mime != tc.mime {
					t.Fatalf("incorrect inspected storage format: key=%s MIME=%s asset=%+v", blobs.key, blobs.mime, asset)
				}
				wantURL := "https://cdn.example/" + wantKey + "?signature=fixture"
				if asset.SourceURL != wantURL || response.Asset.SourceURL != wantURL {
					t.Fatal("storage URL or signature was rewritten after Save")
				}
			})
		}
	}
}

func TestVideoReferenceLegacyStorageKeysRemainUnchanged(t *testing.T) {
	var data bytes.Buffer
	if err := png.Encode(&data, image.NewRGBA(image.Rect(0, 0, 300, 300))); err != nil {
		t.Fatal(err)
	}
	blobs := &testVideoReferenceBlobStore{}
	handler := NewVideoReferenceAssetHandler(service.NewVideoReferenceAssetService(service.NewVideoReferenceAssetStore(time.Now), nil), blobs)
	body, contentType := multipartBody(t, data.Bytes(), "reference.png", map[string]string{
		"model": "bytedance/seedance-2.5/reference-to-video",
	})
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/video-reference-assets", body)
	c.Request.Header.Set("Content-Type", contentType)
	c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{ID: 8, UserID: 7})
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 7})
	handler.Upload(c)
	if rec.Code != http.StatusCreated || !strings.HasPrefix(blobs.key, "video-reference/7/") || filepath.Ext(blobs.key) != "" {
		t.Fatalf("legacy upload changed: status=%d key=%s", rec.Code, blobs.key)
	}
}

func TestVideoReferenceStorageSuffixCoversAdmittedFormatsWithoutGuessing(t *testing.T) {
	for mime, want := range map[string]string{
		"image/png": ".png", "image/jpeg": ".jpg", "image/webp": ".webp",
		"video/mp4": ".mp4", "video/webm": ".webm", "video/quicktime": ".mov",
		"audio/mpeg": ".mp3", "audio/wav": ".wav", "audio/ogg": ".ogg",
		"audio/mp4": ".m4a", "audio/webm": ".webm",
	} {
		got, err := videoReferenceStorageSuffix("seedance-2.5-plus-8", mime)
		if err != nil || got != want {
			t.Fatalf("MIME %s: got %s, want %s, err=%v", mime, got, want, err)
		}
	}
	for _, mime := range []string{"", "application/octet-stream", "image/svg+xml", "image/gif", "image/png;bad=value"} {
		if _, err := videoReferenceStorageSuffix("seedance-2.5-plus-8", mime); err == nil {
			t.Fatalf("unverified MIME %q must not get a guessed extension", mime)
		}
	}
}
