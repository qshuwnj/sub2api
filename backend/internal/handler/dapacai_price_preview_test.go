package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestDapacaiPricePreviewEndpointsAreAuthenticatedControlOnlyAndFailClosed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &OpenAIGatewayHandler{gatewayService: &service.OpenAIGatewayService{}}
	for _, tc := range []struct {
		name, kind, body string
		auth             bool
		status           int
	}{
		{"video auth", "videos", `{"model":"seedance-2.0","mode":"text","controls":{"duration":5,"ratio":"16:9"}}`, false, 401},
		{"image auth", "images", `{"model":"gpt-image-2.5-cdn","quality":"none","size":"1024x1024","count":1}`, false, 401},
		{"image26 requires fresh admission", "images", `{"model":"gpt-image-2.6","quality":"none","size":"1024x1024","count":1}`, true, 503},
		{"seedance201 requires fresh admission", "videos", `{"model":"seedance-2.0-1","mode":"text","controls":{"duration":15,"ratio":"16:9","resolution":"720p"}}`, true, 503},
		{"no stale cached video admission", "videos", `{"model":"seedance-2.0","mode":"text","controls":{"duration":5,"ratio":"16:9"}}`, true, 503},
		{"no stale cached image admission", "images", `{"model":"gpt-image-2.5-cdn","quality":"none","size":"1024x1024","count":1}`, true, 503},
		{"no video content", "videos", `{"model":"seedance-2.0","mode":"image","controls":{"duration":5,"ratio":"16:9","input":{"media":["private-reference"]}}}`, true, 400},
		{"no video caller price", "videos", `{"model":"seedance-2.0","mode":"text","controls":{"duration":5,"ratio":"16:9","unit_price":0}}`, true, 400},
		{"no image content", "images", `{"model":"gpt-image-2.5-cdn","quality":"none","size":"1024x1024","count":1,"prompt":"private"}`, true, 400},
		{"no image caller price", "images", `{"model":"gpt-image-2.5-cdn","quality":"none","size":"1024x1024","count":1,"total_price":0}`, true, 400},
		{"explicit prompt-only ban", "videos", `{"model":"seedance-2.5-plus","mode":"text","controls":{"duration":10,"ratio":"16:9","resolution":"480p"}}`, true, 400},
		{"plus6 text still requires fresh prices", "videos", `{"model":"seedance-2.5-plus-6","mode":"text","controls":{"duration":30,"ratio":"16:9","resolution":"480p"}}`, true, 503},
		{"plus7 text still requires fresh prices", "videos", `{"model":"seedance-2.5-plus-7","mode":"text","controls":{"duration":15,"ratio":"16:9","resolution":"720p"}}`, true, 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/"+tc.kind+"/quote", strings.NewReader(tc.body))
			if tc.auth {
				c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{ID: 11})
			}
			if tc.kind == "videos" {
				h.VideoQuote(c)
			} else {
				h.ImageQuote(c)
			}
			require.Equal(t, tc.status, w.Code, w.Body.String())
			require.Equal(t, "no-store", w.Header().Get("Cache-Control"))
			require.NotContains(t, w.Body.String(), "quote_token")
			if tc.status == 503 {
				require.Contains(t, w.Body.String(), "pricing_unavailable")
			}
		})
	}
}
