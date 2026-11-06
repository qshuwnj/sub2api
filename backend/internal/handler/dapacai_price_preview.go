package handler

import (
	"errors"
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func (h *OpenAIGatewayHandler) dapacaiPricePreviewError(c *gin.Context, err error) {
	status, code, message := http.StatusBadRequest, "pricing_not_configured", err.Error()
	switch {
	case errors.Is(err, service.ErrVideoQuoteUnavailable):
		status, code, message = http.StatusServiceUnavailable, "pricing_unavailable", "Current pricing is temporarily unavailable"
	case message == "no exact capable Dapacai account available":
		status, code = http.StatusServiceUnavailable, "dapacai_no_eligible_account"
	case message == "video model not allowed for this group" || message == "video generation permission denied":
		status, code = http.StatusForbidden, "permission_error"
	case message == "video channel restricts or remaps the exact model":
		status, code = http.StatusForbidden, "permission_error"
	}
	h.errorResponse(c, status, code, message)
}
