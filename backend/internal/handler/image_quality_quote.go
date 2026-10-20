package handler

import (
	"encoding/json"
	"io"
	"net/http"
	"time"

	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// ImageQuote never contacts the supplier or reserves funds.
func (h *OpenAIGatewayHandler) ImageQuote(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	key, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok {
		h.errorResponse(c, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return
	}
	var input service.ImageQuoteRequest
	decoder := json.NewDecoder(io.LimitReader(c.Request.Body, 8192))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil || input.Model == "" {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "Supply model, quality, size and count only")
		return
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "Invalid quote JSON")
		return
	}
	key, err := h.gatewayService.RefreshImageQualityKey(c.Request.Context(), key, input.Model)
	if err != nil {
		h.errorResponse(c, http.StatusServiceUnavailable, "pricing_unavailable", err.Error())
		return
	}
	if !service.GroupAllowsImageGeneration(key.Group) {
		h.errorResponse(c, http.StatusForbidden, "permission_error", service.ImageGenerationPermissionMessage())
		return
	}
	if key.Group.ModelAllowlistEnabled() && !key.Group.ModelAllowlist.Allows(input.Model) {
		h.errorResponse(c, http.StatusNotFound, "model_not_found", "Model is not available for this group")
		return
	}
	if key.GroupID != nil && h.gatewayService.IsModelRestricted(c.Request.Context(), *key.GroupID, input.Model) {
		h.errorResponse(c, http.StatusForbidden, "permission_error", "Model is restricted by this channel")
		return
	}
	ensureCompositeTargetPlatform(c, key, input.Model)
	if !compositeTargetPlatformAllowed(c, key, input.Model, service.PlatformOpenAI) {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "Model is not supported by this endpoint")
		return
	}
	mapping, _ := h.gatewayService.ResolveChannelMappingAndRestrict(c.Request.Context(), key.GroupID, input.Model)
	if mapping.MappedModel != "" && mapping.MappedModel != input.Model {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "Quotes require the exact upstream model ID")
		return
	}
	quote, err := h.gatewayService.QuoteImageQuality(c.Request.Context(), key, input, time.Now())
	if err != nil {
		h.errorResponse(c, http.StatusBadRequest, "pricing_not_configured", err.Error())
		return
	}
	c.JSON(http.StatusOK, quote)
}
