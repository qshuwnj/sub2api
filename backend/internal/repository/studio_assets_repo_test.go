package repository

import (
	"context"
	"database/sql/driver"
	"encoding/base64"
	"encoding/json"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

const studioTestID = "asset_0123456789abcdef0123456789abcdef"

var studioTestTime = time.Date(2026, 10, 31, 12, 0, 0, 123456000, time.UTC)

type studioGeneratedIDArgument struct{}

func (studioGeneratedIDArgument) Match(value driver.Value) bool {
	id, ok := value.(string)
	return ok && regexp.MustCompile(`^asset_[0-9a-f]{32}$`).MatchString(id)
}

type studioInlineBytesArgument struct{ size int }

func (a studioInlineBytesArgument) Match(value driver.Value) bool {
	data, ok := value.([]byte)
	return ok && len(data) == a.size && data[0] == 7
}

func studioRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"id", "user_id", "api_key_id", "kind", "generation_id", "output_index", "video_resource", "model", "prompt", "mime", "storage_key", "state", "last_error_code", "source_url", "source_bytes", "metadata", "size_bytes", "generated_at", "created_at", "updated_at", "deleted_at", "lease_token"})
}

func studioValues(id string) []driver.Value {
	return []driver.Value{id, int64(16), int64(20), "image", "generation-1", 0, "", "original-model", "original prompt", "image/png", "", "pending", "", "https://example.test/private.png", []byte{1, 2}, []byte(`{"aspect_ratio":"3:4"}`), int64(0), studioTestTime, studioTestTime, studioTestTime, nil, ""}
}

func newStudioRepo(t *testing.T) (*usageBillingRepository, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, mock.ExpectationsWereMet()); _ = db.Close() })
	return &usageBillingRepository{db: db}, mock
}

func studioFixture() *service.StudioAsset {
	return &service.StudioAsset{Owner: service.StudioAssetOwner{UserID: 16, APIKeyID: 20}, Kind: "image", GenerationID: "generation-1", Model: "original-model", Prompt: "original prompt", MIME: "image/png", SourceURL: "https://example.test/private.png", Metadata: json.RawMessage(`{"aspect_ratio":"3:4"}`), GeneratedAt: studioTestTime}
}

// A stale upload may only attach cleanup work to a tombstone, never overwrite a live worker's object.
func TestStudioAssetLateUploadCleanupRequiresOwnedTombstone(t *testing.T) {
	r, mock := newStudioRepo(t)
	a := studioFixture()
	a.ID, a.StorageKey = studioTestID, "studio-results/16/20/digest/0"
	mock.ExpectExec(`(?s)UPDATE studio_assets SET storage_key = \$4, lease_token = '', lease_expires_at = NULL, next_retry_at = NOW\(\), updated_at = NOW\(\).*WHERE user_id = \$1 AND api_key_id = \$2 AND id = \$3 AND deleted_at IS NOT NULL AND \(storage_key = '' OR storage_key = \$4\)`).
		WithArgs(int64(16), int64(20), studioTestID, a.StorageKey).WillReturnResult(sqlmock.NewResult(0, 0))
	require.NoError(t, r.RecordStudioAssetCleanup(context.Background(), a))
}

func TestStudioAssetLateUploadInvalidatesEarlierCleanupCompletion(t *testing.T) {
	r, mock := newStudioRepo(t)
	a := studioFixture()
	a.ID, a.StorageKey, a.LeaseToken = studioTestID, "studio-results/16/20/digest/0", "earlier-cleanup"
	mock.ExpectExec(`(?s)SET storage_key = \$4, lease_token = '', lease_expires_at = NULL.*deleted_at IS NOT NULL`).
		WithArgs(int64(16), int64(20), studioTestID, a.StorageKey).WillReturnResult(sqlmock.NewResult(0, 1))
	require.NoError(t, r.RecordStudioAssetCleanup(context.Background(), a))
	mock.ExpectExec(`(?s)WHERE user_id = \$1 AND api_key_id = \$2 AND id = \$3 AND lease_token = \$4 AND lease_expires_at > NOW\(\) AND deleted_at IS NOT NULL AND storage_key = \$5`).
		WithArgs(int64(16), int64(20), studioTestID, "earlier-cleanup", a.StorageKey).WillReturnResult(sqlmock.NewResult(0, 0))
	require.ErrorIs(t, r.CompleteStudioAssetCleanup(context.Background(), a), service.ErrStudioAssetUnavailable)
}

func TestStudioAssetLateUploadCleanupRejectsSharedAndForeignKeys(t *testing.T) {
	r, _ := newStudioRepo(t)
	for _, key := range []string{"video-results/16/20/shared.mp4", "studio-results/16/21/digest/0", "studio-results/16/20/../shared"} {
		a := studioFixture()
		a.ID, a.StorageKey = studioTestID, key
		require.ErrorIs(t, r.RecordStudioAssetCleanup(context.Background(), a), service.ErrStudioAssetInvalidRequest)
	}
}

// Removing either owner predicate must fail these boundary-contract tests.
func TestStudioAssetLookupRequiresBothOwnerIDs(t *testing.T) {
	r, mock := newStudioRepo(t)
	mock.ExpectQuery(`user_id = \$1 AND api_key_id = \$2 AND id = \$3 AND deleted_at IS NULL`).
		WithArgs(int64(16), int64(20), studioTestID).WillReturnRows(studioRows())
	_, err := r.GetStudioAsset(context.Background(), service.StudioAssetOwner{UserID: 16, APIKeyID: 20}, studioTestID)
	require.ErrorIs(t, err, service.ErrStudioAssetNotFound)
}

func TestStudioAssetLookupScansPrivateDurableSource(t *testing.T) {
	r, mock := newStudioRepo(t)
	mock.ExpectQuery(`FROM studio_assets WHERE user_id = \$1 AND api_key_id = \$2 AND id = \$3`).
		WithArgs(int64(16), int64(20), studioTestID).WillReturnRows(studioRows().AddRow(studioValues(studioTestID)...))
	a, err := r.GetStudioAsset(context.Background(), service.StudioAssetOwner{UserID: 16, APIKeyID: 20}, studioTestID)
	require.NoError(t, err)
	require.Equal(t, []byte{1, 2}, a.SourceBytes)
	require.JSONEq(t, `{"aspect_ratio":"3:4"}`, string(a.Metadata))
	require.Equal(t, studioTestTime, a.GeneratedAt)
}

func TestStudioAssetPaginationUsesOwnerAndStableTimeTie(t *testing.T) {
	r, mock := newStudioRepo(t)
	ids := []string{"asset_00000000000000000000000000000003", "asset_00000000000000000000000000000002", "asset_00000000000000000000000000000001"}
	rows := studioRows()
	for _, id := range ids {
		rows.AddRow(studioValues(id)...)
	}
	mock.ExpectQuery(`(?s)WHERE user_id = \$1 AND api_key_id = \$2 AND deleted_at IS NULL ORDER BY generated_at DESC, id DESC LIMIT \$3`).
		WithArgs(int64(16), int64(20), 3).WillReturnRows(rows)
	page, err := r.ListStudioAssets(context.Background(), service.StudioAssetOwner{UserID: 16, APIKeyID: 20}, service.StudioAssetQuery{Limit: 2})
	require.NoError(t, err)
	require.Len(t, page.Items, 2)
	require.Equal(t, ids[0], page.Items[0].ID)
	require.Equal(t, ids[1], page.Items[1].ID)
	decoded, err := base64.RawURLEncoding.DecodeString(page.NextCursor)
	require.NoError(t, err)
	require.JSONEq(t, `{"generated_at":"2026-10-31T12:00:00.123456Z","id":"asset_00000000000000000000000000000002"}`, string(decoded))
	mock.ExpectQuery(`(?s)WHERE user_id = \$1 AND api_key_id = \$2 AND deleted_at IS NULL AND \(generated_at, id\) < \(\$3, \$4\) ORDER BY generated_at DESC, id DESC LIMIT \$5`).
		WithArgs(int64(16), int64(20), studioTestTime, ids[1], 3).
		WillReturnRows(studioRows().AddRow(studioValues(ids[2])...))
	page, err = r.ListStudioAssets(context.Background(), service.StudioAssetOwner{UserID: 16, APIKeyID: 20}, service.StudioAssetQuery{Limit: 2, Cursor: page.NextCursor})
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	require.Equal(t, ids[2], page.Items[0].ID)
	require.Empty(t, page.NextCursor)
}

func TestStudioAssetListBoundsAndKind(t *testing.T) {
	for _, tc := range []struct {
		name            string
		limit, sqlLimit int
	}{{"default", 0, 25}, {"negative defaults", -1, 25}, {"maximum", 1000, 101}} {
		t.Run(tc.name, func(t *testing.T) {
			r, mock := newStudioRepo(t)
			mock.ExpectQuery(`(?s)user_id = \$1 AND api_key_id = \$2 AND deleted_at IS NULL AND kind = \$3 ORDER BY generated_at DESC, id DESC LIMIT \$4`).
				WithArgs(int64(16), int64(20), "video", tc.sqlLimit).WillReturnRows(studioRows())
			page, err := r.ListStudioAssets(context.Background(), service.StudioAssetOwner{UserID: 16, APIKeyID: 20}, service.StudioAssetQuery{Kind: "video", Limit: tc.limit})
			require.NoError(t, err)
			require.NotNil(t, page.Items)
			require.Empty(t, page.NextCursor)
		})
	}
}

func TestStudioAssetInvalidCursorRejectedBeforeSQL(t *testing.T) {
	for _, cursor := range []string{"garbage", base64.RawURLEncoding.EncodeToString([]byte(`{"generated_at":"not-time","id":"` + studioTestID + `"}`)), base64.RawURLEncoding.EncodeToString([]byte(`{"generated_at":"2026-10-31T12:00:00Z","id":"another-owner"}`)), base64.RawURLEncoding.EncodeToString([]byte(`{}`))} {
		t.Run(cursor, func(t *testing.T) {
			r, _ := newStudioRepo(t)
			_, err := r.ListStudioAssets(context.Background(), service.StudioAssetOwner{UserID: 16, APIKeyID: 20}, service.StudioAssetQuery{Cursor: cursor})
			require.ErrorIs(t, err, service.ErrStudioAssetInvalidRequest)
		})
	}
}

func TestStudioAssetListPropagatesDatabaseFailures(t *testing.T) {
	r, mock := newStudioRepo(t)
	want := errors.New("database unavailable")
	mock.ExpectQuery(`FROM studio_assets`).WillReturnError(want)
	page, err := r.ListStudioAssets(context.Background(), service.StudioAssetOwner{UserID: 16, APIKeyID: 20}, service.StudioAssetQuery{})
	require.ErrorIs(t, err, want)
	require.Nil(t, page)
}

func TestStudioAssetUpsertIsOwnerGenerationOutputIdempotent(t *testing.T) {
	r, mock := newStudioRepo(t)
	for _, index := range []int{0, 0, 1} {
		a := studioFixture()
		a.OutputIndex = index
		wantID := studioTestID
		if index == 1 {
			wantID = "asset_00000000000000000000000000000001"
		}
		values := studioValues(wantID)
		values[5] = index
		mock.ExpectQuery(`(?s)INSERT INTO studio_assets.*ON CONFLICT \(user_id, api_key_id, generation_id, output_index\) DO UPDATE SET.*WHERE studio_assets.deleted_at IS NULL.*RETURNING`).
			WithArgs(studioGeneratedIDArgument{}, int64(16), int64(20), "image", "generation-1", index, "", "original-model", "original prompt", "image/png", "pending", "https://example.test/private.png", nil, []byte(`{"aspect_ratio":"3:4"}`), studioTestTime).
			WillReturnRows(studioRows().AddRow(values...))
		require.NoError(t, r.UpsertStudioAsset(context.Background(), a))
		require.Equal(t, wantID, a.ID)
		require.Equal(t, index, a.OutputIndex)
	}
}

func TestStudioAssetEmptyRetryRetainsPendingSource(t *testing.T) {
	r, mock := newStudioRepo(t)
	a := studioFixture()
	a.SourceURL = ""
	mock.ExpectQuery(`(?s)source_url = CASE.*ELSE COALESCE\(NULLIF\(EXCLUDED.source_url, ''\), studio_assets.source_url\) END, source_bytes = CASE.*ELSE COALESCE\(EXCLUDED.source_bytes, studio_assets.source_bytes\) END`).
		WithArgs(studioGeneratedIDArgument{}, int64(16), int64(20), "image", "generation-1", 0, "", "original-model", "original prompt", "image/png", "pending", "", nil, []byte(`{"aspect_ratio":"3:4"}`), studioTestTime).
		WillReturnRows(studioRows().AddRow(studioValues(studioTestID)...))
	require.NoError(t, r.UpsertStudioAsset(context.Background(), a))
	require.Equal(t, "https://example.test/private.png", a.SourceURL)
	require.Equal(t, []byte{1, 2}, a.SourceBytes)
}

func TestStudioAssetUpsertAcceptsInlineBytesValidatedAgainstLargerEffectiveLimit(t *testing.T) {
	r, mock := newStudioRepo(t)
	a := studioFixture()
	a.SourceBytes = make([]byte, 32*1024*1024+1)
	a.SourceBytes[0] = 7
	mock.ExpectQuery(`INSERT INTO studio_assets`).
		WithArgs(studioGeneratedIDArgument{}, int64(16), int64(20), "image", "generation-1", 0, "", "original-model", "original prompt", "image/png", "pending", "https://example.test/private.png", studioInlineBytesArgument{size: 32*1024*1024 + 1}, []byte(`{"aspect_ratio":"3:4"}`), studioTestTime).
		WillReturnRows(studioRows().AddRow(studioValues(studioTestID)...))
	require.NoError(t, r.UpsertStudioAsset(context.Background(), a))
}

func TestStudioAssetListNeverLoadsPrivateRetryPayload(t *testing.T) {
	r, mock := newStudioRepo(t)
	mock.ExpectQuery(`(?s)SELECT.*'' AS source_url, NULL::bytea AS source_bytes.*FROM studio_assets`).
		WithArgs(int64(16), int64(20), 25).WillReturnRows(studioRows())
	_, err := r.ListStudioAssets(context.Background(), service.StudioAssetOwner{UserID: 16, APIKeyID: 20}, service.StudioAssetQuery{})
	require.NoError(t, err)
}

func TestStudioAssetUpsertPreservesReadyObjectAndTombstone(t *testing.T) {
	r, mock := newStudioRepo(t)
	a := studioFixture()
	values := studioValues(studioTestID)
	values[10], values[11], values[13], values[14], values[16] = "private/object.png", "ready", "", nil, int64(24)
	mock.ExpectQuery(`(?s)source_url = CASE WHEN studio_assets.state = 'ready'.*source_bytes = CASE WHEN studio_assets.state = 'ready'.*WHERE studio_assets.deleted_at IS NULL`).WillReturnRows(studioRows().AddRow(values...))
	require.NoError(t, r.UpsertStudioAsset(context.Background(), a))
	require.Equal(t, "ready", a.State)
	require.Equal(t, "private/object.png", a.StorageKey)
	require.Empty(t, a.SourceURL)
	require.Empty(t, a.SourceBytes)
	mock.ExpectQuery(`(?s)WHERE studio_assets.deleted_at IS NULL`).WillReturnRows(studioRows())
	require.ErrorIs(t, r.UpsertStudioAsset(context.Background(), studioFixture()), service.ErrStudioAssetNotFound)
}

func TestStudioAssetDeleteIsOwnedIdempotentTombstone(t *testing.T) {
	r, mock := newStudioRepo(t)
	for _, affected := range []int64{1, 0} {
		mock.ExpectExec(`(?s)UPDATE studio_assets SET deleted_at = NOW\(\), source_url = '', source_bytes = NULL, lease_token = '', lease_expires_at = NULL.*WHERE user_id = \$1 AND api_key_id = \$2 AND id = \$3 AND deleted_at IS NULL`).
			WithArgs(int64(16), int64(20), studioTestID).WillReturnResult(sqlmock.NewResult(0, affected))
		require.NoError(t, r.DeleteStudioAsset(context.Background(), service.StudioAssetOwner{UserID: 16, APIKeyID: 20}, studioTestID))
	}
}

func TestStudioAssetClaimsLeaseOnlyEligibleRows(t *testing.T) {
	r, mock := newStudioRepo(t)
	mock.ExpectQuery(`(?s)UPDATE studio_assets SET lease_token = \$1.*state IN \('pending', 'retrying'\).*deleted_at IS NULL.*lease_expires_at <= NOW\(\).*LIMIT \$2 FOR UPDATE SKIP LOCKED.*RETURNING`).
		WithArgs(sqlmock.AnyArg(), 4).WillReturnRows(studioRows().AddRow(studioValues(studioTestID)...))
	items, err := r.ClaimStudioAssets(context.Background(), 4)
	require.NoError(t, err)
	require.Len(t, items, 1)
}

func TestStudioAssetCompletionRequiresActiveOwnedLease(t *testing.T) {
	for _, affected := range []int64{1, 0} {
		t.Run(string(rune('0'+affected)), func(t *testing.T) {
			r, mock := newStudioRepo(t)
			a := studioFixture()
			a.ID, a.LeaseToken, a.StorageKey, a.SizeBytes = studioTestID, "lease", "private/image.png", 24
			mock.ExpectExec(`(?s)UPDATE studio_assets SET storage_key = \$5.*state = 'ready'.*source_url = '', source_bytes = NULL.*WHERE user_id = \$1 AND api_key_id = \$2 AND id = \$3 AND lease_token = \$4 AND lease_expires_at > NOW\(\) AND deleted_at IS NULL AND state IN \('pending', 'retrying'\)`).
				WithArgs(int64(16), int64(20), studioTestID, "lease", "private/image.png", "image/png", int64(24)).WillReturnResult(sqlmock.NewResult(0, affected))
			err := r.CompleteStudioAsset(context.Background(), a)
			if affected == 1 {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, service.ErrStudioAssetUnavailable)
			}
		})
	}
}

func TestStudioAssetRetryKeepsSourceAndRejectsStaleLease(t *testing.T) {
	r, mock := newStudioRepo(t)
	a := studioFixture()
	a.ID, a.LeaseToken = studioTestID, "stale-lease"
	mock.ExpectExec(`(?s)SET state = 'retrying', last_error_code = \$5,.*WHERE user_id = \$1 AND api_key_id = \$2 AND id = \$3 AND lease_token = \$4 AND lease_expires_at > NOW\(\) AND deleted_at IS NULL AND state IN \('pending', 'retrying'\)`).
		WithArgs(int64(16), int64(20), studioTestID, "stale-lease", "storage_unavailable").WillReturnResult(sqlmock.NewResult(0, 0))
	require.ErrorIs(t, r.RetryStudioAsset(context.Background(), a, "storage_unavailable"), service.ErrStudioAssetUnavailable)
}

func TestStudioAssetRetryCanMarkTerminalSourceUnavailable(t *testing.T) {
	r, mock := newStudioRepo(t)
	a := studioFixture()
	a.ID, a.LeaseToken, a.State = studioTestID, "lease", "unavailable"
	mock.ExpectExec(`(?s)SET state = 'unavailable', last_error_code = \$5,.*WHERE user_id = \$1 AND api_key_id = \$2 AND id = \$3 AND lease_token = \$4 AND lease_expires_at > NOW\(\) AND deleted_at IS NULL`).
		WithArgs(int64(16), int64(20), studioTestID, "lease", "source_expired").WillReturnResult(sqlmock.NewResult(0, 1))
	require.NoError(t, r.RetryStudioAsset(context.Background(), a, "source_expired"))
}

func TestStudioAssetDeletedCleanupRetainsTombstone(t *testing.T) {
	r, mock := newStudioRepo(t)
	mock.ExpectQuery(`(?s)UPDATE studio_assets SET lease_token = \$1.*deleted_at IS NOT NULL AND storage_key <> '' AND.*LIMIT \$2 FOR UPDATE SKIP LOCKED.*RETURNING`).
		WithArgs(sqlmock.AnyArg(), 2).WillReturnRows(studioRows())
	items, err := r.ClaimDeletedStudioAssets(context.Background(), 2)
	require.NoError(t, err)
	require.Empty(t, items)
	a := studioFixture()
	a.ID, a.LeaseToken, a.StorageKey = studioTestID, "cleanup-lease", "private/image.png"
	mock.ExpectExec(`(?s)UPDATE studio_assets SET storage_key = '', state = 'unavailable',.*WHERE user_id = \$1 AND api_key_id = \$2 AND id = \$3 AND lease_token = \$4 AND lease_expires_at > NOW\(\) AND deleted_at IS NOT NULL AND storage_key = \$5`).
		WithArgs(int64(16), int64(20), studioTestID, "cleanup-lease", "private/image.png").WillReturnResult(sqlmock.NewResult(0, 1))
	require.NoError(t, r.CompleteStudioAssetCleanup(context.Background(), a))
}

func TestStudioAssetDeletedCleanupRejectsStaleLease(t *testing.T) {
	r, mock := newStudioRepo(t)
	a := studioFixture()
	a.ID, a.LeaseToken, a.StorageKey = studioTestID, "stale-lease", "private/image.png"
	mock.ExpectExec(`(?s)UPDATE studio_assets SET storage_key = '',.*deleted_at IS NOT NULL AND storage_key = \$5`).
		WithArgs(int64(16), int64(20), studioTestID, "stale-lease", "private/image.png").WillReturnResult(sqlmock.NewResult(0, 0))
	require.ErrorIs(t, r.CompleteStudioAssetCleanup(context.Background(), a), service.ErrStudioAssetUnavailable)
}

func TestStudioAssetRetriesNeverPersistRawErrorMessages(t *testing.T) {
	r, _ := newStudioRepo(t)
	a := studioFixture()
	a.ID, a.LeaseToken = studioTestID, "lease"
	for _, code := range []string{"", "GET https://private.test?api_key=secret failed", "storage-unavailable", string(make([]byte, 65))} {
		require.ErrorIs(t, r.RetryStudioAsset(context.Background(), a, code), service.ErrStudioAssetInvalidRequest)
	}
}

func TestStudioAssetValidationRejectsUnownedAndMalformedWrites(t *testing.T) {
	for _, mutate := range []func(*service.StudioAsset){
		func(a *service.StudioAsset) { a.Owner.UserID = 0 },
		func(a *service.StudioAsset) { a.Owner.APIKeyID = 0 },
		func(a *service.StudioAsset) { a.OutputIndex = -1 },
		func(a *service.StudioAsset) { a.GenerationID = "" },
		func(a *service.StudioAsset) { a.Kind = "audio" },
		func(a *service.StudioAsset) { a.Metadata = json.RawMessage(`broken`) },
		func(a *service.StudioAsset) { a.Metadata = json.RawMessage(`[]`) },
	} {
		r, _ := newStudioRepo(t)
		a := studioFixture()
		mutate(a)
		require.ErrorIs(t, r.UpsertStudioAsset(context.Background(), a), service.ErrStudioAssetInvalidRequest)
	}
}

func TestStudioAssetReconcileExplicitlyUnavailableUntilOwnedDiscovery(t *testing.T) {
	r, _ := newStudioRepo(t)
	result, err := r.ReconcileStudioAssets(context.Background(), service.StudioAssetOwner{UserID: 16, APIKeyID: 20}, nil)
	require.Nil(t, result)
	require.ErrorIs(t, err, service.ErrStudioAssetUnavailable)
}

func TestStudioAssetPrivateFieldsNeverMarshal(t *testing.T) {
	a := studioFixture()
	a.ID, a.StorageKey, a.LeaseToken, a.VideoResource, a.SourceBytes = studioTestID, "private-secret-key", "secret-lease", "supplier-resource", []byte("private-bytes")
	data, err := json.Marshal(a)
	require.NoError(t, err)
	for _, secret := range []string{"private-secret-key", "secret-lease", "supplier-resource", "private.png", "private-bytes", "UserID", "APIKeyID", "SourceBytes"} {
		require.NotContains(t, string(data), secret)
	}
	require.Regexp(t, regexp.MustCompile(`asset_[a-f0-9]{32}`), string(data))
}
