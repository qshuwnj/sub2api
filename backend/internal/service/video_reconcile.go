package service

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

var videoAttemptIDPattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)

func ValidVideoAttemptID(id string) bool { return videoAttemptIDPattern.MatchString(id) }

type VideoAttemptRecoveryLookup func(context.Context, VideoAttemptOwner, string) (*VideoAttempt, error)

// Installed once at startup. Middleware may skip only fresh-generation billing,
// never base identity, disabled-user/key, group or IP authentication.
func (s *APIKeyService) SetVideoAttemptRecoveryLookup(lookup VideoAttemptRecoveryLookup) {
	s.videoRecoveryMu.Lock()
	defer s.videoRecoveryMu.Unlock()
	if s.videoRecoveryLookup == nil {
		s.videoRecoveryLookup = lookup
	}
}

func (s *APIKeyService) CanRecoverVideoAttempt(ctx context.Context, key *APIKey, id, model string) (bool, error) {
	if s == nil || key == nil || !ValidVideoAttemptID(id) || !IsAtlasCloudVideoModel(model) {
		return false, nil
	}
	s.videoRecoveryMu.RLock()
	lookup := s.videoRecoveryLookup
	s.videoRecoveryMu.RUnlock()
	if lookup == nil {
		return false, nil
	}
	a, err := lookup(ctx, VideoAttemptOwner{UserID: key.UserID, APIKeyID: key.ID}, id)
	if errors.Is(err, ErrVideoAttemptNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return a != nil && a.ID == id && a.UserID == key.UserID && a.APIKeyID == key.ID && a.Selection.Model == model, nil
}

func (s *OpenAIGatewayService) FreshVideoPricingEnrolled(ctx context.Context, key *APIKey, model string) (bool, error) {
	if key != nil && key.GroupID != nil && s.channelService != nil && s.channelService.groupRepo != nil {
		group, err := s.channelService.groupRepo.GetByIDLite(ctx, *key.GroupID)
		if err != nil || group == nil {
			return false, ErrVideoQuoteUnavailable
		}
		copy := *key
		copy.Group = group
		key = &copy
	}
	return s.VideoPricingEnrolled(key, model)
}

// VideoAttemptTaskLookup is optional so legacy usage repositories/mocks remain
// unchanged. A task collision across bound accounts must return conflict.
type VideoAttemptTaskLookup interface {
	GetVideoAttemptByTask(context.Context, VideoAttemptOwner, string) (*VideoAttempt, error)
}

type VideoKnownTaskReconcileRepository interface {
	ListKnownTaskVideoAttempts(context.Context, time.Time, int) ([]*VideoAttempt, error)
}

func (s *OpenAIGatewayService) VideoAttempts() *VideoAttemptService {
	s.videoAttemptOnce.Do(func() {
		s.videoAttempts = NewVideoAttemptService(s.usageBillingRepo, func(ctx context.Context, a *VideoAttempt, phase string) {
			ctx, cancel := detachedBillingContext(ctx)
			defer cancel()
			if s.billingCacheService != nil {
				if a.SubscriptionID == nil {
					_ = s.billingCacheService.InvalidateUserBalance(ctx, a.UserID)
				} else {
					_ = s.billingCacheService.InvalidateSubscription(ctx, a.UserID, a.GroupID)
				}
				_ = s.billingCacheService.InvalidateAPIKeyRateLimit(ctx, a.APIKeyID)
			}
			if s.videoAuthInvalidator != nil {
				s.videoAuthInvalidator.InvalidateAuthCacheByUserID(ctx, a.UserID)
			}
			if phase == "capture" && s.deferredService != nil {
				s.deferredService.ScheduleLastUsedUpdate(a.AccountID)
			}
		})
	})
	return s.videoAttempts
}

// RecoverQuotedVideo runs before fresh quote/admission. An accepted but unsent
// attempt stays held; a retry is not permission to send its POST.
func (s *OpenAIGatewayService) RecoverQuotedVideo(ctx context.Context, owner VideoAttemptOwner, id string, native []byte) (*VideoAttempt, error) {
	r, err := s.VideoAttempts().Repository()
	if err != nil {
		return nil, err
	}
	a, err := r.GetVideoAttempt(ctx, owner, id)
	if err != nil {
		return nil, err
	}
	if a.RequestFingerprint != HashUsageRequestPayload(native) {
		return nil, ErrVideoAttemptConflict
	}
	return a, nil
}

func (s *OpenAIGatewayService) DispatchQuotedVideo(ctx context.Context, quote *VideoQuoteSnapshot, id string, native []byte, account *Account) (*VideoAttempt, error) {
	if quote == nil {
		return nil, ErrVideoAttemptIneligible
	}
	owner := VideoAttemptOwner{UserID: quote.UserID(), APIKeyID: quote.APIKeyID()}
	if existing, err := s.RecoverQuotedVideo(ctx, owner, id, native); err == nil {
		return existing, nil
	} else if !errors.Is(err, ErrVideoAttemptNotFound) {
		return nil, err
	}
	if account == nil || !account.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityAtlasCloudVideo) ||
		account.GetMappedModel(quote.Selection().Model) != quote.Selection().Model {
		return nil, ErrVideoAttemptIneligible
	}
	if strings.TrimSpace(account.GetCredential("api_key")) == "" || s.httpUpstream == nil {
		return nil, ErrVideoAttemptUnavailable
	}
	billing := s.VideoAttempts()
	r, err := billing.Repository()
	if err != nil {
		return nil, err
	}
	accepted, err := billing.Accept(ctx, quote, id, native, account.ID)
	if err != nil {
		if errors.Is(err, ErrVideoAttemptConflict) {
			return s.RecoverQuotedVideo(ctx, owner, id, native)
		}
		return nil, err
	}
	if !accepted.Created {
		return accepted.Attempt, nil
	}
	// From the moment reserve commits, cancellation cannot erase a supplier
	// acceptance. This bounded detached context covers POST and ID persistence.
	durableCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 90*time.Second)
	defer cancel()
	a, err := r.UpdateVideoAttempt(durableCtx, owner, id, VideoAttemptUpdate{State: VideoAttemptSubmitting})
	if err != nil {
		return accepted.Attempt, err
	}
	result, transportErr := s.requestQuotedAtlasVideo(durableCtx, account, "", native)
	if result != nil && result.TaskID != "" {
		bound, err := r.UpdateVideoAttempt(durableCtx, owner, id, VideoAttemptUpdate{State: VideoAttemptProcessing, UpstreamTaskID: result.TaskID})
		if err != nil {
			// A POST may have succeeded. Never release or resubmit on a DB error.
			return a, ErrVideoAttemptUnavailable
		}
		a = bound
		if transportErr == nil {
			return s.settleQuotedVideo(durableCtx, a, result)
		}
		return a, transportErr
	}
	if definitiveVideoRejection(transportErr) {
		settled, releaseErr := billing.Release(durableCtx, owner, id)
		if releaseErr != nil {
			return a, releaseErr
		}
		return settled.Attempt, transportErr
	}
	uncertain, err := r.UpdateVideoAttempt(durableCtx, owner, id, VideoAttemptUpdate{State: VideoAttemptUncertain})
	if err != nil {
		return a, ErrVideoAttemptUnavailable
	}
	return uncertain, transportErr
}

func (s *OpenAIGatewayService) settleQuotedVideo(ctx context.Context, a *VideoAttempt, result *VideoSupplierResult) (*VideoAttempt, error) {
	owner := VideoAttemptOwner{UserID: a.UserID, APIKeyID: a.APIKeyID}
	var settled *VideoAttemptSettlementResult
	var err error
	switch result.State {
	case VideoAttemptCompleted:
		settled, err = s.VideoAttempts().Capture(ctx, owner, a.ID, result.ResultURLs)
	case VideoAttemptFailed:
		settled, err = s.VideoAttempts().Release(ctx, owner, a.ID)
	default:
		return a, nil
	}
	if err != nil {
		return a, err
	}
	if settled.Attempt.FinancialState == VideoFinancialCaptured && len(settled.Attempt.ResultURLs) == 1 {
		saved := settled.Attempt
		_ = s.QueueVideoResult(ctx, &VideoResult{Resource: "attempt:" + saved.ID,
			TaskID: saved.UpstreamTaskID, UserID: saved.UserID, APIKeyID: saved.APIKeyID,
			AccountID: saved.AccountID, SourceURL: saved.ResultURLs[0]})
	}
	return settled.Attempt, nil
}

func (s *OpenAIGatewayService) GetQuotedVideo(ctx context.Context, owner VideoAttemptOwner, id string) (*VideoAttempt, error) {
	if s == nil {
		return nil, ErrVideoAttemptUnavailable
	}
	r, err := s.VideoAttempts().Repository()
	if err != nil {
		return nil, err
	}
	return r.GetVideoAttempt(ctx, owner, id)
}

func (s *OpenAIGatewayService) FindQuotedVideoTask(ctx context.Context, owner VideoAttemptOwner, task string) (*VideoAttempt, error) {
	if s == nil {
		return nil, ErrVideoAttemptNotFound
	}
	lookup, ok := s.usageBillingRepo.(VideoAttemptTaskLookup)
	if !ok {
		return nil, ErrVideoAttemptNotFound
	}
	if _, err := s.VideoAttempts().Repository(); err != nil {
		return nil, err // An installed but unavailable durable store is not legacy.
	}
	return lookup.GetVideoAttemptByTask(ctx, owner, task)
}

// PollQuotedVideo never schedules or generates work. It loads credentials from
// the persisted account even when new pricing/enrollment/quota has changed.
func (s *OpenAIGatewayService) PollQuotedVideo(ctx context.Context, owner VideoAttemptOwner, id string, refresh ...bool) (*VideoAttempt, error) {
	a, err := s.GetQuotedVideo(ctx, owner, id)
	if err != nil {
		return a, err
	}
	if a.FinancialState == VideoFinancialCaptured {
		return s.refreshCompletedVideo(ctx, a, len(refresh) > 0 && refresh[0])
	}
	if a.FinancialState != VideoFinancialReserved || a.UpstreamTaskID == "" {
		return a, err
	}
	r, err := s.VideoAttempts().Repository()
	if err != nil {
		return a, err
	}
	lease, err := r.AcquireVideoAttemptLease(ctx, a.ID, uuid.NewString(), 2*time.Minute)
	if errors.Is(err, ErrVideoAttemptLease) {
		return a, nil // Another process is already polling this known task.
	}
	if err != nil {
		return a, err
	}
	defer func() {
		releaseCtx, cancel := detachedBillingContext(ctx)
		defer cancel()
		_ = r.ReleaseVideoAttemptLease(releaseCtx, lease)
	}()
	a, err = r.GetVideoAttempt(ctx, owner, id)
	if err != nil || a.FinancialState != VideoFinancialReserved || a.UpstreamTaskID == "" {
		return a, err
	}
	// Touch the known task under lease before the poll. This bounds retries and
	// prevents a permanently unavailable account starving later task batches.
	a, err = r.UpdateVideoAttempt(ctx, owner, id, VideoAttemptUpdate{State: VideoAttemptProcessing, UpstreamTaskID: a.UpstreamTaskID})
	if err != nil {
		latest, loadErr := r.GetVideoAttempt(ctx, owner, id)
		if loadErr == nil && latest.FinancialState != VideoFinancialReserved {
			return latest, nil
		}
		return a, err
	}
	account, err := s.accountRepo.GetByID(ctx, a.AccountID)
	if err != nil || account == nil || account.ID != a.AccountID {
		return a, ErrVideoAttemptUnavailable
	}
	result, err := s.requestQuotedAtlasVideo(ctx, account, a.UpstreamTaskID, nil)
	if err != nil {
		return a, err
	}
	settleCtx, cancel := detachedBillingContext(ctx)
	defer cancel()
	return s.settleQuotedVideo(settleCtx, a, result)
}

// VideoReconcileService serially polls at most 100 known IDs per bounded tick.
// Database leases, not a process mutex, provide multi-instance exclusion.
type VideoReconcileService struct {
	gateway  *OpenAIGatewayService
	interval time.Duration
	mu       sync.Mutex
	cancel   context.CancelFunc
	done     chan struct{}
}

func NewVideoReconcileService(gateway *OpenAIGatewayService, interval time.Duration) *VideoReconcileService {
	if interval <= 0 {
		interval = 15 * time.Second
	}
	return &VideoReconcileService{gateway: gateway, interval: interval}
}

func (w *VideoReconcileService) ReconcileOnce(ctx context.Context) error {
	r, err := w.gateway.VideoAttempts().Repository()
	if err != nil {
		return err
	}
	before := time.Now().Add(-w.interval)
	var attempts []*VideoAttempt
	if known, ok := r.(VideoKnownTaskReconcileRepository); ok {
		attempts, err = known.ListKnownTaskVideoAttempts(ctx, before, 100)
	} else {
		attempts, err = r.ListReconcilableVideoAttempts(ctx, before, 100)
	}
	if err != nil {
		return err
	}
	var first error
	for _, a := range attempts {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if a.UpstreamTaskID == "" {
			continue // Unknown acceptance is held for explicit repair, never POST.
		}
		if _, err := w.gateway.PollQuotedVideo(ctx, VideoAttemptOwner{a.UserID, a.APIKeyID}, a.ID); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (w *VideoReconcileService) runLoop(ctx context.Context, run func(context.Context) error) {
	delay := w.interval
	for ctx.Err() == nil {
		tickCtx, tickCancel := context.WithTimeout(ctx, 2*time.Minute)
		err := run(tickCtx)
		tickCancel()
		if err != nil && delay < 2*time.Minute {
			delay *= 2
			if delay > 2*time.Minute {
				delay = 2 * time.Minute
			}
		} else if err == nil {
			delay = w.interval
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (w *VideoReconcileService) Start() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	w.cancel, w.done = cancel, make(chan struct{})
	done := w.done
	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		w.runLoop(ctx, w.ReconcileOnce)
	}()
	// Archival has its own deadline and retry cadence; slow supplier status
	// polls must never consume the budget needed to preserve completed files.
	if w.gateway.videoResults != nil {
		workers.Add(1)
		go func() {
			defer workers.Done()
			w.runLoop(ctx, func(ctx context.Context) error {
				return w.gateway.videoResults.ArchivePending(ctx, w.gateway.refreshStoredVideoSource)
			})
		}()
	}
	go func() {
		workers.Wait()
		close(done)
	}()
}

func (w *VideoReconcileService) Stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cancel != nil {
		w.cancel()
		<-w.done
		w.cancel, w.done = nil, nil
	}
}

// Called once by the production handler provider, not by constructors/tests.
func (s *OpenAIGatewayService) StartVideoReconciler(auth APIKeyAuthCacheInvalidator) {
	s.videoRuntimeOnce.Do(func() {
		s.videoAuthInvalidator = auth
		s.videoReconciler = NewVideoReconcileService(s, 15*time.Second)
		s.videoReconciler.Start()
	})
}

func (s *OpenAIGatewayService) StopVideoReconciler() {
	if s.videoReconciler != nil {
		s.videoReconciler.Stop()
	}
}
