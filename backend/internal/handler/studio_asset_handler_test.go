package handler

import (
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestStudioAssetPublicJSONExcludesPrivateFields(t *testing.T) {
	body, err := json.Marshal(studioAssetDTO(&service.StudioAsset{ID: "asset_0123456789abcdef0123456789abcdef", Owner: service.StudioAssetOwner{UserID: 16, APIKeyID: 20}, StorageKey: "private/object", SourceURL: "https://private.example/file", SourceBytes: []byte("private bytes"), LeaseToken: "private lease", Metadata: json.RawMessage(`{"aspect_ratio":"3:4","duration":8,"seed":1,"width":{"source_url":"private nested"},"source_url":"private url","account_id":99}`)}))
	require.NoError(t, err)
	for _, secret := range []string{"private", "api_key_id", "source_url", "account_id", "width"} {
		require.NotContains(t, string(body), secret)
	}
	require.Contains(t, string(body), `"metadata":{"aspect_ratio":"3:4","duration":8,"seed":1}`)
}

func TestStudioAssetPublicMetadataPreservesIntegerSeeds(t *testing.T) {
	body, err := json.Marshal(studioAssetDTO(&service.StudioAsset{Metadata: json.RawMessage(`{"seed":9007199254740993}`)}))
	require.NoError(t, err)
	require.Contains(t, string(body), `"seed":9007199254740993`)
}
