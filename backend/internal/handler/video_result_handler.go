package handler

import (
	"errors"
	"net/http"
	"regexp"
	"strings"

	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

var videoByteRange = regexp.MustCompile(`^bytes=(?:[0-9]{1,18}-[0-9]{0,18}|-[0-9]{1,18})$`)

func (h *OpenAIGatewayHandler) videoPlaybackError(c *gin.Context, err error) {
	status, code, message := http.StatusServiceUnavailable, "video_playback_unavailable", "视频暂时无法读取，请稍后重新读取；不会重新生成或扣费。"
	if errors.Is(err, service.ErrVideoResultNotFound) || errors.Is(err, service.ErrVideoAttemptNotFound) {
		status, code, message = http.StatusNotFound, "not_found_error", "Video result not found"
	} else if errors.Is(err, service.ErrVideoResultExpired) {
		status, code, message = http.StatusGone, "video_output_expired", "上游视频链接已失效，且暂未找到可恢复的文件；重新读取不会重新生成或扣费。"
	} else if errors.Is(err, service.ErrVideoAttemptConflict) {
		status, code, message = http.StatusConflict, "video_attempt_conflict", "Video task identity is ambiguous; use the owned attempt"
	}
	c.Header("Cache-Control", "no-store")
	h.errorResponse(c, status, code, message)
}

// Returns false only when the task predates archiving and no committed receipt
// proves ownership. The existing in-flight settlement path then remains intact.
func (h *OpenAIGatewayHandler) recoveredLegacyVideoStatus(c *gin.Context) bool {
	key, ok := middleware.GetAPIKeyFromContext(c)
	if !ok {
		return false
	}
	playback, err := h.gatewayService.RecoverLegacyVideoResult(c.Request.Context(),
		service.VideoAttemptOwner{UserID: key.UserID, APIKeyID: key.ID}, c.Param("request_id"))
	if errors.Is(err, service.ErrVideoResultNotFound) {
		return false
	}
	if err != nil {
		h.videoPlaybackError(c, err)
		return true
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"id": c.Param("request_id"), "status": "completed",
		"video": gin.H{"url": playback.URL}, "outputs": []string{playback.URL},
		"archived": playback.Archived, "content_path": playback.ContentPath})
	return true
}

func (h *OpenAIGatewayHandler) VideoAttemptContent(c *gin.Context) {
	h.videoResultContent(c, true)
}

func (h *OpenAIGatewayHandler) AtlasCloudVideoContent(c *gin.Context) {
	h.videoResultContent(c, false)
}

func (h *OpenAIGatewayHandler) videoResultContent(c *gin.Context, attempt bool) {
	key, ok := middleware.GetAPIKeyFromContext(c)
	if !ok {
		h.errorResponse(c, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return
	}
	owner := service.VideoAttemptOwner{UserID: key.UserID, APIKeyID: key.ID}
	id, resource := c.Param("request_id"), "task:"+c.Param("request_id")
	if attempt {
		id, resource = c.Param("attempt_id"), "attempt:"+c.Param("attempt_id")
	}
	if !service.ValidVideoAttemptID(id) || !attempt && !strings.HasPrefix(id, "atlascloud:") {
		h.videoPlaybackError(c, service.ErrVideoResultNotFound)
		return
	}
	// Old clients know only the supplier task. Resolve its unique owned attempt
	// before accessing the same archived object, never another account's task.
	if !attempt {
		a, err := h.gatewayService.FindQuotedVideoTask(c.Request.Context(), owner, id)
		if err == nil {
			resource = "attempt:" + a.ID
		} else if !errors.Is(err, service.ErrVideoAttemptNotFound) {
			h.videoPlaybackError(c, err)
			return
		}
	}
	byteRange := c.GetHeader("Range")
	if byteRange != "" && !videoByteRange.MatchString(byteRange) {
		h.errorResponse(c, http.StatusRequestedRangeNotSatisfiable, "invalid_range", "A single video byte range is required")
		return
	}
	content, err := h.gatewayService.OpenVideoResult(c.Request.Context(), owner, resource, byteRange)
	if err != nil {
		h.videoPlaybackError(c, err)
		return
	}
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
}
