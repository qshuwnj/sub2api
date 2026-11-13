package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestAsyncImageAtlasCountValidation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &AsyncImageHandler{openAI: &OpenAIGatewayHandler{gatewayService: &service.OpenAIGatewayService{}}}
	for _, tc := range []struct {
		options string
		valid   bool
	}{
		{`"n":1`, true},
		{`"num_images":1`, true},
		{`"n":1,"num_images":1`, true},
		{`"n":1,"num_images":2`, false},
		{`"n":2,"num_images":1`, false},
		{`"n":1,"num_images":"1"`, false},
		{`"n":1,"num_images":1.5`, false},
		{`"n":1,"seed":123`, false},
		{`"n":1,"resolution":"4k"`, false},
	} {
		t.Run(tc.options, func(t *testing.T) {
			body := []byte(`{"model":"xai/grok-imagine-image-2.0-developer/text-to-image","prompt":"Offline fixture",` + tc.options + `}`)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations/async", strings.NewReader(string(body)))
			c.Request.Header.Set("Content-Type", "application/json")
			err := h.validateRequest(c, service.PlatformOpenAI, body)
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}
