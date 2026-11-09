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
	"os"
	"os/exec"
	"path/filepath"
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
	mime    string
	key     string
}

func (s *testVideoReferenceBlobStore) Save(_ context.Context, key string, contentType string, _ []byte) (string, error) {
	if s.err != nil {
		return "", s.err
	}
	s.saved++
	s.mime = contentType
	s.key = key
	if s.url == "" {
		return "https://cdn.example/" + key + "?signature=fixture", nil
	}
	return s.url, nil
}

func TestVideoReferenceAssetHandlerDapacaiImageAndAudioAdmission(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var imageBytes bytes.Buffer
	if err := png.Encode(&imageBytes, image.NewRGBA(image.Rect(0, 0, 32, 32))); err != nil {
		t.Fatal(err)
	}
	models := []string{"seedance-2.0", "seedance-2.0-plus", "seedance-2.5-plus", "seedance-2.5-plus-1", "seedance-2.5-plus-2", "seedance-2.5-plus-3", "seedance-2.5-plus-4", "seedance-2.5-plus-5", "seedance-2.5-plus-6", "seedance-2.5-plus-7", "seedance-2.5-plus-8", "wan-3.0"}
	for _, model := range models {
		for _, kind := range []string{"image", "audio"} {
			t.Run(model+"/"+kind, func(t *testing.T) {
				store := service.NewVideoReferenceAssetStore(time.Now)
				blobs := &testVideoReferenceBlobStore{url: "https://cdn.example/reference." + map[string]string{"image": "png", "audio": "mp3"}[kind]}
				probe := service.NewVideoReferenceFFProbe(func(context.Context, ...string) ([]byte, error) {
					return []byte(`{"streams":[{"codec_type":"audio","duration":"8.25"},{"codec_type":"video","width":600,"height":600,"disposition":{"attached_pic":1}}],"format":{"duration":"8.25","format_name":"mp3"}}`), nil
				})
				h := NewVideoReferenceAssetHandler(service.NewVideoReferenceAssetService(store, probe), blobs)
				data := imageBytes.Bytes()
				if kind == "audio" {
					data = []byte("ID3synthetic audio fixture; metadata supplied by probe stub")
				}
				body, contentType := multipartBody(t, data, "reference", map[string]string{
					"model": model, "endpoint_family": "dapacai:" + model, "kind": kind,
				})
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/video-reference-assets", body)
				c.Request.Header.Set("Content-Type", contentType)
				c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{ID: 8, UserID: 7})
				c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 7})
				h.Upload(c)
				audioSupported := model == "seedance-2.0-plus" || model == "seedance-2.5-plus" || model == "seedance-2.5-plus-4" || model == "seedance-2.5-plus-6" || model == "seedance-2.5-plus-7" || model == "seedance-2.5-plus-8"
				if kind == "audio" && !audioSupported {
					if rec.Code != http.StatusBadRequest || blobs.saved != 0 {
						t.Fatalf("unsupported audio accepted: status=%d saved=%d", rec.Code, blobs.saved)
					}
					return
				}
				if rec.Code != http.StatusCreated || blobs.saved != 1 {
					t.Fatalf("upload failed: status=%d body=%s", rec.Code, rec.Body.String())
				}
				var result struct {
					Asset service.VideoReferenceAsset `json:"asset"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				asset, err := store.GetOwned(result.Asset.ID, 7, 8)
				wantMIME := map[string]string{"image": "image/png", "audio": "audio/mpeg"}[kind]
				if err != nil || asset.SourceURL != blobs.url || string(asset.Kind) != kind || asset.EndpointFamily != "dapacai:"+model || blobs.mime != wantMIME {
					t.Fatalf("upload changed source model/kind/MIME: asset=%+v mime=%s err=%v", asset, blobs.mime, err)
				}
				wantExtension := map[string]string{"image": ".png", "audio": ".mp3"}[kind]
				if filepath.Ext(blobs.key) != wantExtension || asset.StorageKey != blobs.key {
					t.Fatalf("upload missing verified file format: key=%s asset=%+v", blobs.key, asset)
				}
			})
		}
	}
}

func TestVideoReferenceAssetHandlerRealMP3WithCover(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is not installed")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe is not installed")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "reference-with-cover.mp3")
	audioPath := filepath.Join(dir, "audio.mp3")
	coverPath := filepath.Join(dir, "cover.png")
	var cover bytes.Buffer
	if err := png.Encode(&cover, image.NewRGBA(image.Rect(0, 0, 32, 32))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(coverPath, cover.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "ffmpeg", "-v", "error", "-y", "-f", "lavfi", "-i", "sine=frequency=440:duration=1", "-c:a", "libmp3lame", audioPath)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("generate audio fixture: %v %s", err, output)
	}
	command = exec.CommandContext(ctx, "ffmpeg", "-v", "error", "-y", "-i", audioPath, "-i", coverPath, "-map", "0:a", "-map", "1:v", "-c:a", "copy", "-c:v", "mjpeg", "-disposition:v:0", "attached_pic", path)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("attach audio cover: %v %s", err, output)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	blobs := &testVideoReferenceBlobStore{url: "https://cdn.example/reference-with-cover.mp3"}
	h := NewVideoReferenceAssetHandler(service.NewVideoReferenceAssetService(service.NewVideoReferenceAssetStore(time.Now), nil), blobs)
	body, contentType := multipartBody(t, data, "reference-with-cover.mp3", map[string]string{
		"model": "seedance-2.5-plus-7", "endpoint_family": "dapacai:seedance-2.5-plus-7", "kind": "audio",
	})
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/video-reference-assets", body)
	c.Request.Header.Set("Content-Type", contentType)
	c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{ID: 8, UserID: 7})
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 7})
	h.Upload(c)
	if rec.Code != http.StatusCreated || blobs.saved != 1 || blobs.mime != "audio/mpeg" {
		t.Fatalf("real audio upload rejected: status=%d MIME=%s body=%s", rec.Code, blobs.mime, rec.Body.String())
	}
	var result struct {
		Asset service.VideoReferenceAsset `json:"asset"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Asset.Kind != service.VideoReferenceAudio || result.Asset.Duration < 1 || result.Asset.Width != 0 || result.Asset.Height != 0 {
		t.Fatalf("cover art changed real MP3 metadata: %+v", result.Asset)
	}
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
