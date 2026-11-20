//go:build unit

package middleware

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestVideoAttemptRecoveryAuthenticationBoundary(t *testing.T) {
	const native = `{"model":"google/veo3.1/text-to-video","prompt":"private"}`
	for _, scenario := range []string{"owned spent", "owned expired", "new", "foreign", "model mismatch", "legacy model", "lookup failure", "disabled", "inactive user", "inactive group", "denied IP", "other route", "body limit", "get attempt", "get native", "get attempt content", "get native content", "get quote"} {
		t.Run(scenario, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			group := &service.Group{ID: 42, Status: service.StatusActive, Hydrated: true, Platform: service.PlatformOpenAI}
			user := &service.User{ID: 7, Status: service.StatusActive, Balance: 0}
			key := &service.APIKey{ID: 100, UserID: 7, Key: "test-key", User: user, Group: group, GroupID: &group.ID,
				Status: service.StatusAPIKeyQuotaExhausted, Quota: 1, QuotaUsed: 1}
			if scenario == "owned expired" {
				expired := time.Now().Add(-time.Hour)
				key.Status, key.ExpiresAt = service.StatusAPIKeyExpired, &expired
			}
			if scenario == "disabled" {
				key.Status = service.StatusDisabled
			}
			if scenario == "inactive user" {
				user.Status = service.StatusDisabled
			}
			if scenario == "inactive group" {
				group.Status = service.StatusDisabled
			}
			if scenario == "denied IP" {
				key.IPWhitelist = []string{"10.20.30.40"}
			}
			repo := &stubApiKeyRepo{getByKey: func(context.Context, string) (*service.APIKey, error) {
				copy := *key
				return &copy, nil
			}}
			cfg := &config.Config{RunMode: config.RunModeStandard}
			keys := service.NewAPIKeyService(repo, nil, nil, nil, nil, nil, cfg)
			lookups := 0
			keys.SetVideoAttemptRecoveryLookup(func(_ context.Context, owner service.VideoAttemptOwner, id string) (*service.VideoAttempt, error) {
				lookups++
				require.Equal(t, service.VideoAttemptOwner{UserID: 7, APIKeyID: 100}, owner)
				require.Equal(t, "attempt", id)
				if scenario == "lookup failure" {
					return nil, errors.New("storage credentials must not leak")
				}
				if scenario == "new" || scenario == "foreign" {
					return nil, service.ErrVideoAttemptNotFound
				}
				return &service.VideoAttempt{ID: id, UserID: 7, APIKeyID: 100,
					Selection: service.VideoQuoteRequest{Model: "google/veo3.1/text-to-video"}}, nil
			})
			path, method, body := "/v1/videos/generations", http.MethodPost, native
			switch scenario {
			case "model mismatch":
				body = `{"model":"google/veo3.1/image-to-video","prompt":"different"}`
			case "legacy model":
				body = `{"model":"grok-imagine-video","prompt":"different"}`
			case "other route":
				path = "/v1/images/generations"
			case "body limit":
				body += strings.Repeat(" ", 1024)
			case "get attempt":
				path, method, body = "/v1/videos/attempts/attempt", http.MethodGet, ""
			case "get native":
				path, method, body = "/v1/videos/atlascloud:job", http.MethodGet, ""
			case "get attempt content":
				path, method, body = "/v1/videos/attempts/attempt/content", http.MethodGet, ""
			case "get native content":
				path, method, body = "/v1/videos/atlascloud:job/content", http.MethodGet, ""
			case "get quote":
				path, method, body = "/v1/videos/quote", http.MethodGet, ""
			}
			router := gin.New()
			router.Use(RequestBodyLimit(512), gin.HandlerFunc(NewAPIKeyAuthMiddleware(keys, nil, cfg)))
			reached := false
			router.Handle(method, path, func(c *gin.Context) {
				reached = true
				got, err := io.ReadAll(c.Request.Body)
				require.NoError(t, err)
				require.Equal(t, body, string(got), "middleware must restore the whole native body")
				c.Status(204)
			})
			req := httptest.NewRequest(method, path, strings.NewReader(body))
			req.Header.Set("X-API-Key", "test-key")
			req.Header.Set("Idempotency-Key", "attempt")
			req.Header.Set("X-Video-Quote", "expired")
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)
			switch scenario {
			case "owned spent", "owned expired", "get attempt", "get native", "get attempt content", "get native content":
				require.Equal(t, 204, w.Code, w.Body.String())
				require.True(t, reached)
			case "lookup failure":
				require.Equal(t, 503, w.Code, w.Body.String())
				require.NotContains(t, w.Body.String(), "credentials")
			case "disabled", "inactive user":
				require.Equal(t, 401, w.Code, w.Body.String())
				require.Zero(t, lookups, "base authentication still precedes recovery")
			case "inactive group", "denied IP":
				require.Equal(t, 403, w.Code, w.Body.String())
				require.Zero(t, lookups, "group and IP authorization still precede recovery")
			case "body limit":
				require.Equal(t, 413, w.Code, w.Body.String())
			default:
				require.Equal(t, 429, w.Code, w.Body.String())
				require.False(t, reached, "headers alone cannot bypass new-generation enforcement")
			}
		})
	}
}

func TestVideoAttemptRecoveryRejectsConflictingFieldsBeforeLookup(t *testing.T) {
	cases := []struct {
		name, contentType, body string
	}{
		{"model", "application/json", `{"model":"google/veo3.1/text-to-video","model":"other"}`},
		{"identical model", "application/json", `{"model":"google/veo3.1/text-to-video","model":"google/veo3.1/text-to-video"}`},
		{"case variant", "application/json", `{"model":"google/veo3.1/text-to-video","Model":"other"}`},
		{"escaped name", "application/json", `{"model":"google/veo3.1/text-to-video","mo\u0064el":"other"}`},
		{"nested", "application/json", `{"model":"google/veo3.1/text-to-video","options":{"duration":5,"duration":10}}`},
		{"BOM and misleading type", "multipart/form-data; boundary=test", "\xef\xbb\xbf" + `{"model":"google/veo3.1/text-to-video","model":"other"}`},
		{"multipart", "multipart/form-data; boundary=test", "--test\r\nContent-Disposition: form-data; name=\"model\"\r\n\r\ngoogle/veo3.1/text-to-video\r\n--test\r\nContent-Disposition: form-data; name=\"model\"\r\n\r\nother\r\n--test--\r\n"},
	}
	for _, mode := range []string{config.RunModeStandard, config.RunModeSimple} {
		for _, path := range []string{"/v1/videos/generations", "/videos/generations"} {
			for _, tc := range cases {
				t.Run(mode+path+"/"+tc.name, func(t *testing.T) {
					gin.SetMode(gin.TestMode)
					group := &service.Group{ID: 42, Status: service.StatusActive, Hydrated: true, Platform: service.PlatformOpenAI}
					key := &service.APIKey{ID: 100, UserID: 7, Key: "test-key", Group: group, GroupID: &group.ID,
						User:   &service.User{ID: 7, Status: service.StatusActive},
						Status: service.StatusAPIKeyQuotaExhausted, Quota: 1, QuotaUsed: 1}
					repo := &stubApiKeyRepo{getByKey: func(context.Context, string) (*service.APIKey, error) {
						copy := *key
						return &copy, nil
					}}
					cfg := &config.Config{RunMode: mode}
					keys := service.NewAPIKeyService(repo, nil, nil, nil, nil, nil, cfg)
					lookups := 0
					keys.SetVideoAttemptRecoveryLookup(func(_ context.Context, _ service.VideoAttemptOwner, id string) (*service.VideoAttempt, error) {
						lookups++
						return &service.VideoAttempt{ID: id, UserID: 7, APIKeyID: 100,
							Selection: service.VideoQuoteRequest{Model: "google/veo3.1/text-to-video"}}, nil
					})
					router := gin.New()
					router.Use(RequestBodyLimit(512), gin.HandlerFunc(NewAPIKeyAuthMiddleware(keys, nil, cfg)))
					reached := false
					router.POST(path, func(c *gin.Context) {
						reached = true
						c.Status(http.StatusNoContent)
					})
					req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(tc.body))
					req.Header.Set("X-API-Key", "test-key")
					req.Header.Set("Idempotency-Key", "attempt")
					req.Header.Set("Content-Type", tc.contentType)
					w := httptest.NewRecorder()
					router.ServeHTTP(w, req)
					require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
					require.Contains(t, w.Body.String(), "more than once")
					require.Zero(t, lookups, "ambiguous input must not authorize a recovery")
					require.False(t, reached, "ambiguous input must not reach recovery or generation")
				})
			}
		}
	}
}
