package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

type videoBudgetAccountRepo struct {
	AccountRepository
	err error
}

func (r videoBudgetAccountRepo) GetByID(context.Context, int64) (*Account, error) {
	return &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true}, nil
}

func (r videoBudgetAccountRepo) CheckVideoReservationHeadroom(_ context.Context, check VideoReservationBudgetCheck) error {
	if check.AccountID != 42 {
		return errors.New("wrong account budget scope")
	}
	return r.err
}

func TestVideoBudgetFreshAccountTypeCannotBypassHeldHeadroom(t *testing.T) {
	stale := &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true}
	s := &OpenAIGatewayService{accountRepo: videoBudgetAccountRepo{err: ErrVideoAttemptLimit}, schedulerSnapshot: &SchedulerSnapshotService{}}
	require.Nil(t, s.recheckSelectedOpenAIAccountFromDBBeforeProfit(context.Background(), stale, nil, PlatformOpenAI, "", false, ""))
}

func TestVideoBudgetOpenAISchedulingRejectsHeldOrUnreadableAccount(t *testing.T) {
	account := &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true}
	for _, failure := range []error{ErrVideoAttemptLimit, errors.New("database unavailable")} {
		s := &OpenAIGatewayService{accountRepo: videoBudgetAccountRepo{err: failure}}
		require.Nil(t, s.recheckSelectedOpenAIAccountFromDBBeforeProfit(context.Background(), account, nil, PlatformOpenAI, "", false, ""))
		generic := &GatewayService{accountRepo: videoBudgetAccountRepo{err: failure}}
		require.False(t, generic.isAccountSchedulableForModelSelection(context.Background(), account, ""))
	}
	s := &OpenAIGatewayService{accountRepo: videoBudgetAccountRepo{}}
	require.Same(t, account, s.recheckSelectedOpenAIAccountFromDBBeforeProfit(context.Background(), account, nil, PlatformOpenAI, "", false, ""))
	legacy := &OpenAIGatewayService{}
	require.Same(t, account, legacy.recheckSelectedOpenAIAccountFromDBBeforeProfit(context.Background(), account, nil, PlatformOpenAI, "", false, ""))
	require.True(t, (&GatewayService{}).isAccountSchedulableForModelSelection(context.Background(), account, ""))
}

func TestVideoBudgetLegacyCapabilityAbsentDoesNotChangeAdmission(t *testing.T) {
	s := &BillingCacheService{}
	require.NoError(t, s.checkVideoReservedHeadroom(context.Background(), &APIKey{ID: 1, UserID: 2}, nil, nil, 0, 0, false))
}
