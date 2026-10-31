package middleware

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStudioAssetBillingClassificationHasNoGenerationOrPrefixBypass(t *testing.T) {
	for _, tt := range []struct {
		method, path string
		want         bool
	}{
		{"GET", "/v1/studio/assets", true},
		{"GET", "/v1/studio/assets/asset_bad", true},
		{"GET", "/v1/studio/assets/asset_bad/content", true},
		{"DELETE", "/v1/studio/assets/asset_bad", true},
		{"POST", "/v1/studio/assets/reconcile", true},
		{"POST", "/v1/studio/assets", false},
		{"POST", "/v1/studio/assets/asset_bad/content", false},
		{"GET", "/v1/studio/assets/asset_bad/content/elsewhere", false},
		{"POST", "/v1/images/generations", false},
		{"GET", "/v1/studio/assetsevil", false},
		{"POST", "/v1/studio/assets/reconcile/generations", false},
		{"GET", "/v1/studio/assets//content", false},
	} {
		t.Run(tt.method+tt.path, func(t *testing.T) { require.Equal(t, tt.want, isStudioAssetMetadataRequest(tt.method, tt.path)) })
	}
}
