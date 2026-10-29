package repository

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/shopspring/decimal"
)

// These optional capabilities deliberately do not alter normal usage DTOs.
// Each scope reads actual usage and durable holds in ONE statement, so an
// atomic capture is seen either as held or as used, never both or neither.
func (r *apiKeyRepository) CheckVideoReservationHeadroom(ctx context.Context, check service.VideoReservationBudgetCheck) error {
	return checkVideoBudgetHeadroom(ctx, r.sql, check)
}

func (r *accountRepository) CheckVideoReservationHeadroom(ctx context.Context, check service.VideoReservationBudgetCheck) error {
	return checkVideoBudgetHeadroom(ctx, r.sql, check)
}

func videoBudgetRow(ctx context.Context, q sqlExecutor, query string, args []any, dest ...any) error {
	if q == nil {
		return service.ErrVideoAttemptUnavailable
	}
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return err
		}
		return service.ErrVideoAttemptIneligible
	}
	return rows.Scan(dest...)
}

func videoBudgetPending(v string) (decimal.Decimal, error) {
	if v == "" {
		return decimal.Zero, nil
	}
	d, err := decimal.NewFromString(v)
	if err != nil || d.IsNegative() {
		return decimal.Zero, service.ErrVideoAttemptMoney
	}
	return d, nil
}

func videoBudgetExceeded(used, held, limit string, pending decimal.Decimal) (bool, error) {
	u, err := decimal.NewFromString(used)
	if err != nil {
		return false, err
	}
	h, err := decimal.NewFromString(held)
	if err != nil {
		return false, err
	}
	l, err := decimal.NewFromString(limit)
	if err != nil {
		return false, err
	}
	if !l.IsPositive() {
		return false, nil
	}
	sum := u.Add(h).Add(pending)
	return sum.GreaterThan(l) || (pending.IsZero() && sum.Equal(l)), nil
}

func checkVideoBudgetHeadroom(ctx context.Context, q sqlExecutor, check service.VideoReservationBudgetCheck) error {
	pending, err := videoBudgetPending(check.KeyPending)
	if err != nil {
		return err
	}
	if check.APIKeyID > 0 {
		var held string
		var used, limits [4]string
		err = videoBudgetRow(ctx, q, `SELECT
			COALESCE((SELECT sum(total_price) FROM video_attempts WHERE api_key_id=k.id AND financial_state='reserved'),0)::text,
			k.quota_used::text,k.quota::text,
			(CASE WHEN window_5h_start IS NULL OR window_5h_start+interval '5 hours'<=NOW() THEN 0 ELSE usage_5h END)::text,rate_limit_5h::text,
			(CASE WHEN window_1d_start IS NULL OR window_1d_start+interval '24 hours'<=NOW() THEN 0 ELSE usage_1d END)::text,rate_limit_1d::text,
			(CASE WHEN window_7d_start IS NULL OR window_7d_start+interval '7 days'<=NOW() THEN 0 ELSE usage_7d END)::text,rate_limit_7d::text
			FROM api_keys k WHERE k.id=$1 AND k.user_id=$2 AND k.deleted_at IS NULL`, []any{check.APIKeyID, check.UserID},
			&held, &used[0], &limits[0], &used[1], &limits[1], &used[2], &limits[2], &used[3], &limits[3])
		if err != nil {
			return err
		}
		failures := []error{service.ErrAPIKeyQuotaExhausted, service.ErrAPIKeyRateLimit5hExceeded, service.ErrAPIKeyRateLimit1dExceeded, service.ErrAPIKeyRateLimit7dExceeded}
		for i := range used {
			if i == 0 && check.KeyWindowsOnly {
				continue
			}
			exceeded, err := videoBudgetExceeded(used[i], held, limits[i], pending)
			if err != nil {
				return err
			}
			if exceeded {
				return failures[i]
			}
		}
	}
	if check.SubscriptionBilling {
		pending, err = videoBudgetPending(check.SubscriptionPending)
		if err != nil {
			return err
		}
		var sub service.UserSubscription
		var now time.Time
		var starts [3]sql.NullTime
		var used, limits [3]string
		var held string
		err = videoBudgetRow(ctx, q, `SELECT us.starts_at,us.expires_at,NOW(),
			us.daily_window_start,us.weekly_window_start,us.monthly_window_start,
			us.daily_usage_usd::text,us.weekly_usage_usd::text,us.monthly_usage_usd::text,
			COALESCE(g.daily_limit_usd,0)::text,COALESCE(g.weekly_limit_usd,0)::text,COALESCE(g.monthly_limit_usd,0)::text,
			COALESCE((SELECT sum(total_price) FROM video_attempts WHERE subscription_id=us.id AND financial_state='reserved'),0)::text
			FROM user_subscriptions us JOIN groups g ON g.id=us.group_id
			WHERE us.user_id=$1 AND us.group_id=$2 AND us.deleted_at IS NULL AND g.deleted_at IS NULL
			AND us.status='active' AND us.starts_at<=NOW() AND us.expires_at>NOW()`,
			[]any{check.UserID, check.GroupID}, &sub.StartsAt, &sub.ExpiresAt, &now, &starts[0], &starts[1], &starts[2],
			&used[0], &used[1], &used[2], &limits[0], &limits[1], &limits[2], &held)
		if err != nil {
			if errors.Is(err, service.ErrVideoAttemptIneligible) {
				return service.ErrSubscriptionInvalid
			}
			return err
		}
		for i, p := range []**time.Time{&sub.DailyWindowStart, &sub.WeeklyWindowStart, &sub.MonthlyWindowStart} {
			if starts[i].Valid {
				*p = &starts[i].Time
			}
		}
		d, w, m := service.VideoSubscriptionWindowStarts(&sub, now)
		failures := []error{service.ErrDailyLimitExceeded, service.ErrWeeklyLimitExceeded, service.ErrMonthlyLimitExceeded}
		for i, reset := range []*time.Time{d, w, m} {
			if reset != nil {
				used[i] = "0"
			}
			exceeded, err := videoBudgetExceeded(used[i], held, limits[i], pending)
			if err != nil {
				return err
			}
			if exceeded {
				return failures[i]
			}
		}
	}
	if check.AccountID > 0 {
		var held string
		var used, limits [3]string
		err = videoBudgetRow(ctx, q, `SELECT
			COALESCE((SELECT sum(account_cost) FROM video_attempts WHERE account_id=accounts.id AND financial_state='reserved'),0)::text,
			COALESCE(extra->>'quota_used','0'),COALESCE(extra->>'quota_limit','0'),
			(CASE WHEN `+dailyExpiredExpr+` THEN 0 ELSE COALESCE((extra->>'quota_daily_used')::numeric,0) END)::text,COALESCE(extra->>'quota_daily_limit','0'),
			(CASE WHEN `+weeklyExpiredExpr+` THEN 0 ELSE COALESCE((extra->>'quota_weekly_used')::numeric,0) END)::text,COALESCE(extra->>'quota_weekly_limit','0')
			FROM accounts WHERE id=$1 AND deleted_at IS NULL`, []any{check.AccountID},
			&held, &used[0], &limits[0], &used[1], &limits[1], &used[2], &limits[2])
		if err != nil {
			return err
		}
		for i := range used {
			exceeded, err := videoBudgetExceeded(used[i], held, limits[i], decimal.Zero)
			if err != nil {
				return err
			}
			if exceeded {
				return service.ErrVideoAttemptLimit
			}
		}
	}
	return nil
}
