package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestVideoQuoteRejectsContentAndCallerPrices(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &OpenAIGatewayHandler{gatewayService: &service.OpenAIGatewayService{}}
	for _, tc := range []struct {
		body string
		auth bool
		code int
	}{
		{`{"model":"anything","mode":"t2v","controls":{}}`, false, 401},
		{`{"model":"anything","mode":"t2v","controls":{},"total_price":"0"}`, true, 400},
		{`{"model":"anything","mode":"t2v","controls":{},"prompt":"secret"}`, true, 400},
		{`{"model":"anything","mode":"t2v","controls":{}} {}`, true, 400},
		{`{"model":"google/veo3.1/image-to-video","mode":"i2v","controls":{"aspect_ratio":"16:9","duration":8,"generate_audio":false,"resolution":"720p","end_image":"https://assets.example/secret.png"}}`, true, 400},
		{`{"model":"google/veo3.1/text-to-video","mode":"t2v","controls":{"aspect_ratio":"16:9","duration":8,"generate_audio":false,"resolution":"720p"}}` + strings.Repeat(" ", 17000) + `{}`, true, 400},
	} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos/quote", strings.NewReader(tc.body))
		if tc.auth {
			c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{ID: 11})
		}
		h.VideoQuote(c)
		require.Equal(t, tc.code, w.Code, w.Body.String())
		require.Equal(t, "no-store", w.Header().Get("Cache-Control"))
	}
}
