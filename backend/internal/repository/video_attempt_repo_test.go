package repository

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// An explicitly requested database is never silently skipped, including failed
// connectivity and migration setup. Use only a disposable local test database.
func videoTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("VIDEO_BILLING_TEST_DSN")
	if dsn == "" {
		t.Skip("set VIDEO_BILLING_TEST_DSN for real PostgreSQL financial tests")
	}
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	require.NoError(t, db.Ping())
	require.NoError(t, ApplyMigrations(context.Background(), db))
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return db
}

func TestVideoAttemptPostgresRecoveryIndexes(t *testing.T) {
	db := videoTestDB(t)
	for _, index := range []struct {
		name, columns string
		predicates    []string
	}{
		{"video_attempts_known_reconcile", "(updated_at, id)", []string{"financial_state", "reserved", "upstream_task_id IS NOT NULL", "processing", "uncertain"}},
		{"video_attempts_owned_task", "(user_id, api_key_id, upstream_task_id)", []string{"upstream_task_id IS NOT NULL"}},
	} {
		t.Run(index.name, func(t *testing.T) {
			var definition string
			require.NoError(t, db.QueryRow(`SELECT indexdef FROM pg_indexes WHERE schemaname='public' AND tablename='video_attempts' AND indexname=$1`, index.name).Scan(&definition))
			require.NotContains(t, definition, "UNIQUE", "same task ID on different accounts must remain representable")
			require.Contains(t, definition, index.columns)
			for _, predicate := range index.predicates {
				require.Contains(t, definition, predicate)
			}
		})
	}
}

type videoFixture struct {
	db         *sql.DB
	r          *usageBillingRepository
	u, k, g, a int64
}

func newVideoFixture(t *testing.T, balance string) *videoFixture {
	t.Helper()
	f := &videoFixture{db: videoTestDB(t)}
	f.r = &usageBillingRepository{db: f.db}
	err := f.db.QueryRow(`INSERT INTO users(email,password_hash,balance,concurrency) VALUES($1,'hash',$2,100) RETURNING id`, uuid.NewString()+"@test.example", balance).Scan(&f.u)
	require.NoError(t, err)
	require.NoError(t, f.db.QueryRow(`INSERT INTO groups(name,platform) VALUES($1,'openai') RETURNING id`, uuid.NewString()).Scan(&f.g))
	require.NoError(t, f.db.QueryRow(`INSERT INTO api_keys(user_id,group_id,key,name) VALUES($1,$2,$3,'video') RETURNING id`, f.u, f.g, uuid.NewString()).Scan(&f.k))
	require.NoError(t, f.db.QueryRow(`INSERT INTO accounts(name,platform,type,concurrency) VALUES($1,'openai','apikey',100) RETURNING id`, uuid.NewString()).Scan(&f.a))
	_, err = f.db.Exec(`INSERT INTO account_groups(account_id,group_id) VALUES($1,$2)`, f.a, f.g)
	require.NoError(t, err)
	return f
}

func (f *videoFixture) command(amount string) *service.VideoAttemptAcceptCommand {
	return &service.VideoAttemptAcceptCommand{
		Attempt: &service.VideoAttempt{
			ID: uuid.NewString(), QuoteID: uuid.NewString(), UserID: f.u, APIKeyID: f.k, GroupID: f.g, AccountID: f.a,
			RequestFingerprint: service.HashUsageRequestPayload([]byte(uuid.NewString())),
			Selection:          service.VideoQuoteRequest{Model: "google/veo3.1/text-to-video", Mode: "t2v", Controls: map[string]any{"duration": 8, "resolution": "720p", "aspect_ratio": "16:9", "generate_audio": false}},
			TotalPrice:         amount, BasePrice: amount, Multiplier: "1.0000", TariffRevision: "tariff", ContractRevision: "contract",
		},
		QuoteExpiresAt: time.Now().Add(time.Minute),
	}
}

func (f *videoFixture) owner() service.VideoAttemptOwner {
	return service.VideoAttemptOwner{UserID: f.u, APIKeyID: f.k}
}

func TestVideoAttemptPostgresOwnedTaskLookupAndCollision(t *testing.T) {
	f := newVideoFixture(t, "10")
	ctx := context.Background()
	first := f.command("1.00000000")
	_, err := f.r.AcceptVideoAttempt(ctx, first)
	require.NoError(t, err)
	for _, update := range []service.VideoAttemptUpdate{
		{State: service.VideoAttemptSubmitting},
		{State: service.VideoAttemptProcessing, UpstreamTaskID: "atlascloud:collision"},
	} {
		_, err = f.r.UpdateVideoAttempt(ctx, f.owner(), first.Attempt.ID, update)
		require.NoError(t, err)
	}
	found, err := f.r.GetVideoAttemptByTask(ctx, f.owner(), "atlascloud:collision")
	require.NoError(t, err)
	require.Equal(t, first.Attempt.ID, found.ID)
	_, err = f.r.GetVideoAttemptByTask(ctx, service.VideoAttemptOwner{UserID: f.u, APIKeyID: f.k + 10000}, "atlascloud:collision")
	require.ErrorIs(t, err, service.ErrVideoAttemptNotFound)
	var account int64
	require.NoError(t, f.db.QueryRow(`INSERT INTO accounts(name,platform,type,concurrency) VALUES($1,'openai','apikey',100) RETURNING id`, uuid.NewString()).Scan(&account))
	videoExec(t, f.db, `INSERT INTO account_groups(account_id,group_id) VALUES($1,$2)`, account, f.g)
	second := f.command("1.00000000")
	second.Attempt.AccountID = account
	_, err = f.r.AcceptVideoAttempt(ctx, second)
	require.NoError(t, err)
	for _, update := range []service.VideoAttemptUpdate{
		{State: service.VideoAttemptSubmitting},
		{State: service.VideoAttemptProcessing, UpstreamTaskID: "atlascloud:collision"},
	} {
		_, err = f.r.UpdateVideoAttempt(ctx, f.owner(), second.Attempt.ID, update)
		require.NoError(t, err)
	}
	_, err = f.r.GetVideoAttemptByTask(ctx, f.owner(), "atlascloud:collision")
	require.ErrorIs(t, err, service.ErrVideoAttemptConflict, "never choose an arbitrary account's task")
	found, err = f.r.GetVideoAttempt(ctx, f.owner(), first.Attempt.ID)
	require.NoError(t, err)
	require.Equal(t, f.a, found.AccountID, "attempt recovery remains unambiguous")
}

func TestVideoAttemptPostgresKnownTaskListCannotStarveBehindUnknown(t *testing.T) {
	f := newVideoFixture(t, "10")
	ctx := context.Background()
	unknown, known := f.command("1.00000000"), f.command("1.00000000")
	for _, cmd := range []*service.VideoAttemptAcceptCommand{unknown, known} {
		_, err := f.r.AcceptVideoAttempt(ctx, cmd)
		require.NoError(t, err)
		_, err = f.r.UpdateVideoAttempt(ctx, f.owner(), cmd.Attempt.ID, service.VideoAttemptUpdate{State: service.VideoAttemptSubmitting})
		require.NoError(t, err)
	}
	_, err := f.r.UpdateVideoAttempt(ctx, f.owner(), unknown.Attempt.ID, service.VideoAttemptUpdate{State: service.VideoAttemptUncertain})
	require.NoError(t, err)
	_, err = f.r.UpdateVideoAttempt(ctx, f.owner(), known.Attempt.ID, service.VideoAttemptUpdate{State: service.VideoAttemptProcessing, UpstreamTaskID: "atlascloud:known"})
	require.NoError(t, err)
	// This query is deliberately global. Keep its sentinel rows outside prior
	// fixture dates and restore them so repeated local-DB runs stay isolated.
	t.Cleanup(func() {
		videoExec(t, f.db, `UPDATE video_attempts SET updated_at=NOW() WHERE id IN ($1,$2)`, unknown.Attempt.ID, known.Attempt.ID)
	})
	videoExec(t, f.db, `UPDATE video_attempts SET updated_at='0001-01-01' WHERE id=$1`, unknown.Attempt.ID)
	videoExec(t, f.db, `UPDATE video_attempts SET updated_at='0001-01-02' WHERE id=$1`, known.Attempt.ID)
	found, err := f.r.ListKnownTaskVideoAttempts(ctx, time.Date(2, 1, 1, 0, 0, 0, 0, time.UTC), 1)
	require.NoError(t, err)
	require.Len(t, found, 1)
	require.Equal(t, known.Attempt.ID, found[0].ID)
}
func videoNumber(t *testing.T, db *sql.DB, query string, args ...any) string {
	t.Helper()
	var s string
	require.NoError(t, db.QueryRow(query, args...).Scan(&s))
	return s
}

func TestVideoAttemptPostgresConcurrentSingleUseAndExactCapture(t *testing.T) {
	f := newVideoFixture(t, "9999999999.12345678")
	videoExec(t, f.db, `UPDATE api_keys SET quota=9999999999.12345678,rate_limit_5h=9999999999.12345678 WHERE id=$1`, f.k)
	c := f.command("9999999999.12345678")
	const n = 8
	var wg sync.WaitGroup
	results := make(chan *service.VideoAttemptAcceptResult, n)
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := &usageBillingRepository{db: f.db}
			a, e := r.AcceptVideoAttempt(context.Background(), c)
			results <- a
			errs <- e
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for e := range errs {
		require.NoError(t, e)
	}
	created := 0
	for a := range results {
		if a.Created {
			created++
		}
	}
	require.Equal(t, 1, created)
	require.Equal(t, "0.00000000", videoNumber(t, f.db, `SELECT balance FROM users WHERE id=$1`, f.u))
	require.Equal(t, "9999999999.12345678", videoNumber(t, f.db, `SELECT frozen_balance FROM users WHERE id=$1`, f.u))
	_, err := f.r.UpdateVideoAttempt(context.Background(), f.owner(), c.Attempt.ID, service.VideoAttemptUpdate{State: service.VideoAttemptSubmitting})
	require.NoError(t, err)
	_, err = f.r.UpdateVideoAttempt(context.Background(), f.owner(), c.Attempt.ID, service.VideoAttemptUpdate{State: service.VideoAttemptProcessing, UpstreamTaskID: "provider-task"})
	require.NoError(t, err)
	var captured sync.WaitGroup
	for i := 0; i < n; i++ {
		captured.Add(1)
		go func() {
			defer captured.Done()
			_, e := (&usageBillingRepository{db: f.db}).CaptureVideoAttempt(context.Background(), f.owner(), c.Attempt.ID, []string{"https://result.example/video.mp4"})
			errs2 := e
			if errs2 != nil {
				t.Errorf("capture: %v", errs2)
			}
		}()
	}
	captured.Wait()
	require.Equal(t, "0.00000000", videoNumber(t, f.db, `SELECT balance FROM users WHERE id=$1`, f.u))
	require.Equal(t, "0.00000000", videoNumber(t, f.db, `SELECT frozen_balance FROM users WHERE id=$1`, f.u))
	require.Equal(t, "9999999999.12345678", videoNumber(t, f.db, `SELECT quota_used FROM api_keys WHERE id=$1`, f.k))
	require.Equal(t, "1", videoNumber(t, f.db, `SELECT count(*) FROM usage_logs WHERE request_id=$1`, c.Attempt.RequestID()))
	require.Equal(t, "8", videoNumber(t, f.db, `SELECT video_duration_seconds FROM usage_logs WHERE request_id=$1`, c.Attempt.RequestID()))
	require.Equal(t, "720p", videoNumber(t, f.db, `SELECT video_resolution FROM usage_logs WHERE request_id=$1`, c.Attempt.RequestID()))
	a, err := f.r.GetVideoAttempt(context.Background(), f.owner(), c.Attempt.ID)
	require.NoError(t, err)
	require.Equal(t, service.VideoFinancialCaptured, a.FinancialState)
	require.Equal(t, "9999999999.12345678", a.Receipt.Amount)
}

func TestVideoAttemptPostgresUnlimitedCountersPreserveExistingSemantics(t *testing.T) {
	f := newVideoFixture(t, "10")
	c := f.command("1.00000000")
	_, err := f.r.AcceptVideoAttempt(context.Background(), c)
	require.NoError(t, err)
	f.processing(t, c)
	_, err = f.r.CaptureVideoAttempt(context.Background(), f.owner(), c.Attempt.ID, []string{"https://result.example/video.mp4"})
	require.NoError(t, err)
	require.Equal(t, "0.00000000", videoNumber(t, f.db, `SELECT quota_used FROM api_keys WHERE id=$1`, f.k))
	require.Equal(t, "0.00000000", videoNumber(t, f.db, `SELECT usage_5h FROM api_keys WHERE id=$1`, f.k))
	require.Equal(t, "0", videoNumber(t, f.db, `SELECT COALESCE((extra->>'quota_used')::numeric,0) FROM accounts WHERE id=$1`, f.a))
}

func TestVideoAttemptPostgresHoldsLimitsReleaseAndOwnership(t *testing.T) {
	f := newVideoFixture(t, "10")
	c := f.command("6.00000000")
	_, err := f.r.AcceptVideoAttempt(context.Background(), c)
	require.NoError(t, err)
	restarted := &usageBillingRepository{db: f.db}
	_, err = restarted.AcceptVideoAttempt(context.Background(), f.command("6.00000000"))
	require.ErrorIs(t, err, service.ErrVideoAttemptInsufficientFunds)
	changed := *c
	a := *c.Attempt
	changed.Attempt = &a
	a.RequestFingerprint = service.HashUsageRequestPayload([]byte("different"))
	_, err = restarted.AcceptVideoAttempt(context.Background(), &changed)
	require.ErrorIs(t, err, service.ErrVideoAttemptConflict)
	_, err = restarted.GetVideoAttempt(context.Background(), service.VideoAttemptOwner{UserID: f.u, APIKeyID: f.k + 1}, a.ID)
	require.ErrorIs(t, err, service.ErrVideoAttemptNotFound)
	_, err = restarted.ReleaseVideoAttempt(context.Background(), f.owner(), a.ID)
	require.NoError(t, err)
	_, err = restarted.ReleaseVideoAttempt(context.Background(), f.owner(), a.ID)
	require.NoError(t, err)
	require.Equal(t, "10.00000000", videoNumber(t, f.db, `SELECT balance FROM users WHERE id=$1`, f.u))
	_, err = restarted.CaptureVideoAttempt(context.Background(), f.owner(), a.ID, []string{"https://result.example/video.mp4"})
	require.ErrorIs(t, err, service.ErrVideoAttemptTransition)
}

func videoExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	_, err := db.Exec(q, args...)
	require.NoError(t, err)
}

func (f *videoFixture) processing(t *testing.T, c *service.VideoAttemptAcceptCommand) {
	t.Helper()
	_, err := f.r.UpdateVideoAttempt(context.Background(), f.owner(), c.Attempt.ID, service.VideoAttemptUpdate{State: service.VideoAttemptSubmitting})
	require.NoError(t, err)
	_, err = f.r.UpdateVideoAttempt(context.Background(), f.owner(), c.Attempt.ID, service.VideoAttemptUpdate{State: service.VideoAttemptProcessing, UpstreamTaskID: uuid.NewString()})
	require.NoError(t, err)
}

func TestVideoAttemptPostgresSharedUserConcurrentHoldsAcrossKeys(t *testing.T) {
	f := newVideoFixture(t, "10")
	c1, c2 := f.command("6.00000000"), f.command("6.00000000")
	require.NoError(t, f.db.QueryRow(`INSERT INTO api_keys(user_id,group_id,key,name) VALUES($1,$2,$3,'other') RETURNING id`,
		f.u, f.g, uuid.NewString()).Scan(&c2.Attempt.APIKeyID))
	errors := make(chan error, 2)
	for _, c := range []*service.VideoAttemptAcceptCommand{c1, c2} {
		go func(c *service.VideoAttemptAcceptCommand) {
			_, err := (&usageBillingRepository{db: f.db}).AcceptVideoAttempt(context.Background(), c)
			errors <- err
		}(c)
	}
	winners := 0
	for i := 0; i < 2; i++ {
		err := <-errors
		if err == nil {
			winners++
		} else {
			require.ErrorIs(t, err, service.ErrVideoAttemptInsufficientFunds)
		}
	}
	require.Equal(t, 1, winners)
	require.Equal(t, "4.00000000", videoNumber(t, f.db, `SELECT balance FROM users WHERE id=$1`, f.u))
	require.Equal(t, "6.00000000", videoNumber(t, f.db, `SELECT frozen_balance FROM users WHERE id=$1`, f.u))
}

func TestVideoAttemptPostgresQuotaReservationsPersistAcrossRequestsAndRestart(t *testing.T) {
	for _, dimension := range []string{"key total", "key 5h", "key 1d", "key 7d", "account total", "account daily", "account weekly", "user concurrency", "account concurrency"} {
		t.Run(dimension, func(t *testing.T) {
			f := newVideoFixture(t, "100")
			switch dimension {
			case "key total":
				videoExec(t, f.db, `UPDATE api_keys SET quota=10 WHERE id=$1`, f.k)
			case "key 5h":
				videoExec(t, f.db, `UPDATE api_keys SET rate_limit_5h=10 WHERE id=$1`, f.k)
			case "key 1d":
				videoExec(t, f.db, `UPDATE api_keys SET rate_limit_1d=10 WHERE id=$1`, f.k)
			case "key 7d":
				videoExec(t, f.db, `UPDATE api_keys SET rate_limit_7d=10 WHERE id=$1`, f.k)
			case "account total":
				videoExec(t, f.db, `UPDATE accounts SET extra='{"quota_limit":10}' WHERE id=$1`, f.a)
			case "account daily":
				videoExec(t, f.db, `UPDATE accounts SET extra='{"quota_daily_limit":10}' WHERE id=$1`, f.a)
			case "account weekly":
				videoExec(t, f.db, `UPDATE accounts SET extra='{"quota_weekly_limit":10}' WHERE id=$1`, f.a)
			case "user concurrency":
				videoExec(t, f.db, `UPDATE users SET concurrency=1 WHERE id=$1`, f.u)
			case "account concurrency":
				videoExec(t, f.db, `UPDATE accounts SET concurrency=1 WHERE id=$1`, f.a)
			}
			c := f.command("6.00000000")
			_, err := f.r.AcceptVideoAttempt(context.Background(), c)
			require.NoError(t, err)
			// Existing usage counters are not fake reservations.
			require.Equal(t, "0.00000000", videoNumber(t, f.db, `SELECT quota_used FROM api_keys WHERE id=$1`, f.k))
			restarted := &usageBillingRepository{db: f.db}
			_, err = restarted.AcceptVideoAttempt(context.Background(), f.command("6.00000000"))
			require.ErrorIs(t, err, service.ErrVideoAttemptLimit)
			require.Equal(t, "94.00000000", videoNumber(t, f.db, `SELECT balance FROM users WHERE id=$1`, f.u))
			_, err = restarted.ReleaseVideoAttempt(context.Background(), f.owner(), c.Attempt.ID)
			require.NoError(t, err)
			_, err = restarted.AcceptVideoAttempt(context.Background(), f.command("6.00000000"))
			require.NoError(t, err, "only the released attempt's own reservation is removed")
		})
	}
}

func videoSubscription(t *testing.T, f *videoFixture, dailyLimit any) int64 {
	t.Helper()
	videoExec(t, f.db, `UPDATE groups SET subscription_type='subscription',daily_limit_usd=$1 WHERE id=$2`, dailyLimit, f.g)
	var id int64
	require.NoError(t, f.db.QueryRow(`INSERT INTO user_subscriptions(user_id,group_id,starts_at,expires_at) VALUES($1,$2,NOW()-interval '1 day',NOW()+interval '31 days') RETURNING id`,
		f.u, f.g).Scan(&id))
	return id
}

func TestVideoAttemptPostgresSubscriptionBudgetAndEligibility(t *testing.T) {
	t.Run("unlimited exact capture after price edits", func(t *testing.T) {
		f := newVideoFixture(t, "0")
		subID := videoSubscription(t, f, nil)
		c := f.command("9999999999.12345678")
		got, err := f.r.AcceptVideoAttempt(context.Background(), c)
		require.NoError(t, err)
		require.Equal(t, &subID, got.Attempt.SubscriptionID)
		f.processing(t, c)
		videoExec(t, f.db, `UPDATE groups SET rate_multiplier=3,video_rate_multiplier=4,status='disabled' WHERE id=$1`, f.g)
		videoExec(t, f.db, `UPDATE accounts SET rate_multiplier=2 WHERE id=$1`, f.a)
		videoExec(t, f.db, `UPDATE user_subscriptions SET expires_at=NOW()-interval '1 second',status='expired' WHERE id=$1`, subID)
		result, err := f.r.CaptureVideoAttempt(context.Background(), f.owner(), c.Attempt.ID, []string{"https://result.example/video.mp4"})
		require.NoError(t, err)
		require.Equal(t, "9999999999.12345678", result.Attempt.Receipt.Amount)
		require.Equal(t, "9999999999.1234567800", videoNumber(t, f.db, `SELECT daily_usage_usd FROM user_subscriptions WHERE id=$1`, subID))
		require.Equal(t, "0.00000000", videoNumber(t, f.db, `SELECT balance FROM users WHERE id=$1`, f.u))
	})
	t.Run("different keys share budget and outstanding holds survive reset", func(t *testing.T) {
		f := newVideoFixture(t, "100")
		sub := videoSubscription(t, f, "10")
		c1, c2 := f.command("6.00000000"), f.command("6.00000000")
		require.NoError(t, f.db.QueryRow(`INSERT INTO api_keys(user_id,group_id,key,name) VALUES($1,$2,$3,'sub-other') RETURNING id`, f.u, f.g, uuid.NewString()).Scan(&c2.Attempt.APIKeyID))
		_, err := f.r.AcceptVideoAttempt(context.Background(), c1)
		require.NoError(t, err)
		videoExec(t, f.db, `UPDATE user_subscriptions SET daily_window_start=NOW()-interval '2 days',daily_usage_usd=9 WHERE id=$1`, sub)
		_, err = (&usageBillingRepository{db: f.db}).AcceptVideoAttempt(context.Background(), c2)
		require.ErrorIs(t, err, service.ErrVideoAttemptLimit)
		require.Equal(t, "100.00000000", videoNumber(t, f.db, `SELECT balance FROM users WHERE id=$1`, f.u))
		_, err = f.r.ReleaseVideoAttempt(context.Background(), f.owner(), c1.Attempt.ID)
		require.NoError(t, err)
		_, err = (&usageBillingRepository{db: f.db}).AcceptVideoAttempt(context.Background(), c2)
		require.NoError(t, err)
	})
	for _, problem := range []string{"missing", "suspended", "expired", "not started", "wrong owner"} {
		t.Run(problem, func(t *testing.T) {
			f := newVideoFixture(t, "100")
			sub := videoSubscription(t, f, "10")
			switch problem {
			case "missing":
				videoExec(t, f.db, `UPDATE user_subscriptions SET deleted_at=NOW() WHERE id=$1`, sub)
			case "suspended":
				videoExec(t, f.db, `UPDATE user_subscriptions SET status='suspended' WHERE id=$1`, sub)
			case "expired":
				videoExec(t, f.db, `UPDATE user_subscriptions SET expires_at=NOW()-interval '1 second' WHERE id=$1`, sub)
			case "not started":
				videoExec(t, f.db, `UPDATE user_subscriptions SET starts_at=NOW()+interval '1 day' WHERE id=$1`, sub)
			case "wrong owner":
				var otherGroup int64
				require.NoError(t, f.db.QueryRow(`INSERT INTO groups(name,platform) VALUES($1,'openai') RETURNING id`, uuid.NewString()).Scan(&otherGroup))
				videoExec(t, f.db, `UPDATE user_subscriptions SET group_id=$1 WHERE id=$2`, otherGroup, sub)
			}
			_, err := f.r.AcceptVideoAttempt(context.Background(), f.command("1.00000000"))
			require.ErrorIs(t, err, service.ErrVideoAttemptIneligible)
			require.Equal(t, "100.00000000", videoNumber(t, f.db, `SELECT balance FROM users WHERE id=$1`, f.u))
		})
	}
}

func TestVideoAttemptPostgresPlatformQuotaFailClosedAndSubscriptionExemption(t *testing.T) {
	for _, kind := range []string{"enforced positive", "explicit zero", "all NULL", "deleted", "subscription exempt"} {
		t.Run(kind, func(t *testing.T) {
			f := newVideoFixture(t, "100")
			var limit any = "10"
			if kind == "explicit zero" {
				limit = "0"
			} else if kind == "all NULL" {
				limit = nil
			}
			videoExec(t, f.db, `INSERT INTO user_platform_quotas(user_id,platform,daily_limit_usd) VALUES($1,'openai',$2)`, f.u, limit)
			if kind == "deleted" {
				videoExec(t, f.db, `UPDATE user_platform_quotas SET deleted_at=NOW() WHERE user_id=$1`, f.u)
			} else if kind == "subscription exempt" {
				videoSubscription(t, f, "10")
			}
			c := f.command("1.00000000")
			_, err := f.r.AcceptVideoAttempt(context.Background(), c)
			if kind == "enforced positive" || kind == "explicit zero" {
				require.ErrorIs(t, err, service.ErrVideoAttemptPlatformQuota)
				require.Equal(t, "100.00000000", videoNumber(t, f.db, `SELECT balance FROM users WHERE id=$1`, f.u))
				require.Equal(t, "0", videoNumber(t, f.db, `SELECT count(*) FROM video_attempts WHERE quote_id=$1`, c.Attempt.QuoteID))
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestVideoAttemptPostgresCaptureReleaseRaceAndUnknownAcceptance(t *testing.T) {
	for i := 0; i < 6; i++ {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			f := newVideoFixture(t, "10")
			c := f.command("6.00000000")
			_, err := f.r.AcceptVideoAttempt(context.Background(), c)
			require.NoError(t, err)
			f.processing(t, c)
			start := make(chan struct{})
			errs := make(chan error, 2)
			go func() {
				<-start
				_, e := f.r.CaptureVideoAttempt(context.Background(), f.owner(), c.Attempt.ID, []string{"https://result.example/video.mp4"})
				errs <- e
			}()
			go func() {
				<-start
				_, e := (&usageBillingRepository{db: f.db}).ReleaseVideoAttempt(context.Background(), f.owner(), c.Attempt.ID)
				errs <- e
			}()
			close(start)
			success := 0
			for j := 0; j < 2; j++ {
				if err = <-errs; err == nil {
					success++
				} else {
					require.ErrorIs(t, err, service.ErrVideoAttemptTransition)
				}
			}
			require.Equal(t, 1, success)
			a, err := f.r.GetVideoAttempt(context.Background(), f.owner(), c.Attempt.ID)
			require.NoError(t, err)
			expectedBalance, expectedLogs := "10.00000000", "0"
			if a.FinancialState == service.VideoFinancialCaptured {
				expectedBalance, expectedLogs = "4.00000000", "1"
			}
			require.Equal(t, expectedBalance, videoNumber(t, f.db, `SELECT balance FROM users WHERE id=$1`, f.u))
			require.Equal(t, "0.00000000", videoNumber(t, f.db, `SELECT frozen_balance FROM users WHERE id=$1`, f.u))
			require.Equal(t, expectedLogs, videoNumber(t, f.db, `SELECT count(*) FROM usage_logs WHERE request_id=$1`, c.Attempt.RequestID()))
		})
	}
	t.Run("unknown remains held", func(t *testing.T) {
		f := newVideoFixture(t, "10")
		c := f.command("6.00000000")
		_, err := f.r.AcceptVideoAttempt(context.Background(), c)
		require.NoError(t, err)
		_, err = f.r.UpdateVideoAttempt(context.Background(), f.owner(), c.Attempt.ID, service.VideoAttemptUpdate{State: service.VideoAttemptSubmitting})
		require.NoError(t, err)
		_, err = f.r.UpdateVideoAttempt(context.Background(), f.owner(), c.Attempt.ID, service.VideoAttemptUpdate{State: service.VideoAttemptUncertain})
		require.NoError(t, err)
		_, err = f.r.ReleaseVideoAttempt(context.Background(), f.owner(), c.Attempt.ID)
		require.ErrorIs(t, err, service.ErrVideoAttemptTransition)
		_, err = f.r.UpdateVideoAttempt(context.Background(), f.owner(), c.Attempt.ID, service.VideoAttemptUpdate{State: service.VideoAttemptSubmitting})
		require.ErrorIs(t, err, service.ErrVideoAttemptTransition)
		require.Equal(t, "6.00000000", videoNumber(t, f.db, `SELECT frozen_balance FROM users WHERE id=$1`, f.u))
	})
}

func TestVideoAttemptPostgresRollbackAndArchivedDedup(t *testing.T) {
	for _, stage := range []string{"reservation insert", "capture log"} {
		t.Run(stage, func(t *testing.T) {
			f := newVideoFixture(t, "10")
			c := f.command("6.00000000")
			// A transaction-local check failure, scoped to this fixture, exercises
			// rollback after real financial mutations without replacing the DB.
			if stage == "reservation insert" {
				c.Attempt.RequestFingerprint = strings.Repeat("x", 64)
				_, err := f.r.AcceptVideoAttempt(context.Background(), c)
				require.Error(t, err)
				require.Equal(t, "10.00000000", videoNumber(t, f.db, `SELECT balance FROM users WHERE id=$1`, f.u))
				require.Equal(t, "0.00000000", videoNumber(t, f.db, `SELECT frozen_balance FROM users WHERE id=$1`, f.u))
			} else {
				videoExec(t, f.db, `UPDATE api_keys SET quota=10,rate_limit_5h=10 WHERE id=$1`, f.k)
				videoExec(t, f.db, `UPDATE accounts SET extra='{"quota_limit":10,"quota_used":4}' WHERE id=$1`, f.a)
				_, err := f.r.AcceptVideoAttempt(context.Background(), c)
				require.NoError(t, err)
				f.processing(t, c)
				// Existing log with this key forces a late unique violation after
				// dedup, quota increments, and frozen-fund capture have run.
				videoExec(t, f.db, `INSERT INTO usage_logs(user_id,api_key_id,account_id,request_id,model) VALUES($1,$2,$3,$4,'synthetic')`, f.u, f.k, f.a, c.Attempt.RequestID())
				_, err = f.r.CaptureVideoAttempt(context.Background(), f.owner(), c.Attempt.ID, []string{"https://result.example/video.mp4"})
				require.Error(t, err)
				require.Equal(t, "4.00000000", videoNumber(t, f.db, `SELECT balance FROM users WHERE id=$1`, f.u))
				require.Equal(t, "6.00000000", videoNumber(t, f.db, `SELECT frozen_balance FROM users WHERE id=$1`, f.u))
				require.Equal(t, "0.00000000", videoNumber(t, f.db, `SELECT quota_used FROM api_keys WHERE id=$1`, f.k))
				require.Equal(t, "0.00000000", videoNumber(t, f.db, `SELECT usage_5h FROM api_keys WHERE id=$1`, f.k))
				require.Equal(t, "4", videoNumber(t, f.db, `SELECT extra->>'quota_used' FROM accounts WHERE id=$1`, f.a))
				require.Equal(t, "0", videoNumber(t, f.db, `SELECT count(*) FROM scheduler_outbox WHERE account_id=$1`, f.a))
				require.Equal(t, "0", videoNumber(t, f.db, `SELECT count(*) FROM usage_billing_dedup WHERE request_id=$1`, c.Attempt.RequestID()))
				a, err := f.r.GetVideoAttempt(context.Background(), f.owner(), c.Attempt.ID)
				require.NoError(t, err)
				require.Equal(t, service.VideoFinancialReserved, a.FinancialState)
			}
		})
	}
	t.Run("archived capture receipt remains authoritative", func(t *testing.T) {
		f := newVideoFixture(t, "10")
		c := f.command("6.00000000")
		_, err := f.r.AcceptVideoAttempt(context.Background(), c)
		require.NoError(t, err)
		f.processing(t, c)
		got, err := f.r.CaptureVideoAttempt(context.Background(), f.owner(), c.Attempt.ID, []string{"https://result.example/video.mp4"})
		require.NoError(t, err)
		videoExec(t, f.db, `WITH moved AS (DELETE FROM usage_billing_dedup WHERE request_id=$1 RETURNING request_id,api_key_id,request_fingerprint,created_at)
			INSERT INTO usage_billing_dedup_archive(request_id,api_key_id,request_fingerprint,created_at) SELECT * FROM moved`, c.Attempt.RequestID())
		restarted := &usageBillingRepository{db: f.db}
		replayed, err := restarted.CaptureVideoAttempt(context.Background(), f.owner(), c.Attempt.ID, nil)
		require.NoError(t, err)
		require.False(t, replayed.Applied)
		require.Equal(t, got.Attempt.Receipt, replayed.Attempt.Receipt)
		require.Equal(t, "4.00000000", videoNumber(t, f.db, `SELECT balance FROM users WHERE id=$1`, f.u))
	})
}

func TestVideoAttemptPostgresConcurrentSubscriptionReservations(t *testing.T) {
	f := newVideoFixture(t, "100")
	videoSubscription(t, f, "10")
	c1, c2 := f.command("6.00000000"), f.command("6.00000000")
	require.NoError(t, f.db.QueryRow(`INSERT INTO api_keys(user_id,group_id,key,name) VALUES($1,$2,$3,'other-sub') RETURNING id`, f.u, f.g, uuid.NewString()).Scan(&c2.Attempt.APIKeyID))
	start := make(chan struct{})
	errs := make(chan error, 2)
	for _, c := range []*service.VideoAttemptAcceptCommand{c1, c2} {
		go func(c *service.VideoAttemptAcceptCommand) {
			<-start
			_, e := (&usageBillingRepository{db: f.db}).AcceptVideoAttempt(context.Background(), c)
			errs <- e
		}(c)
	}
	close(start)
	winners := 0
	for i := 0; i < 2; i++ {
		if e := <-errs; e == nil {
			winners++
		} else {
			require.ErrorIs(t, e, service.ErrVideoAttemptLimit)
		}
	}
	require.Equal(t, 1, winners)
	require.Equal(t, "100.00000000", videoNumber(t, f.db, `SELECT balance FROM users WHERE id=$1`, f.u))
}

func TestVideoAttemptPostgresSubscriptionStorageCapacityFailsBeforeReserve(t *testing.T) {
	f := newVideoFixture(t, "100")
	id := videoSubscription(t, f, nil)
	videoExec(t, f.db, `UPDATE user_subscriptions SET daily_window_start=NOW(),weekly_window_start=NOW(),monthly_window_start=NOW(),
		daily_usage_usd=9999999999.5 WHERE id=$1`, id)
	_, err := f.r.AcceptVideoAttempt(context.Background(), f.command("1.00000000"))
	require.ErrorIs(t, err, service.ErrVideoAttemptLimit, "an unlimited budget is not unlimited numeric storage")
	require.Equal(t, "0", videoNumber(t, f.db, `SELECT count(*) FROM video_attempts WHERE user_id=$1`, f.u))
}

func TestVideoAttemptReadinessRequiresAppliedSchemaAndDatabase(t *testing.T) {
	require.False(t, (&usageBillingRepository{}).VideoQuotedBillingReady())
	for _, ready := range []bool{false, true} {
		db, mock, err := sqlmock.New()
		require.NoError(t, err)
		mock.ExpectQuery(`SELECT EXISTS`).WillReturnRows(sqlmock.NewRows([]string{"ready"}).AddRow(ready))
		require.Equal(t, ready, (&usageBillingRepository{db: db}).VideoQuotedBillingReady())
		require.NoError(t, mock.ExpectationsWereMet())
		mock.ExpectClose()
		require.NoError(t, db.Close())
	}
}

func TestVideoAttemptPostgresQuoteReplayCannotCreateAnotherAttempt(t *testing.T) {
	f := newVideoFixture(t, "100")
	c1, c2 := f.command("1.00000000"), f.command("1.00000000")
	c2.Attempt.QuoteID = c1.Attempt.QuoteID
	start := make(chan struct{})
	errs := make(chan error, 2)
	for _, c := range []*service.VideoAttemptAcceptCommand{c1, c2} {
		go func(c *service.VideoAttemptAcceptCommand) {
			<-start
			_, e := (&usageBillingRepository{db: f.db}).AcceptVideoAttempt(context.Background(), c)
			errs <- e
		}(c)
	}
	close(start)
	winners := 0
	for i := 0; i < 2; i++ {
		if e := <-errs; e == nil {
			winners++
		} else {
			require.ErrorIs(t, e, service.ErrVideoAttemptConflict)
		}
	}
	require.Equal(t, 1, winners)
	require.Equal(t, "1", videoNumber(t, f.db, `SELECT count(*) FROM video_attempts WHERE quote_id=$1`, c1.Attempt.QuoteID))
	require.Equal(t, "99.00000000", videoNumber(t, f.db, `SELECT balance FROM users WHERE id=$1`, f.u))
}

func TestVideoAttemptPostgresTransitionsNeverRebindTaskOrReleaseForeignHold(t *testing.T) {
	f := newVideoFixture(t, "10")
	c := f.command("6.00000000")
	_, err := f.r.AcceptVideoAttempt(context.Background(), c)
	require.NoError(t, err)
	_, err = f.r.ReleaseVideoAttempt(context.Background(), f.owner(), "never-reserved")
	require.ErrorIs(t, err, service.ErrVideoAttemptNotFound)
	require.Equal(t, "6.00000000", videoNumber(t, f.db, `SELECT frozen_balance FROM users WHERE id=$1`, f.u))
	_, err = f.r.UpdateVideoAttempt(context.Background(), f.owner(), c.Attempt.ID, service.VideoAttemptUpdate{State: service.VideoAttemptCompleted})
	require.ErrorIs(t, err, service.ErrVideoAttemptTransition)
	_, err = f.r.UpdateVideoAttempt(context.Background(), f.owner(), c.Attempt.ID, service.VideoAttemptUpdate{State: service.VideoAttemptSubmitting})
	require.NoError(t, err)
	_, err = f.r.UpdateVideoAttempt(context.Background(), f.owner(), c.Attempt.ID, service.VideoAttemptUpdate{State: service.VideoAttemptSubmitting})
	require.ErrorIs(t, err, service.ErrVideoAttemptTransition)
	_, err = f.r.UpdateVideoAttempt(context.Background(), f.owner(), c.Attempt.ID, service.VideoAttemptUpdate{State: service.VideoAttemptProcessing})
	require.ErrorIs(t, err, service.ErrVideoAttemptTransition)
	_, err = f.r.UpdateVideoAttempt(context.Background(), f.owner(), c.Attempt.ID, service.VideoAttemptUpdate{State: service.VideoAttemptProcessing, UpstreamTaskID: "one"})
	require.NoError(t, err)
	_, err = f.r.UpdateVideoAttempt(context.Background(), f.owner(), c.Attempt.ID, service.VideoAttemptUpdate{State: service.VideoAttemptProcessing, UpstreamTaskID: "two"})
	require.ErrorIs(t, err, service.ErrVideoAttemptTransition)
	_, err = f.r.CaptureVideoAttempt(context.Background(), f.owner(), c.Attempt.ID, []string{"https://user:secret@result.example/output.mp4"})
	require.ErrorIs(t, err, service.ErrVideoAttemptTransition)
	_, err = f.r.CaptureVideoAttempt(context.Background(), f.owner(), c.Attempt.ID, []string{"https://result.example/output.mp4"})
	require.NoError(t, err)
	_, err = f.r.ReleaseVideoAttempt(context.Background(), f.owner(), c.Attempt.ID)
	require.ErrorIs(t, err, service.ErrVideoAttemptTransition, "download failure never refunds successful generation")
}

func TestVideoAttemptPostgresExactAccountQuotaCrossingInvalidatesScheduler(t *testing.T) {
	f := newVideoFixture(t, "10")
	videoExec(t, f.db, `UPDATE accounts SET extra='{"quota_limit":9999999999.12345678,"quota_used":9999999999.12345677}' WHERE id=$1`, f.a)
	c := f.command("0.00000001")
	_, err := f.r.AcceptVideoAttempt(context.Background(), c)
	require.NoError(t, err)
	f.processing(t, c)
	_, err = f.r.CaptureVideoAttempt(context.Background(), f.owner(), c.Attempt.ID, []string{"https://result.example/output.mp4"})
	require.NoError(t, err)
	require.Equal(t, "9999999999.12345678", videoNumber(t, f.db, `SELECT extra->>'quota_used' FROM accounts WHERE id=$1`, f.a))
	require.Equal(t, "1", videoNumber(t, f.db, `SELECT count(*) FROM scheduler_outbox WHERE account_id=$1`, f.a))
}

func TestVideoAttemptPostgresRejectsWrongPlatformAccount(t *testing.T) {
	f := newVideoFixture(t, "10")
	videoExec(t, f.db, `UPDATE accounts SET platform='anthropic' WHERE id=$1`, f.a)
	_, err := f.r.AcceptVideoAttempt(context.Background(), f.command("1.00000000"))
	require.ErrorIs(t, err, service.ErrVideoAttemptIneligible)
	require.Equal(t, "10.00000000", videoNumber(t, f.db, `SELECT balance FROM users WHERE id=$1`, f.u))
}

func TestVideoAttemptPostgresQuoteExpiringWhileWaitingForFundsLockIsRejected(t *testing.T) {
	f := newVideoFixture(t, "10")
	blocker, err := f.db.Begin()
	require.NoError(t, err)
	defer blocker.Rollback()
	_, err = blocker.Exec(`SELECT id FROM users WHERE id=$1 FOR UPDATE`, f.u)
	require.NoError(t, err)
	c := f.command("1.00000000")
	c.QuoteExpiresAt = time.Now().Add(time.Second)
	result := make(chan error, 1)
	go func() { _, e := f.r.AcceptVideoAttempt(context.Background(), c); result <- e }()
	require.Eventually(t, func() bool {
		var waiting bool
		e := f.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database()
			AND wait_event_type='Lock' AND query LIKE 'SELECT status,deleted_at FROM users%')`).Scan(&waiting)
		return e == nil && waiting
	}, 900*time.Millisecond, 10*time.Millisecond)
	time.Sleep(time.Until(c.QuoteExpiresAt.Add(20 * time.Millisecond)))
	require.NoError(t, blocker.Commit())
	require.ErrorIs(t, <-result, service.ErrVideoAttemptIneligible)
	require.Equal(t, "10.00000000", videoNumber(t, f.db, `SELECT balance FROM users WHERE id=$1`, f.u))
}

func TestVideoAttemptPostgresLeaseExpiryRecoveryAndQuoteExpiry(t *testing.T) {
	f := newVideoFixture(t, "10")
	require.True(t, f.r.VideoQuotedBillingReady())
	c := f.command("6.00000000")
	_, err := f.r.AcceptVideoAttempt(context.Background(), c)
	require.NoError(t, err)
	c.QuoteExpiresAt = time.Now().Add(-time.Hour)
	recovered, err := (&usageBillingRepository{db: f.db}).AcceptVideoAttempt(context.Background(), c)
	require.NoError(t, err)
	require.False(t, recovered.Created)
	c2 := f.command("1.00000000")
	c2.QuoteExpiresAt = c.QuoteExpiresAt
	_, err = f.r.AcceptVideoAttempt(context.Background(), c2)
	require.ErrorIs(t, err, service.ErrVideoAttemptIneligible)
	first, err := f.r.AcquireVideoAttemptLease(context.Background(), c.Attempt.ID, "worker-a", time.Minute)
	require.NoError(t, err)
	_, err = (&usageBillingRepository{db: f.db}).AcquireVideoAttemptLease(context.Background(), c.Attempt.ID, "worker-b", time.Minute)
	require.ErrorIs(t, err, service.ErrVideoAttemptLease)
	videoExec(t, f.db, `UPDATE video_attempts SET lease_expires_at=NOW()-interval '1 second',updated_at=NOW()-interval '1 hour' WHERE id=$1`, c.Attempt.ID)
	list, err := f.r.ListReconcilableVideoAttempts(context.Background(), time.Now(), 1000)
	require.NoError(t, err)
	found := false
	for _, a := range list {
		if a.ID == c.Attempt.ID {
			found = true
		}
	}
	require.True(t, found)
	next, err := (&usageBillingRepository{db: f.db}).AcquireVideoAttemptLease(context.Background(), c.Attempt.ID, "worker-b", time.Minute)
	require.NoError(t, err)
	require.NotEqual(t, first.Token, next.Token)
	require.ErrorIs(t, f.r.ReleaseVideoAttemptLease(context.Background(), first), service.ErrVideoAttemptLease)
	require.NoError(t, f.r.ReleaseVideoAttemptLease(context.Background(), next))
}

func videoNamedTestDB(t *testing.T, name string) *sql.DB {
	t.Helper()
	dsn := os.Getenv("VIDEO_BILLING_TEST_DSN")
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		var err error
		dsn, err = pq.ParseURL(dsn)
		require.NoError(t, err)
	}
	db, err := sql.Open("postgres", dsn+" application_name="+name)
	require.NoError(t, err)
	require.NoError(t, db.Ping())
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return db
}

func videoWaitForDatabaseLock(t *testing.T, db *sql.DB, application string) {
	t.Helper()
	require.Eventually(t, func() bool {
		var waiting bool
		err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_stat_activity
			WHERE datname=current_database() AND application_name=$1 AND wait_event_type='Lock')`, application).Scan(&waiting)
		return err == nil && waiting
	}, 3*time.Second, 5*time.Millisecond)
}

// A real trigger pauses the legacy Apply AFTER its subscription update holds
// that row, not a mock. Letting Apply then reach the key detects the old
// subscription->key / key->subscription cycle in both video entry points.
func TestVideoAttemptPostgresMixedLegacySubscriptionLockOrder(t *testing.T) {
	for _, operation := range []string{"accept", "capture"} {
		t.Run(operation, func(t *testing.T) {
			f := newVideoFixture(t, "10")
			sub := videoSubscription(t, f, "100")
			videoExec(t, f.db, `UPDATE user_subscriptions SET daily_window_start=NOW(),weekly_window_start=NOW(),monthly_window_start=NOW() WHERE id=$1`, sub)
			videoExec(t, f.db, `UPDATE api_keys SET quota=100,rate_limit_5h=100 WHERE id=$1`, f.k)
			videoExec(t, f.db, `UPDATE accounts SET extra='{"quota_limit":100}' WHERE id=$1`, f.a)
			c := f.command("2.00000000")
			if operation == "capture" {
				_, err := f.r.AcceptVideoAttempt(context.Background(), c)
				require.NoError(t, err)
				f.processing(t, c)
			}
			name := "video_lock_" + strings.ReplaceAll(uuid.NewString(), "-", "")
			legacyDB := videoNamedTestDB(t, name+"_legacy")
			videoDB := videoNamedTestDB(t, name+"_video")
			blocker, err := f.db.Begin()
			require.NoError(t, err)
			defer blocker.Rollback()
			lockID := sub + 246000000
			_, err = blocker.Exec(`SELECT pg_advisory_xact_lock($1)`, lockID)
			require.NoError(t, err)
			videoExec(t, f.db, fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
				BEGIN PERFORM pg_advisory_xact_lock(%d); RETURN NEW; END $$`, name, lockID))
			videoExec(t, f.db, fmt.Sprintf(`CREATE TRIGGER %s AFTER UPDATE ON user_subscriptions
				FOR EACH ROW WHEN (NEW.id=%d) EXECUTE FUNCTION %s()`, name, sub, name))
			t.Cleanup(func() {
				videoExec(t, f.db, "DROP TRIGGER IF EXISTS "+name+" ON user_subscriptions")
				videoExec(t, f.db, "DROP FUNCTION IF EXISTS "+name+"()")
			})
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			legacyResult := make(chan error, 1)
			legacyCmd := &service.UsageBillingCommand{RequestID: uuid.NewString(), UserID: f.u, APIKeyID: f.k, AccountID: f.a,
				SubscriptionID: &sub, AccountType: service.AccountTypeAPIKey, SubscriptionCost: 1, APIKeyQuotaCost: 1, APIKeyRateLimitCost: 1, AccountQuotaCost: 1}
			go func() { _, e := (&usageBillingRepository{db: legacyDB}).Apply(ctx, legacyCmd); legacyResult <- e }()
			videoWaitForDatabaseLock(t, f.db, name+"_legacy")
			videoResult := make(chan error, 1)
			go func() {
				r := &usageBillingRepository{db: videoDB}
				var e error
				if operation == "capture" {
					_, e = r.CaptureVideoAttempt(ctx, f.owner(), c.Attempt.ID, []string{"https://result.example/video.mp4"})
				} else {
					_, e = r.AcceptVideoAttempt(ctx, c)
				}
				videoResult <- e
			}()
			videoWaitForDatabaseLock(t, f.db, name+"_video")
			require.NoError(t, blocker.Commit())
			legacyErr, videoErr := <-legacyResult, <-videoResult
			require.NoError(t, legacyErr, "legacy settlement must not be chosen as a deadlock victim")
			require.NoError(t, videoErr, "video must take the compatible subscription/key/account order")
			if operation == "accept" {
				f.processing(t, c)
				_, err = f.r.CaptureVideoAttempt(context.Background(), f.owner(), c.Attempt.ID, []string{"https://result.example/video.mp4"})
				require.NoError(t, err)
			}
			require.Equal(t, "3.0000000000", videoNumber(t, f.db, `SELECT daily_usage_usd FROM user_subscriptions WHERE id=$1`, sub))
			require.Equal(t, "3.00000000", videoNumber(t, f.db, `SELECT quota_used FROM api_keys WHERE id=$1`, f.k))
			require.Equal(t, "3.00000000", videoNumber(t, f.db, `SELECT usage_5h FROM api_keys WHERE id=$1`, f.k))
			require.Equal(t, "3.00000000", videoNumber(t, f.db, `SELECT extra->>'quota_used' FROM accounts WHERE id=$1`, f.a))
			require.Equal(t, "1", videoNumber(t, f.db, `SELECT count(*) FROM usage_billing_dedup WHERE request_id=$1`, legacyCmd.RequestID))
			require.Equal(t, "1", videoNumber(t, f.db, `SELECT count(*) FROM usage_logs WHERE request_id=$1`, c.Attempt.RequestID()))
			require.Equal(t, "10.00000000", videoNumber(t, f.db, `SELECT balance FROM users WHERE id=$1`, f.u))
			require.Equal(t, "0.00000000", videoNumber(t, f.db, `SELECT frozen_balance FROM users WHERE id=$1`, f.u))
		})
	}
}

func TestVideoAttemptPostgresAdmissionExpiryDuringBillingLockWait(t *testing.T) {
	for _, resource := range []string{"subscription", "key"} {
		for _, lock := range []string{"subscription", "account"} {
			t.Run(resource+"_expiry_at_"+lock+"_lock", func(t *testing.T) {
				f := newVideoFixture(t, "10")
				sub := videoSubscription(t, f, "100")
				expires := time.Now().Add(1200 * time.Millisecond)
				if resource == "subscription" {
					videoExec(t, f.db, `UPDATE user_subscriptions SET expires_at=$2 WHERE id=$1`, sub, expires)
				} else {
					videoExec(t, f.db, `UPDATE api_keys SET expires_at=$2 WHERE id=$1`, f.k, expires)
				}
				blocker, err := f.db.Begin()
				require.NoError(t, err)
				defer blocker.Rollback()
				table, id := "user_subscriptions", sub
				if lock == "account" {
					table, id = "accounts", f.a
				}
				_, err = blocker.Exec("SELECT id FROM "+table+" WHERE id=$1 FOR UPDATE", id)
				require.NoError(t, err)
				name := "video_expiry_" + strings.ReplaceAll(uuid.NewString(), "-", "")
				db := videoNamedTestDB(t, name)
				c := f.command("1.00000000")
				c.QuoteExpiresAt = time.Now().Add(time.Minute)
				result := make(chan error, 1)
				go func() {
					_, e := (&usageBillingRepository{db: db}).AcceptVideoAttempt(context.Background(), c)
					result <- e
				}()
				videoWaitForDatabaseLock(t, f.db, name)
				time.Sleep(time.Until(expires.Add(20 * time.Millisecond)))
				require.NoError(t, blocker.Commit())
				require.ErrorIs(t, <-result, service.ErrVideoAttemptIneligible)
				require.Equal(t, "0", videoNumber(t, f.db, `SELECT count(*) FROM video_attempts WHERE id=$1`, c.Attempt.ID))
				require.Equal(t, "10.00000000", videoNumber(t, f.db, `SELECT balance FROM users WHERE id=$1`, f.u))
				require.Equal(t, "0.00000000", videoNumber(t, f.db, `SELECT frozen_balance FROM users WHERE id=$1`, f.u))
				require.Equal(t, "0.0000000000", videoNumber(t, f.db, `SELECT daily_usage_usd FROM user_subscriptions WHERE id=$1`, sub))
			})
		}
	}
}

func TestVideoAttemptPostgresBillingModeChangeDuringLockWaitRejects(t *testing.T) {
	for _, mode := range []string{"standard", "subscription"} {
		t.Run(mode, func(t *testing.T) {
			f := newVideoFixture(t, "10")
			videoSubscription(t, f, "100")
			videoExec(t, f.db, `UPDATE groups SET subscription_type=$2 WHERE id=$1`, f.g, mode)
			blocker, err := f.db.Begin()
			require.NoError(t, err)
			defer blocker.Rollback()
			_, err = blocker.Exec(`SELECT id FROM accounts WHERE id=$1 FOR UPDATE`, f.a)
			require.NoError(t, err)
			name := "video_mode_" + strings.ReplaceAll(uuid.NewString(), "-", "")
			db := videoNamedTestDB(t, name)
			c := f.command("1.00000000")
			result := make(chan error, 1)
			go func() {
				_, e := (&usageBillingRepository{db: db}).AcceptVideoAttempt(context.Background(), c)
				result <- e
			}()
			videoWaitForDatabaseLock(t, f.db, name)
			next := "subscription"
			if mode == next {
				next = "standard"
			}
			videoExec(t, f.db, `UPDATE groups SET subscription_type=$2 WHERE id=$1`, f.g, next)
			require.NoError(t, blocker.Commit())
			require.ErrorIs(t, <-result, service.ErrVideoAttemptIneligible)
			require.Equal(t, "0", videoNumber(t, f.db, `SELECT count(*) FROM video_attempts WHERE id=$1`, c.Attempt.ID))
			require.Equal(t, "10.00000000", videoNumber(t, f.db, `SELECT balance FROM users WHERE id=$1`, f.u))
			require.Equal(t, "0.00000000", videoNumber(t, f.db, `SELECT frozen_balance FROM users WHERE id=$1`, f.u))
		})
	}
}

func TestVideoAttemptPostgresAcceptedSubscriptionSurvivesExpiryAndModeChange(t *testing.T) {
	f := newVideoFixture(t, "10")
	sub := videoSubscription(t, f, "100")
	videoExec(t, f.db, `UPDATE api_keys SET quota=100 WHERE id=$1`, f.k)
	videoExec(t, f.db, `UPDATE accounts SET extra='{"quota_limit":100}' WHERE id=$1`, f.a)
	c := f.command("2.12345678")
	_, err := f.r.AcceptVideoAttempt(context.Background(), c)
	require.NoError(t, err)
	videoExec(t, f.db, `UPDATE api_keys SET expires_at=NOW()-interval '1 second' WHERE id=$1`, f.k)
	videoExec(t, f.db, `UPDATE user_subscriptions SET expires_at=NOW()-interval '1 second',status='expired' WHERE id=$1`, sub)
	videoExec(t, f.db, `UPDATE groups SET subscription_type='standard',rate_multiplier=9 WHERE id=$1`, f.g)
	videoExec(t, f.db, `UPDATE accounts SET rate_multiplier=7 WHERE id=$1`, f.a)
	c.QuoteExpiresAt = time.Now().Add(-time.Hour)
	retry, err := f.r.AcceptVideoAttempt(context.Background(), c)
	require.NoError(t, err)
	require.False(t, retry.Created)
	require.Equal(t, &sub, retry.Attempt.SubscriptionID)
	f.processing(t, c)
	result, err := f.r.CaptureVideoAttempt(context.Background(), f.owner(), c.Attempt.ID, []string{"https://result.example/video.mp4"})
	require.NoError(t, err)
	require.True(t, result.Applied)
	require.Equal(t, "2.12345678", result.Attempt.Receipt.Amount)
	require.Equal(t, &sub, result.Attempt.SubscriptionID)
	require.Equal(t, "2.1234567800", videoNumber(t, f.db, `SELECT daily_usage_usd FROM user_subscriptions WHERE id=$1`, sub))
	require.Equal(t, "2.12345678", videoNumber(t, f.db, `SELECT quota_used FROM api_keys WHERE id=$1`, f.k))
	require.Equal(t, "2.12345678", videoNumber(t, f.db, `SELECT extra->>'quota_used' FROM accounts WHERE id=$1`, f.a))
	require.Equal(t, "10.00000000", videoNumber(t, f.db, `SELECT balance FROM users WHERE id=$1`, f.u))
	require.Equal(t, "0.00000000", videoNumber(t, f.db, `SELECT frozen_balance FROM users WHERE id=$1`, f.u))
}
