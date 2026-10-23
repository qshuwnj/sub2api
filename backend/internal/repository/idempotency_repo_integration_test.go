//go:build integration

package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// hashedTestValue returns a unique SHA-256 hex string (64 chars) that fits VARCHAR(64) columns.
func hashedTestValue(t *testing.T, prefix string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(uniqueTestValue(t, prefix)))
	return hex.EncodeToString(sum[:])
}

func TestIdempotencyRepo_CreateProcessing_CompeteSameKey(t *testing.T) {
	tx := testTx(t)
	repo := &idempotencyRepository{sql: tx}
	ctx := context.Background()

	now := time.Now().UTC()
	acceptedTariff := `{"accepted":{"unit":"0.06000000"},"child_index":0}`
	record := &service.IdempotencyRecord{
		Scope:              uniqueTestValue(t, "idem-scope-create"),
		IdempotencyKeyHash: hashedTestValue(t, "idem-hash"),
		RequestFingerprint: hashedTestValue(t, "idem-fp"),
		Status:             service.IdempotencyStatusProcessing,
		LockedUntil:        ptrTime(now.Add(30 * time.Second)),
		ExpiresAt:          now.Add(24 * time.Hour),
		ResponseBody:       &acceptedTariff,
	}
	owner, err := repo.CreateProcessing(ctx, record)
	require.NoError(t, err)
	require.True(t, owner)
	require.NotZero(t, record.ID)
	stored, err := repo.GetByScopeAndKeyHash(ctx, record.Scope, record.IdempotencyKeyHash)
	require.NoError(t, err)
	require.NotNil(t, stored.ResponseBody)
	require.Equal(t, acceptedTariff, *stored.ResponseBody)

	duplicate := &service.IdempotencyRecord{
		Scope:              record.Scope,
		IdempotencyKeyHash: record.IdempotencyKeyHash,
		RequestFingerprint: hashedTestValue(t, "idem-fp-other"),
		Status:             service.IdempotencyStatusProcessing,
		LockedUntil:        ptrTime(now.Add(30 * time.Second)),
		ExpiresAt:          now.Add(24 * time.Hour),
	}
	owner, err = repo.CreateProcessing(ctx, duplicate)
	require.NoError(t, err)
	require.False(t, owner, "same scope+key hash should be de-duplicated")
}

func TestIdempotencyRepo_ImageQualityAuditRetainsProcessingLock(t *testing.T) {
	repo := &idempotencyRepository{sql: testTx(t)}
	ctx := context.Background()
	expiry := time.Now().UTC().Add(24 * time.Hour)
	record := &service.IdempotencyRecord{
		Scope: "image-quality-quote-v1", IdempotencyKeyHash: hashedTestValue(t, "image-audit"),
		RequestFingerprint: hashedTestValue(t, "image-body"), Status: service.IdempotencyStatusProcessing,
		LockedUntil: &expiry, ExpiresAt: expiry,
	}
	owner, err := repo.CreateProcessing(ctx, record)
	require.NoError(t, err)
	require.True(t, owner)
	body := `{"outcome":{"status":"pending","upstream_request_id":"synthetic-prediction"}}`
	require.NoError(t, repo.UpdateImageQualityClaim(ctx, record.ID, body, expiry))
	stored, err := repo.GetByScopeAndKeyHash(ctx, record.Scope, record.IdempotencyKeyHash)
	require.NoError(t, err)
	require.Equal(t, service.IdempotencyStatusProcessing, stored.Status)
	require.NotNil(t, stored.LockedUntil)
	require.Equal(t, body, *stored.ResponseBody)
	require.NoError(t, repo.MarkSucceeded(ctx, record.ID, 200, `{"status":"settled"}`, expiry))
	require.Error(t, repo.UpdateImageQualityClaim(ctx, record.ID, body, expiry), "never overwrite a terminal receipt with an intermediate state")
	require.Error(t, repo.UpdateImageQualityClaim(ctx, -1, body, expiry))
}

func TestIdempotencyRepo_TryReclaim_StatusAndLockWindow(t *testing.T) {
	tx := testTx(t)
	repo := &idempotencyRepository{sql: tx}
	ctx := context.Background()

	now := time.Now().UTC()
	record := &service.IdempotencyRecord{
		Scope:              uniqueTestValue(t, "idem-scope-reclaim"),
		IdempotencyKeyHash: hashedTestValue(t, "idem-hash-reclaim"),
		RequestFingerprint: hashedTestValue(t, "idem-fp-reclaim"),
		Status:             service.IdempotencyStatusProcessing,
		LockedUntil:        ptrTime(now.Add(10 * time.Second)),
		ExpiresAt:          now.Add(24 * time.Hour),
	}
	owner, err := repo.CreateProcessing(ctx, record)
	require.NoError(t, err)
	require.True(t, owner)

	require.NoError(t, repo.MarkFailedRetryable(
		ctx,
		record.ID,
		"RETRYABLE_FAILURE",
		now.Add(-2*time.Second),
		now.Add(24*time.Hour),
	))

	newLockedUntil := now.Add(20 * time.Second)
	reclaimed, err := repo.TryReclaim(
		ctx,
		record.ID,
		service.IdempotencyStatusFailedRetryable,
		now,
		newLockedUntil,
		now.Add(24*time.Hour),
	)
	require.NoError(t, err)
	require.True(t, reclaimed, "failed_retryable + expired lock should allow reclaim")

	got, err := repo.GetByScopeAndKeyHash(ctx, record.Scope, record.IdempotencyKeyHash)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, service.IdempotencyStatusProcessing, got.Status)
	require.NotNil(t, got.LockedUntil)
	require.True(t, got.LockedUntil.After(now))

	require.NoError(t, repo.MarkFailedRetryable(
		ctx,
		record.ID,
		"RETRYABLE_FAILURE",
		now.Add(20*time.Second),
		now.Add(24*time.Hour),
	))

	reclaimed, err = repo.TryReclaim(
		ctx,
		record.ID,
		service.IdempotencyStatusFailedRetryable,
		now,
		now.Add(40*time.Second),
		now.Add(24*time.Hour),
	)
	require.NoError(t, err)
	require.False(t, reclaimed, "within lock window should not reclaim")
}

func TestIdempotencyRepo_StatusTransition_ToSucceeded(t *testing.T) {
	tx := testTx(t)
	repo := &idempotencyRepository{sql: tx}
	ctx := context.Background()

	now := time.Now().UTC()
	record := &service.IdempotencyRecord{
		Scope:              uniqueTestValue(t, "idem-scope-success"),
		IdempotencyKeyHash: hashedTestValue(t, "idem-hash-success"),
		RequestFingerprint: hashedTestValue(t, "idem-fp-success"),
		Status:             service.IdempotencyStatusProcessing,
		LockedUntil:        ptrTime(now.Add(10 * time.Second)),
		ExpiresAt:          now.Add(24 * time.Hour),
	}
	owner, err := repo.CreateProcessing(ctx, record)
	require.NoError(t, err)
	require.True(t, owner)

	require.NoError(t, repo.MarkSucceeded(ctx, record.ID, 200, `{"ok":true}`, now.Add(24*time.Hour)))

	got, err := repo.GetByScopeAndKeyHash(ctx, record.Scope, record.IdempotencyKeyHash)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, service.IdempotencyStatusSucceeded, got.Status)
	require.NotNil(t, got.ResponseStatus)
	require.Equal(t, 200, *got.ResponseStatus)
	require.NotNil(t, got.ResponseBody)
	require.Equal(t, `{"ok":true}`, *got.ResponseBody)
	require.Nil(t, got.LockedUntil)
}
