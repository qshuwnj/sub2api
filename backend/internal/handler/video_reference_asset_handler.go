package handler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	_ "golang.org/x/image/webp"
)

const maxVideoReferenceUploadBytes int64 = 220 << 20

type VideoReferenceAssetHandler struct {
	assets *service.VideoReferenceAssetService
	blobs  service.VideoReferenceBlobStore
}

func NewVideoReferenceAssetHandler(assets *service.VideoReferenceAssetService, blobs service.VideoReferenceBlobStore) *VideoReferenceAssetHandler {
	return &VideoReferenceAssetHandler{assets: assets, blobs: blobs}
}

func (h *VideoReferenceAssetHandler) Upload(c *gin.Context) {
	if h == nil || h.assets == nil || h.blobs == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{"message": "video reference storage is unavailable"}})
		return
	}
	apiKey, ok := middleware.GetAPIKeyFromContext(c)
	subject, subjectOK := middleware.GetAuthSubjectFromContext(c)
	if !ok || apiKey == nil || !subjectOK || subject.UserID <= 0 {
		c.JSON(http.StatusUnauthorized, gin.H{"error": gin.H{"message": "invalid API key"}})
		return
	}
	if c.Request.ContentLength > maxVideoReferenceUploadBytes {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": gin.H{"message": "reference upload exceeds 220 MiB"}})
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxVideoReferenceUploadBytes)
	defer func() {
		if c.Request.MultipartForm != nil {
			_ = c.Request.MultipartForm.RemoveAll()
		}
	}()
	if err := c.Request.ParseMultipartForm(8 << 20); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "reference upload must be multipart/form-data"}})
		return
	}
	model := strings.TrimSpace(c.PostForm("model"))
	purpose := strings.TrimSpace(c.PostForm("purpose"))
	var family string
	var err error
	switch purpose {
	case "":
		family, err = service.VideoReferenceFamilyForModel(model)
	case "frame":
		family, err = service.VideoFrameFamilyForModel(model)
	default:
		err = errors.New("unsupported upload purpose")
	}
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": err.Error()}})
		return
	}
	if requestedFamily := strings.TrimSpace(c.PostForm("endpoint_family")); requestedFamily != "" && requestedFamily != family {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "endpoint_family does not match model"}})
		return
	}
	file, header, err := c.Request.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "file is required"}})
		return
	}
	defer file.Close()
	data, err := readBoundedVideoReference(file, maxVideoReferenceUploadBytes)
	if err != nil {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": gin.H{"message": err.Error()}})
		return
	}
	metadata, err := inspectVideoReference(data, header.Filename, h.assets.FFProbe())
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": err.Error()}})
		return
	}
	if purpose == "frame" && metadata.Kind != service.VideoReferenceImage {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "frames require PNG, JPEG, or WebP images"}})
		return
	}
	requestedKind := strings.TrimSpace(c.PostForm("kind"))
	if requestedKind != "" && requestedKind != string(metadata.Kind) {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "kind does not match the uploaded bytes"}})
		return
	}
	id := uuid.NewString()
	key := filepath.ToSlash("video-reference/" + fmt.Sprintf("%d", subject.UserID) + "/" + id)
	accountID, _ := c.Request.Context().Value(ctxkey.AccountID).(int64)
	asset := &service.VideoReferenceAsset{
		ID: id, UserID: subject.UserID, APIKeyID: apiKey.ID, EndpointFamily: family,
		AccountID: accountID,
		Kind:      metadata.Kind, MIME: metadata.MIME, Size: int64(len(data)),
		Width: metadata.Width, Height: metadata.Height, Duration: metadata.Duration,
		FrameRate: metadata.FrameRate, StorageKey: key, ExpiresAt: time.Now().Add(30 * time.Minute),
		State: service.VideoReferenceStaged,
	}
	if err := service.ValidateVideoReferenceDescriptor(asset); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": err.Error()}})
		return
	}
	sourceURL, err := h.blobs.Save(c.Request.Context(), key, metadata.MIME, data)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{"message": "reference storage is unavailable"}})
		return
	}
	asset.SourceURL = sourceURL
	if err := h.assets.Admit(asset); err != nil {
		_ = h.blobs.Delete(c.Request.Context(), key)
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": err.Error()}})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"asset": asset})
}

func (h *VideoReferenceAssetHandler) Delete(c *gin.Context) {
	if h == nil || h.assets == nil || h.blobs == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{"message": "video reference storage is unavailable"}})
		return
	}
	apiKey, ok := middleware.GetAPIKeyFromContext(c)
	subject, subjectOK := middleware.GetAuthSubjectFromContext(c)
	if !ok || apiKey == nil || !subjectOK || subject.UserID <= 0 {
		c.JSON(http.StatusUnauthorized, gin.H{"error": gin.H{"message": "invalid API key"}})
		return
	}
	var input struct {
		ID string `json:"asset_id"`
	}
	if err := c.ShouldBindJSON(&input); err != nil || strings.TrimSpace(input.ID) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "asset_id is required"}})
		return
	}
	accountID, _ := c.Request.Context().Value(ctxkey.AccountID).(int64)
	asset, err := h.assets.ResolveOwned(input.ID, subject.UserID, apiKey.ID, accountID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"message": err.Error()}})
		return
	}
	if err := h.blobs.Delete(c.Request.Context(), asset.StorageKey); err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{"message": "reference storage is unavailable"}})
		return
	}
	if err := h.assets.Cancel(input.ID); err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": gin.H{"message": err.Error()}})
		return
	}
	c.Status(http.StatusNoContent)
}

func readBoundedVideoReference(reader io.Reader, maxBytes int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read reference: %w", err)
	}
	if int64(len(data)) > maxBytes {
		return nil, errors.New("reference upload exceeds the byte limit")
	}
	return data, nil
}

func inspectVideoReference(data []byte, filename string, probe *service.VideoReferenceFFProbe) (service.VideoReferenceMediaMetadata, error) {
	if len(data) == 0 {
		return service.VideoReferenceMediaMetadata{}, errors.New("reference file is empty")
	}
	contentType := sniffReferenceContentType(data, filename)
	if strings.HasPrefix(contentType, "image/") {
		config, _, err := image.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			return service.VideoReferenceMediaMetadata{}, errors.New("invalid image bytes")
		}
		if contentType != "image/png" && contentType != "image/jpeg" && contentType != "image/webp" {
			return service.VideoReferenceMediaMetadata{}, errors.New("unsupported image MIME")
		}
		return service.VideoReferenceMediaMetadata{Kind: service.VideoReferenceImage, MIME: contentType, Width: config.Width, Height: config.Height}, nil
	}
	if probe == nil {
		return service.VideoReferenceMediaMetadata{}, errors.New("ffprobe is not configured")
	}
	temp, err := os.CreateTemp("", "video-reference-*")
	if err != nil {
		return service.VideoReferenceMediaMetadata{}, errors.New("unable to inspect reference")
	}
	path := temp.Name()
	defer os.Remove(path)
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return service.VideoReferenceMediaMetadata{}, errors.New("unable to inspect reference")
	}
	if err := temp.Close(); err != nil {
		return service.VideoReferenceMediaMetadata{}, errors.New("unable to inspect reference")
	}
	metadata, err := probe.Probe(context.Background(), path)
	if err != nil {
		return service.VideoReferenceMediaMetadata{}, fmt.Errorf("invalid media bytes: %w", err)
	}
	return metadata, nil
}

func sniffReferenceContentType(data []byte, filename string) string {
	if len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP" {
		return "image/webp"
	}
	detected := http.DetectContentType(data)
	if detected == "application/octet-stream" {
		ext := strings.ToLower(filepath.Ext(filename))
		switch ext {
		case ".mp4", ".m4v", ".mov":
			return "video/mp4"
		case ".webm":
			return "video/webm"
		case ".mp3":
			return "audio/mpeg"
		case ".wav":
			return "audio/wav"
		case ".ogg":
			return "audio/ogg"
		}
	}
	return strings.Split(detected, ";")[0]
}
