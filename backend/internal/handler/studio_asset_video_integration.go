package handler

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func studioVideoMetadata(body []byte) json.RawMessage {
	return studioImageMetadata(body, &service.OpenAIImagesRequest{})
}

// Called only from authenticated successful create/owned task lookup paths.
// Preserve original fields before provider-specific model rewriting.
func (h *OpenAIGatewayHandler) prepareStudioVideos(c *gin.Context, create bool, info service.GrokMediaRequestInfo, body []byte) {
	if create && !service.StudioLibraryRequested(c) {
		return
	}
	key, owned := middleware.GetAPIKeyFromContext(c)
	if !owned || key == nil || key.UserID <= 0 || key.ID <= 0 {
		return
	}
	owner := service.StudioAssetOwner{UserID: key.UserID, APIKeyID: key.ID}
	service.SetStudioVideoObserver(c, func(task, source string, completed bool, accountID int64) *service.StudioLibraryStatus {
		resource := "task:" + task
		status := &service.StudioLibraryStatus{Status: "warning", GenerationID: resource, Code: "studio_asset_unavailable"}
		library := h.gatewayService.StudioAssets()
		if library == nil {
			if !create {
				return nil
			}
			return status
		}
		ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), 10*time.Second)
		defer cancel()
		var err error
		if create {
			err = library.RecordVideoIntent(ctx, owner, resource, info.Model, info.Prompt, studioVideoMetadata(body), accountID)
		} else {
			err = h.gatewayService.CompleteStudioNativeVideo(ctx, owner, resource, source, accountID, completed)
			if errors.Is(err, service.ErrStudioAssetNotFound) {
				return nil // No durable opt-in: do not collect a status-only request.
			}
		}
		if err == nil {
			status.Status, status.Code = "queued", ""
		}
		return status
	})
}

func (h *OpenAIGatewayHandler) collectQuotedStudioVideo(c *gin.Context, a *service.VideoAttempt) *service.StudioLibraryStatus {
	value, exists := c.Get("studio_video_submission_body")
	body, ok := value.([]byte)
	if !exists || !ok || a == nil || !service.StudioLibraryRequested(c) {
		return nil
	}
	status := &service.StudioLibraryStatus{Status: "warning", GenerationID: "attempt:" + a.ID, Code: "studio_asset_unavailable"}
	key, owned := middleware.GetAPIKeyFromContext(c)
	if !owned || key == nil || key.UserID != a.UserID || key.ID != a.APIKeyID {
		return status
	}
	library := h.gatewayService.StudioAssets()
	if library == nil {
		return status
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), 10*time.Second)
	defer cancel()
	if err := library.RecordVideoIntent(ctx, service.StudioAssetOwner{UserID: key.UserID, APIKeyID: key.ID},
		status.GenerationID, gjson.GetBytes(body, "model").String(), gjson.GetBytes(body, "prompt").String(), studioVideoMetadata(body), a.AccountID); err == nil {
		status.Status, status.Code = "queued", ""
	}
	return status
}
