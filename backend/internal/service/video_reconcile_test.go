package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// This double replaces the external supplier only. The actual HTTP request,
// serializer, admission, dispatch and settlement coordinator stay under test.
type quotedVideoHTTP struct {
	HTTPUpstream
	call func(*http.Request, int64) (*http.Response, error)
}

func (u *quotedVideoHTTP) Do(r *http.Request, _ string, id int64, _ int) (*http.Response, error) {
	return u.call(r, id)
}

func videoHTTPResponse(code int, body string) *http.Response {
	return &http.Response{StatusCode: code, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
}

func TestQuotedVideoTransportOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name, body, task string
		code             int
		transport        error
		definitive       bool
	}{
		{"rejected", `{"error":"invalid request"}`, "", 422, nil, true},
		{"auth rejected", `{}`, "", 401, nil, true},
		{"malformed rejection", `{`, "", 422, nil, false},
		{"non-object rejection", `[]`, "", 422, nil, false},
		{"invalid task rejection", `{"data":{"id":17,"status":"processing"}}`, "", 422, nil, false},
		{"contradictory acceptance rejection", `{"data":{"status":"processing"}}`, "", 422, nil, false},
		{"timeout response", `{}`, "", 408, nil, false},
		{"server error", `{}`, "", 500, nil, false},
		{"server error known task", `{"data":{"id":"job","status":"processing"}}`, "atlascloud:job", 500, nil, false},
		{"rejection with task evidence", `{"data":{"id":"job","status":"processing"}}`, "atlascloud:job", 422, nil, false},
		{"timeout", "", "", 0, context.DeadlineExceeded, false},
		{"malformed", `{`, "", 200, nil, false},
		{"invalid acceptance", `{"data":{"status":"processing"}}`, "", 200, nil, false},
		{"known task malformed status", `{"data":{"id":"job","status":"unexpected"}}`, "atlascloud:job", 200, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := videoFixture(t)
			a := &s.accountRepo.(*videoQuoteAccounts).accounts[0]
			count := 0
			s.httpUpstream = &quotedVideoHTTP{call: func(r *http.Request, id int64) (*http.Response, error) {
				count++
				require.Equal(t, a.ID, id)
				require.Empty(t, r.Header.Get("X-Video-Quote"))
				require.Empty(t, r.Header.Get("Idempotency-Key"))
				require.Equal(t, "Bearer synthetic", r.Header.Get("Authorization"))
				if tc.transport != nil {
					return nil, tc.transport
				}
				return videoHTTPResponse(tc.code, tc.body), nil
			}}
			result, err := s.requestQuotedAtlasVideo(context.Background(), a, "", []byte(`{"model":"google/veo3.1/text-to-video","prompt":"private"}`))
			var outcome *VideoTransportError
			require.ErrorAs(t, err, &outcome)
			require.Equal(t, tc.definitive, outcome.DefinitiveRejection)
			if tc.task != "" {
				require.NotNil(t, result)
				require.Equal(t, tc.task, result.TaskID)
			}
			require.Equal(t, 1, count, "create is never retried")
			require.NotContains(t, err.Error(), "private")
			require.NotContains(t, err.Error(), "synthetic")
		})
	}
}

// The real financial transaction has PostgreSQL coverage. This synchronized
// boundary double models only durable state transitions for orchestration races.
type quotedVideoRepository struct {
	UsageBillingRepository
	VideoAttemptRepository
	mu             sync.Mutex
	attempts       map[string]*VideoAttempt
	captures       int
	releases       int
	reserves       int
	failBind       bool
	onReserve      func()
	leases         map[string]*VideoAttemptLease
	subscriptionID *int64
}

func newQuotedVideoRepository() *quotedVideoRepository {
	return &quotedVideoRepository{attempts: map[string]*VideoAttempt{}, leases: map[string]*VideoAttemptLease{}}
}
func (*quotedVideoRepository) VideoQuotedBillingReady() bool { return true }
func cloneQuotedAttempt(a *VideoAttempt) *VideoAttempt {
	if a == nil {
		return nil
	}
	b := *a
	b.ResultURLs = append([]string(nil), a.ResultURLs...)
	return &b
}
func (r *quotedVideoRepository) GetVideoAttempt(_ context.Context, owner VideoAttemptOwner, id string) (*VideoAttempt, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a := r.attempts[id]
	if a == nil || a.UserID != owner.UserID || a.APIKeyID != owner.APIKeyID {
		return nil, ErrVideoAttemptNotFound
	}
	return cloneQuotedAttempt(a), nil
}
func (r *quotedVideoRepository) AcceptVideoAttempt(_ context.Context, cmd *VideoAttemptAcceptCommand) (*VideoAttemptAcceptResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if old := r.attempts[cmd.Attempt.ID]; old != nil {
		if old.RequestFingerprint != cmd.Attempt.RequestFingerprint {
			return nil, ErrVideoAttemptConflict
		}
		return &VideoAttemptAcceptResult{Attempt: cloneQuotedAttempt(old)}, nil
	}
	a := cloneQuotedAttempt(cmd.Attempt)
	a.SubscriptionID = r.subscriptionID
	a.State, a.FinancialState = VideoAttemptAccepted, VideoFinancialReserved
	r.attempts[a.ID] = a
	r.reserves++
	if r.onReserve != nil {
		r.onReserve()
	}
	return &VideoAttemptAcceptResult{Attempt: cloneQuotedAttempt(a), Created: true}, nil
}
func (r *quotedVideoRepository) UpdateVideoAttempt(_ context.Context, owner VideoAttemptOwner, id string, update VideoAttemptUpdate) (*VideoAttempt, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a := r.attempts[id]
	if a == nil || a.UserID != owner.UserID || a.APIKeyID != owner.APIKeyID {
		return nil, ErrVideoAttemptNotFound
	}
	if update.State == VideoAttemptSubmitting && a.State != VideoAttemptAccepted {
		return nil, ErrVideoAttemptTransition
	}
	if update.State == VideoAttemptProcessing && r.failBind {
		return nil, errors.New("synthetic persistence failure")
	}
	a.State = update.State
	if update.UpstreamTaskID != "" {
		a.UpstreamTaskID = update.UpstreamTaskID
	}
	return cloneQuotedAttempt(a), nil
}
func (r *quotedVideoRepository) CaptureVideoAttempt(_ context.Context, owner VideoAttemptOwner, id string, urls []string) (*VideoAttemptSettlementResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a := r.attempts[id]
	if a == nil || a.UserID != owner.UserID || a.APIKeyID != owner.APIKeyID {
		return nil, ErrVideoAttemptNotFound
	}
	if a.FinancialState == VideoFinancialCaptured {
		return &VideoAttemptSettlementResult{Attempt: cloneQuotedAttempt(a)}, nil
	}
	if a.State != VideoAttemptProcessing || a.UpstreamTaskID == "" || a.FinancialState != VideoFinancialReserved {
		return nil, ErrVideoAttemptTransition
	}
	r.captures++
	a.State, a.FinancialState, a.ResultURLs = VideoAttemptCompleted, VideoFinancialCaptured, urls
	a.Receipt = &VideoChargeReceipt{RequestID: a.RequestID(), Amount: a.TotalPrice, Currency: "USD", UsageLogID: 19}
	return &VideoAttemptSettlementResult{Attempt: cloneQuotedAttempt(a), Applied: true}, nil
}
func (r *quotedVideoRepository) ReleaseVideoAttempt(_ context.Context, owner VideoAttemptOwner, id string) (*VideoAttemptSettlementResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a := r.attempts[id]
	if a == nil || a.UserID != owner.UserID || a.APIKeyID != owner.APIKeyID {
		return nil, ErrVideoAttemptNotFound
	}
	if a.FinancialState == VideoFinancialReleased {
		return &VideoAttemptSettlementResult{Attempt: cloneQuotedAttempt(a)}, nil
	}
	if a.FinancialState != VideoFinancialReserved || a.State == VideoAttemptUncertain {
		return nil, ErrVideoAttemptTransition
	}
	r.releases++
	a.State, a.FinancialState = VideoAttemptFailed, VideoFinancialReleased
	return &VideoAttemptSettlementResult{Attempt: cloneQuotedAttempt(a), Applied: true}, nil
}

func quotedDispatchFixture(t *testing.T) (*OpenAIGatewayService, *APIKey, *quotedVideoRepository, *VideoQuoteSnapshot, []byte, *Account) {
	t.Helper()
	s, k := videoFixture(t)
	r := newQuotedVideoRepository()
	s.usageBillingRepo = r
	body := []byte(`{"model":"google/veo3.1/text-to-video","prompt":"private","aspect_ratio":"16:9","duration":8,"generate_audio":false,"resolution":"720p"}`)
	q, err := s.QuoteVideo(context.Background(), k, videoTestRequest(), time.Now())
	require.NoError(t, err)
	snapshot, err := s.AcceptVideoQuote(context.Background(), k, body, q.Token, time.Now())
	require.NoError(t, err)
	_, native, err := SerializeAtlasCloudVideoRequest(body)
	require.NoError(t, err)
	a := &s.accountRepo.(*videoQuoteAccounts).accounts[0]
	return s, k, r, snapshot, native, a
}

func TestQuotedVideoDispatchOnePostLockedBeforeEffectAndDisconnect(t *testing.T) {
	s, _, r, q, native, a := quotedDispatchFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	r.onReserve = cancel
	var posts int
	s.httpUpstream = &quotedVideoHTTP{call: func(req *http.Request, id int64) (*http.Response, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		posts++
		require.Equal(t, 1, r.reserves)
		require.Equal(t, VideoAttemptSubmitting, r.attempts["attempt"].State)
		require.Equal(t, "13.33171200", r.attempts["attempt"].TotalPrice)
		require.NoError(t, req.Context().Err(), "accepted create must survive client cancellation")
		return videoHTTPResponse(200, `{"data":{"id":"job","status":"processing"}}`), nil
	}}
	const n = 8
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.DispatchQuotedVideo(ctx, q, "attempt", native, a)
			require.NoError(t, err)
		}()
	}
	wg.Wait()
	require.Equal(t, 1, posts)
	got, err := r.GetVideoAttempt(context.Background(), VideoAttemptOwner{UserID: q.UserID(), APIKeyID: q.APIKeyID()}, "attempt")
	require.NoError(t, err)
	require.Equal(t, "atlascloud:job", got.UpstreamTaskID)
	require.Equal(t, VideoAttemptProcessing, got.State)
	require.Zero(t, r.captures)
}

func TestQuotedVideoDispatchRejectionUnknownAndPersistence(t *testing.T) {
	for _, tc := range []struct {
		name, body, state, financial string
		code                         int
		failBind                     bool
	}{
		{"definitive", `{}`, VideoAttemptFailed, VideoFinancialReleased, 422, false},
		{"server unknown", `{}`, VideoAttemptUncertain, VideoFinancialReserved, 503, false},
		{"malformed unknown", `{`, VideoAttemptUncertain, VideoFinancialReserved, 200, false},
		{"malformed rejection unknown", `{`, VideoAttemptUncertain, VideoFinancialReserved, 422, false},
		{"missing id", `{"data":{"status":"processing"}}`, VideoAttemptUncertain, VideoFinancialReserved, 200, false},
		{"bind failure", `{"data":{"id":"job","status":"processing"}}`, VideoAttemptSubmitting, VideoFinancialReserved, 200, true},
		{"known invalid status", `{"data":{"id":"job","status":"bogus"}}`, VideoAttemptProcessing, VideoFinancialReserved, 200, false},
		{"server failure known task", `{"data":{"id":"job","status":"processing"}}`, VideoAttemptProcessing, VideoFinancialReserved, 500, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, r, q, native, a := quotedDispatchFixture(t)
			r.failBind = tc.failBind
			count := 0
			s.httpUpstream = &quotedVideoHTTP{call: func(*http.Request, int64) (*http.Response, error) {
				count++
				return videoHTTPResponse(tc.code, tc.body), nil
			}}
			response, err := s.DispatchQuotedVideo(context.Background(), q, "attempt", native, a)
			require.Error(t, err)
			require.NotNil(t, response)
			require.Equal(t, tc.state, response.State, "respond with the last proven durable state, not a stale pre-POST state")
			got, err := s.RecoverQuotedVideo(context.Background(), VideoAttemptOwner{q.UserID(), q.APIKeyID()}, "attempt", native)
			require.NoError(t, err)
			require.Equal(t, tc.state, got.State)
			require.Equal(t, tc.financial, got.FinancialState)
			_, err = s.DispatchQuotedVideo(context.Background(), q, "attempt", native, a)
			require.NoError(t, err, "existing attempt cannot POST again")
			require.Equal(t, 1, count)
		})
	}
}

func TestQuotedVideoRecoveryDoesNotRepriceOrDiscloseForeignAttempt(t *testing.T) {
	s, k, r, q, native, _ := quotedDispatchFixture(t)
	_, err := s.VideoAttempts().Accept(context.Background(), q, "attempt", native, 30)
	require.NoError(t, err)
	k.Group.ModelPricing = nil
	s.channelService, s.billingCacheService = nil, nil
	owner := VideoAttemptOwner{q.UserID(), q.APIKeyID()}
	got, err := s.RecoverQuotedVideo(context.Background(), owner, "attempt", native)
	require.NoError(t, err)
	require.Equal(t, VideoAttemptAccepted, got.State, "accepted but unsent work is not replayed")
	require.Equal(t, "13.33171200", got.TotalPrice)
	_, err = s.RecoverQuotedVideo(context.Background(), owner, "attempt", []byte(`{"different":"body"}`))
	require.ErrorIs(t, err, ErrVideoAttemptConflict)
	_, err = s.RecoverQuotedVideo(context.Background(), VideoAttemptOwner{owner.UserID, 999}, "attempt", native)
	require.ErrorIs(t, err, ErrVideoAttemptNotFound)
	require.Equal(t, 1, r.reserves)
}

func TestQuotedVideoDispatchPrerequisitesBeforeHoldButAfterRecovery(t *testing.T) {
	for _, scenario := range []string{"missing credential", "missing transport", "accepted configuration removed"} {
		t.Run(scenario, func(t *testing.T) {
			s, _, r, quote, native, account := quotedDispatchFixture(t)
			var posts int
			s.httpUpstream = &quotedVideoHTTP{call: func(*http.Request, int64) (*http.Response, error) {
				posts++
				return videoHTTPResponse(200, `{"data":{"id":"job","status":"processing"}}`), nil
			}}
			if scenario == "accepted configuration removed" {
				_, err := s.DispatchQuotedVideo(context.Background(), quote, "attempt", native, account)
				require.NoError(t, err)
				account, s.httpUpstream = nil, nil
			} else if scenario == "missing credential" {
				account.Credentials["api_key"] = ""
			} else {
				s.httpUpstream = nil
			}
			got, err := s.DispatchQuotedVideo(context.Background(), quote, "attempt", native, account)
			if scenario == "accepted configuration removed" {
				require.NoError(t, err)
				require.Equal(t, VideoAttemptProcessing, got.State)
				require.Equal(t, "13.33171200", got.TotalPrice)
				require.Equal(t, 1, posts)
				require.Equal(t, 1, r.reserves)
				_, err = s.DispatchQuotedVideo(context.Background(), quote, "attempt", []byte(`{"different":"content"}`), account)
				require.ErrorIs(t, err, ErrVideoAttemptConflict)
			} else {
				require.ErrorIs(t, err, ErrVideoAttemptUnavailable)
				require.Zero(t, r.reserves, "known local prerequisites reject before holding customer funds")
				require.Zero(t, posts)
			}
		})
	}
}

type quotedBoundAccounts struct {
	*videoQuoteAccounts
}

func (r *quotedBoundAccounts) GetByID(_ context.Context, id int64) (*Account, error) {
	for _, a := range r.accounts {
		if a.ID == id {
			copy := a
			return &copy, nil
		}
	}
	return nil, errors.New("bound account unavailable")
}

func (r *quotedVideoRepository) ListReconcilableVideoAttempts(_ context.Context, _ time.Time, limit int) ([]*VideoAttempt, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var attempts []*VideoAttempt
	for _, a := range r.attempts {
		if a.FinancialState == VideoFinancialReserved && len(attempts) < limit {
			attempts = append(attempts, cloneQuotedAttempt(a))
		}
	}
	return attempts, nil
}
func (r *quotedVideoRepository) AcquireVideoAttemptLease(_ context.Context, id, owner string, ttl time.Duration) (*VideoAttemptLease, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if l := r.leases[id]; l != nil && l.ExpiresAt.After(time.Now()) {
		return nil, ErrVideoAttemptLease
	}
	l := &VideoAttemptLease{AttemptID: id, Owner: owner, Token: owner, ExpiresAt: time.Now().Add(ttl)}
	r.leases[id] = l
	copy := *l
	return &copy, nil
}
func (r *quotedVideoRepository) ReleaseVideoAttemptLease(_ context.Context, lease *VideoAttemptLease) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if current := r.leases[lease.AttemptID]; current == nil || current.Token != lease.Token {
		return ErrVideoAttemptLease
	}
	delete(r.leases, lease.AttemptID)
	return nil
}

func TestQuotedVideoPollSettlesKnownTaskOnceAndUnknownStaysHeld(t *testing.T) {
	for _, terminal := range []string{"completed", "failed"} {
		t.Run(terminal, func(t *testing.T) {
			s, _, r, q, native, a := quotedDispatchFixture(t)
			owner := VideoAttemptOwner{q.UserID(), q.APIKeyID()}
			s.accountRepo = &quotedBoundAccounts{s.accountRepo.(*videoQuoteAccounts)}
			var posts, gets atomic.Int32
			entered, proceed := make(chan struct{}), make(chan struct{})
			s.httpUpstream = &quotedVideoHTTP{call: func(req *http.Request, accountID int64) (*http.Response, error) {
				require.Equal(t, a.ID, accountID, "never reroute a persisted job")
				if req.Method == http.MethodPost {
					posts.Add(1)
					return videoHTTPResponse(200, `{"data":{"id":"job","status":"processing"}}`), nil
				}
				require.Equal(t, "https://api.atlascloud.ai/api/v1/model/prediction/job", req.URL.String())
				gets.Add(1)
				close(entered)
				<-proceed
				return videoHTTPResponse(200, `{"data":{"id":"job","status":"`+terminal+`","outputs":["https://cdn.example/result.mp4"]}}`), nil
			}}
			_, err := s.DispatchQuotedVideo(context.Background(), q, "attempt", native, a)
			require.NoError(t, err)
			// Native status does not need current enrollment, new balance or a
			// still-schedulable account once a known supplier ID is durable.
			s.channelService, s.billingCacheService = nil, nil
			a.Schedulable = false
			worker := NewVideoReconcileService(s, time.Second)
			backgroundDone := make(chan error, 1)
			go func() { backgroundDone <- worker.ReconcileOnce(context.Background()) }()
			<-entered
			got, err := s.PollQuotedVideo(context.Background(), owner, "attempt")
			require.NoError(t, err)
			require.Equal(t, VideoAttemptProcessing, got.State)
			require.Equal(t, int32(1), gets.Load(), "lease prevents concurrent foreground/background poll")
			close(proceed)
			require.NoError(t, <-backgroundDone)
			for range 3 {
				got, err = s.PollQuotedVideo(context.Background(), owner, "attempt")
				require.NoError(t, err)
				require.Equal(t, terminal, got.State)
			}
			require.Equal(t, int32(1), posts.Load())
			require.Equal(t, int32(1), gets.Load())
			if terminal == "completed" {
				require.Equal(t, 1, r.captures)
				require.Equal(t, "13.33171200", got.Receipt.Amount)
			} else {
				require.Equal(t, 1, r.releases)
				require.Nil(t, got.Receipt)
				require.Empty(t, got.ResultURLs)
			}
			unknown := cloneQuotedAttempt(got)
			unknown.ID, unknown.State, unknown.FinancialState, unknown.UpstreamTaskID = "unknown", VideoAttemptUncertain, VideoFinancialReserved, ""
			r.attempts[unknown.ID] = unknown
			require.NoError(t, NewVideoReconcileService(s, time.Second).ReconcileOnce(context.Background()))
			require.Equal(t, int32(1), gets.Load())
			require.Equal(t, VideoFinancialReserved, r.attempts["unknown"].FinancialState)
		})
	}
}

func TestQuotedVideoWorkerStopAndRestartKnownOnly(t *testing.T) {
	s, _, r, q, native, _ := quotedDispatchFixture(t)
	_, err := s.VideoAttempts().Accept(context.Background(), q, "unsent", native, 30)
	require.NoError(t, err)
	var requests atomic.Int32
	s.httpUpstream = &quotedVideoHTTP{call: func(*http.Request, int64) (*http.Response, error) {
		requests.Add(1)
		return nil, errors.New("unexpected supplier call")
	}}
	worker := NewVideoReconcileService(s, time.Millisecond)
	worker.Start()
	worker.Stop()
	worker.Start()
	worker.Stop()
	require.Zero(t, requests.Load(), "restart never replays accepted unsent work")
	require.Equal(t, VideoFinancialReserved, r.attempts["unsent"].FinancialState)
}

func TestQuotedVideoWorkerStopCancelsInflightKnownPoll(t *testing.T) {
	s, _, r, q, native, account := quotedDispatchFixture(t)
	s.httpUpstream = &quotedVideoHTTP{call: func(*http.Request, int64) (*http.Response, error) {
		return videoHTTPResponse(200, `{"data":{"id":"job","status":"processing"}}`), nil
	}}
	_, err := s.DispatchQuotedVideo(context.Background(), q, "attempt", native, account)
	require.NoError(t, err)
	s.accountRepo = &quotedBoundAccounts{s.accountRepo.(*videoQuoteAccounts)}
	entered := make(chan struct{})
	s.httpUpstream = &quotedVideoHTTP{call: func(req *http.Request, _ int64) (*http.Response, error) {
		require.Equal(t, http.MethodGet, req.Method)
		close(entered)
		<-req.Context().Done()
		return nil, req.Context().Err()
	}}
	worker := NewVideoReconcileService(s, time.Millisecond)
	worker.Start()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not start the known-ID poll")
	}
	stopped := make(chan struct{})
	go func() {
		worker.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not cancel its in-flight poll")
	}
	require.Empty(t, r.leases, "shutdown releases its exact lease using detached cleanup")
	require.Equal(t, VideoFinancialReserved, r.attempts["attempt"].FinancialState)
	require.Zero(t, r.captures+r.releases)
}

type quotedInvalidationCache struct {
	BillingCache
	balance, subscription, key int
}

func (c *quotedInvalidationCache) InvalidateUserBalance(ctx context.Context, _ int64) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	c.balance++
	return nil
}
func (c *quotedInvalidationCache) InvalidateSubscriptionCache(ctx context.Context, _, _ int64) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	c.subscription++
	return nil
}
func (c *quotedInvalidationCache) InvalidateAPIKeyRateLimit(ctx context.Context, _ int64) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	c.key++
	return nil
}

type quotedAuthInvalidator struct {
	APIKeyAuthCacheInvalidator
	calls int
}

func (a *quotedAuthInvalidator) InvalidateAuthCacheByUserID(ctx context.Context, _ int64) {
	if ctx.Err() == nil {
		a.calls++
	}
}

func TestQuotedVideoCommittedPhasesInvalidateOnlyDetachedCaches(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		subscription, capture bool
	}{
		{"balance capture", false, true},
		{"balance release", false, false},
		{"subscription capture", true, true},
		{"subscription release", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, r, q, native, a := quotedDispatchFixture(t)
			cache, auth := &quotedInvalidationCache{}, &quotedAuthInvalidator{}
			s.billingCacheService, s.videoAuthInvalidator = &BillingCacheService{cache: cache}, auth
			ctx, cancel := context.WithCancel(context.Background())
			r.onReserve = cancel
			if tc.subscription {
				sub := int64(90)
				r.subscriptionID = &sub
			}
			s.httpUpstream = &quotedVideoHTTP{call: func(*http.Request, int64) (*http.Response, error) {
				return videoHTTPResponse(200, `{"data":{"id":"job","status":"processing"}}`), nil
			}}
			got, err := s.DispatchQuotedVideo(ctx, q, "attempt", native, a)
			require.NoError(t, err)
			if tc.subscription {
				require.Equal(t, 1, cache.subscription, "subscription reserve invalidation survives disconnection")
			} else {
				require.Equal(t, 1, cache.balance, "balance reserve invalidation survives disconnection")
			}
			owner := VideoAttemptOwner{got.UserID, got.APIKeyID}
			for range 2 {
				if tc.capture {
					_, err = s.VideoAttempts().Capture(ctx, owner, got.ID, []string{"https://cdn.example/result.mp4"})
				} else {
					_, err = s.VideoAttempts().Release(ctx, owner, got.ID)
				}
				require.NoError(t, err)
			}
			require.Equal(t, 2, cache.key)
			require.Equal(t, 2, auth.calls)
			if tc.subscription {
				require.Equal(t, 2, cache.subscription)
				require.Zero(t, cache.balance)
			} else {
				require.Equal(t, 2, cache.balance)
			}
			// Embedded financial cache methods are intentionally nil: any
			// increment/deduct/secondary-ledger call would panic this test.
			require.Equal(t, 1, r.captures+r.releases)
		})
	}
}

func TestQuotedVideoSyntheticHTTPSupplierRecoveryAfterRestart(t *testing.T) {
	s, _, r, q, native, account := quotedDispatchFixture(t)
	var posts, polls atomic.Int32
	supplier := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		require.Equal(t, "Bearer synthetic", req.Header.Get("Authorization"))
		require.Empty(t, req.Header.Get("X-Video-Quote"))
		require.Empty(t, req.Header.Get("Idempotency-Key"))
		if req.Method == http.MethodPost {
			posts.Add(1)
			require.Equal(t, "/api/v1/model/generateVideo", req.URL.Path)
			body, err := io.ReadAll(req.Body)
			require.NoError(t, err)
			require.Equal(t, string(native), string(body), "supplier receives the fingerprinted bytes, unchanged")
			_, _ = io.WriteString(w, `{"data":{"id":"restart-job","status":"processing"}}`)
		} else {
			polls.Add(1)
			require.Equal(t, "/api/v1/model/prediction/restart-job", req.URL.Path)
			_, _ = io.WriteString(w, `{"data":{"id":"restart-job","status":"completed","outputs":["https://cdn.example/restarted.mp4"]}}`)
		}
	}))
	defer supplier.Close()
	local, err := url.Parse(supplier.URL)
	require.NoError(t, err)
	transport := &quotedVideoHTTP{call: func(req *http.Request, id int64) (*http.Response, error) {
		require.Equal(t, account.ID, id)
		require.Equal(t, "api.atlascloud.ai", req.URL.Host)
		req.URL.Scheme, req.URL.Host = local.Scheme, local.Host
		return supplier.Client().Do(req)
	}}
	s.httpUpstream = transport
	_, err = s.DispatchQuotedVideo(context.Background(), q, "attempt", native, account)
	require.NoError(t, err)
	restarted := &OpenAIGatewayService{usageBillingRepo: r, httpUpstream: transport, accountRepo: &quotedBoundAccounts{s.accountRepo.(*videoQuoteAccounts)}}
	require.NoError(t, NewVideoReconcileService(restarted, time.Second).ReconcileOnce(context.Background()))
	got, err := restarted.GetQuotedVideo(context.Background(), VideoAttemptOwner{q.UserID(), q.APIKeyID()}, "attempt")
	require.NoError(t, err)
	require.Equal(t, VideoFinancialCaptured, got.FinancialState)
	require.Equal(t, "13.33171200", got.Receipt.Amount)
	require.Equal(t, []string{"https://cdn.example/restarted.mp4"}, got.ResultURLs)
	require.NoError(t, NewVideoReconcileService(restarted, time.Second).ReconcileOnce(context.Background()))
	require.Equal(t, int32(1), posts.Load())
	require.Equal(t, int32(1), polls.Load())
}

func TestQuotedVideoSyntheticHTTPRedirectDoesNotReplayCreate(t *testing.T) {
	s, _, r, q, native, account := quotedDispatchFixture(t)
	var posts atomic.Int32
	supplier := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		require.Equal(t, http.MethodPost, req.Method)
		posts.Add(1)
		if req.URL.Path == "/api/v1/model/generateVideo" {
			http.Redirect(w, req, "/duplicate-generation", http.StatusTemporaryRedirect)
			return
		}
		_, _ = io.WriteString(w, `{"data":{"id":"unwanted-second-post","status":"processing"}}`)
	}))
	defer supplier.Close()
	local, err := url.Parse(supplier.URL)
	require.NoError(t, err)
	s.httpUpstream = &quotedVideoHTTP{call: func(req *http.Request, _ int64) (*http.Response, error) {
		req.URL.Scheme, req.URL.Host = local.Scheme, local.Host
		client := *supplier.Client()
		// Mirror the existing production HTTPUpstream context contract.
		if HTTPUpstreamRedirectsDisabled(req.Context()) {
			client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		}
		return client.Do(req)
	}}
	got, err := s.DispatchQuotedVideo(context.Background(), q, "attempt", native, account)
	require.Equal(t, int32(1), posts.Load(), "a 307/308 must never replay the accepted native POST")
	require.Error(t, err)
	require.Equal(t, VideoAttemptUncertain, got.State)
	require.Equal(t, VideoFinancialReserved, r.attempts["attempt"].FinancialState)
	require.Zero(t, r.captures+r.releases)
}
