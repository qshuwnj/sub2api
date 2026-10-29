package handler

import (
	"bytes"
	"context"
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
