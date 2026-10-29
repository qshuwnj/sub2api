package service

import (
	"context"
	"math"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/google/uuid"
)

// Unlike legacy estimates, a confirmed fixed price must never fail open or be capped.
func (s *BillingCacheService) ReserveQuotedImageCharge(ctx context.Context, user *User, key *APIKey, group *Group, subscription *UserSubscription, amount float64) (*InflightReservation, error) {
	if s == nil || user == nil || math.IsNaN(amount) || math.IsInf(amount, 0) || amount < 0 ||
		s.cfg == nil || s.cfg.RunMode == config.RunModeSimple {
		return nil, ErrBillingServiceUnavailable
	}
	if amount == 0 {
		return nil, nil
	}
	if !s.InflightReservationEnabled() {
		return nil, ErrBillingServiceUnavailable
	}
	balanceCache, balanceOK := s.cache.(InflightBalanceReservationCache)
	balanceRegistrar, registrarOK := s.cache.(InflightBalanceReservationRegistrar)
	scopeCache, scopeOK := s.cache.(InflightScopeReservationCache)
	subscriptionBill := group != nil && group.IsSubscriptionType() && subscription != nil
	if (!subscriptionBill && (!balanceOK || !registrarOK || s.userRepo == nil)) || !scopeOK {
		return nil, ErrBillingServiceUnavailable
	}
	r := &InflightReservation{userID: user.ID, requestID: uuid.NewString(), amount: amount,
		ttl: defaultInflightReservationTTL, scopeCache: scopeCache}
	r.refs.Store(1)
	succeeded := false
	defer func() {
		if !succeeded {
			r.Release()
		}
	}()
	reserveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if !subscriptionBill {
		r.cache = balanceCache
		pending, err := balanceRegistrar.RegisterInflightBalance(reserveCtx, user.ID, r.requestID, amount, r.ttl)
		if err != nil {
			return nil, ErrBillingServiceUnavailable
		}
		// The fixed-price gate must not trust stale Redis data or an async cache
		// refill. Read the ledger-backed balance only after registering the hold.
		balance, err := s.getUserBalanceFromDB(reserveCtx, user.ID)
		if err != nil {
			return nil, ErrBillingServiceUnavailable
		}
		if math.IsNaN(balance) || math.IsInf(balance, 0) || balance-pending < amount {
			return nil, ErrInsufficientBalance
		}
	}
	reserveScope := func(scope string) (float64, error) {
		// Register before reading authoritative usage, preserving concurrent headroom.
		r.scopes = append(r.scopes, scope)
		pending, err := scopeCache.ReserveInflightScope(reserveCtx, scope, r.requestID, amount, r.ttl)
		if err != nil {
			return 0, ErrBillingServiceUnavailable
		}
		return pending + amount, nil
	}
	if subscriptionBill {
		pending, err := reserveScope(inflightSubscriptionScope(subscription.ID))
		if err != nil {
			return nil, err
		}
		if err := s.checkFixedChargeSubscription(reserveCtx, user.ID, group, pending); err != nil {
			return nil, err
		}
	}
	if key != nil && (key.Quota > 0 || key.HasRateLimits()) {
		pending, err := reserveScope(inflightAPIKeyScope(key.ID))
		if err != nil {
			return nil, err
		}
		if err := s.checkFixedChargeAPIKey(reserveCtx, key, pending); err != nil {
			return nil, err
		}
	}
	if renewer, ok := s.cache.(InflightBalanceReservationRenewer); !subscriptionBill && ok {
		r.startRenewal(renewer)
	}
	succeeded = true
	return r, nil
}
