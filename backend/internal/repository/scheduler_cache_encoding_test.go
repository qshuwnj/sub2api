//go:build unit

package repository

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestSchedulerAccountCompressionRoundTrip(t *testing.T) {
	account := service.Account{
		ID: 73, Platform: service.PlatformOpenAI,
		Credentials: map[string]any{"large_mapping": strings.Repeat("exact-value-", 100000)},
		GroupIDs:    []int64{24},
	}
	raw, err := json.Marshal(account)
	require.NoError(t, err)
	full, meta, err := marshalSchedulerCacheAccount(account)
	require.NoError(t, err)
	require.Less(t, len(full), len(raw)/10)
	wantMeta, err := json.Marshal(buildSchedulerMetadataAccount(account))
	require.NoError(t, err)
	require.Equal(t, wantMeta, meta)
	expanded, err := expandSchedulerAccountPayload(full)
	require.NoError(t, err)
	require.Equal(t, raw, expanded)
	want, err := decodeCachedAccount(raw)
	require.NoError(t, err)
	for _, val := range []any{full, string(full)} {
		got, err := decodeCachedAccount(val)
		require.NoError(t, err)
		require.Equal(t, want, got)
	}
}

func TestSchedulerAccountCompressionRejectsDamagedPayload(t *testing.T) {
	raw := bytes.Repeat([]byte("repeated-cache-payload"), 10000)
	compressed, err := encodeSchedulerAccountPayload(raw)
	require.NoError(t, err)
	for _, broken := range [][]byte{
		{0x1f, 0x8b},
		compressed[:len(compressed)-1],
		append(append([]byte{}, compressed[:len(compressed)-1]...), compressed[len(compressed)-1]^0xff),
	} {
		_, err := decodeCachedAccount(broken)
		require.Error(t, err)
	}
}

func TestSchedulerAccountCompressionBoundsExpansion(t *testing.T) {
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	chunk := bytes.Repeat([]byte("a"), 1<<20)
	for i := 0; i <= schedulerCacheMaxExpandedBytes/len(chunk); i++ {
		_, err := writer.Write(chunk)
		require.NoError(t, err)
	}
	require.NoError(t, writer.Close())
	_, err := expandSchedulerAccountPayload(compressed.Bytes())
	require.ErrorContains(t, err, "expansion limit")
}

func TestSchedulerAccountCompressionPreservesProductionTariffs(t *testing.T) {
	filename := os.Getenv("SUB2API_TEST_PRICING_DOCUMENT")
	if filename == "" {
		t.Skip("optional private production pricing fixture not provided")
	}
	data, err := os.ReadFile(filename)
	require.NoError(t, err)
	var pricing struct {
		ModelPricing []service.ChannelModelPricing `json:"model_pricing"`
	}
	require.NoError(t, json.Unmarshal(data, &pricing))
	require.NotEmpty(t, pricing.ModelPricing)
	account := service.Account{
		ID: 73, GroupIDs: []int64{24},
		Groups: []*service.Group{{ID: 24, ModelPricing: pricing.ModelPricing}},
	}
	raw, err := json.Marshal(account)
	require.NoError(t, err)
	full, _, err := marshalSchedulerCacheAccount(account)
	require.NoError(t, err)
	expanded, err := expandSchedulerAccountPayload(full)
	require.NoError(t, err)
	require.Equal(t, raw, expanded)
	require.Less(t, len(full), len(raw)/5)
	got, err := decodeCachedAccount(full)
	require.NoError(t, err)
	want, err := decodeCachedAccount(raw)
	require.NoError(t, err)
	require.Equal(t, want, got)
	t.Logf("full account JSON=%d bytes, compressed=%d bytes", len(raw), len(full))
}
