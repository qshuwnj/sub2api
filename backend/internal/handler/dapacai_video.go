package handler

import (
	"net/http"
	"strings"

	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func (h *OpenAIGatewayHandler) DapacaiVideoGeneration(c *gin.Context) {
	h.handleGrokMedia(c, service.DapacaiEndpointVideoCreate, "")
}

func (h *OpenAIGatewayHandler) DapacaiVideoStatus(c *gin.Context) {
	key, ok := middleware.GetAPIKeyFromContext(c)
	task := c.Param("request_id")
	if ok && key != nil && strings.HasPrefix(task, "dapacai:") {
		owner := service.VideoAttemptOwner{UserID: key.UserID, APIKeyID: key.ID}
		if playback, err := h.gatewayService.VideoPlayback(c.Request.Context(), owner, "task:"+task); err == nil {
			receipt, err := h.gatewayService.LookupVideoChargeReceipt(c.Request.Context(), owner, task)
			if err != nil || receipt == nil {
				h.errorResponse(c, http.StatusServiceUnavailable, "billing_state_unavailable", "Video charge receipt is unavailable; retry status later")
				return
			}
			c.Header("Cache-Control", "private, no-store")
			c.JSON(http.StatusOK, gin.H{"id": task, "status": "completed", "archived": true,
				"content_path": playback.ContentPath, "receipt": receipt})
			return
		}
	}
	h.handleGrokMedia(c, service.DapacaiEndpointVideoStatus, c.Param("request_id"))
}

func (h *OpenAIGatewayHandler) DapacaiVideoContent(c *gin.Context) {
	key, ok := middleware.GetAPIKeyFromContext(c)
	task := c.Param("request_id")
	if !ok || key == nil || !strings.HasPrefix(task, "dapacai:") {
		h.errorResponse(c, http.StatusNotFound, "not_found_error", "Video request not found")
		return
	}
	if value := c.GetHeader("Range"); value != "" && !videoByteRange.MatchString(value) {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "Invalid byte range")
		return
	}
	// An authenticated supplier file is not proof of a captured customer debit.
	// Do not stream anything until the existing owned receipt has committed.
	receipt, err := h.gatewayService.LookupVideoChargeReceipt(c.Request.Context(),
		service.VideoAttemptOwner{UserID: key.UserID, APIKeyID: key.ID}, task)
	if err != nil || receipt == nil {
		h.errorResponse(c, http.StatusServiceUnavailable, "billing_state_unavailable", "请先刷新视频任务状态，确认本次扣费后再下载；不会重新生成")
		return
	}
	if content, err := h.gatewayService.OpenVideoResult(c.Request.Context(),
		service.VideoAttemptOwner{UserID: key.UserID, APIKeyID: key.ID}, "task:"+task, c.GetHeader("Range")); err == nil {
		defer content.Body.Close()
		if content.MIME != "video/mp4" && content.MIME != "video/webm" {
			h.videoPlaybackError(c, service.ErrVideoResultUnavailable)
			return
		}
		headers := map[string]string{"Cache-Control": "private, no-store", "X-Content-Type-Options": "nosniff", "Accept-Ranges": "bytes"}
		if content.ContentRange != "" {
			headers["Content-Range"] = content.ContentRange
		}
		c.DataFromReader(content.Status, content.Size, content.MIME, content.Body, headers)
		return
	}
	h.handleGrokMedia(c, service.DapacaiEndpointVideoContent, task)
}
