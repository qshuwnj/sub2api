package service

import (
	"context"
	"encoding/json"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

// Optional capability: update an image's audit while retaining the processing
// lock. These records are never reclaimed to submit a second supplier job.
type ImageQualityClaimStore interface {
	UpdateImageQualityClaim(ctx context.Context, id int64, body string, expiresAt time.Time) error
}

type ImageQualityOutcome struct {
	QuoteID           string `json:"quote_id"`
	Child             int    `json:"child_index"`
	Status            string `json:"status"`
	BillingStatus     string `json:"billing_status"`
	UpstreamRequestID string `json:"upstream_request_id,omitempty"`
	AccountID         int64  `json:"account_id,omitempty"`
}

type imageQualityAttempt struct {
	repo    IdempotencyRepository
	id      int64
	outcome ImageQualityOutcome
}

func (s *ImageQualityPriceSnapshot) Outcome() ImageQualityOutcome {
	if s.attempt != nil {
		return s.attempt.outcome
	}
	return ImageQualityOutcome{QuoteID: s.claims.ID, Child: s.index, Status: "not_submitted", BillingStatus: "not_charged"}
}

func (s *ImageQualityPriceSnapshot) auditBody(outcome ImageQualityOutcome, receipt *ImageChargeReceipt) (string, error) {
	body, err := json.Marshal(struct {
		Accepted imageQuoteClaims    `json:"accepted"`
		Child    int                 `json:"child_index"`
		Outcome  ImageQualityOutcome `json:"outcome"`
		Receipt  *ImageChargeReceipt `json:"receipt,omitempty"`
	}{s.claims, s.index, outcome, receipt})
	return string(body), err
}

func (s *ImageQualityPriceSnapshot) persistOutcome(ctx context.Context) error {
	if s.attempt == nil {
		return ErrIdempotencyStoreUnavail
	}
	store, ok := s.attempt.repo.(ImageQualityClaimStore)
	if !ok {
		return ErrIdempotencyStoreUnavail
	}
	body, err := s.auditBody(s.attempt.outcome, nil)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	err = store.UpdateImageQualityClaim(ctx, s.attempt.id, body, time.Now().Add(24*time.Hour))
	if err != nil {
		logger.LegacyPrintf("service.image_quality", "Image audit update failed quote=%s child=%d prediction=%s status=%s: %v",
			s.claims.ID, s.index, s.attempt.outcome.UpstreamRequestID, s.attempt.outcome.Status, err)
	}
	return err
}

func (s *ImageQualityPriceSnapshot) markOutcome(ctx context.Context, status, predictionID string) error {
	if s.attempt == nil {
		return ErrIdempotencyStoreUnavail
	}
	s.attempt.outcome.Status = status
	if predictionID != "" {
		s.attempt.outcome.UpstreamRequestID = predictionID
	}
	return s.persistOutcome(ctx)
}

func (s *ImageQualityPriceSnapshot) markSubmitting(ctx context.Context, accountID int64) error {
	if s.attempt == nil {
		return ErrIdempotencyStoreUnavail
	}
	s.attempt.outcome.AccountID = accountID
	return s.markOutcome(ctx, "submitting", "")
}

func (s *ImageQualityPriceSnapshot) MarkForwardFailure(ctx context.Context) error {
	if s.attempt == nil {
		return ErrIdempotencyStoreUnavail
	}
	status := s.attempt.outcome.Status
	if status == "accepted" {
		status = "not_submitted"
	} else if status != "failed" && status != "not_submitted" {
		status = "unknown"
	}
	return s.markOutcome(ctx, status, "")
}

func (s *ImageQualityPriceSnapshot) MarkSettlementPending(ctx context.Context) error {
	if s.attempt == nil {
		return ErrIdempotencyStoreUnavail
	}
	s.attempt.outcome.BillingStatus = "unresolved"
	err := s.markOutcome(ctx, "settlement_pending", "")
	if err != nil {
		// No ledger operation is allowed when the pre-settlement audit fails.
		s.attempt.outcome.BillingStatus = "not_charged"
		s.attempt.outcome.Status = "generated"
	}
	return err
}

func (s *ImageQualityPriceSnapshot) MarkSettlementUnknown(ctx context.Context) error {
	return s.markOutcome(ctx, "settlement_unknown", "")
}

func (s *ImageQualityPriceSnapshot) MarkReceiptUnavailable(ctx context.Context) error {
	return s.markOutcome(ctx, "receipt_unavailable", "")
}

func (s *ImageQualityPriceSnapshot) MarkSettled(ctx context.Context, claimID int64, predictionID string) error {
	if s.attempt == nil || s.attempt.id != claimID {
		return ErrIdempotencyStoreUnavail
	}
	s.attempt.outcome.Status = "settled"
	s.attempt.outcome.BillingStatus = "settled"
	s.attempt.outcome.UpstreamRequestID = predictionID
	receipt := s.Receipt()
	body, err := s.auditBody(s.attempt.outcome, &receipt)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	return s.attempt.repo.MarkSucceeded(ctx, claimID, 200, body, time.Now().Add(24*time.Hour))
}
