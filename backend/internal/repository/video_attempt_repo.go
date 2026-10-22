package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

var _ service.VideoAttemptRepository = (*usageBillingRepository)(nil)

func (r *usageBillingRepository) VideoQuotedBillingReady() bool {
	if r == nil || r.db == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var ready bool
	err := r.db.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM schema_migrations WHERE filename = '246_video_attempts.sql'
	) AND to_regclass('video_attempts') IS NOT NULL`).Scan(&ready)
	return err == nil && ready
}

const videoAttemptColumns = `id,quote_id,request_fingerprint,user_id,api_key_id,group_id,account_id,subscription_id,
	model,mode,controls,base_price::text,total_price::text,multiplier::text,account_cost::text,account_multiplier::text,
	tariff_revision,contract_revision,supplier_estimated,state,financial_state,COALESCE(upstream_task_id,''),
	result_urls,receipt_usage_log_id,receipt_captured_at,created_at,updated_at`

type videoScanner interface{ Scan(...any) error }

func scanVideoAttempt(row videoScanner) (*service.VideoAttempt, error) {
	a := &service.VideoAttempt{}
	var controls, urls []byte
	var sub, receiptID sql.NullInt64
	var captured sql.NullTime
	err := row.Scan(&a.ID, &a.QuoteID, &a.RequestFingerprint, &a.UserID, &a.APIKeyID, &a.GroupID, &a.AccountID, &sub,
		&a.Selection.Model, &a.Selection.Mode, &controls, &a.BasePrice, &a.TotalPrice, &a.Multiplier, &a.AccountCost, &a.AccountMultiplier,
		&a.TariffRevision, &a.ContractRevision, &a.SupplierEstimated, &a.State, &a.FinancialState, &a.UpstreamTaskID,
		&urls, &receiptID, &captured, &a.CreatedAt, &a.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, service.ErrVideoAttemptNotFound
	}
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(controls, &a.Selection.Controls); err != nil {
		return nil, err
	}
	if err = json.Unmarshal(urls, &a.ResultURLs); err != nil {
		return nil, err
	}
	if sub.Valid {
		a.SubscriptionID = &sub.Int64
	}
	if receiptID.Valid {
		a.Receipt = &service.VideoChargeReceipt{RequestID: a.RequestID(), Amount: a.TotalPrice, Currency: "USD", UsageLogID: receiptID.Int64, CapturedAt: captured.Time}
	}
	return a, nil
}

func (r *usageBillingRepository) GetVideoAttempt(ctx context.Context, owner service.VideoAttemptOwner, id string) (*service.VideoAttempt, error) {
	return scanVideoAttempt(r.db.QueryRowContext(ctx, `SELECT `+videoAttemptColumns+`
		FROM video_attempts WHERE id=$1 AND user_id=$2 AND api_key_id=$3`, id, owner.UserID, owner.APIKeyID))
}

func (r *usageBillingRepository) GetVideoAttemptByTask(ctx context.Context, owner service.VideoAttemptOwner, taskID string) (*service.VideoAttempt, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+videoAttemptColumns+`
		FROM video_attempts WHERE user_id=$1 AND api_key_id=$2 AND upstream_task_id=$3 LIMIT 2`,
		owner.UserID, owner.APIKeyID, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var attempt *service.VideoAttempt
	for rows.Next() {
		if attempt != nil {
			// Supplier IDs are unique per account, not globally. Require the
			// stable attempt resource if two owned accounts use the same ID.
			return nil, service.ErrVideoAttemptConflict
		}
		attempt, err = scanVideoAttempt(rows)
		if err != nil {
			return nil, err
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if attempt == nil {
		return nil, service.ErrVideoAttemptNotFound
	}
	return attempt, nil
}

func videoOwnedAttempt(ctx context.Context, tx *sql.Tx, owner service.VideoAttemptOwner, id string) (*service.VideoAttempt, error) {
	return scanVideoAttempt(tx.QueryRowContext(ctx, `SELECT `+videoAttemptColumns+`
		FROM video_attempts WHERE id=$1 AND user_id=$2 AND api_key_id=$3 FOR UPDATE`, id, owner.UserID, owner.APIKeyID))
}

func videoBegin(ctx context.Context, r *usageBillingRepository) (*sql.Tx, error) {
	if r == nil || r.db == nil {
		return nil, service.ErrVideoAttemptUnavailable
	}
	return r.db.BeginTx(ctx, nil)
}

func sameVideoAcceptance(old, a *service.VideoAttempt) bool {
	x, _ := json.Marshal(old.Selection)
	y, _ := json.Marshal(a.Selection)
	return old.ID == a.ID && old.QuoteID == a.QuoteID && old.UserID == a.UserID && old.APIKeyID == a.APIKeyID &&
		old.GroupID == a.GroupID && old.AccountID == a.AccountID && old.RequestFingerprint == a.RequestFingerprint &&
		old.TotalPrice == a.TotalPrice && old.BasePrice == a.BasePrice &&
		old.TariffRevision == a.TariffRevision && old.ContractRevision == a.ContractRevision && string(x) == string(y)
}

func (r *usageBillingRepository) AcceptVideoAttempt(ctx context.Context, cmd *service.VideoAttemptAcceptCommand) (*service.VideoAttemptAcceptResult, error) {
	if cmd == nil || cmd.Attempt == nil {
		return nil, service.ErrVideoAttemptIneligible
	}
	// Clone before deriving database-authoritative financial fields; callers may
	// concurrently retry the same immutable command from separate processes.
	copy := *cmd.Attempt
	a := &copy
	if !service.ValidVideoLedgerMoney(a.TotalPrice) || !service.ValidVideoLedgerMoney(a.BasePrice) {
		return nil, service.ErrVideoAttemptMoney
	}
	if a.ID == "" || len(a.ID) > 128 || a.QuoteID == "" || len(a.RequestID()) > 64 ||
		len(a.RequestFingerprint) != 64 || a.UserID <= 0 || a.APIKeyID <= 0 || a.GroupID <= 0 || a.AccountID <= 0 {
		return nil, service.ErrVideoAttemptIneligible
	}
	if _, err := service.VideoTariffKey(a.Selection); err != nil {
		return nil, service.ErrVideoAttemptIneligible
	}
	tx, err := videoBegin(ctx, r)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// Serialize a quote before taking its existing attempt row and owner locks.
	// Hash collisions only serialize unrelated quotes, never weaken uniqueness.
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,246))`, a.QuoteID); err != nil {
		return nil, err
	}
	old, err := scanVideoAttempt(tx.QueryRowContext(ctx, `SELECT `+videoAttemptColumns+` FROM video_attempts WHERE quote_id=$1 OR id=$2 FOR UPDATE`, a.QuoteID, a.ID))
	if err == nil {
		if !sameVideoAcceptance(old, a) {
			return nil, service.ErrVideoAttemptConflict
		}
		return &service.VideoAttemptAcceptResult{Attempt: old}, nil
	}
	if !errors.Is(err, service.ErrVideoAttemptNotFound) {
		return nil, err
	}
	var now time.Time
	if err = tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return nil, err
	}
	if !now.Before(cmd.QuoteExpiresAt) {
		return nil, service.ErrVideoAttemptIneligible
	}
	// Match the existing ledger: subscription (if billed), then user, key,
	// account, group. Actual counters stay usage-only; holds survive resets.
	admission := &videoAdmissionWindow{quoteExpiresAt: cmd.QuoteExpiresAt}
	if err = lockVideoBillingRows(ctx, tx, a, admission); err != nil {
		return nil, err
	}
	if err = checkVideoReservationLimits(ctx, tx, a, admission.now); err != nil {
		return nil, err
	}
	if a.SubscriptionID == nil {
		res, e := tx.ExecContext(ctx, `UPDATE users SET balance=balance-$1::numeric,
			frozen_balance=frozen_balance+$1::numeric,updated_at=NOW()
			WHERE id=$2 AND deleted_at IS NULL AND balance >= $1::numeric`, a.TotalPrice, a.UserID)
		if e != nil {
			return nil, e
		}
		n, e := res.RowsAffected()
		if e != nil {
			return nil, e
		}
		if n != 1 {
			return nil, service.ErrVideoAttemptInsufficientFunds
		}
	}
	raw, err := json.Marshal(a.Selection.Controls)
	if err != nil {
		return nil, err
	}
	a, err = scanVideoAttempt(tx.QueryRowContext(ctx, `INSERT INTO video_attempts
		(id,quote_id,request_fingerprint,user_id,api_key_id,group_id,account_id,subscription_id,model,mode,controls,
		base_price,total_price,multiplier,account_cost,account_multiplier,tariff_revision,contract_revision,supplier_estimated)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)
		RETURNING `+videoAttemptColumns, a.ID, a.QuoteID, a.RequestFingerprint, a.UserID, a.APIKeyID, a.GroupID, a.AccountID, a.SubscriptionID,
		a.Selection.Model, a.Selection.Mode, string(raw), a.BasePrice, a.TotalPrice, a.Multiplier, a.AccountCost, a.AccountMultiplier,
		a.TariffRevision, a.ContractRevision, a.SupplierEstimated))
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return &service.VideoAttemptAcceptResult{Attempt: a, Created: true}, nil
}

type videoAdmissionWindow struct {
	quoteExpiresAt time.Time
	now            time.Time
}

func lockVideoBillingRows(ctx context.Context, tx *sql.Tx, a *service.VideoAttempt, admission *videoAdmissionWindow) error {
	// Settlement must remain possible after administrative disable or price
	// edits. Only a non-nil admission tests current eligibility and expiry.
	// Existing Apply updates subscription before user/key/account. Never hold
	// a key/account while waiting for that subscription in either video path.
	var observedSubType string
	var subStarts, subExpires time.Time
	if admission != nil {
		// Observe without a row lock to choose the funding-row lock order.
		// The locked group must still have this mode at final validation;
		// a concurrent mode change rejects rather than changing funding source.
		if err := tx.QueryRowContext(ctx, `SELECT subscription_type FROM groups WHERE id=$1`, a.GroupID).Scan(&observedSubType); err != nil {
			return service.ErrVideoAttemptIneligible
		}
		a.SubscriptionID = nil
		if observedSubType == service.SubscriptionTypeSubscription {
			var id int64
			if err := tx.QueryRowContext(ctx, `SELECT id,starts_at,expires_at FROM user_subscriptions
				WHERE user_id=$1 AND group_id=$2 AND deleted_at IS NULL AND status='active' FOR UPDATE`,
				a.UserID, a.GroupID).Scan(&id, &subStarts, &subExpires); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return service.ErrVideoAttemptIneligible
				}
				return err
			}
			a.SubscriptionID = &id
		}
	} else if a.SubscriptionID != nil {
		var id int64
		if err := tx.QueryRowContext(ctx, `SELECT id FROM user_subscriptions WHERE id=$1 FOR UPDATE`, *a.SubscriptionID).Scan(&id); err != nil {
			return err
		}
	}
	var userStatus, keyStatus, accountStatus, accountType, accountPlatform, groupStatus, platform, subType string
	var userDeleted, keyDeleted, accountDeleted, groupDeleted sql.NullTime
	var keyUser int64
	var keyGroup sql.NullInt64
	var keyExpires sql.NullTime
	var schedulable bool
	var accountMultiplier string
	err := tx.QueryRowContext(ctx, `SELECT status,deleted_at FROM users WHERE id=$1 FOR UPDATE`, a.UserID).Scan(&userStatus, &userDeleted)
	if err != nil {
		return service.ErrVideoAttemptIneligible
	}
	err = tx.QueryRowContext(ctx, `SELECT user_id,group_id,status,deleted_at,expires_at FROM api_keys WHERE id=$1 FOR UPDATE`, a.APIKeyID).
		Scan(&keyUser, &keyGroup, &keyStatus, &keyDeleted, &keyExpires)
	if err != nil {
		return service.ErrVideoAttemptIneligible
	}
	err = tx.QueryRowContext(ctx, `SELECT status,type,platform,deleted_at,schedulable,COALESCE(rate_multiplier,1)::text FROM accounts WHERE id=$1 FOR UPDATE`, a.AccountID).
		Scan(&accountStatus, &accountType, &accountPlatform, &accountDeleted, &schedulable, &accountMultiplier)
	if err != nil {
		return service.ErrVideoAttemptIneligible
	}
	err = tx.QueryRowContext(ctx, `SELECT status,platform,subscription_type,deleted_at FROM groups WHERE id=$1 FOR SHARE`, a.GroupID).
		Scan(&groupStatus, &platform, &subType, &groupDeleted)
	if err != nil {
		return service.ErrVideoAttemptIneligible
	}
	if admission != nil {
		var now time.Time
		if err = tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
			return err
		}
		admission.now = now
		if userStatus != service.StatusActive || userDeleted.Valid || keyStatus != service.StatusAPIKeyActive || keyDeleted.Valid ||
			keyUser != a.UserID || !keyGroup.Valid || keyGroup.Int64 != a.GroupID || (keyExpires.Valid && !now.Before(keyExpires.Time)) ||
			accountStatus != service.StatusActive || accountDeleted.Valid || !schedulable || accountType != service.AccountTypeAPIKey || accountPlatform != service.PlatformOpenAI ||
			groupStatus != service.StatusActive || groupDeleted.Valid || platform != service.PlatformOpenAI || subType != observedSubType ||
			!now.Before(admission.quoteExpiresAt) || (a.SubscriptionID != nil && (now.Before(subStarts) || !now.Before(subExpires))) {
			return service.ErrVideoAttemptIneligible
		}
		var member bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM account_groups WHERE account_id=$1 AND group_id=$2)`, a.AccountID, a.GroupID).Scan(&member); err != nil {
			return err
		}
		if !member {
			return service.ErrVideoAttemptIneligible
		}
		d, err := decimal.NewFromString(accountMultiplier)
		if err != nil || d.IsNegative() {
			return service.ErrVideoAttemptMoney
		}
		// Existing account quota semantics use pre-user/group retail cost times
		// the account multiplier. Supplier actual cost is never consulted.
		a.AccountCost = decimal.RequireFromString(a.BasePrice).Mul(d).Round(8).StringFixed(8)
		a.AccountMultiplier = d.String()
		if !service.ValidVideoLedgerMoney(a.AccountCost) {
			return service.ErrVideoAttemptMoney
		}
		if a.SubscriptionID == nil {
			// Any non-NULL limit, including explicit zero, is enforced by the
			// existing quota semantics. Unlimited/all-NULL and deleted rows do
			// not track usage. The legacy Redis absolute-snapshot flusher could
			// overwrite a direct exact DB capture: fail before reserving funds.
			var enforced bool
			err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM user_platform_quotas
				WHERE user_id=$1 AND platform='openai' AND deleted_at IS NULL
				AND (daily_limit_usd IS NOT NULL OR weekly_limit_usd IS NOT NULL OR monthly_limit_usd IS NOT NULL))`, a.UserID).Scan(&enforced)
			if err != nil {
				return err
			}
			if enforced {
				return service.ErrVideoAttemptPlatformQuota
			}
		}
	}
	return nil
}

func checkVideoReservationLimits(ctx context.Context, tx *sql.Tx, a *service.VideoAttempt, now time.Time) error {
	var limited bool
	// User and account concurrency are additional durable generation bounds;
	// Task 3 still acquires/releases the existing request/supplier slots.
	err := tx.QueryRowContext(ctx, `SELECT
		(u.concurrency>0 AND (SELECT count(*) FROM video_attempts WHERE user_id=u.id AND financial_state='reserved')>=u.concurrency)
		OR (ac.concurrency>0 AND (SELECT count(*) FROM video_attempts WHERE account_id=ac.id AND financial_state='reserved')>=ac.concurrency)
		FROM users u, accounts ac WHERE u.id=$1 AND ac.id=$2`, a.UserID, a.AccountID).Scan(&limited)
	if err != nil {
		return err
	}
	if limited {
		return service.ErrVideoAttemptLimit
	}
	err = tx.QueryRowContext(ctx, `WITH held AS (
		SELECT COALESCE(sum(total_price),0) AS n FROM video_attempts WHERE api_key_id=$1 AND financial_state='reserved'
	) SELECT (quota>0 AND quota_used+held.n+$2::numeric>quota)
		OR (rate_limit_5h>0 AND (CASE WHEN window_5h_start IS NOT NULL AND window_5h_start+interval '5 hours'<=NOW() THEN 0 ELSE usage_5h END)+held.n+$2::numeric>rate_limit_5h)
		OR (rate_limit_1d>0 AND (CASE WHEN window_1d_start IS NOT NULL AND window_1d_start+interval '24 hours'<=NOW() THEN 0 ELSE usage_1d END)+held.n+$2::numeric>rate_limit_1d)
		OR (rate_limit_7d>0 AND (CASE WHEN window_7d_start IS NOT NULL AND window_7d_start+interval '7 days'<=NOW() THEN 0 ELSE usage_7d END)+held.n+$2::numeric>rate_limit_7d)
		FROM api_keys,held WHERE id=$1`, a.APIKeyID, a.TotalPrice).Scan(&limited)
	if err != nil {
		return err
	}
	if limited {
		return service.ErrVideoAttemptLimit
	}
	err = tx.QueryRowContext(ctx, `WITH held AS (
		SELECT COALESCE(sum(account_cost),0) AS n FROM video_attempts WHERE account_id=$1 AND financial_state='reserved'
	) SELECT
		(COALESCE((extra->>'quota_limit')::numeric,0)>0 AND COALESCE((extra->>'quota_used')::numeric,0)+held.n+$2::numeric>(extra->>'quota_limit')::numeric)
		OR (COALESCE((extra->>'quota_daily_limit')::numeric,0)>0 AND (CASE WHEN `+dailyExpiredExpr+` THEN 0 ELSE COALESCE((extra->>'quota_daily_used')::numeric,0) END)+held.n+$2::numeric>(extra->>'quota_daily_limit')::numeric)
		OR (COALESCE((extra->>'quota_weekly_limit')::numeric,0)>0 AND (CASE WHEN `+weeklyExpiredExpr+` THEN 0 ELSE COALESCE((extra->>'quota_weekly_used')::numeric,0) END)+held.n+$2::numeric>(extra->>'quota_weekly_limit')::numeric)
		FROM accounts,held WHERE id=$1`, a.AccountID, a.AccountCost).Scan(&limited)
	if err != nil {
		return err
	}
	if limited {
		return service.ErrVideoAttemptLimit
	}
	if a.SubscriptionID == nil {
		return nil
	}
	if err = resetVideoSubscriptionWindows(ctx, tx, *a.SubscriptionID, now); err != nil {
		return err
	}
	err = tx.QueryRowContext(ctx, `WITH held AS (
		SELECT COALESCE(sum(total_price),0) AS n FROM video_attempts WHERE subscription_id=$1 AND financial_state='reserved'
	) SELECT (COALESCE(g.daily_limit_usd,0)>0 AND us.daily_usage_usd+held.n+$2::numeric>g.daily_limit_usd)
		OR (COALESCE(g.weekly_limit_usd,0)>0 AND us.weekly_usage_usd+held.n+$2::numeric>g.weekly_limit_usd)
		OR (COALESCE(g.monthly_limit_usd,0)>0 AND us.monthly_usage_usd+held.n+$2::numeric>g.monthly_limit_usd)
		OR us.daily_usage_usd+held.n+$2::numeric>=10000000000
		OR us.weekly_usage_usd+held.n+$2::numeric>=10000000000
		OR us.monthly_usage_usd+held.n+$2::numeric>=10000000000
		FROM user_subscriptions us JOIN groups g ON us.group_id=g.id CROSS JOIN held WHERE us.id=$1`,
		*a.SubscriptionID, a.TotalPrice).Scan(&limited)
	if err != nil {
		return err
	}
	if limited {
		return service.ErrVideoAttemptLimit
	}
	return nil
}

func resetVideoSubscriptionWindows(ctx context.Context, tx *sql.Tx, id int64, now time.Time) error {
	var sub service.UserSubscription
	var daily, weekly, monthly sql.NullTime
	err := tx.QueryRowContext(ctx, `SELECT starts_at,expires_at,daily_window_start,weekly_window_start,monthly_window_start
		FROM user_subscriptions WHERE id=$1`, id).Scan(&sub.StartsAt, &sub.ExpiresAt, &daily, &weekly, &monthly)
	if err != nil {
		return err
	}
	if daily.Valid {
		sub.DailyWindowStart = &daily.Time
	}
	if weekly.Valid {
		sub.WeeklyWindowStart = &weekly.Time
	}
	if monthly.Valid {
		sub.MonthlyWindowStart = &monthly.Time
	}
	d, w, m := service.VideoSubscriptionWindowStarts(&sub, now)
	for i, name := range []string{"daily", "weekly", "monthly"} {
		next := []*time.Time{d, w, m}[i]
		if next == nil {
			continue
		}
		_, err = tx.ExecContext(ctx, `UPDATE user_subscriptions SET `+name+`_usage_usd=0,`+name+`_window_start=$1,updated_at=NOW() WHERE id=$2`, *next, id)
		if err != nil {
			return err
		}
	}
	return nil
}

func (r *usageBillingRepository) UpdateVideoAttempt(ctx context.Context, owner service.VideoAttemptOwner, id string, update service.VideoAttemptUpdate) (*service.VideoAttempt, error) {
	tx, err := videoBegin(ctx, r)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	a, err := videoOwnedAttempt(ctx, tx, owner, id)
	if err != nil {
		return nil, err
	}
	if a.FinancialState != service.VideoFinancialReserved {
		return nil, service.ErrVideoAttemptTransition
	}
	legal := (a.State == service.VideoAttemptAccepted && update.State == service.VideoAttemptSubmitting) ||
		(a.State == service.VideoAttemptSubmitting && (update.State == service.VideoAttemptUncertain || update.State == service.VideoAttemptProcessing)) ||
		(a.State == service.VideoAttemptUncertain && update.State == service.VideoAttemptProcessing) ||
		(a.State == service.VideoAttemptProcessing && update.State == service.VideoAttemptProcessing)
	taskID := strings.TrimSpace(update.UpstreamTaskID)
	if !legal || len(taskID) > 255 || (a.UpstreamTaskID != "" && taskID != "" && a.UpstreamTaskID != taskID) ||
		(update.State == service.VideoAttemptProcessing && a.UpstreamTaskID == "" && taskID == "") ||
		(update.State != service.VideoAttemptProcessing && taskID != "") {
		return nil, service.ErrVideoAttemptTransition
	}
	if taskID == "" {
		taskID = a.UpstreamTaskID
	}
	a, err = scanVideoAttempt(tx.QueryRowContext(ctx, `UPDATE video_attempts SET state=$1,upstream_task_id=NULLIF($2,''),updated_at=NOW()
		WHERE id=$3 RETURNING `+videoAttemptColumns, update.State, taskID, id))
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return a, nil
}

func (r *usageBillingRepository) ListReconcilableVideoAttempts(ctx context.Context, before time.Time, limit int) ([]*service.VideoAttempt, error) {
	return r.listVideoAttempts(ctx, before, limit, false)
}

// Filter before LIMIT so old unknown acceptances cannot starve known-ID work.
func (r *usageBillingRepository) ListKnownTaskVideoAttempts(ctx context.Context, before time.Time, limit int) ([]*service.VideoAttempt, error) {
	return r.listVideoAttempts(ctx, before, limit, true)
}

func (r *usageBillingRepository) listVideoAttempts(ctx context.Context, before time.Time, limit int, knownOnly bool) ([]*service.VideoAttempt, error) {
	if limit < 1 || limit > 1000 {
		return nil, service.ErrVideoAttemptIneligible
	}
	filter := ""
	if knownOnly {
		filter = " AND upstream_task_id IS NOT NULL AND state IN ('processing','uncertain')"
	}
	rows, err := r.db.QueryContext(ctx, `SELECT `+videoAttemptColumns+` FROM video_attempts
		WHERE financial_state='reserved' AND updated_at<=$1 AND (lease_expires_at IS NULL OR lease_expires_at<=NOW())
		`+filter+` ORDER BY updated_at,id LIMIT $2`, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*service.VideoAttempt
	for rows.Next() {
		a, e := scanVideoAttempt(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (r *usageBillingRepository) AcquireVideoAttemptLease(ctx context.Context, id, owner string, ttl time.Duration) (*service.VideoAttemptLease, error) {
	if owner == "" || len(owner) > 128 || ttl <= 0 || ttl > 5*time.Minute {
		return nil, service.ErrVideoAttemptLease
	}
	l := &service.VideoAttemptLease{AttemptID: id, Owner: owner, Token: uuid.NewString()}
	err := r.db.QueryRowContext(ctx, `UPDATE video_attempts SET lease_owner=$1,lease_token=$2,
		lease_expires_at=NOW()+($3::bigint * interval '1 millisecond')
		WHERE id=$4 AND financial_state='reserved' AND (lease_expires_at IS NULL OR lease_expires_at<=NOW())
		RETURNING lease_expires_at`, owner, l.Token, ttl.Milliseconds(), id).Scan(&l.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, service.ErrVideoAttemptLease
	}
	return l, err
}

func (r *usageBillingRepository) ReleaseVideoAttemptLease(ctx context.Context, lease *service.VideoAttemptLease) error {
	if lease == nil {
		return service.ErrVideoAttemptLease
	}
	res, err := r.db.ExecContext(ctx, `UPDATE video_attempts SET lease_owner=NULL,lease_token=NULL,lease_expires_at=NULL
		WHERE id=$1 AND lease_owner=$2 AND lease_token=$3`, lease.AttemptID, lease.Owner, lease.Token)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err == nil && n == 0 {
		return service.ErrVideoAttemptLease
	}
	return err
}

func (r *usageBillingRepository) CaptureVideoAttempt(ctx context.Context, owner service.VideoAttemptOwner, id string, urls []string) (*service.VideoAttemptSettlementResult, error) {
	return r.settleVideoAttempt(ctx, owner, id, urls, true)
}

func (r *usageBillingRepository) ReleaseVideoAttempt(ctx context.Context, owner service.VideoAttemptOwner, id string) (*service.VideoAttemptSettlementResult, error) {
	return r.settleVideoAttempt(ctx, owner, id, nil, false)
}

func (r *usageBillingRepository) settleVideoAttempt(ctx context.Context, owner service.VideoAttemptOwner, id string, urls []string, capture bool) (*service.VideoAttemptSettlementResult, error) {
	tx, err := videoBegin(ctx, r)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	a, err := videoOwnedAttempt(ctx, tx, owner, id)
	if err != nil {
		return nil, err
	}
	wanted := service.VideoFinancialReleased
	if capture {
		wanted = service.VideoFinancialCaptured
	}
	if a.FinancialState == wanted {
		return &service.VideoAttemptSettlementResult{Attempt: a}, nil
	}
	if a.FinancialState != service.VideoFinancialReserved || a.State == service.VideoAttemptUncertain ||
		(capture && a.State != service.VideoAttemptProcessing) {
		return nil, service.ErrVideoAttemptTransition
	}
	if capture {
		if len(urls) == 0 || len(urls) > 16 {
			return nil, service.ErrVideoAttemptTransition
		}
		for _, raw := range urls {
			u, e := url.Parse(raw)
			if e != nil || len(raw) > 8192 || u.Host == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") {
				return nil, service.ErrVideoAttemptTransition
			}
		}
	}
	if err = lockVideoBillingRows(ctx, tx, a, nil); err != nil {
		return nil, err
	}
	if capture {
		// The single shared charge claim is in the same transaction as every
		// counter, frozen-fund capture, usage log and terminal receipt.
		applied, e := r.claimUsageBillingRequest(ctx, tx, a.RequestID(), a.APIKeyID, a.RequestFingerprint)
		if e != nil {
			return nil, e
		}
		if !applied {
			// A reserved attempt cannot truthfully invent a receipt from a
			// dedup row without its atomic receipt; fail closed for repair.
			return nil, service.ErrVideoAttemptConflict
		}
		if a.SubscriptionID != nil {
			var now time.Time
			if err = tx.QueryRowContext(ctx, `SELECT NOW()`).Scan(&now); err != nil {
				return nil, err
			}
			if err = resetVideoSubscriptionWindows(ctx, tx, *a.SubscriptionID, now); err != nil {
				return nil, err
			}
			if err = incrementUsageBillingSubscription(ctx, tx, *a.SubscriptionID, a.TotalPrice); err != nil {
				return nil, err
			}
		}
		if err = captureVideoQuotaUsage(ctx, tx, a); err != nil {
			return nil, err
		}
	}
	if a.SubscriptionID == nil {
		refund := "0.00000000"
		if !capture {
			refund = a.TotalPrice
		}
		res, e := tx.ExecContext(ctx, `UPDATE users SET balance=balance+$1::numeric,frozen_balance=frozen_balance-$2::numeric,updated_at=NOW()
			WHERE id=$3 AND frozen_balance >= $2::numeric`, refund, a.TotalPrice, a.UserID)
		if e != nil {
			return nil, e
		}
		n, e := res.RowsAffected()
		if e != nil || n != 1 {
			return nil, fmt.Errorf("video reserved frozen funds unavailable: %w", service.ErrVideoAttemptConflict)
		}
	}
	if capture {
		var logID int64
		billingType := service.BillingTypeBalance
		if a.SubscriptionID != nil {
			billingType = service.BillingTypeSubscription
		}
		duration, durationErr := decimal.NewFromString(fmt.Sprint(a.Selection.Controls["duration"]))
		if durationErr != nil {
			return nil, durationErr
		}
		var resolution any
		if value, ok := a.Selection.Controls["resolution"].(string); ok && len(value) <= 10 {
			resolution = value
		}
		err = tx.QueryRowContext(ctx, `INSERT INTO usage_logs(user_id,api_key_id,account_id,group_id,subscription_id,request_id,
			model,requested_model,upstream_model,billing_mode,billing_type,total_cost,actual_cost,rate_multiplier,account_rate_multiplier,video_count,video_duration_seconds,video_resolution)
			VALUES($1,$2,$3,$4,$5,$6,$7,$7,$7,'video',$8,$9,$10,$11,$12,1,$13,$14) RETURNING id`,
			a.UserID, a.APIKeyID, a.AccountID, a.GroupID, a.SubscriptionID, a.RequestID(), a.Selection.Model,
			billingType, a.BasePrice, a.TotalPrice, a.Multiplier, a.AccountMultiplier, duration.IntPart(), resolution).Scan(&logID)
		if err != nil {
			return nil, err
		}
		// Existing reconciliation ledger is keyed by the same usage log.
		if _, err = tx.ExecContext(ctx, `INSERT INTO billing_usage_entries(usage_log_id,user_id,api_key_id,subscription_id,billing_type,delta_usd)
			VALUES($1,$2,$3,$4,$5,$6)`, logID, a.UserID, a.APIKeyID, a.SubscriptionID, billingType, a.TotalPrice); err != nil {
			return nil, err
		}
		raw, e := json.Marshal(urls)
		if e != nil {
			return nil, e
		}
		a, err = scanVideoAttempt(tx.QueryRowContext(ctx, `UPDATE video_attempts SET state='completed',financial_state='captured',
			result_urls=$1,receipt_usage_log_id=$2,receipt_captured_at=NOW(),lease_owner=NULL,lease_token=NULL,lease_expires_at=NULL,updated_at=NOW()
			WHERE id=$3 RETURNING `+videoAttemptColumns, string(raw), logID, id))
	} else {
		a, err = scanVideoAttempt(tx.QueryRowContext(ctx, `UPDATE video_attempts SET state='failed',financial_state='released',
			lease_owner=NULL,lease_token=NULL,lease_expires_at=NULL,updated_at=NOW() WHERE id=$1 RETURNING `+videoAttemptColumns, id))
	}
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return &service.VideoAttemptSettlementResult{Attempt: a, Applied: true}, nil
}

func captureVideoQuotaUsage(ctx context.Context, tx *sql.Tx, a *service.VideoAttempt) error {
	// Match the existing billing command's shouldDeductAPIKeyQuota,
	// shouldUpdateRateLimits and shouldUpdateAccountQuota guards. Unlimited
	// keys/accounts do not acquire synthetic usage counters.
	var keyQuota, keyRates, accountQuota bool
	err := tx.QueryRowContext(ctx, `SELECT
		k.deleted_at IS NULL AND k.quota>0,
		k.deleted_at IS NULL AND (k.rate_limit_5h>0 OR k.rate_limit_1d>0 OR k.rate_limit_7d>0),
		COALESCE((a.extra->>'quota_limit')::numeric,0)>0 OR COALESCE((a.extra->>'quota_daily_limit')::numeric,0)>0 OR COALESCE((a.extra->>'quota_weekly_limit')::numeric,0)>0
		FROM api_keys k,accounts a WHERE k.id=$1 AND a.id=$2`, a.APIKeyID, a.AccountID).Scan(&keyQuota, &keyRates, &accountQuota)
	if err != nil {
		return err
	}
	if a.TotalPrice != "0.00000000" {
		if keyQuota {
			if _, err = incrementUsageBillingAPIKeyQuota(ctx, tx, a.APIKeyID, a.TotalPrice); err != nil {
				return err
			}
		}
		if keyRates {
			if err = incrementUsageBillingAPIKeyRateLimit(ctx, tx, a.APIKeyID, a.TotalPrice); err != nil {
				return err
			}
		}
	}
	if accountQuota && a.AccountCost != "0.00000000" {
		_, err = incrementUsageBillingAccountQuotaAmount(ctx, tx, a.AccountID, a.AccountCost)
	}
	return err
}
