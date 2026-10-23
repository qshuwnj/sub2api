//go:build unit

package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestImageQualityQuoteEndpointAdmission(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &OpenAIGatewayHandler{gatewayService: &service.OpenAIGatewayService{}}
	for _, tc := range []struct {
		name string
		key  *service.APIKey
		body string
		code int
	}{
		{"missing key", nil, `{"model":"anything","count":1}`, 401},
		{"group permission", &service.APIKey{Group: &service.Group{Platform: service.PlatformOpenAI}}, `{"model":"anything","count":1}`, 403},
		{"legacy remains available", &service.APIKey{Group: &service.Group{Platform: service.PlatformOpenAI, AllowImageGeneration: true}}, `{"model":"openai/gpt-image-1-mini/text-to-image","count":1}`, 200},
		{"client price not accepted", &service.APIKey{Group: &service.Group{Platform: service.PlatformOpenAI, AllowImageGeneration: true}}, `{"model":"anything","count":1,"unit_price":0}`, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/quote", strings.NewReader(tc.body))
			if tc.key != nil {
				c.Set(string(middleware2.ContextKeyAPIKey), tc.key)
			}
			h.ImageQuote(c)
			require.Equal(t, tc.code, w.Code, w.Body.String())
			if tc.code == 200 {
				var response map[string]any
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
				require.Equal(t, false, response["enrolled"])
				require.NotContains(t, response, "unit_price")
				require.Equal(t, "no-store", w.Header().Get("Cache-Control"))
			}
		})
	}
}
