package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestVideoTariffVerifiedAdministratorCellSignedQuote(t *testing.T) {
	raw, err := os.ReadFile("testdata/atlas_video_verified_admin_cell.json")
	require.NoError(t, err)
	var fixture struct {
		Selection        VideoQuoteRequest   `json:"selection"`
		EvidenceRevision string              `json:"evidenceRevision"`
		ExpectedPrice    string              `json:"expectedPrice"`
		Card             ChannelModelPricing `json:"card"`
	}
	require.NoError(t, json.Unmarshal(raw, &fixture))
	require.Len(t, fixture.EvidenceRevision, 64)
	s, key := videoFixture(t)
	key.Group.ModelPricing, err = normalizeGroupModelPricing(PlatformOpenAI, []ChannelModelPricing{fixture.Card})
	require.NoError(t, err)
	now := time.Now().UTC()
	quote, err := s.QuoteVideo(context.Background(), key, fixture.Selection, now)
	require.NoError(t, err)
	require.Equal(t, "1.66646400", fixture.ExpectedPrice)
	require.Equal(t, fixture.ExpectedPrice, quote.TotalPrice)
	body := make(map[string]any, len(fixture.Selection.Controls)+2)
	for name, value := range fixture.Selection.Controls {
		body[name] = value
	}
	body["model"] = fixture.Selection.Model
	body["prompt"] = "synthetic offline verification"
	payload, err := json.Marshal(body)
	require.NoError(t, err)
	snapshot, err := s.AcceptVideoQuote(context.Background(), key, payload, quote.Token, now)
	require.NoError(t, err)
	require.Equal(t, fixture.ExpectedPrice, snapshot.TotalPrice())
	require.False(t, snapshot.SupplierEstimated())
}

func TestVideoTariffExhaustiveAdministratorNativeParity(t *testing.T) {
	raw, err := os.ReadFile("../../../frontend/src/components/admin/channel/atlasVideoTariffProfiles.json")
	require.NoError(t, err)
	var profiles map[string]struct {
		Mode             string `json:"mode"`
		ContractRevision string `json:"contractRevision"`
		KeysSHA256       string `json:"keysSHA256"`
		Variants         []struct {
			Controls  map[string]any `json:"controls"`
			Durations []any          `json:"durations"`
		} `json:"variants"`
	}
	require.NoError(t, json.Unmarshal(raw, &profiles))
	require.Len(t, profiles, 91)
	total := 0
	for model, profile := range profiles {
		t.Run(model, func(t *testing.T) {
			revision, err := VideoContractRevision(model)
			require.NoError(t, err)
			require.Equal(t, profile.ContractRevision, revision)
			var keys []string
			for _, variant := range profile.Variants {
				for _, duration := range variant.Durations {
					controls := make(map[string]any, len(variant.Controls)+1)
					for key, value := range variant.Controls {
						controls[key] = value
					}
					controls["duration"] = duration
					key, err := VideoTariffKey(VideoQuoteRequest{Model: model, Mode: profile.Mode, Controls: controls})
					require.NoError(t, err)
					keys = append(keys, key)
				}
			}
			sort.Strings(keys)
			hash := sha256.Sum256([]byte(strings.Join(keys, "\n")))
			require.Equal(t, profile.KeysSHA256, hex.EncodeToString(hash[:]))
			total += len(keys)
		})
	}
	require.Equal(t, 13757, total)
}
