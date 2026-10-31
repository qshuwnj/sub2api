package routes

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// This runs the registered public route, auth, service validation and real SQL
// together. A fake repository accepting "all" would conceal the client mismatch.
func TestStudioAssetRoutesPostgresPublicAllOwnerPagination(t *testing.T) {
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
	schema := pq.QuoteIdentifier("studio_final_route_" + strings.ReplaceAll(uuid.NewString(), "-", ""))
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
		INSERT INTO users VALUES(7),(8); INSERT INTO api_keys VALUES(100),(101),(102);`)
	require.NoError(t, err)
	migration, err := os.ReadFile(filepath.Join("..", "..", "..", "migrations", "253_studio_assets.sql"))
	require.NoError(t, err)
	_, err = db.Exec(string(migration))
	require.NoError(t, err)
	repo := repository.NewUsageBillingRepository(nil, db)
	assets := repo.(service.StudioAssetRepository)
	gateway := service.NewOpenAIGatewayService(nil, nil, repo, nil, nil, nil, nil, &config.Config{}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	gateway.SetStudioAssetStorage(nil)
	router := studioRouteRouter(t, gateway, config.RunModeStandard, service.StatusActive, false)
	type page struct {
		Items []struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		} `json:"items"`
		Cursor string `json:"next_cursor"`
	}
	readPage := func(t *testing.T, target, key string) page {
		t.Helper()
		w := studioRouteRequest(router, "GET", target, key, "", "")
		require.Equal(t, 200, w.Code, w.Body.String())
		var p page
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &p))
		return p
	}
	for ownerIndex, fixture := range []struct {
		key   string
		owner service.StudioAssetOwner
	}{{"owner", service.StudioAssetOwner{UserID: 7, APIKeyID: 100}},
		{"same-user-key", service.StudioAssetOwner{UserID: 7, APIKeyID: 101}},
		{"other-user", service.StudioAssetOwner{UserID: 8, APIKeyID: 102}}} {
		for n := 0; n < 106; n++ {
			kind := "image"
			if n%2 == 1 {
				kind = "video"
			}
			require.NoError(t, assets.UpsertStudioAsset(context.Background(), &service.StudioAsset{
				ID: fmt.Sprintf("asset_%032x", (ownerIndex+1)*1000+n), Owner: fixture.owner,
				Kind: kind, GenerationID: fmt.Sprintf("fixture-%d", n),
				GeneratedAt: time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC).Add(-time.Duration(n) * time.Second),
			}))
		}
	}
	for ownerIndex, key := range []string{"owner", "same-user-key", "other-user"} {
		t.Run(key, func(t *testing.T) {
			read := func(target, key string) page { return readPage(t, target, key) }
			// Exact client default, not the formerly tested query-less URL.
			all := read("/v1/studio/assets?type=all&limit=24", key)
			require.Len(t, all.Items, 24)
			require.NotEmpty(t, all.Cursor)
			require.Equal(t, read("/v1/studio/assets?limit=24", key), all)
			require.Equal(t, read("/v1/studio/assets", key), all)
			require.Len(t, read("/v1/studio/assets?type=all&limit=101", key).Items, 100)
			for _, kind := range []string{"all", "image", "video"} {
				var got []string
				cursor := ""
				for {
					p := read("/v1/studio/assets?type="+kind+"&limit=7&cursor="+url.QueryEscape(cursor), key)
					require.LessOrEqual(t, len(p.Items), 7)
					for _, a := range p.Items {
						if kind != "all" {
							require.Equal(t, kind, a.Type)
						}
						got = append(got, a.ID)
					}
					if p.Cursor == "" {
						break
					}
					require.NotEqual(t, cursor, p.Cursor)
					cursor = p.Cursor
					require.LessOrEqual(t, len(got), 106)
				}
				var want []string
				for n := 0; n < 106; n++ {
					if kind == "all" || kind == "image" && n%2 == 0 || kind == "video" && n%2 == 1 {
						want = append(want, fmt.Sprintf("asset_%032x", (ownerIndex+1)*1000+n))
					}
				}
				require.Equal(t, want, got, "each owner/filter cursor visits only its complete ordered catalog once")
			}
		})
	}
	for _, query := range []string{"type=audio", "type=ALL", "type=all%20", "type=all&limit=0",
		"type=all&limit=-1", "type=all&limit=oops", "type=all&cursor=bad"} {
		w := studioRouteRequest(router, "GET", "/v1/studio/assets?"+query, "owner", "", "")
		require.Equal(t, 400, w.Code, query+": "+w.Body.String())
	}
}
