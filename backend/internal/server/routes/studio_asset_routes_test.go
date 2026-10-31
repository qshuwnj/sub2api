package routes

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const studioRouteID = "asset_0123456789abcdef0123456789abcdef"

type studioRouteKeys struct {
	service.APIKeyRepository
	keys map[string]*service.APIKey
}

func (r *studioRouteKeys) GetByKeyForAuth(_ context.Context, value string) (*service.APIKey, error) {
	k := r.keys[value]
	if k == nil {
		return nil, service.ErrAPIKeyNotFound
	}
	copy := *k
	return &copy, nil
}
func (*studioRouteKeys) UpdateLastUsed(context.Context, int64, time.Time) error { return nil }

func studioRouteRouter(t *testing.T, gateway *service.OpenAIGatewayService, mode string, status string, expired bool) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{RunMode: mode}
	cfg.Gateway.MaxBodySize = 4096
	group := &service.Group{ID: 42, Status: service.StatusActive, Hydrated: true, Platform: service.PlatformComposite,
		ModelAllowlist: service.GroupModelAllowlist{Enabled: true, Models: []string{"no-library-model"}}}
	keys := &studioRouteKeys{keys: map[string]*service.APIKey{}}
	for _, pair := range []struct {
		value     string
		user, key int64
	}{{"owner", 7, 100}, {"same-user-key", 7, 101}, {"other-user", 8, 102}} {
		k := &service.APIKey{ID: pair.key, UserID: pair.user, Key: pair.value, Status: status, Group: group, GroupID: &group.ID,
			User: &service.User{ID: pair.user, Status: service.StatusActive, Balance: 0}}
		if expired {
			at := time.Now().Add(-time.Hour)
			k.ExpiresAt = &at
		}
		keys.keys[pair.value] = k
	}
	auth := service.NewAPIKeyService(keys, nil, nil, nil, nil, nil, cfg)
	h := handler.NewOpenAIGatewayHandler(gateway, nil, nil, auth, nil, nil, nil, nil, cfg)
	r := gin.New()
	RegisterGatewayRoutes(r, &handler.Handlers{Gateway: &handler.GatewayHandler{}, OpenAIGateway: h, AsyncImage: handler.NewAsyncImageHandler(nil, nil)},
		middleware.NewAPIKeyAuthMiddleware(auth, nil, cfg), auth, nil, nil, nil, nil, cfg)
	return r
}

func studioRouteRequest(r *gin.Engine, method, path, key, body, byteRange string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	req.Header.Set("Range", byteRange)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestStudioAssetRoutesAuthenticateAndFailClosedWithoutLibrary(t *testing.T) {
	for _, endpoint := range []struct{ method, path, body string }{
		{"GET", "/v1/studio/assets", ""}, {"GET", "/v1/studio/assets/" + studioRouteID, ""},
		{"GET", "/v1/studio/assets/" + studioRouteID + "/content", ""},
		{"POST", "/v1/studio/assets/reconcile", `{"generation_ids":[]}`}, {"DELETE", "/v1/studio/assets/" + studioRouteID, ""},
	} {
		for _, scenario := range []struct {
			name, status, key string
			expired           bool
			want              int
		}{
			{"unauthenticated", service.StatusActive, "", false, 401},
			{"zero balance", service.StatusActive, "owner", false, 503},
			{"quota exhausted", service.StatusAPIKeyQuotaExhausted, "owner", false, 503},
			{"disabled", service.StatusDisabled, "owner", false, 401},
			{"expired status", service.StatusAPIKeyExpired, "owner", false, 403},
			{"runtime expiry", service.StatusActive, "owner", true, 403},
		} {
			t.Run(endpoint.method+endpoint.path+scenario.name, func(t *testing.T) {
				r := studioRouteRouter(t, nil, config.RunModeStandard, scenario.status, scenario.expired)
				w := studioRouteRequest(r, endpoint.method, endpoint.path, scenario.key, endpoint.body, "")
				require.Equal(t, scenario.want, w.Code, w.Body.String())
				require.Equal(t, "private, no-store", w.Header().Get("Cache-Control"))
				require.Equal(t, "nosniff", w.Header().Get("X-Content-Type-Options"))
			})
		}
	}
}

type studioRouteRepo struct {
	service.UsageBillingRepository
	service.StudioAssetRepository
	asset          *service.StudioAsset
	err            error
	reconcileOwner service.StudioAssetOwner
}

func (r *studioRouteRepo) GetStudioAsset(_ context.Context, o service.StudioAssetOwner, id string) (*service.StudioAsset, error) {
	if r.err != nil {
		return nil, r.err
	}
	if r.asset == nil || r.asset.Owner != o || r.asset.ID != id || r.asset.DeletedAt != nil {
		return nil, service.ErrStudioAssetNotFound
	}
	a := *r.asset
	return &a, nil
}
func (r *studioRouteRepo) ListStudioAssets(_ context.Context, o service.StudioAssetOwner, q service.StudioAssetQuery) (*service.StudioAssetPage, error) {
	if r.err != nil {
		return nil, r.err
	}
	p := &service.StudioAssetPage{Items: []*service.StudioAsset{}}
	if r.asset != nil && r.asset.Owner == o && r.asset.DeletedAt == nil && (q.Kind == "" || q.Kind == r.asset.Kind) {
		p.Items = append(p.Items, r.asset)
	}
	return p, nil
}
func (r *studioRouteRepo) DeleteStudioAsset(ctx context.Context, o service.StudioAssetOwner, id string) error {
	a, err := r.GetStudioAsset(ctx, o, id)
	if errors.Is(err, service.ErrStudioAssetNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	at := time.Now()
	a.DeletedAt = &at
	r.asset = a
	return nil
}
func (r *studioRouteRepo) ReconcileStudioAssets(_ context.Context, o service.StudioAssetOwner, ids []string) (*service.StudioAssetReconcileResult, error) {
	r.reconcileOwner = o
	if r.err != nil {
		return nil, r.err
	}
	if r.asset == nil || r.asset.Owner != o {
		return &service.StudioAssetReconcileResult{}, nil
	}
	return &service.StudioAssetReconcileResult{Queued: len(ids)}, nil
}

type studioRouteObjects struct{ opens int }

func (*studioRouteObjects) Save(context.Context, string, string, []byte) (string, error) {
	panic("library must not upload inline")
}
func (o *studioRouteObjects) OpenStudioAsset(_ context.Context, _ string, byteRange string) (*service.VideoResultContent, error) {
	o.opens++
	if byteRange != "" {
		return &service.VideoResultContent{Body: io.NopCloser(strings.NewReader("PNG")), MIME: "image/png", Size: 3, Status: 206, ContentRange: "bytes 0-2/10"}, nil
	}
	return &service.VideoResultContent{Body: io.NopCloser(strings.NewReader("0123456789")), MIME: "image/png", Size: 10, Status: 200}, nil
}
func (*studioRouteObjects) StudioAssetURL(context.Context, string, time.Duration) (string, error) {
	panic("grid does not sign or prefetch")
}
func newStudioRouteLibrary(t *testing.T, enabled ...bool) (*gin.Engine, *studioRouteRepo, *studioRouteObjects) {
	t.Helper()
	repo := &studioRouteRepo{asset: &service.StudioAsset{ID: studioRouteID, Owner: service.StudioAssetOwner{UserID: 7, APIKeyID: 100}, Kind: "image", GenerationID: "generation-1", State: "ready", Model: "original-model", Prompt: "original prompt", MIME: "image/png", SizeBytes: 10, GeneratedAt: time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC), Metadata: json.RawMessage(`{"aspect_ratio":"3:4","source_url":"https://secret.example","nested":{"secret":true}}`)}}
	digest := sha256.Sum256([]byte("generation-1"))
	repo.asset.StorageKey = fmt.Sprintf("studio-results/7/100/%x/0", digest)
	cfg := &config.Config{}
	gateway := service.NewOpenAIGatewayService(nil, nil, repo, nil, nil, nil, nil, cfg, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	objects := &studioRouteObjects{}
	u := service.NewImageResultUploader(objects, "images/", 32<<20, nil)
	gateway.SetStudioAssetStorage(func() (*service.ImageResultUploader, bool) { return u, len(enabled) == 0 || enabled[0] })
	return studioRouteRouter(t, gateway, config.RunModeStandard, service.StatusActive, false), repo, objects
}
func TestStudioAssetRoutesOwnerIsolationAndPublicShape(t *testing.T) {
	for _, key := range []string{"owner", "same-user-key", "other-user"} {
		t.Run(key, func(t *testing.T) {
			r, repo, objects := newStudioRouteLibrary(t)
			w := studioRouteRequest(r, "GET", "/v1/studio/assets", key, "", "")
			require.Equal(t, 200, w.Code, w.Body.String())
			var page struct {
				Items      []map[string]any `json:"items"`
				NextCursor any              `json:"next_cursor"`
				Storage    bool             `json:"storage_available"`
			}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &page))
			require.True(t, page.Storage)
			require.Nil(t, page.NextCursor)
			if key == "owner" {
				require.Len(t, page.Items, 1)
				require.Equal(t, "original-model", page.Items[0]["model"])
				require.Equal(t, map[string]any{"aspect_ratio": "3:4"}, page.Items[0]["metadata"])
			} else {
				require.Empty(t, page.Items)
			}
			w = studioRouteRequest(r, "GET", "/v1/studio/assets/"+studioRouteID, key, "", "")
			if key == "owner" {
				require.Equal(t, 200, w.Code)
				require.Contains(t, w.Body.String(), `"content_path":"/v1/studio/assets/`)
			} else {
				require.Equal(t, 404, w.Code)
			}
			for _, private := range []string{"source_url", "StorageKey", "studio-results/", "user_id", "api_key_id", "lease", "secret.example", "nested"} {
				require.NotContains(t, w.Body.String(), private)
			}
			w = studioRouteRequest(r, "GET", "/v1/studio/assets/"+studioRouteID+"/content?download=1", key, "", "")
			if key == "owner" {
				require.Equal(t, 200, w.Code)
				require.Contains(t, w.Header().Get("Content-Disposition"), "attachment;")
			} else {
				require.Equal(t, 404, w.Code)
				require.Zero(t, objects.opens)
			}
			w = studioRouteRequest(r, "POST", "/v1/studio/assets/reconcile", key, `{"generation_ids":["generation-1"]}`, "")
			require.Equal(t, 200, w.Code, w.Body.String())
			require.Equal(t, key == "owner", repo.reconcileOwner == repo.asset.Owner)
			if key != "owner" {
				require.Contains(t, w.Body.String(), `"queued":0`)
			}
			w = studioRouteRequest(r, "DELETE", "/v1/studio/assets/"+studioRouteID, key, "", "")
			require.Equal(t, 204, w.Code)
			require.Equal(t, key == "owner", repo.asset.DeletedAt != nil)
			w = studioRouteRequest(r, "DELETE", "/v1/studio/assets/"+studioRouteID, key, "", "")
			require.Equal(t, 204, w.Code)
		})
	}
}
func TestStudioAssetRoutesValidateInputRangeAndUnavailable(t *testing.T) {
	for _, tt := range []struct {
		method, path, body, byteRange string
		status                        int
	}{
		{"GET", "/v1/studio/assets?cursor=not-a-cursor", "", "", 400},
		{"GET", "/v1/studio/assets?limit=oops", "", "", 400},
		{"GET", "/v1/studio/assets?type=audio", "", "", 400},
		{"GET", "/v1/studio/assets/bad-id", "", "", 400},
		{"GET", "/v1/studio/assets/reconcile", "", "", 400},
		{"DELETE", "/v1/studio/assets/%2e%2e", "", "", 400},
		{"GET", "/v1/studio/assets/" + studioRouteID + "/content", "", "bytes=0-2,4-5", 416},
		{"GET", "/v1/studio/assets/" + studioRouteID + "/content", "", "bytes=90-99", 416},
		{"GET", "/v1/studio/assets/bad-id/content", "", "", 400},
		{"GET", "/v1/studio/assets/" + studioRouteID + "/content", "", "bytes=0-2", 206},
		{"POST", "/v1/studio/assets/reconcile", `{"generation_ids":[1]}`, "", 400},
		{"POST", "/v1/studio/assets/reconcile", `{"generation_ids":[],"user_id":8}`, "", 400},
		{"POST", "/v1/studio/assets/reconcile", `{"generation_ids":[]} {}`, "", 400},
		{"POST", "/v1/studio/assets/reconcile", `null`, "", 400},
		{"POST", "/v1/studio/assets/reconcile", `{}`, "", 400},
		{"POST", "/v1/studio/assets/reconcile", `{"generation_ids":null}`, "", 400},
	} {
		t.Run(tt.method+tt.path+tt.body+tt.byteRange, func(t *testing.T) {
			r, _, _ := newStudioRouteLibrary(t)
			w := studioRouteRequest(r, tt.method, tt.path, "owner", tt.body, tt.byteRange)
			require.Equal(t, tt.status, w.Code, w.Body.String())
			require.Equal(t, "private, no-store", w.Header().Get("Cache-Control"))
			if tt.status == 206 {
				require.Equal(t, "bytes 0-2/10", w.Header().Get("Content-Range"))
				require.Equal(t, "PNG", w.Body.String())
			}
		})
	}
	r, repo, _ := newStudioRouteLibrary(t)
	repo.err = errors.New("private database credential")
	w := studioRouteRequest(r, "GET", "/v1/studio/assets", "owner", "", "")
	require.Equal(t, 503, w.Code)
	require.NotContains(t, w.Body.String(), "credential")
}

func TestStudioAssetRouteBodyLimitAndUnknownDelete(t *testing.T) {
	r, _, _ := newStudioRouteLibrary(t)
	w := studioRouteRequest(r, "POST", "/v1/studio/assets/reconcile", "owner", `{"generation_ids":["`+strings.Repeat("x", 5000)+`"]}`, "")
	require.Equal(t, 400, w.Code, w.Body.String())
	w = studioRouteRequest(r, "DELETE", "/v1/studio/assets/asset_ffffffffffffffffffffffffffffffff", "owner", "", "")
	require.Equal(t, 204, w.Code)
}

func TestStudioAssetStorageUnavailablePreservesMetadataAndDeniesContent(t *testing.T) {
	r, repo, objects := newStudioRouteLibrary(t, false)
	w := studioRouteRequest(r, "GET", "/v1/studio/assets", "owner", "", "")
	require.Equal(t, 200, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), `"storage_available":false`)
	require.Contains(t, w.Body.String(), `"prompt":"original prompt"`)
	w = studioRouteRequest(r, "GET", "/v1/studio/assets/"+studioRouteID+"/content", "owner", "", "")
	require.Equal(t, 503, w.Code)
	require.Zero(t, objects.opens)
	repo.err = service.ErrStudioAssetUnavailable
	w = studioRouteRequest(r, "POST", "/v1/studio/assets/reconcile", "owner", `{"generation_ids":[]}`, "")
	require.Equal(t, 503, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "studio_asset_unavailable")
}

func TestStudioAssetReadExemptionCannotEnableGeneration(t *testing.T) {
	r := studioRouteRouter(t, nil, config.RunModeStandard, service.StatusActive, false)
	w := studioRouteRequest(r, "POST", "/v1/images/generations", "owner", `{"model":"x"}`, "")
	require.Equal(t, 403, w.Code, w.Body.String())
	for _, status := range []string{service.StatusAPIKeyExpired, service.StatusActive} {
		r := studioRouteRouter(t, nil, config.RunModeSimple, status, true)
		w := studioRouteRequest(r, "GET", "/v1/studio/assets", "owner", "", "")
		require.Equal(t, 403, w.Code, w.Body.String())
	}
}
