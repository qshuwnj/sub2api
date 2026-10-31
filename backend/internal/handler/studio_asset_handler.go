package handler

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

type studioAssetPublic struct {
	ID          string         `json:"id"`
	Type        string         `json:"type"`
	Model       string         `json:"model"`
	Prompt      string         `json:"prompt"`
	GeneratedAt time.Time      `json:"generated_at"`
	Status      string         `json:"status"`
	MIME        string         `json:"mime_type"`
	SizeBytes   int64          `json:"size_bytes"`
	Metadata    map[string]any `json:"metadata"`
	ContentPath string         `json:"content_path"`
}

func studioAssetDTO(a *service.StudioAsset) studioAssetPublic {
	metadata := map[string]any{}
	var raw map[string]any
	decoder := json.NewDecoder(bytes.NewReader(a.Metadata))
	decoder.UseNumber()
	if decoder.Decode(&raw) == nil {
		// Unknown and nested metadata is private by default. Duration is seconds.
		for _, key := range []string{"aspect_ratio", "resolution", "duration", "width", "height", "seed"} {
			switch value := raw[key].(type) {
			case string:
				metadata[key] = value
			case json.Number:
				metadata[key] = value
			case bool:
				metadata[key] = value
			}
		}
	}
	return studioAssetPublic{
		ID: a.ID, Type: a.Kind, Model: a.Model, Prompt: a.Prompt,
		GeneratedAt: a.GeneratedAt, Status: a.State, MIME: a.MIME,
		SizeBytes: a.SizeBytes, Metadata: metadata,
		ContentPath: "/v1/studio/assets/" + a.ID + "/content",
	}
}

func (h *OpenAIGatewayHandler) studioAssetOwner(c *gin.Context) (service.StudioAssetOwner, bool) {
	c.Header("Cache-Control", "private, no-store")
	c.Header("X-Content-Type-Options", "nosniff")
	key, ok := middleware.GetAPIKeyFromContext(c)
	if !ok || key.UserID <= 0 || key.ID <= 0 {
		h.errorResponse(c, 401, "authentication_error", "Invalid API key")
		return service.StudioAssetOwner{}, false
	}
	return service.StudioAssetOwner{UserID: key.UserID, APIKeyID: key.ID}, true
}

func (h *OpenAIGatewayHandler) studioAssetError(c *gin.Context, err error) {
	status, code, message := 503, "studio_asset_unavailable", "Private media library is temporarily unavailable"
	switch {
	case errors.Is(err, service.ErrStudioAssetNotFound):
		status, code, message = 404, "not_found_error", "Studio asset not found"
	case errors.Is(err, service.ErrStudioAssetInvalidRange):
		status, code, message = 416, "invalid_range", "A satisfiable single byte range is required"
	case errors.Is(err, service.ErrStudioAssetInvalidRequest):
		status, code, message = 400, "invalid_request_error", "Invalid studio asset request"
	}
	h.errorResponse(c, status, code, message)
}

func (h *OpenAIGatewayHandler) StudioAssets(c *gin.Context) {
	owner, ok := h.studioAssetOwner(c)
	if !ok {
		return
	}
	q := service.StudioAssetQuery{Kind: c.Query("type"), Cursor: c.Query("cursor")}
	if value, exists := c.GetQuery("limit"); exists {
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 {
			h.studioAssetError(c, service.ErrStudioAssetInvalidRequest)
			return
		}
		q.Limit = n
	}
	library := h.gatewayService.StudioAssets()
	page, err := library.List(c.Request.Context(), owner, q)
	if err != nil {
		h.studioAssetError(c, err)
		return
	}
	items := make([]studioAssetPublic, 0, len(page.Items))
	for _, a := range page.Items {
		items = append(items, studioAssetDTO(a))
	}
	var cursor any
	if page.NextCursor != "" {
		cursor = page.NextCursor
	}
	c.JSON(200, gin.H{"items": items, "next_cursor": cursor, "storage_available": library.StorageAvailable()})
}

func (h *OpenAIGatewayHandler) StudioAsset(c *gin.Context) {
	owner, ok := h.studioAssetOwner(c)
	if !ok {
		return
	}
	a, err := h.gatewayService.StudioAssets().Get(c.Request.Context(), owner, c.Param("asset_id"))
	if err != nil {
		h.studioAssetError(c, err)
		return
	}
	c.JSON(200, studioAssetDTO(a))
}

func (h *OpenAIGatewayHandler) StudioAssetsReconcile(c *gin.Context) {
	owner, ok := h.studioAssetOwner(c)
	if !ok {
		return
	}
	var request struct {
		GenerationIDs *[]string `json:"generation_ids"`
	}
	d := json.NewDecoder(c.Request.Body)
	d.DisallowUnknownFields()
	if d.Decode(&request) != nil || request.GenerationIDs == nil || d.Decode(new(any)) != io.EOF {
		h.studioAssetError(c, service.ErrStudioAssetInvalidRequest)
		return
	}
	result, err := h.gatewayService.StudioAssets().Reconcile(c.Request.Context(), owner, *request.GenerationIDs)
	if err != nil {
		h.studioAssetError(c, err)
		return
	}
	c.JSON(200, gin.H{"queued": result.Queued, "already_saved": result.AlreadySaved, "unrecoverable": result.Unrecoverable})
}

func (h *OpenAIGatewayHandler) StudioAssetDelete(c *gin.Context) {
	owner, ok := h.studioAssetOwner(c)
	if !ok {
		return
	}
	if err := h.gatewayService.StudioAssets().Delete(c.Request.Context(), owner, c.Param("asset_id")); err != nil {
		h.studioAssetError(c, err)
		return
	}
	c.Status(204)
}

func (h *OpenAIGatewayHandler) StudioAssetContent(c *gin.Context) {
	owner, ok := h.studioAssetOwner(c)
	if !ok {
		return
	}
	content, err := h.gatewayService.StudioAssets().Open(c.Request.Context(), owner, c.Param("asset_id"), c.GetHeader("Range"))
	if err != nil {
		h.studioAssetError(c, err)
		return
	}
	if content == nil || content.Body == nil {
		h.studioAssetError(c, service.ErrStudioAssetUnavailable)
		return
	}
	defer content.Body.Close()
	extensions := map[string]string{"image/png": "png", "image/jpeg": "jpg", "image/webp": "webp", "image/gif": "gif", "video/mp4": "mp4", "video/webm": "webm"}
	ext, allowed := extensions[content.MIME]
	if !allowed || content.Status != 200 && content.Status != 206 || content.Size < 0 {
		h.studioAssetError(c, service.ErrStudioAssetUnavailable)
		return
	}
	headers := map[string]string{"Cache-Control": "private, no-store", "X-Content-Type-Options": "nosniff", "Accept-Ranges": "bytes"}
	if content.ContentRange != "" {
		headers["Content-Range"] = content.ContentRange
	}
	if c.Query("download") == "1" {
		headers["Content-Disposition"] = `attachment; filename="` + c.Param("asset_id") + "." + ext + `"`
	}
	c.DataFromReader(content.Status, content.Size, content.MIME, content.Body, headers)
}
