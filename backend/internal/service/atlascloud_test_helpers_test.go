//go:build unit

package service

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// Shared AtlasCloud fixtures were referenced by the pinned release's tests but
// omitted from that commit. Keep this restoration test-only.
type atlasImageUpstream struct {
	HTTPUpstream
	replies             []string
	status              int
	methods, urls, auth []string
	bodies              [][]byte
}

func (u *atlasImageUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.methods = append(u.methods, req.Method)
	u.urls = append(u.urls, req.URL.String())
	u.auth = append(u.auth, req.Header.Get("Authorization"))
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
	}
	u.bodies = append(u.bodies, body)
	reply := u.replies[0]
	if len(u.replies) > 1 {
		u.replies = u.replies[1:]
	}
	status := u.status
	if status == 0 {
		status = 200
	}
	return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(reply))}, nil
}
func atlasImageFixture(t *testing.T, upstream *atlasImageUpstream) (*OpenAIGatewayService, *gin.Context, *httptest.ResponseRecorder, *Account, *OpenAIImagesRequest) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := []byte(`{"model":"gpt-image-1","prompt":"draw","size":"1024x1024","response_format":"url"}`)
	c.Request = httptest.NewRequest(http.MethodPost, openAIImagesGenerationsEndpoint, bytes.NewReader(body))
	c.Request.Header.Set("x-api-key", "customer-only")
	svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
	parsed, err := svc.ParseOpenAIImagesRequest(c, body)
	require.NoError(t, err)
	account := &Account{ID: 7, Concurrency: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{
		"api_key": "upstream-only", "base_url": "https://api.atlascloud.ai/api/v1",
		"model_mapping": map[string]any{"gpt-image-1": "bytedance/seedream-v4.5"},
	}}
	return svc, c, w, account, parsed
}
