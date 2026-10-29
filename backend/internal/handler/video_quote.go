package handler

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// VideoQuote accepts controls only. It never calls a supplier or reserves money.
func (h *OpenAIGatewayHandler) VideoQuote(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	key, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok {
		h.errorResponse(c, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return
	}
	var input service.VideoQuoteRequest
	decoder := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 16384))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil || input.Model == "" || input.Mode == "" || input.Controls == nil {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "Supply exact model, mode and non-content controls only")
		return
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "Invalid quote JSON")
		return
	}
	if _, err := service.VideoTariffKey(input); err != nil {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	for _, field := range []string{"prompt", "negative_prompt", "image", "image_url", "images", "image_urls", "reference_images", "reference_videos", "reference_audios", "last_image", "video", "audio", "asset_id", "source_url", "total_price", "unit_price"} {
		if _, exists := input.Controls[field]; exists {
			h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "Quote controls must not contain content or prices")
			return
		}
	}
	quote, err := h.gatewayService.QuoteVideo(c.Request.Context(), key, input, time.Now())
	if err != nil {
		if errors.Is(err, service.ErrVideoQuoteUnavailable) {
			h.errorResponse(c, http.StatusServiceUnavailable, "pricing_unavailable", err.Error())
			return
		}
		h.errorResponse(c, http.StatusBadRequest, "video_quote_rejected", err.Error())
		return
	}
	c.JSON(http.StatusOK, quote)
}
