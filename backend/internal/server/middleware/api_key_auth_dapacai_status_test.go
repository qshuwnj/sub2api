//go:build unit

package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestDapacaiVideoStatusReadBoundary(t *testing.T) {
	for _, root := range []string{"/v1/videos/", "/videos/"} {
		require.True(t, isDapacaiVideoStatusRead(http.MethodGet, root+"dapacai:task_abc-123"))
		for _, task := range []string{
			"", "task_abc", "dapacai:", "dapacai:task_", "dapacai:../keys",
			"dapacai:task_abc/content", "dapacai:task_abc/receipt",
			"dapacai:task_abc/extra", "dapacai:task_abc%2Fcontent", "dapacai:task_abc\\content",
		} {
			require.False(t, isDapacaiVideoStatusRead(http.MethodGet, root+task), task)
		}
		for _, method := range []string{http.MethodPost, http.MethodDelete, http.MethodPut, http.MethodHead} {
			require.False(t, isDapacaiVideoStatusRead(method, root+"dapacai:task_abc"))
		}
	}
	require.False(t, isDapacaiVideoStatusRead(http.MethodGet, "/v1/videos/generations/dapacai:task_abc"))
}

func TestDapacaiStatusAuthenticationPreservesSecurityAndGenerationAdmission(t *testing.T) {
	for _, mode := range []string{config.RunModeStandard, config.RunModeSimple} {
		for _, scenario := range []string{
			"spent", "disabled key", "expired status", "expired time", "inactive user",
			"inactive group", "denied IP", "new generation", "content", "malformed path",
		} {
			t.Run(mode+"/"+scenario, func(t *testing.T) {
				gin.SetMode(gin.TestMode)
				group := &service.Group{ID: 42, Status: service.StatusActive, Hydrated: true, Platform: service.PlatformOpenAI}
				user := &service.User{ID: 7, Status: service.StatusActive, Balance: 0}
				key := &service.APIKey{ID: 100, UserID: user.ID, Key: "test-key", User: user, Group: group, GroupID: &group.ID,
					Status: service.StatusAPIKeyQuotaExhausted, Quota: 1, QuotaUsed: 1}
				method, path := http.MethodGet, "/v1/videos/dapacai:task_abc"
				switch scenario {
				case "disabled key":
					key.Status = service.StatusDisabled
				case "expired status":
					key.Status = service.StatusAPIKeyExpired
				case "expired time":
					expired := time.Now().Add(-time.Hour)
					key.Status, key.ExpiresAt = service.StatusActive, &expired
				case "inactive user":
					user.Status = service.StatusDisabled
				case "inactive group":
					group.Status = service.StatusDisabled
				case "denied IP":
					key.IPWhitelist = []string{"10.20.30.40"}
				case "new generation":
					method, path = http.MethodPost, "/v1/videos"
				case "content":
					path += "/content"
				case "malformed path":
					path += "/extra"
				}
				repo := &stubApiKeyRepo{getByKey: func(context.Context, string) (*service.APIKey, error) {
					copy := *key
					return &copy, nil
				}}
				cfg := &config.Config{RunMode: mode}
				keys := service.NewAPIKeyService(repo, nil, nil, nil, nil, nil, cfg)
				router := gin.New()
				router.Use(gin.HandlerFunc(NewAPIKeyAuthMiddleware(keys, nil, cfg)))
				reached := false
				router.Handle(method, path, func(c *gin.Context) {
					reached = true
					c.Status(http.StatusNoContent)
				})
				req := httptest.NewRequest(method, path, nil)
				req.Header.Set("X-API-Key", "test-key")
				w := httptest.NewRecorder()
				router.ServeHTTP(w, req)
				want := http.StatusNoContent
				switch scenario {
				case "disabled key", "inactive user":
					want = http.StatusUnauthorized
				case "expired status", "expired time", "inactive group", "denied IP":
					want = http.StatusForbidden
				case "new generation", "content", "malformed path":
					if mode == config.RunModeStandard {
						want = http.StatusTooManyRequests
					}
				}
				require.Equal(t, want, w.Code, w.Body.String())
				require.Equal(t, want == http.StatusNoContent, reached)
			})
		}
	}
}
