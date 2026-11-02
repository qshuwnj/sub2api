package service

import (
	"context"
	"strings"
)

// Optional, read-only recovery of a legacy video's committed customer debit.
// Quoted attempts already carry their immutable receipt in the status response.
type VideoChargeReceiptReader interface {
	FindVideoChargeReceipt(context.Context, VideoAttemptOwner, string) (*VideoChargeReceipt, error)
}

func ValidVideoReceiptTaskID(task string) bool {
	if !ValidVideoAttemptID(task) || strings.HasPrefix(task, "grok-video:") {
		return false
	}
	switch task {
	case "generations", "edits", "extensions", "attempts", "quote":
		return false
	}
	return true
}

func (s *OpenAIGatewayService) LookupVideoChargeReceipt(ctx context.Context, owner VideoAttemptOwner, task string) (*VideoChargeReceipt, error) {
	if s == nil || owner.UserID <= 0 || owner.APIKeyID <= 0 || !ValidVideoReceiptTaskID(task) {
		return nil, ErrVideoResultNotFound
	}
	reader, ok := s.usageBillingRepo.(VideoChargeReceiptReader)
	if !ok {
		return nil, ErrVideoResultNotFound
	}
	return reader.FindVideoChargeReceipt(ctx, owner, task)
}
