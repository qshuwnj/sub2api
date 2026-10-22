package service

import (
	"context"
	"errors"
	"regexp"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
)

const (
	VideoAttemptAccepted   = "accepted"
	VideoAttemptSubmitting = "submitting"
	VideoAttemptUncertain  = "uncertain"
	VideoAttemptProcessing = "processing"
	VideoAttemptCompleted  = "completed"
	VideoAttemptFailed     = "failed"
	VideoFinancialReserved = "reserved"
	VideoFinancialCaptured = "captured"
	VideoFinancialReleased = "released"
)

var (
	ErrVideoAttemptUnavailable       = errors.New("durable video billing is unavailable")
	ErrVideoAttemptConflict          = errors.New("video attempt identity or submission content conflicts")
	ErrVideoAttemptNotFound          = errors.New("video attempt not found")
	ErrVideoAttemptTransition        = errors.New("illegal video attempt transition")
	ErrVideoAttemptInsufficientFunds = errors.New("insufficient available balance for video reservation")
	ErrVideoAttemptLimit             = errors.New("video reservation exceeds available quota or concurrency")
	ErrVideoAttemptIneligible        = errors.New("video billing owner, key, group, account or subscription is ineligible")
	ErrVideoAttemptMoney             = errors.New("video amount requires eight decimals and must be below 10000000000")
	ErrVideoAttemptPlatformQuota     = errors.New("quoted video is unavailable for balance billing with an enforced platform quota; use an eligible subscription or ask an administrator")
	ErrVideoAttemptLease             = errors.New("video attempt lease unavailable")
)

// The shared usage/subscription ledger uses numeric(20,10), so its integral
// range, not float64 or int64 minor units, sets the supported video boundary.
var videoLedgerMoney = regexp.MustCompile(`^(?:0|[1-9][0-9]{0,9})\.[0-9]{8}$`)

func ValidVideoLedgerMoney(v string) bool { return videoLedgerMoney.MatchString(v) }

// VideoSubscriptionWindowStarts reuses the subscription's calendar-day,
// anchored seven/thirty-day and short-lived one-time quota conventions.
// A non-nil returned time means reset that counter before checking/capturing.
func VideoSubscriptionWindowStarts(sub *UserSubscription, now time.Time) (daily, weekly, monthly *time.Time) {
	if sub.DailyWindowStart == nil {
		v := timezone.StartOfDay(now)
		daily = &v
	} else if v, ok := sub.automaticDailyWindowStartAt(now); ok {
		daily = &v
	}
	if sub.WeeklyWindowStart == nil {
		v := now
		weekly = &v
	} else if v, ok := sub.automaticWindowStartAt(sub.WeeklyWindowStart, 7*24*time.Hour, now); ok {
		weekly = &v
	}
	if sub.MonthlyWindowStart == nil {
		v := now
		monthly = &v
	} else if v, ok := sub.automaticWindowStartAt(sub.MonthlyWindowStart, 30*24*time.Hour, now); ok {
		monthly = &v
	}
	return
}

type VideoAttemptOwner struct {
	UserID   int64
	APIKeyID int64
}

type VideoChargeReceipt struct {
	RequestID  string    `json:"request_id"`
	Amount     string    `json:"amount"`
	Currency   string    `json:"currency"`
	UsageLogID int64     `json:"usage_log_id"`
	CapturedAt time.Time `json:"captured_at"`
}

// VideoAttempt deliberately excludes prompts, reference inputs and credentials.
// Controls come only from the verified quote; RequestFingerprint hashes the
// actual serialized supplier body without retaining that body.
type VideoAttempt struct {
	ID                 string
	QuoteID            string
	RequestFingerprint string
	UserID             int64
	APIKeyID           int64
	GroupID            int64
	AccountID          int64
	SubscriptionID     *int64
	Selection          VideoQuoteRequest
	BasePrice          string
	TotalPrice         string
	Multiplier         string
	AccountCost        string
	AccountMultiplier  string
	TariffRevision     string
	ContractRevision   string
	SupplierEstimated  bool
	State              string
	FinancialState     string
	UpstreamTaskID     string
	ResultURLs         []string
	Receipt            *VideoChargeReceipt
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

func (a *VideoAttempt) RequestID() string { return "video-quote:" + a.QuoteID }

// Internal repository command. HTTP callers must go through Accept with a
// verified immutable snapshot, never decode this struct from client JSON.
type VideoAttemptAcceptCommand struct {
	Attempt        *VideoAttempt
	QuoteExpiresAt time.Time
}

type VideoAttemptAcceptResult struct {
	Attempt *VideoAttempt
	Created bool // Only the creator may try accepted -> submitting, once.
}

type VideoAttemptUpdate struct {
	State          string
	UpstreamTaskID string
}

type VideoAttemptSettlementResult struct {
	Attempt *VideoAttempt
	Applied bool
}

type VideoAttemptLease struct {
	AttemptID string
	Owner     string
	Token     string
	ExpiresAt time.Time
}

// Optional capability on the existing usage repository. Legacy repositories and
// mocks need not implement it. No alternative ledger or dedup store is used.
type VideoAttemptRepository interface {
	VideoQuotedBillingReady() bool
	AcceptVideoAttempt(context.Context, *VideoAttemptAcceptCommand) (*VideoAttemptAcceptResult, error)
	GetVideoAttempt(context.Context, VideoAttemptOwner, string) (*VideoAttempt, error)
	UpdateVideoAttempt(context.Context, VideoAttemptOwner, string, VideoAttemptUpdate) (*VideoAttempt, error)
	ListReconcilableVideoAttempts(context.Context, time.Time, int) ([]*VideoAttempt, error)
	CaptureVideoAttempt(context.Context, VideoAttemptOwner, string, []string) (*VideoAttemptSettlementResult, error)
	ReleaseVideoAttempt(context.Context, VideoAttemptOwner, string) (*VideoAttemptSettlementResult, error)
	AcquireVideoAttemptLease(context.Context, string, string, time.Duration) (*VideoAttemptLease, error)
	ReleaseVideoAttemptLease(context.Context, *VideoAttemptLease) error
}

// AfterCommit is an invalidation/notification hook only: all money, quotas,
// dedup and the usage log are already committed. It must not bill again.
type VideoAttemptAfterCommit func(context.Context, *VideoAttempt, string)

type VideoAttemptService struct {
	repo        VideoAttemptRepository
	afterCommit VideoAttemptAfterCommit
}

func NewVideoAttemptService(repo any, hooks ...VideoAttemptAfterCommit) *VideoAttemptService {
	r, _ := repo.(VideoAttemptRepository)
	s := &VideoAttemptService{repo: r}
	if len(hooks) > 0 {
		s.afterCommit = hooks[0]
	}
	return s
}

func (s *VideoAttemptService) Repository() (VideoAttemptRepository, error) {
	if s == nil || s.repo == nil || !s.repo.VideoQuotedBillingReady() {
		return nil, ErrVideoAttemptUnavailable
	}
	return s.repo, nil
}

func (s *VideoAttemptService) Accept(ctx context.Context, q *VideoQuoteSnapshot, id string, nativeBody []byte, accountID int64) (*VideoAttemptAcceptResult, error) {
	r, err := s.Repository()
	if err != nil {
		return nil, err
	}
	if q == nil || q.QuoteID() == "" || q.UserID() <= 0 || q.APIKeyID() <= 0 || q.GroupID() <= 0 ||
		id == "" || len(id) > 128 || accountID <= 0 || len(nativeBody) == 0 {
		return nil, ErrVideoAttemptIneligible
	}
	if !ValidVideoLedgerMoney(q.TotalPrice()) || !ValidVideoLedgerMoney(q.BasePrice()) {
		return nil, ErrVideoAttemptMoney
	}
	a := &VideoAttempt{
		ID: id, QuoteID: q.QuoteID(), UserID: q.UserID(), APIKeyID: q.APIKeyID(), GroupID: q.GroupID(), AccountID: accountID,
		RequestFingerprint: HashUsageRequestPayload(nativeBody), Selection: q.Selection(),
		BasePrice: q.BasePrice(), TotalPrice: q.TotalPrice(), Multiplier: q.Multiplier(),
		TariffRevision: q.TariffRevision(), ContractRevision: q.ContractRevision(), SupplierEstimated: q.SupplierEstimated(),
	}
	result, err := r.AcceptVideoAttempt(ctx, &VideoAttemptAcceptCommand{Attempt: a, QuoteExpiresAt: q.ExpiresAt()})
	if err == nil && result.Created && s.afterCommit != nil {
		s.afterCommit(ctx, result.Attempt, "reserve")
	}
	return result, err
}

func (s *VideoAttemptService) Capture(ctx context.Context, owner VideoAttemptOwner, id string, urls []string) (*VideoAttemptSettlementResult, error) {
	r, err := s.Repository()
	if err != nil {
		return nil, err
	}
	result, err := r.CaptureVideoAttempt(ctx, owner, id, urls)
	if err == nil && result.Applied && s.afterCommit != nil {
		s.afterCommit(ctx, result.Attempt, "capture")
	}
	return result, err
}

// Release must only be called for a known definitive rejection/failure, never
// for an HTTP timeout. The repository additionally rejects uncertain attempts.
func (s *VideoAttemptService) Release(ctx context.Context, owner VideoAttemptOwner, id string) (*VideoAttemptSettlementResult, error) {
	r, err := s.Repository()
	if err != nil {
		return nil, err
	}
	result, err := r.ReleaseVideoAttempt(ctx, owner, id)
	if err == nil && result.Applied && s.afterCommit != nil {
		s.afterCommit(ctx, result.Attempt, "release")
	}
	return result, err
}
