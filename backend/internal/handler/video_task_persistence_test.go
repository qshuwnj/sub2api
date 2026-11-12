package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestAcceptedVideoSaveFailurePreservesIDAndForbidsGenerationRetry(t *testing.T) {
	for _, dapacai := range []bool{false, true} {
		source := gin.H{"id": "original-job", "status": "queued"}
		result := &service.OpenAIForwardResult{ResponseID: "original-job"}
		if dapacai {
			result.DapacaiVideoResponse = source
		} else {
			result.AtlasCloudVideoResponse = source
		}
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		respondAcceptedVideoPersistenceFailure(c, result)
		require.Equal(t, http.StatusAccepted, w.Code)
		require.Equal(t, "15", w.Header().Get("Retry-After"))
		var body map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
		require.Equal(t, "original-job", body["id"])
		require.Equal(t, "queued", body["status"])
		require.Equal(t, false, body["retry_generation"])
		require.Equal(t, "unavailable", body["persistence_status"])
		require.Len(t, source, 2, "do not mutate provider response")
	}
}
