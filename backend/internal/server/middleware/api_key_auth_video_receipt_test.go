//go:build unit

package middleware

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestVideoReceiptReadCannotExemptAnotherVideoOperation(t *testing.T) {
	for _, prefix := range []string{"/v1", ""} {
		for _, task := range []string{"atlascloud:job", "seedance:task-ark", "native-grok-job"} {
			require.True(t, isVideoAttemptRead(http.MethodGet, prefix+"/videos/"+task+"/receipt"))
			require.False(t, isVideoAttemptRead(http.MethodPost, prefix+"/videos/"+task+"/receipt"))
		}
		for _, reserved := range []string{"generations", "edits", "extensions", "quote", "grok-video:job"} {
			require.False(t, isVideoAttemptRead(http.MethodGet, prefix+"/videos/"+reserved+"/receipt"), reserved)
		}
		require.True(t, isVideoAttemptRead(http.MethodGet, prefix+"/videos/attempts/receipt"),
			"an owned attempt named receipt must retain its existing status-read behavior")
		require.False(t, isVideoAttemptRead(http.MethodGet, prefix+"/videos/native-job/receipt/extra"))
	}
}
