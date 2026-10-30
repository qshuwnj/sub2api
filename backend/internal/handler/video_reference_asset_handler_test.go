package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

type testVideoReferenceBlobStore struct {
	url     string
	saved   int
	deleted int
	err     error
}

func (s *testVideoReferenceBlobStore) Save(context.Context, string, string, []byte) (string, error) {
	if s.err != nil {
		return "", s.err
	}
	s.saved++
	return s.url, nil
}

func (s *testVideoReferenceBlobStore) Delete(context.Context, string) error {
	s.deleted++
	return nil
}

func TestVideoReferenceAssetHandlerUploadsOnlyAfterByteAdmission(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := service.NewVideoReferenceAssetStore(time.Now)
	blobs := &testVideoReferenceBlobStore{url: "https://cdn.example/reference.png"}
	h := NewVideoReferenceAssetHandler(service.NewVideoReferenceAssetService(store, nil), blobs)

	var imageBytes bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 300, 300))
	for y := 0; y < 300; y++ {
		for x := 0; x < 300; x++ {
			img.SetRGBA(x, y, color.RGBA{R: 20, G: 40, B: 80, A: 255})
		}
	}
	if err := png.Encode(&imageBytes, img); err != nil {
		t.Fatal(err)
	}
	body, contentType := multipartBody(t, imageBytes.Bytes(), "reference.png", map[string]string{
		"model": "bytedance/seedance-2.5/reference-to-video",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/video-reference-assets", body)
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = req
	c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{ID: 8, UserID: 7})
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 7})

	h.Upload(c)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if blobs.saved != 1 {
		t.Fatalf("saved = %d", blobs.saved)
	}
}

func TestVideoReferenceAssetHandlerFailsClosedWhenStorageUnavailable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewVideoReferenceAssetHandler(nil, nil)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/video-reference-assets", nil)

	h.Upload(c)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestVideoReferenceAssetHandlerStagesLocalFramesForExactImageModels(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var data bytes.Buffer
	if err := png.Encode(&data, image.NewRGBA(image.Rect(0, 0, 300, 300))); err != nil {
		t.Fatal(err)
	}
	for _, model := range []string{
		"bytedance/seedance-2.0/image-to-video",
		"bytedance/seedance-2.0-fast/image-to-video",
		"google/veo3.1/image-to-video",
		"grok-imagine-video",
		"doubao-seedance-1-0-pro-250528",
		"doubao-seedance-2-0-260128",
	} {
		t.Run(model, func(t *testing.T) {
			store := service.NewVideoReferenceAssetStore(time.Now)
			blobs := &testVideoReferenceBlobStore{url: "https://cdn.example/frame.png"}
			h := NewVideoReferenceAssetHandler(service.NewVideoReferenceAssetService(store, nil), blobs)
			body, contentType := multipartBody(t, data.Bytes(), "frame.png", map[string]string{
				"model": model, "purpose": "frame", "endpoint_family": "frame:" + model, "kind": "image",
			})
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/video-reference-assets", body)
			c.Request.Header.Set("Content-Type", contentType)
			c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{ID: 8, UserID: 7})
			c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 7})
			h.Upload(c)
			if rec.Code != http.StatusCreated {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			var result struct {
				Asset service.VideoReferenceAsset `json:"asset"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.Asset.SourceURL != "https://cdn.example/frame.png" || result.Asset.Kind != service.VideoReferenceImage || blobs.saved != 1 {
				t.Fatalf("invalid staged frame: %+v", result.Asset)
			}
			if _, err := store.GetOwned(result.Asset.ID, 7, 8); err != nil {
				t.Fatalf("frame metadata was not stored: %v", err)
			}
			if _, err := store.GetOwned(result.Asset.ID, 99, 8); err == nil {
				t.Fatal("frame upload lost owner isolation")
			}
		})
	}
}

func TestVideoReferenceAssetHandlerRejectsInvalidFrameUploadsBeforeStorage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var data bytes.Buffer
	_ = png.Encode(&data, image.NewRGBA(image.Rect(0, 0, 300, 300)))
	for _, tc := range []struct {
		name, model, purpose, kind, family string
		data                               []byte
	}{
		{"text model", "bytedance/seedance-2.0/text-to-video", "frame", "image", "", data.Bytes()},
		{"unknown model", "fake/image-to-video", "frame", "image", "", data.Bytes()},
		{"reference model", "bytedance/seedance-2.0/reference-to-video", "frame", "image", "", data.Bytes()},
		{"wrong family", "bytedance/seedance-2.0/image-to-video", "frame", "image", "2.0", data.Bytes()},
		{"wrong kind", "bytedance/seedance-2.0/image-to-video", "frame", "video", "", data.Bytes()},
		{"svg", "bytedance/seedance-2.0/image-to-video", "frame", "image", "", []byte("<svg></svg>")},
		{"unknown purpose", "bytedance/seedance-2.0/reference-to-video", "untrusted", "image", "", data.Bytes()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blobs := &testVideoReferenceBlobStore{url: "https://cdn.example/frame.png"}
			h := NewVideoReferenceAssetHandler(service.NewVideoReferenceAssetService(service.NewVideoReferenceAssetStore(time.Now), nil), blobs)
			body, contentType := multipartBody(t, tc.data, "frame.png", map[string]string{
				"model": tc.model, "purpose": tc.purpose, "kind": tc.kind, "endpoint_family": tc.family,
			})
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/video-reference-assets", body)
			c.Request.Header.Set("Content-Type", contentType)
			c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{ID: 8, UserID: 7})
			c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 7})
			h.Upload(c)
			if rec.Code != http.StatusBadRequest || blobs.saved != 0 {
				t.Fatalf("status = %d, saved = %d, body = %s", rec.Code, blobs.saved, rec.Body.String())
			}
		})
	}
}

func multipartBody(t *testing.T, data []byte, filename string, fields map[string]string) (*bytes.Buffer, string) {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for key, value := range fields {
		if err := writer.WriteField(key, value); err != nil {
			t.Fatal(err)
		}
	}
	part, err := writer.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return &body, writer.FormDataContentType()
}
