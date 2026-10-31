package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func studioHistoryDB(t *testing.T) (*usageBillingRepository, context.Context) {
	t.Helper()
	dsn := os.Getenv("STUDIO_ASSET_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set STUDIO_ASSET_TEST_POSTGRES_DSN for disposable PostgreSQL fixture")
	}
	u, err := url.Parse(dsn)
	require.NoError(t, err)
	require.Equal(t, "127.0.0.1:55439", u.Host)
	require.Equal(t, "/studio_library_fixture", u.Path)
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	schema := pq.QuoteIdentifier("studio_task5_" + strings.ReplaceAll(uuid.NewString(), "-", ""))
	_, err = db.Exec("CREATE SCHEMA " + schema)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := db.Exec("DROP SCHEMA " + schema + " CASCADE")
		require.NoError(t, err)
		require.NoError(t, db.Close())
	})
	_, err = db.Exec("SET search_path TO " + schema)
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TABLE users(id bigint PRIMARY KEY); CREATE TABLE api_keys(id bigint PRIMARY KEY);
		INSERT INTO users VALUES(16),(17); INSERT INTO api_keys VALUES(20),(21);
		CREATE TABLE video_attempts(id text, user_id bigint,api_key_id bigint,state text, financial_state text,
		receipt_usage_log_id bigint,upstream_task_id text,created_at timestamptz DEFAULT NOW(),updated_at timestamptz DEFAULT NOW());
		CREATE TABLE usage_logs(id bigserial,user_id bigint,api_key_id bigint,account_id bigint,request_id text,video_count int,output_tokens int DEFAULT 0,created_at timestamptz DEFAULT NOW());
		CREATE TABLE usage_billing_dedup(request_id text,api_key_id bigint);
		CREATE TABLE usage_billing_dedup_archive(request_id text,api_key_id bigint);`)
	require.NoError(t, err)
	for _, file := range []string{"252_video_results.sql", "253_studio_assets.sql", "254_studio_video_intakes.sql", "255_studio_asset_recovery_checks.sql"} {
		b, err := os.ReadFile(filepath.Join("..", "..", "migrations", file))
		require.NoError(t, err)
		_, err = db.Exec(string(b))
		require.NoError(t, err)
	}
	return &usageBillingRepository{db: db}, context.Background()
}

func TestStudioAssetPostgresVideoIntakeKeepsOriginalMetadataAndAccountBinding(t *testing.T) {
	r, ctx := studioHistoryDB(t)
	in := &service.StudioVideoIntake{Owner: service.StudioAssetOwner{UserID: 16, APIKeyID: 20}, Resource: "task:native",
		Model: "original", Prompt: "original prompt", AccountID: 90, Metadata: json.RawMessage(`{"duration":6}`)}
	require.NoError(t, r.SaveStudioVideoIntake(ctx, in))
	in.Model, in.Prompt, in.Metadata = "changed", "changed prompt", json.RawMessage(`{"duration":99}`)
	require.NoError(t, r.SaveStudioVideoIntake(ctx, in))
	saved, err := r.GetStudioVideoIntake(ctx, in.Owner, in.Resource)
	require.NoError(t, err)
	require.Equal(t, "original", saved.Model)
	require.Equal(t, "original prompt", saved.Prompt)
	require.JSONEq(t, `{"duration":6}`, string(saved.Metadata))
	in.AccountID, in.Completed = 91, true
	require.ErrorIs(t, r.SaveStudioVideoIntake(ctx, in), service.ErrVideoAttemptConflict)
	saved, err = r.GetStudioVideoIntake(ctx, in.Owner, in.Resource)
	require.NoError(t, err)
	require.False(t, saved.Completed)
	require.Equal(t, int64(90), saved.AccountID)
}

func TestStudioAssetPostgresReconcileOwnedVideoHistory(t *testing.T) {
	r, ctx := studioHistoryDB(t)
	_, err := r.db.Exec(`INSERT INTO video_attempts VALUES
		('done',16,20,'completed','captured',1,'atlascloud:done',NOW(),NOW()),
		('pending',16,20,'completed','captured',2,'atlascloud:pending',NOW(),NOW()),
		('foreign',16,21,'completed','captured',3,'atlascloud:foreign',NOW(),NOW());
		INSERT INTO studio_video_intakes(user_id,api_key_id,resource,generation_id,model,prompt) VALUES
		(16,20,'attempt:done','attempt:done','original-model','original prompt'),
		(16,20,'attempt:pending','attempt:pending','original-model','pending prompt'),
		(16,21,'attempt:foreign','attempt:foreign','secret model','secret prompt');
		INSERT INTO video_results(user_id,api_key_id,resource,upstream_task_id,account_id,storage_key,mime,size_bytes) VALUES
		(16,20,'attempt:done','atlascloud:done',90,'video-results/16/20/done.mp4','video/mp4',42);
		INSERT INTO video_results(user_id,api_key_id,resource,upstream_task_id,account_id,source_url) VALUES
		(16,20,'attempt:pending','atlascloud:pending',90,'https://example.com/pending.mp4');`)
	require.NoError(t, err)
	owner := service.StudioAssetOwner{UserID: 16, APIKeyID: 20}
	got, err := r.ReconcileStudioAssets(ctx, owner, []string{"attempt:done", "attempt:pending", "attempt:foreign"})
	require.NoError(t, err)
	require.Equal(t, &service.StudioAssetReconcileResult{Queued: 2, Unrecoverable: 1}, got)
	page, err := r.ListStudioAssets(ctx, owner, service.StudioAssetQuery{})
	require.NoError(t, err)
	require.Len(t, page.Items, 2)
	for _, a := range page.Items {
		require.Equal(t, "original-model", a.Model)
		require.Empty(t, a.SourceURL)
	}
	got, err = r.ReconcileStudioAssets(ctx, owner, []string{"attempt:done", "attempt:pending"})
	require.NoError(t, err)
	require.Equal(t, &service.StudioAssetReconcileResult{AlreadySaved: 2}, got)
	require.NoError(t, r.DeleteStudioAsset(ctx, owner, page.Items[0].ID))
	got, err = r.ReconcileStudioAssets(ctx, owner, []string{page.Items[0].GenerationID})
	require.NoError(t, err)
	require.Equal(t, &service.StudioAssetReconcileResult{Unrecoverable: 1}, got)
	page, err = r.ListStudioAssets(ctx, owner, service.StudioAssetQuery{})
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
}

func TestStudioAssetPostgresVideoExplicitRetryDoesNotReviveTombstone(t *testing.T) {
	r, ctx := studioHistoryDB(t)
	_, err := r.db.Exec(`INSERT INTO video_attempts(id,user_id,api_key_id,state,financial_state,receipt_usage_log_id,upstream_task_id)
		VALUES('retry',16,20,'completed','captured',1,'atlascloud:retry');
		INSERT INTO video_results(user_id,api_key_id,resource,upstream_task_id,account_id,storage_key,mime,size_bytes)
		VALUES(16,20,'attempt:retry','atlascloud:retry',90,'video-results/16/20/retry.mp4','video/mp4',42);`)
	require.NoError(t, err)
	owner := service.StudioAssetOwner{UserID: 16, APIKeyID: 20}
	_, err = r.ReconcileStudioAssets(ctx, owner, []string{"attempt:retry"})
	require.NoError(t, err)
	_, err = r.db.Exec(`UPDATE studio_assets SET state='unavailable',last_error_code='source_expired'`)
	require.NoError(t, err)
	got, err := r.ReconcileStudioAssets(ctx, owner, []string{"attempt:retry"})
	require.NoError(t, err)
	require.Equal(t, &service.StudioAssetReconcileResult{Queued: 1}, got)
	a, err := r.FindStudioAssetGeneration(ctx, owner, "attempt:retry")
	require.NoError(t, err)
	require.Equal(t, "retrying", a.State)
	require.Empty(t, a.LastErrorCode)
	require.NoError(t, r.DeleteStudioAsset(ctx, owner, a.ID))
	got, err = r.ReconcileStudioAssets(ctx, owner, []string{"attempt:retry"})
	require.NoError(t, err)
	require.Equal(t, &service.StudioAssetReconcileResult{Unrecoverable: 1}, got)
}

func TestStudioAssetPostgresVideoProtectedNativeRequiresCompletedBoundIntake(t *testing.T) {
	r, ctx := studioHistoryDB(t)
	_, err := r.db.Exec(`INSERT INTO video_results(user_id,api_key_id,resource,upstream_task_id,account_id)
		VALUES(16,20,'task:native','native',90);
		INSERT INTO studio_video_intakes(user_id,api_key_id,resource,generation_id,model,prompt,completed,account_id)
		VALUES(16,20,'task:native','task:native','original','original prompt',false,90);`)
	require.NoError(t, err)
	owner := service.StudioAssetOwner{UserID: 16, APIKeyID: 20}
	got, err := r.ReconcileStudioAssets(ctx, owner, []string{"task:native"})
	require.NoError(t, err)
	require.Equal(t, &service.StudioAssetReconcileResult{Unrecoverable: 1}, got)
	_, err = r.db.Exec(`UPDATE studio_video_intakes SET completed=true`)
	require.NoError(t, err)
	got, err = r.ReconcileStudioAssets(ctx, owner, []string{"task:native"})
	require.NoError(t, err)
	require.Equal(t, &service.StudioAssetReconcileResult{Unrecoverable: 1}, got, "supplier completion is not a committed customer receipt")
	_, err = r.db.Exec(`INSERT INTO usage_logs(user_id,api_key_id,account_id,request_id,video_count)
		VALUES(16,20,90,'grok-video:native',1);
		INSERT INTO usage_billing_dedup VALUES('grok-video:native',20);`)
	require.NoError(t, err)
	got, err = r.ReconcileStudioAssets(ctx, owner, []string{"task:native"})
	require.NoError(t, err)
	require.Equal(t, &service.StudioAssetReconcileResult{Queued: 1}, got)
}

func TestStudioAssetPostgresNativeReceiptCannotBeReplacedByReadyArchive(t *testing.T) {
	for _, task := range []string{"native", "seedance:native"} {
		t.Run(task, func(t *testing.T) {
			r, ctx := studioHistoryDB(t)
			owner := service.StudioAssetOwner{UserID: 16, APIKeyID: 20}
			_, err := r.db.Exec(`INSERT INTO video_results(user_id,api_key_id,resource,upstream_task_id,account_id,storage_key,mime,size_bytes)
				VALUES(16,20,$1,$2,90,'video-results/16/20/native.mp4','video/mp4',42)`, "task:"+task, task)
			require.NoError(t, err)
			result, err := r.ReconcileStudioAssets(ctx, owner, []string{"task:" + task})
			require.NoError(t, err)
			require.Equal(t, &service.StudioAssetReconcileResult{Unrecoverable: 1}, result)
			old := &service.StudioAsset{Owner: owner, Kind: "video", GenerationID: "task:" + task,
				VideoResource: "task:" + task, GeneratedAt: time.Now()}
			require.NoError(t, r.UpsertStudioAsset(ctx, old))
			_, err = r.db.Exec(`UPDATE studio_assets SET state='ready',storage_key='video-results/16/20/native.mp4',mime='video/mp4',size_bytes=42`)
			require.NoError(t, err)
			result, err = service.NewStudioAssetService(r, nil).Reconcile(ctx, owner, []string{"task:" + task})
			require.NoError(t, err)
			require.Equal(t, &service.StudioAssetReconcileResult{Unrecoverable: 1}, result, "previously indexed ready rows cannot report saved without a receipt")
			// Keep the positive case focused on new indexing after proof exists.
			_, err = r.db.Exec(`DELETE FROM studio_assets`)
			require.NoError(t, err)
			videoCount, outputTokens := 1, 0
			if strings.HasPrefix(task, "seedance:") {
				videoCount, outputTokens = 0, 42
			}
			_, err = r.db.Exec(`WITH inserted AS (
				INSERT INTO usage_logs(user_id,api_key_id,account_id,request_id,video_count,output_tokens)
				VALUES(16,21,90,$1,$2,$3) RETURNING request_id,api_key_id)
				INSERT INTO usage_billing_dedup_archive SELECT request_id,api_key_id FROM inserted`,
				service.StableGrokVideoBillingRequestID(task), videoCount, outputTokens)
			require.NoError(t, err)
			result, err = r.ReconcileStudioAssets(ctx, owner, []string{"task:" + task})
			require.NoError(t, err)
			require.Equal(t, &service.StudioAssetReconcileResult{Unrecoverable: 1}, result, "same user, different key cannot provide the receipt")
			_, err = r.db.Exec(`WITH inserted AS (
				INSERT INTO usage_logs(user_id,api_key_id,account_id,request_id,video_count,output_tokens)
				VALUES(16,20,90,$1,$2,$3) RETURNING request_id,api_key_id)
				INSERT INTO usage_billing_dedup_archive SELECT request_id,api_key_id FROM inserted`,
				service.StableGrokVideoBillingRequestID(task), videoCount, outputTokens)
			require.NoError(t, err)
			result, err = r.ReconcileStudioAssets(ctx, owner, []string{"task:" + task})
			require.NoError(t, err)
			require.Equal(t, &service.StudioAssetReconcileResult{Queued: 1}, result)
			_, err = r.FindStudioNativeVideoReceipt(ctx, service.VideoAttemptOwner{UserID: owner.UserID, APIKeyID: owner.APIKeyID}, "grok-video:"+task)
			require.ErrorIs(t, err, service.ErrVideoResultNotFound, "a billing-key alias is not the original native task identity")
		})
	}
}

func TestStudioAssetPostgresReconcileArchivedLegacyVideoWithoutInventedMetadata(t *testing.T) {
	r, ctx := studioHistoryDB(t)
	_, err := r.db.Exec(`INSERT INTO usage_logs(user_id,api_key_id,account_id,request_id,video_count) VALUES
		(16,20,90,'grok-video:atlascloud:old',1),
		(16,20,90,'grok-video:atlascloud:ambiguous',1),
		(16,20,91,'grok-video:atlascloud:ambiguous',1);
		INSERT INTO usage_billing_dedup VALUES('grok-video:atlascloud:old',20),('grok-video:atlascloud:ambiguous',20);
		INSERT INTO video_results(user_id,api_key_id,resource,upstream_task_id,account_id,storage_key,mime,size_bytes) VALUES
		(16,20,'task:atlascloud:old','atlascloud:old',90,'video-results/16/20/old.mp4','video/mp4',42),
		(16,20,'task:atlascloud:ambiguous','atlascloud:ambiguous',90,'video-results/16/20/ambiguous.mp4','video/mp4',42);`)
	require.NoError(t, err)
	got, err := r.ReconcileStudioAssets(ctx, service.StudioAssetOwner{UserID: 16, APIKeyID: 20},
		[]string{"task:atlascloud:old", "task:atlascloud:ambiguous", "task:atlascloud:foreign"})
	require.NoError(t, err)
	require.Equal(t, &service.StudioAssetReconcileResult{Queued: 1, Unrecoverable: 2}, got)
	page, err := r.ListStudioAssets(ctx, service.StudioAssetOwner{UserID: 16, APIKeyID: 20}, service.StudioAssetQuery{})
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	require.Empty(t, page.Items[0].Model)
	require.Empty(t, page.Items[0].Prompt)
}

func TestStudioAssetPostgresVideoBackgroundAndHistoryDiscoveryBound(t *testing.T) {
	r, ctx := studioHistoryDB(t)
	_, err := r.db.Exec(`INSERT INTO video_attempts(id,user_id,api_key_id,state,financial_state,receipt_usage_log_id,upstream_task_id)
		SELECT 'batch-'||n,16,20,'completed','captured',n,'atlascloud:batch-'||n FROM generate_series(1,125) n;
		INSERT INTO video_results(user_id,api_key_id,resource,upstream_task_id,account_id,storage_key,mime,size_bytes)
		SELECT 16,20,'attempt:batch-'||n,'atlascloud:batch-'||n,90,'video-results/16/20/batch-'||n||'.mp4','video/mp4',42
		FROM generate_series(1,125) n;
		INSERT INTO studio_video_intakes(user_id,api_key_id,resource,generation_id,model,prompt)
		VALUES(16,20,'attempt:batch-1','attempt:batch-1','original','original prompt');`)
	require.NoError(t, err)
	require.NoError(t, r.ProcessStudioVideoIntakes(ctx, 100))
	var count int
	require.NoError(t, r.db.QueryRow(`SELECT COUNT(*) FROM studio_assets`).Scan(&count))
	require.Equal(t, 1, count, "the durable intake must materialize after completion without an HTTP poll")
	owner := service.StudioAssetOwner{UserID: 16, APIKeyID: 20}
	for _, want := range []int{100, 24, 0} {
		result, err := r.ReconcileStudioAssets(ctx, owner, nil)
		require.NoError(t, err)
		require.Equal(t, want, result.Queued)
		require.Zero(t, result.Unrecoverable)
	}
	require.NoError(t, r.db.QueryRow(`SELECT COUNT(*) FROM studio_assets`).Scan(&count))
	require.Equal(t, 125, count)
	require.NoError(t, r.db.QueryRow(`SELECT COUNT(*) FROM video_results`).Scan(&count))
	require.Equal(t, 125, count, "indexing cannot create a second archive for any video")
	require.NoError(t, r.db.QueryRow(`SELECT COUNT(*) FROM usage_logs`).Scan(&count))
	require.Zero(t, count, "library recovery cannot settle or generate a usage receipt")
}

func TestStudioAssetPostgresVideoExpiredAndUncapturedHistoryIsUnrecoverable(t *testing.T) {
	r, ctx := studioHistoryDB(t)
	_, err := r.db.Exec(`INSERT INTO video_attempts(id,user_id,api_key_id,state,financial_state,receipt_usage_log_id,upstream_task_id) VALUES
		('expired',16,20,'completed','captured',1,'atlascloud:expired'),
		('uncaptured',16,20,'processing','reserved',NULL,'atlascloud:uncaptured');
		INSERT INTO video_results(user_id,api_key_id,resource,upstream_task_id,account_id,source_url) VALUES
		(16,20,'attempt:expired','atlascloud:expired',90,'https://example.com/dead.mp4?X-Amz-Date=20000101T000000Z&X-Amz-Expires=60');
		INSERT INTO video_results(user_id,api_key_id,resource,upstream_task_id,account_id,storage_key,mime,size_bytes) VALUES
		(16,20,'attempt:uncaptured','atlascloud:uncaptured',90,'video-results/16/20/uncaptured.mp4','video/mp4',42);`)
	require.NoError(t, err)
	result, err := r.ReconcileStudioAssets(ctx, service.StudioAssetOwner{UserID: 16, APIKeyID: 20}, []string{"attempt:expired", "attempt:uncaptured"})
	require.NoError(t, err)
	require.Equal(t, &service.StudioAssetReconcileResult{Unrecoverable: 2}, result)
	page, err := r.ListStudioAssets(ctx, service.StudioAssetOwner{UserID: 16, APIKeyID: 20}, service.StudioAssetQuery{})
	require.NoError(t, err)
	require.Empty(t, page.Items)
}

func TestStudioAssetPostgresHistoryAdvancesPastUnrecoverableHundred(t *testing.T) {
	r, ctx := studioHistoryDB(t)
	_, err := r.db.Exec(`INSERT INTO video_attempts(id,user_id,api_key_id,state,financial_state,receipt_usage_log_id,upstream_task_id)
		SELECT 'a-expired-'||n,16,20,'completed','captured',n,'atlascloud:a-expired-'||n FROM generate_series(1,100) n;
		INSERT INTO video_attempts(id,user_id,api_key_id,state,financial_state,receipt_usage_log_id,upstream_task_id)
		VALUES('z-valid',16,20,'completed','captured',101,'atlascloud:z-valid');
		INSERT INTO video_results(user_id,api_key_id,resource,upstream_task_id,account_id,source_url)
		SELECT 16,20,'attempt:a-expired-'||n,'atlascloud:a-expired-'||n,90,
		'https://example.com/expired.mp4?X-Amz-Date=20000101T000000Z&X-Amz-Expires=60' FROM generate_series(1,100) n;
		INSERT INTO video_results(user_id,api_key_id,resource,upstream_task_id,account_id,storage_key,mime,size_bytes)
		VALUES(16,20,'attempt:z-valid','atlascloud:z-valid',90,'video-results/16/20/z-valid.mp4','video/mp4',42);`)
	require.NoError(t, err)
	owner := service.StudioAssetOwner{UserID: 16, APIKeyID: 20}
	first, err := r.ReconcileStudioAssets(ctx, owner, nil)
	require.NoError(t, err)
	require.Equal(t, &service.StudioAssetReconcileResult{Unrecoverable: 100}, first)
	second, err := r.ReconcileStudioAssets(ctx, owner, nil)
	require.NoError(t, err)
	require.Equal(t, &service.StudioAssetReconcileResult{Queued: 1}, second, "unrecoverable lexical head must back off so the next bounded pass advances")
}

func TestStudioAssetPostgresBackgroundLookupAdvancesPastUnresolvedHundred(t *testing.T) {
	r, ctx := studioHistoryDB(t)
	_, err := r.db.Exec(`INSERT INTO video_attempts(id,user_id,api_key_id,state,financial_state,receipt_usage_log_id,upstream_task_id)
		SELECT 'attempt-'||n,16,20,'completed','captured',n,'atlascloud:attempt-'||n FROM generate_series(1,101) n;
		INSERT INTO studio_video_intakes(user_id,api_key_id,resource,generation_id,model,prompt,account_id)
		SELECT 16,20,'attempt:attempt-'||n,'attempt:attempt-'||n,'original','original prompt',90 FROM generate_series(1,101) n;`)
	require.NoError(t, err)
	first, err := r.ListStudioVideoIntakes(ctx, 100)
	require.NoError(t, err)
	require.Len(t, first, 100)
	second, err := r.ListStudioVideoIntakes(ctx, 100)
	require.NoError(t, err)
	require.Len(t, second, 1, "unresolved older intakes must not monopolize every lookup pass")
	for _, old := range first {
		require.NotEqual(t, old.Resource, second[0].Resource)
	}
}

func TestStudioAssetPostgresDiscoveryDoesNotSelectUnbilledIncompleteNative(t *testing.T) {
	r, ctx := studioHistoryDB(t)
	_, err := r.db.Exec(`INSERT INTO studio_video_intakes(user_id,api_key_id,resource,generation_id,model,prompt,account_id)
		SELECT 16,20,'task:a-pending-'||n,'task:a-pending-'||n,'original','original prompt',90 FROM generate_series(1,100) n;
		INSERT INTO video_results(user_id,api_key_id,resource,upstream_task_id,account_id,source_url)
		SELECT 16,20,'task:a-pending-'||n,'a-pending-'||n,90,'https://vidgen.x.ai/not-paid.mp4' FROM generate_series(1,100) n;`)
	require.NoError(t, err)
	ids, err := r.ListStudioAssetHistory(ctx, service.StudioAssetOwner{UserID: 16, APIKeyID: 20}, 100)
	require.NoError(t, err)
	require.Empty(t, ids, "unbilled or incomplete intakes are not completed-history candidates")
	intakes, err := r.ListStudioVideoIntakes(ctx, 100)
	require.NoError(t, err)
	require.Empty(t, intakes)
	_, err = r.ReconcileStudioAssets(ctx, service.StudioAssetOwner{UserID: 16, APIKeyID: 20}, []string{"task:foreign"})
	require.NoError(t, err)
	var checks int
	require.NoError(t, r.db.QueryRow(`SELECT COUNT(*) FROM studio_asset_recovery_checks`).Scan(&checks))
	require.Zero(t, checks, "caller-supplied unproven identities must not create recovery scheduling rows")
}

func TestStudioAssetPostgresBackgroundIndexAdvancesPastUnrecoverableHundred(t *testing.T) {
	r, ctx := studioHistoryDB(t)
	_, err := r.db.Exec(`INSERT INTO video_attempts(id,user_id,api_key_id,state,financial_state,receipt_usage_log_id,upstream_task_id)
		SELECT 'a-expired-'||n,16,20,'completed','captured',n,'atlascloud:a-expired-'||n FROM generate_series(1,100) n;
		INSERT INTO video_attempts(id,user_id,api_key_id,state,financial_state,receipt_usage_log_id,upstream_task_id)
		VALUES('z-valid',16,20,'completed','captured',101,'atlascloud:z-valid');
		INSERT INTO studio_video_intakes(user_id,api_key_id,resource,generation_id,model,prompt,account_id)
		SELECT 16,20,'attempt:'||id,'attempt:'||id,'original','original prompt',90 FROM video_attempts;
		INSERT INTO video_results(user_id,api_key_id,resource,upstream_task_id,account_id,source_url)
		SELECT 16,20,'attempt:a-expired-'||n,'atlascloud:a-expired-'||n,90,
		'https://example.com/expired.mp4?X-Amz-Date=20000101T000000Z&X-Amz-Expires=60' FROM generate_series(1,100) n;
		INSERT INTO video_results(user_id,api_key_id,resource,upstream_task_id,account_id,storage_key,mime,size_bytes)
		VALUES(16,20,'attempt:z-valid','atlascloud:z-valid',90,'video-results/16/20/z-valid.mp4','video/mp4',42);`)
	require.NoError(t, err)
	require.NoError(t, r.ProcessStudioVideoIntakes(ctx, 100))
	var count int
	require.NoError(t, r.db.QueryRow(`SELECT COUNT(*) FROM studio_assets`).Scan(&count))
	require.Zero(t, count)
	require.NoError(t, r.ProcessStudioVideoIntakes(ctx, 100))
	require.NoError(t, r.db.QueryRow(`SELECT COUNT(*) FROM studio_assets WHERE generation_id='attempt:z-valid'`).Scan(&count))
	require.Equal(t, 1, count, "background indexing must also advance past failed old sources")
}
