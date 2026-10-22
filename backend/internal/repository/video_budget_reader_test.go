package repository

import (
	"context"
	"testing"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func newVideoBudgetFixture(t *testing.T) *videoFixture {
	t.Helper()
	f := newVideoFixture(t, "20")
	videoExec(t, f.db, `UPDATE api_keys SET window_5h_start=NOW(),window_1d_start=NOW(),window_7d_start=NOW() WHERE id=$1`, f.k)
	return f
}

// These tests catch admission readers ignoring a real durable video hold.
// Actual counters must stay actual: neither rejection nor release may persist
// virtual usage or a quota_exhausted status.
func TestVideoBudgetPostgresLegacyKeyAdmissionSeesReservedHeadroom(t *testing.T) {
	for _, dimension := range []string{"quota", "rate_limit_5h", "rate_limit_1d", "rate_limit_7d"} {
		t.Run(dimension, func(t *testing.T) {
			f := newVideoBudgetFixture(t)
			videoExec(t, f.db, `UPDATE api_keys SET `+dimension+`=2 WHERE id=$1`, f.k)
			c := f.command("2.00000000")
			_, err := f.r.AcceptVideoAttempt(context.Background(), c)
			require.NoError(t, err)
			client := dbent.NewClient(dbent.Driver(entsql.OpenDB("postgres", f.db)))
			keyRepo := NewAPIKeyRepository(client, f.db)
			cache := service.NewBillingCacheService(videoBudgetWarmCache{}, NewUserRepository(client, f.db), NewUserSubscriptionRepository(client), keyRepo, nil, nil, &config.Config{}, nil)
			defer cache.Stop()
			key, err := keyRepo.GetByID(context.Background(), f.k)
			require.NoError(t, err)
			want := map[string]error{"quota": service.ErrAPIKeyQuotaExhausted, "rate_limit_5h": service.ErrAPIKeyRateLimit5hExceeded, "rate_limit_1d": service.ErrAPIKeyRateLimit1dExceeded, "rate_limit_7d": service.ErrAPIKeyRateLimit7dExceeded}[dimension]
			require.ErrorIs(t, cache.CheckBillingEligibility(context.Background(), key.User, key, key.Group, nil, ""), want)
			require.Equal(t, "0.00000000", videoNumber(t, f.db, `SELECT quota_used FROM api_keys WHERE id=$1`, f.k))
			require.Equal(t, service.StatusAPIKeyActive, videoNumber(t, f.db, `SELECT status FROM api_keys WHERE id=$1`, f.k))
			_, err = f.r.ReleaseVideoAttempt(context.Background(), f.owner(), c.Attempt.ID)
			require.NoError(t, err)
			require.NoError(t, cache.CheckBillingEligibility(context.Background(), key.User, key, key.Group, nil, ""))
		})
	}
}

func TestVideoBudgetPostgresLegacyFixedChargeCaptureAndRelease(t *testing.T) {
	f := newVideoBudgetFixture(t)
	videoExec(t, f.db, `UPDATE api_keys SET quota=3,rate_limit_5h=3 WHERE id=$1`, f.k)
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB("postgres", f.db)))
	keyRepo := NewAPIKeyRepository(client, f.db)
	cache := service.NewBillingCacheService(nil, NewUserRepository(client, f.db), NewUserSubscriptionRepository(client), keyRepo, nil, nil, &config.Config{}, nil)
	defer cache.Stop()
	key, err := keyRepo.GetByID(context.Background(), f.k)
	require.NoError(t, err)
	c := f.command("2.00000000")
	_, err = f.r.AcceptVideoAttempt(context.Background(), c)
	require.NoError(t, err)
	res, err := cache.ReserveFixedCharge(context.Background(), key.User, key, key.Group, nil, 1.01)
	require.ErrorIs(t, err, service.ErrAPIKeyQuotaExhausted)
	require.Nil(t, res)
	_, err = f.r.UpdateVideoAttempt(context.Background(), f.owner(), c.Attempt.ID, service.VideoAttemptUpdate{State: service.VideoAttemptSubmitting})
	require.NoError(t, err)
	_, err = f.r.UpdateVideoAttempt(context.Background(), f.owner(), c.Attempt.ID, service.VideoAttemptUpdate{State: service.VideoAttemptProcessing, UpstreamTaskID: "budget-capture"})
	require.NoError(t, err)
	_, err = f.r.CaptureVideoAttempt(context.Background(), f.owner(), c.Attempt.ID, []string{"https://result.example/video.mp4"})
	require.NoError(t, err)
	res, err = cache.ReserveFixedCharge(context.Background(), key.User, key, key.Group, nil, 1)
	require.NoError(t, err, "captured usage must replace the hold, not add it twice")
	if res != nil {
		res.Release()
	}
	require.Equal(t, "2.00000000", videoNumber(t, f.db, `SELECT quota_used FROM api_keys WHERE id=$1`, f.k))
}

func TestVideoBudgetPostgresReadOnlyAccountHeadroomAcrossExpiredWindows(t *testing.T) {
	for name, extra := range map[string]string{
		"total":          `{"quota_limit":2}`,
		"daily rolling":  `{"quota_daily_limit":2,"quota_daily_used":99,"quota_daily_start":"2000-01-01T00:00:00Z"}`,
		"weekly rolling": `{"quota_weekly_limit":2,"quota_weekly_used":99,"quota_weekly_start":"2000-01-01T00:00:00Z"}`,
		"daily fixed":    `{"quota_daily_limit":2,"quota_daily_used":99,"quota_daily_reset_mode":"fixed","quota_daily_reset_at":"2000-01-01T00:00:00Z"}`,
		"weekly fixed":   `{"quota_weekly_limit":2,"quota_weekly_used":99,"quota_weekly_reset_mode":"fixed","quota_weekly_reset_at":"2000-01-01T00:00:00Z"}`,
	} {
		t.Run(name, func(t *testing.T) {
			f := newVideoBudgetFixture(t)
			videoExec(t, f.db, `UPDATE accounts SET extra=$2::jsonb WHERE id=$1`, f.a, extra)
			c := f.command("2.00000000")
			_, err := f.r.AcceptVideoAttempt(context.Background(), c)
			require.NoError(t, err)
			reader := NewAccountRepository(nil, f.db, nil).(service.VideoReservationBudgetReader)
			request := service.VideoReservationBudgetCheck{AccountID: f.a}
			require.ErrorIs(t, reader.CheckVideoReservationHeadroom(context.Background(), request), service.ErrVideoAttemptLimit)
			_, err = f.r.ReleaseVideoAttempt(context.Background(), f.owner(), c.Attempt.ID)
			require.NoError(t, err)
			require.NoError(t, reader.CheckVideoReservationHeadroom(context.Background(), request))
			videoExec(t, f.db, `UPDATE accounts SET deleted_at=$2 WHERE id=$1`, f.a, time.Now())
			require.Error(t, reader.CheckVideoReservationHeadroom(context.Background(), request))
		})
	}
}

type videoBudgetWarmCache struct{ service.BillingCache }

func (videoBudgetWarmCache) GetUserBalance(context.Context, int64) (float64, error) {
	return 20, nil
}
func (videoBudgetWarmCache) GetSubscriptionCache(context.Context, int64, int64) (*service.SubscriptionCacheData, error) {
	return &service.SubscriptionCacheData{Status: service.SubscriptionStatusActive, ExpiresAt: time.Now().Add(time.Hour)}, nil
}
func (videoBudgetWarmCache) GetAPIKeyRateLimit(context.Context, int64) (*service.APIKeyRateLimitCacheData, error) {
	now := time.Now().Unix()
	return &service.APIKeyRateLimitCacheData{Window5h: now, Window1d: now, Window7d: now}, nil
}

func TestVideoBudgetPostgresLegacySubscriptionSeesHoldsDespiteWarmCache(t *testing.T) {
	for _, dimension := range []string{"daily", "weekly", "monthly"} {
		t.Run(dimension, func(t *testing.T) {
			f := newVideoBudgetFixture(t)
			subID := videoSubscription(t, f, "100")
			videoExec(t, f.db, `UPDATE groups SET `+dimension+`_limit_usd=2 WHERE id=$1`, f.g)
			client := dbent.NewClient(dbent.Driver(entsql.OpenDB("postgres", f.db)))
			keyRepo := NewAPIKeyRepository(client, f.db)
			subRepo := NewUserSubscriptionRepository(client)
			cache := service.NewBillingCacheService(videoBudgetWarmCache{}, NewUserRepository(client, f.db), subRepo, keyRepo, nil, nil, &config.Config{}, nil)
			defer cache.Stop()
			key, err := keyRepo.GetByID(context.Background(), f.k)
			require.NoError(t, err)
			sub, err := subRepo.GetByID(context.Background(), subID)
			require.NoError(t, err)
			c := f.command("2.00000000")
			_, err = f.r.AcceptVideoAttempt(context.Background(), c)
			require.NoError(t, err)
			want := map[string]error{"daily": service.ErrDailyLimitExceeded, "weekly": service.ErrWeeklyLimitExceeded, "monthly": service.ErrMonthlyLimitExceeded}[dimension]
			require.ErrorIs(t, cache.CheckBillingEligibility(context.Background(), key.User, key, key.Group, sub, ""), want)
			_, err = f.r.ReleaseVideoAttempt(context.Background(), f.owner(), c.Attempt.ID)
			require.NoError(t, err)
			require.NoError(t, cache.CheckBillingEligibility(context.Background(), key.User, key, key.Group, sub, ""))
			require.Equal(t, "0.0000000000", videoNumber(t, f.db, `SELECT `+dimension+`_usage_usd FROM user_subscriptions WHERE id=$1`, subID))
		})
	}
}

func TestVideoBudgetPostgresExpiredSubscriptionWindowAndFixedCharge(t *testing.T) {
	f := newVideoBudgetFixture(t)
	subID := videoSubscription(t, f, "3")
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB("postgres", f.db)))
	keyRepo := NewAPIKeyRepository(client, f.db)
	subRepo := NewUserSubscriptionRepository(client)
	cache := service.NewBillingCacheService(nil, NewUserRepository(client, f.db), subRepo, keyRepo, nil, nil, &config.Config{}, nil)
	defer cache.Stop()
	key, err := keyRepo.GetByID(context.Background(), f.k)
	require.NoError(t, err)
	sub, err := subRepo.GetByID(context.Background(), subID)
	require.NoError(t, err)
	c := f.command("2.00000000")
	_, err = f.r.AcceptVideoAttempt(context.Background(), c)
	require.NoError(t, err)
	videoExec(t, f.db, `UPDATE user_subscriptions SET daily_window_start=NOW()-interval '2 days',daily_usage_usd=9 WHERE id=$1`, subID)
	res, err := cache.ReserveFixedCharge(context.Background(), key.User, key, key.Group, sub, 1)
	require.NoError(t, err, "expired actual usage resets, but the two-dollar video hold survives")
	if res != nil {
		res.Release()
	}
	res, err = cache.ReserveFixedCharge(context.Background(), key.User, key, key.Group, sub, 1.01)
	require.ErrorIs(t, err, service.ErrDailyLimitExceeded)
	require.Nil(t, res)
	_, err = f.r.ReleaseVideoAttempt(context.Background(), f.owner(), c.Attempt.ID)
	require.NoError(t, err)
	require.NoError(t, cache.CheckBillingEligibility(context.Background(), key.User, key, key.Group, sub, ""))
	reader := keyRepo.(service.VideoReservationBudgetReader)
	videoExec(t, f.db, `UPDATE user_subscriptions SET deleted_at=NOW() WHERE id=$1`, subID)
	require.ErrorIs(t, reader.CheckVideoReservationHeadroom(context.Background(), service.VideoReservationBudgetCheck{APIKeyID: f.k, UserID: f.u, GroupID: f.g, SubscriptionBilling: true}), service.ErrSubscriptionInvalid)
}

func TestVideoBudgetPostgresAccountCaptureReplacesHold(t *testing.T) {
	for _, dimension := range []string{"quota", "quota_daily", "quota_weekly"} {
		t.Run(dimension, func(t *testing.T) {
			f := newVideoBudgetFixture(t)
			videoExec(t, f.db, `UPDATE accounts SET extra=jsonb_build_object($2::text,3,'quota_daily_start',NOW(),'quota_weekly_start',NOW()) WHERE id=$1`, f.a, dimension+"_limit")
			c := f.command("2.00000000")
			_, err := f.r.AcceptVideoAttempt(context.Background(), c)
			require.NoError(t, err)
			reader := NewAccountRepository(nil, f.db, nil).(service.VideoReservationBudgetReader)
			check := service.VideoReservationBudgetCheck{AccountID: f.a}
			require.NoError(t, reader.CheckVideoReservationHeadroom(context.Background(), check))
			f.processing(t, c)
			_, err = f.r.CaptureVideoAttempt(context.Background(), f.owner(), c.Attempt.ID, []string{"https://result.example/video.mp4"})
			require.NoError(t, err)
			require.NoError(t, reader.CheckVideoReservationHeadroom(context.Background(), check), "two actual dollars must not become four")
			require.Equal(t, "2.00000000", videoNumber(t, f.db, `SELECT extra->>$2 FROM accounts WHERE id=$1`, f.a, dimension+"_used"))
		})
	}
}

func TestVideoBudgetPostgresSubscriptionCaptureReplacesHold(t *testing.T) {
	f := newVideoBudgetFixture(t)
	subID := videoSubscription(t, f, "3")
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB("postgres", f.db)))
	keyRepo := NewAPIKeyRepository(client, f.db)
	subRepo := NewUserSubscriptionRepository(client)
	cache := service.NewBillingCacheService(videoBudgetWarmCache{}, NewUserRepository(client, f.db), subRepo, keyRepo, nil, nil, &config.Config{}, nil)
	defer cache.Stop()
	key, err := keyRepo.GetByID(context.Background(), f.k)
	require.NoError(t, err)
	sub, err := subRepo.GetByID(context.Background(), subID)
	require.NoError(t, err)
	c := f.command("2.00000000")
	_, err = f.r.AcceptVideoAttempt(context.Background(), c)
	require.NoError(t, err)
	f.processing(t, c)
	_, err = f.r.CaptureVideoAttempt(context.Background(), f.owner(), c.Attempt.ID, []string{"https://result.example/video.mp4"})
	require.NoError(t, err)
	res, err := cache.ReserveFixedCharge(context.Background(), key.User, key, key.Group, sub, 1)
	require.NoError(t, err)
	if res != nil {
		res.Release()
	}
	res, err = cache.ReserveFixedCharge(context.Background(), key.User, key, key.Group, sub, 1.01)
	require.ErrorIs(t, err, service.ErrDailyLimitExceeded)
	require.Nil(t, res)
}

func TestVideoBudgetPostgresExpiredKeyWindowsKeepHoldsAndDeletedKeysReject(t *testing.T) {
	f := newVideoBudgetFixture(t)
	videoExec(t, f.db, `UPDATE api_keys SET rate_limit_5h=2,rate_limit_1d=2,rate_limit_7d=2 WHERE id=$1`, f.k)
	c := f.command("2.00000000")
	_, err := f.r.AcceptVideoAttempt(context.Background(), c)
	require.NoError(t, err)
	videoExec(t, f.db, `UPDATE api_keys SET usage_5h=99,usage_1d=99,usage_7d=99,window_5h_start=NOW()-interval '8 days',window_1d_start=NOW()-interval '8 days',window_7d_start=NOW()-interval '8 days' WHERE id=$1`, f.k)
	reader := NewAPIKeyRepository(nil, f.db).(service.VideoReservationBudgetReader)
	check := service.VideoReservationBudgetCheck{APIKeyID: f.k, UserID: f.u}
	require.ErrorIs(t, reader.CheckVideoReservationHeadroom(context.Background(), check), service.ErrAPIKeyRateLimit5hExceeded)
	_, err = f.r.ReleaseVideoAttempt(context.Background(), f.owner(), c.Attempt.ID)
	require.NoError(t, err)
	require.NoError(t, reader.CheckVideoReservationHeadroom(context.Background(), check))
	require.Equal(t, "99.00000000", videoNumber(t, f.db, `SELECT usage_5h FROM api_keys WHERE id=$1`, f.k), "read-only window handling must not write virtual usage")
	videoExec(t, f.db, `UPDATE api_keys SET window_5h_start=NULL,window_1d_start=NULL,window_7d_start=NULL WHERE id=$1`, f.k)
	require.NoError(t, reader.CheckVideoReservationHeadroom(context.Background(), check), "uninitialized windows have stale actual usage, but no enduring hold after release")
	videoExec(t, f.db, `UPDATE api_keys SET deleted_at=NOW() WHERE id=$1`, f.k)
	require.Error(t, reader.CheckVideoReservationHeadroom(context.Background(), check))
}

func TestVideoBudgetPostgresSimpleModeRetainsWindowOnlyOptIn(t *testing.T) {
	f := newVideoBudgetFixture(t)
	videoExec(t, f.db, `UPDATE api_keys SET quota=2,rate_limit_5h=3 WHERE id=$1`, f.k)
	_, err := f.r.AcceptVideoAttempt(context.Background(), f.command("2.00000000"))
	require.NoError(t, err)
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB("postgres", f.db)))
	keyRepo := NewAPIKeyRepository(client, f.db)
	key, err := keyRepo.GetByID(context.Background(), f.k)
	require.NoError(t, err)
	cache := service.NewBillingCacheService(nil, nil, nil, keyRepo, nil, nil, &config.Config{RunMode: config.RunModeSimple, SimpleModeKeyRateLimitEnabled: true}, nil)
	defer cache.Stop()
	require.NoError(t, cache.CheckBillingEligibility(context.Background(), key.User, key, key.Group, nil, ""), "simple mode opted into windows, not lifetime quota")
	videoExec(t, f.db, `UPDATE api_keys SET rate_limit_5h=2 WHERE id=$1`, f.k)
	require.ErrorIs(t, cache.CheckBillingEligibility(context.Background(), key.User, key, key.Group, nil, ""), service.ErrAPIKeyRateLimit5hExceeded)
}
