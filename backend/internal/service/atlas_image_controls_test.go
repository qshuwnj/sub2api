//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// Captured from the real Studio serializer, independently checked against the
// official schemas. Only the upstream HTTP transport is replaced; no paid job.
func TestAtlasImageRatioQualityForwardMatrix(t *testing.T) {
	raw, err := os.ReadFile("testdata/atlas_image_controls_requests.json")
	require.NoError(t, err)
	var fixture struct {
		Requests []struct {
			Model string          `json:"model"`
			Body  json.RawMessage `json:"body"`
		} `json:"requests"`
	}
	require.NoError(t, json.Unmarshal(raw, &fixture))
	require.NotEmpty(t, fixture.Requests)
	models := map[string]bool{}
	qualities := map[string]bool{}
	for index, sample := range fixture.Requests {
		t.Run(fmt.Sprintf("%s/%d", sample.Model, index), func(t *testing.T) {
			models[sample.Model] = true
			upstream := &atlasImageUpstream{replies: []string{
				`{"code":200,"data":{"id":"offline-image","status":"completed","outputs":["https://storage.atlascloud.ai/test/output.png"]}}`,
			}}
			svc, c, w, account, _ := atlasImageFixture(t, upstream)
			c.Request = httptest.NewRequest(http.MethodPost, openAIImagesGenerationsEndpoint, bytes.NewReader(sample.Body))
			c.Request.Header.Set("Content-Type", "application/json")
			req, err := svc.ParseOpenAIImagesRequest(c, sample.Body)
			require.NoError(t, err)
			result, err := svc.forwardOpenAIImagesAPIKey(context.Background(), c, account, sample.Body, req, "")
			require.NoError(t, err)
			require.Equal(t, 1, result.ImageCount)
			require.Len(t, upstream.bodies, 1)
			require.Equal(t, "https://api.atlascloud.ai/api/v1/model/generateImage", upstream.urls[0])
			var output any
			require.NoError(t, json.Unmarshal(upstream.bodies[0], &output))
			require.NoError(t, validateAtlasMediaValue(output, atlasMediaModel(sample.Model, "image").Input, "request", true))
			for _, field := range []string{"model", "aspect_ratio", "size", "resolution", "quality"} {
				want := gjson.GetBytes(sample.Body, field)
				if want.Exists() {
					require.Equal(t, want.Value(), gjson.GetBytes(upstream.bodies[0], field).Value(), field)
				}
			}
			if gjson.GetBytes(sample.Body, "quality").Exists() {
				qualities[sample.Model] = true
			}
			require.Empty(t, w.Body.String(), "output must still wait for settlement")
		})
	}
	require.Len(t, models, 58)
	require.Len(t, qualities, 18)
}
