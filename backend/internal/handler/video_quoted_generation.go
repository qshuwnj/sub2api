package handler

import (
	"errors"
	"net/http"
	"strings"
	"time"

	pkghttputil "github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/Wei-Shaw/sub2api/internal/pkg/requestmodel"
	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

type videoAttemptResponse struct {
	AttemptID      string                      `json:"attempt_id"`
	ID             string                      `json:"id,omitempty"`
	RequestID      string                      `json:"request_id,omitempty"`
	Status         string                      `json:"status"`
	FinancialState string                      `json:"financial_state"`
	Model          string                      `json:"model"`
	QuoteID        string                      `json:"quote_id"`
	LockedPrice    videoLockedPrice            `json:"locked_price"`
	Outputs        []string                    `json:"outputs,omitempty"`
	Video          *videoResult                `json:"video,omitempty"`
	Receipt        *service.VideoChargeReceipt `json:"receipt,omitempty"`
	RecoveryNeeded bool                        `json:"recovery_required"`
	Error          *videoAttemptResponseError  `json:"error,omitempty"`
}
type videoLockedPrice struct {
	Amount            string `json:"amount"`
	Currency          string `json:"currency"`
	BillingUnit       string `json:"billing_unit"`
	UnitCount         int    `json:"unit_count"`
	SupplierEstimated bool   `json:"supplier_estimated"`
}
type videoResult struct {
	URL string `json:"url"`
}
type videoAttemptResponseError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

func (h *OpenAIGatewayHandler) respondVideoAttempt(c *gin.Context, a *service.VideoAttempt, err error) {
	if a == nil {
		h.videoAttemptError(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	dto := videoAttemptResponse{
		AttemptID: a.ID, ID: a.UpstreamTaskID, RequestID: a.UpstreamTaskID, Status: a.State,
		FinancialState: a.FinancialState, Model: a.Selection.Model, QuoteID: a.QuoteID,
		LockedPrice:    videoLockedPrice{Amount: a.TotalPrice, Currency: "USD", BillingUnit: "job", UnitCount: 1, SupplierEstimated: a.SupplierEstimated},
		RecoveryNeeded: a.FinancialState == service.VideoFinancialReserved,
	}
	status := http.StatusOK
	if dto.RecoveryNeeded {
		status = http.StatusAccepted
		c.Header("Retry-After", "15")
	}
	// A supplier output is not a customer result until the durable capture and
	// receipt exist together. No internal account, cost or snapshot is exposed.
	if a.FinancialState == service.VideoFinancialCaptured && a.Receipt != nil {
		dto.Receipt, dto.Outputs = a.Receipt, a.ResultURLs
		if len(a.ResultURLs) > 0 {
			dto.Video = &videoResult{URL: a.ResultURLs[0]}
		}
	}
	if err != nil {
		dto.Error = &videoAttemptResponseError{Type: "video_recovery_pending", Message: "Recover this existing attempt; do not submit another generation"}
		var transport *service.VideoTransportError
		if errors.As(err, &transport) && transport.DefinitiveRejection {
			status = http.StatusBadGateway
			dto.Error = &videoAttemptResponseError{Type: "video_submission_rejected", Message: "The supplier rejected this submission; its reservation was released"}
		} else if errors.Is(err, service.ErrVideoAttemptUnavailable) {
			status = http.StatusServiceUnavailable
		}
	}
	c.JSON(status, dto)
}

func (h *OpenAIGatewayHandler) videoAttemptError(c *gin.Context, err error) {
	c.Header("Cache-Control", "no-store")
	status, code, message := http.StatusServiceUnavailable, "video_billing_unavailable", "Durable video billing is unavailable; recover an existing attempt rather than resubmitting"
	switch {
	case errors.Is(err, service.ErrVideoAttemptNotFound):
		status, code, message = 404, "not_found_error", "Video attempt not found"
	case errors.Is(err, service.ErrVideoAttemptConflict):
		status, code, message = 409, "video_attempt_conflict", "Attempt content conflicts or supplier task identity is ambiguous; use the owned attempt resource"
	case errors.Is(err, service.ErrVideoAttemptPlatformQuota):
		code, message = "video_platform_quota_unsupported", service.ErrVideoAttemptPlatformQuota.Error()
	case errors.Is(err, service.ErrVideoAttemptInsufficientFunds):
		status, code, message = 403, "insufficient_balance", service.ErrVideoAttemptInsufficientFunds.Error()
	case errors.Is(err, service.ErrVideoAttemptLimit):
		status, code, message = 429, "video_reservation_limit", service.ErrVideoAttemptLimit.Error()
	case errors.Is(err, service.ErrVideoAttemptIneligible), errors.Is(err, service.ErrVideoAttemptMoney):
		status, code, message = 400, "video_attempt_rejected", err.Error()
	}
	h.errorResponse(c, status, code, message)
}

// tryQuotedAtlasVideo returns false only for truly unenrolled legacy requests.
// A quote/idempotency header can never silently fall through to legacy billing.
func (h *OpenAIGatewayHandler) tryQuotedAtlasVideo(c *gin.Context) bool {
	key, ok := middleware.GetAPIKeyFromContext(c)
	if !ok {
		h.errorResponse(c, 401, "authentication_error", "Invalid API key")
		return true
	}
	body, err := pkghttputil.ReadRequestBodyWithPrealloc(c.Request)
	if err != nil {
		if maxErr, ok := extractMaxBytesError(err); ok {
			h.errorResponse(c, http.StatusRequestEntityTooLarge, "invalid_request_error", buildBodyTooLargeMessage(maxErr.Limit))
			return true
		}
		h.errorResponse(c, 400, "invalid_request_error", "Failed to read video request")
		return true
	}
	requestmodel.ResetRequestBody(c.Request, body)
	info, native, err := service.SerializeAtlasCloudVideoRequest(body)
	if err != nil {
		h.errorResponse(c, 400, "invalid_request_error", err.Error())
		return true
	}
	id, token := strings.TrimSpace(c.GetHeader("Idempotency-Key")), strings.TrimSpace(c.GetHeader("X-Video-Quote"))
	owner := service.VideoAttemptOwner{UserID: key.UserID, APIKeyID: key.ID}
	if id != "" {
		if !service.ValidVideoAttemptID(id) {
			h.errorResponse(c, 400, "invalid_request_error", "Idempotency-Key must be a stable 1-128 character video attempt identity")
			return true
		}
		a, recoveryErr := h.gatewayService.RecoverQuotedVideo(c.Request.Context(), owner, id, native)
		if recoveryErr == nil {
			h.respondVideoAttempt(c, a, nil)
			return true
		}
		if !errors.Is(recoveryErr, service.ErrVideoAttemptNotFound) {
			h.videoAttemptError(c, recoveryErr)
			return true
		}
	}
	enrolled, err := h.gatewayService.FreshVideoPricingEnrolled(c.Request.Context(), key, info.Model)
	if err != nil {
		h.errorResponse(c, 503, "pricing_unavailable", "Native video pricing could not be refreshed")
		return true
	}
	if !enrolled && token == "" && id == "" {
		return false
	}
	if token == "" || id == "" {
		h.errorResponse(c, 400, "video_quote_required", "Confirm a current video quote and provide X-Video-Quote plus a stable Idempotency-Key")
		return true
	}
	snapshot, err := h.gatewayService.AcceptVideoQuote(c.Request.Context(), key, native, token, time.Now())
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, service.ErrVideoQuoteUnavailable) {
			status = http.StatusServiceUnavailable
		}
		h.errorResponse(c, status, "video_quote_rejected", err.Error())
		return true
	}
	key, err = h.gatewayService.RefreshVideoQuoteKey(c.Request.Context(), key, info.Model, time.Now())
	if err != nil {
		h.errorResponse(c, 400, "video_quote_rejected", "Video admission changed; obtain a fresh quote")
		return true
	}
	subject, ok := middleware.GetAuthSubjectFromContext(c)
	if !ok || subject.UserID != key.UserID {
		h.errorResponse(c, 401, "authentication_error", "Invalid video owner")
		return true
	}
	log := requestLogger(c, "handler.openai_gateway.quoted_video")
	if !h.ensureResponsesDependencies(c, log) {
		return true
	}
	if moderation := info.ModerationBody(); len(moderation) > 0 {
		if decision := h.checkSecurityAudit(c, log, key, subject, service.ContentModerationProtocolOpenAIImages, info.Model, moderation); decision != nil && !decision.AllowNextStage {
			h.openAISecurityAuditError(c, decision)
			return true
		}
	}
	streamStarted := false
	imageRelease, acquired := h.acquireImageGenerationSlot(c, false)
	if !acquired {
		return true
	}
	if imageRelease != nil {
		defer imageRelease()
	}
	userRelease, acquired := h.acquireResponsesUserSlot(c, subject.UserID, subject.Concurrency, false, &streamStarted, log)
	if !acquired {
		return true
	}
	if userRelease != nil {
		defer userRelease()
	}
	subscription, _ := middleware.GetSubscriptionFromContext(c)
	if err := h.billingCacheService.CheckBillingEligibility(c.Request.Context(), key.User, key, key.Group, subscription, service.QuotaPlatform(c.Request.Context(), key)); err != nil {
		status, code, message, _ := billingErrorDetails(err)
		h.errorResponse(c, status, code, message)
		return true
	}
	requestCtx := service.WithOpenAIProfitControlSuppressed(c.Request.Context())
	session := h.gatewayService.GenerateExplicitSessionHash(c, []byte(id))
	selection, _, err := h.gatewayService.SelectAccountWithSchedulerForCapability(
		requestCtx, key.GroupID, "", session, info.Model, nil, service.OpenAIUpstreamTransportHTTPSSE,
		service.OpenAIEndpointCapabilityAtlasCloudVideo, false, false, false, service.PlatformOpenAI,
	)
	if selection != nil && selection.Acquired && selection.ReleaseFunc != nil {
		selection.ReleaseFunc = wrapReleaseOnDone(requestCtx, selection.ReleaseFunc)
		defer selection.ReleaseFunc()
	}
	if err != nil || selection == nil || selection.Account == nil {
		h.errorResponse(c, 503, "atlascloud_no_eligible_account", "No eligible exact-model AtlasCloud account")
		return true
	}
	releaseAccount, result := h.acquireResponsesAccountSlot(c, key.GroupID, session, selection, false, &streamStarted, log)
	if releaseAccount != nil {
		defer releaseAccount()
	}
	if result != openAISlotAcquireOK {
		return true
	}
	setOpsSelectedAccount(c, selection.Account.ID, selection.Account.Platform)
	a, dispatchErr := h.gatewayService.DispatchQuotedVideo(requestCtx, snapshot, id, native, selection.Account)
	h.respondVideoAttempt(c, a, dispatchErr)
	return true
}

// VideoAttempt is the caller-owned durable recovery resource. Polling accepted
// work is not fresh generation admission and never checks a current tariff.
func (h *OpenAIGatewayHandler) VideoAttempt(c *gin.Context) {
	key, ok := middleware.GetAPIKeyFromContext(c)
	if !ok {
		h.errorResponse(c, 401, "authentication_error", "Invalid API key")
		return
	}
	id := c.Param("attempt_id")
	if !service.ValidVideoAttemptID(id) {
		h.videoAttemptError(c, service.ErrVideoAttemptNotFound)
		return
	}
	a, err := h.gatewayService.PollQuotedVideo(c.Request.Context(), service.VideoAttemptOwner{UserID: key.UserID, APIKeyID: key.ID}, id)
	h.respondVideoAttempt(c, a, err)
}

// RecoverVideoSubmission is exclusively the server-verified POST retry path.
// A disappeared record, changed body or unavailable store cannot create work.
func (h *OpenAIGatewayHandler) RecoverVideoSubmission(c *gin.Context) {
	key, ok := middleware.GetAPIKeyFromContext(c)
	if !ok || !middleware.VerifiedVideoAttemptRecovery(c) {
		h.errorResponse(c, 401, "authentication_error", "Video recovery was not authenticated")
		return
	}
	body, err := pkghttputil.ReadRequestBodyWithPrealloc(c.Request)
	if err != nil {
		if maxErr, ok := extractMaxBytesError(err); ok {
			h.errorResponse(c, http.StatusRequestEntityTooLarge, "invalid_request_error", buildBodyTooLargeMessage(maxErr.Limit))
			return
		}
		h.errorResponse(c, 400, "invalid_request_error", "Failed to read video recovery request")
		return
	}
	_, native, err := service.SerializeAtlasCloudVideoRequest(body)
	if err != nil {
		h.errorResponse(c, 409, "video_attempt_conflict", "Recovery body does not match the accepted native request")
		return
	}
	a, err := h.gatewayService.RecoverQuotedVideo(c.Request.Context(),
		service.VideoAttemptOwner{UserID: key.UserID, APIKeyID: key.ID}, c.GetHeader("Idempotency-Key"), native)
	h.respondVideoAttempt(c, a, err)
}

// QuotedVideoStatus discovers durable tasks before the legacy Redis-pending
// path. Ambiguous owned task IDs fail closed, never falling into legacy billing.
func (h *OpenAIGatewayHandler) QuotedVideoStatus(c *gin.Context) bool {
	key, ok := middleware.GetAPIKeyFromContext(c)
	if !ok {
		return false
	}
	owner := service.VideoAttemptOwner{UserID: key.UserID, APIKeyID: key.ID}
	a, err := h.gatewayService.FindQuotedVideoTask(c.Request.Context(), owner, c.Param("request_id"))
	if errors.Is(err, service.ErrVideoAttemptNotFound) {
		return false
	}
	if err != nil {
		h.videoAttemptError(c, err)
		return true
	}
	a, err = h.gatewayService.PollQuotedVideo(c.Request.Context(), owner, a.ID)
	h.respondVideoAttempt(c, a, err)
	return true
}
