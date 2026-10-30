package service

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestVideoSettingsQuoteKeepsChangedNativeControlsInTariffIdentity(t *testing.T) {
	original := videoTestRequest()
	changed := videoTestRequest()
	changed.Controls["generate_audio"] = true
	oldKey, err := VideoTariffKey(original)
	require.NoError(t, err)
	newKey, err := VideoTariffKey(changed)
	require.NoError(t, err)
	require.NotEqual(t, oldKey, newKey)
	changed.Controls["generate_audio"] = "yes"
	_, err = VideoTariffKey(changed)
	require.Error(t, err)
}

func TestVideoSettingsLegacyPricingCannotGuessNonDefaultSettingPrice(t *testing.T) {
	req := videoTestRequest()
	req.Controls["generate_audio"] = true
	s := &OpenAIGatewayService{}
	_, err := s.videoLegacyEstimate(context.Background(), nil, req)
	require.ErrorContains(t, err, "settings require an exact configured quote")
}

func TestVideoSettingsRejectsUnadaptedStructuredControls(t *testing.T) {
	req := VideoQuoteRequest{
		Model: "kwaivgi/kling-v3.0-pro/text-to-video", Mode: "t2v",
		Controls: map[string]any{
			"aspect_ratio": "16:9", "duration": 5, "resolution": "1080P",
			"cfg_scale": 0.5, "sound": true, "multi_shot": true,
			"shot_type": "intelligence",
		},
	}
	_, err := VideoTariffKey(req)
	require.Error(t, err)
}

func TestVideoSettingsLegacyGenerationGate(t *testing.T) {
	for _, tc := range []struct {
		body   string
		reject bool
	}{
		{`{"model":"google/veo3.1/text-to-video","generate_audio":false}`, false},
		{`{"model":"google/veo3.1/text-to-video","generate_audio":true}`, true},
		{`{"model":"bytedance/seedance-2.0/text-to-video","seed":42}`, true},
		{`{"model":"bytedance/seedance-2.0/text-to-video","seed":-1,"bitrate_mode":"standard","generate_audio":true}`, false},
		{`{"model":"kwaivgi/kling-v3.0-pro/image-to-video","sound":true,"multi_shot":true}`, true},
	} {
		err := ValidateLegacyVideoSettings([]byte(tc.body))
		if tc.reject {
			require.ErrorIs(t, err, ErrVideoSettingsQuoteRequired, tc.body)
		} else {
			require.NoError(t, err, tc.body)
		}
	}
}

func TestVideoSettingsSpecialModelUsesCanonicalContract(t *testing.T) {
	for _, id := range []string{atlasCloudGeminiVideoModel, atlasCloudMinimaxVideoModel, atlasCloudGrokVideoModel} {
		c := atlasMediaModel(id, "video")
		require.NotNil(t, c)
		body, err := json.Marshal(map[string]any{"model": id, "prompt": "Camera moves through a valley"})
		require.NoError(t, err)
		_, actual, err := SerializeAtlasCloudVideoRequest(body)
		require.NoError(t, err)
		_, expected, err := buildAtlasMediaVideoPayload(body, c)
		require.NoError(t, err)
		require.JSONEq(t, string(expected), string(actual), id)
	}
}

func TestVideoSettingsNonDefaultLegacyQuoteCannotDowngradeToUnenrolled(t *testing.T) {
	s, key := videoFixture(t)
	key.Group.ModelPricing = nil
	req := videoTestRequest()
	req.Controls["generate_audio"] = true
	quote, err := s.QuoteVideo(context.Background(), key, req, time.Now())
	require.ErrorIs(t, err, ErrVideoSettingsQuoteRequired)
	require.Nil(t, quote)
}

func TestVideoSettingsSignedQuoteBindsChangedControls(t *testing.T) {
	s, key := videoFixture(t)
	req := videoTestRequest()
	req.Controls["generate_audio"] = true
	cell, err := VideoTariffKey(req)
	require.NoError(t, err)
	card := &key.Group.ModelPricing[0]
	card.Intervals[0].TierLabel = cell
	evidence := card.VideoTariff.Cells[videoTestKey]
	card.VideoTariff.Cells = map[string]VideoTariffProvenance{cell: evidence}
	now := time.Now().UTC()
	quote, err := s.QuoteVideo(context.Background(), key, req, now)
	require.NoError(t, err)
	body := []byte(`{"model":"google/veo3.1/text-to-video","prompt":"offline fixture","aspect_ratio":"16:9","duration":8,"resolution":"720p","generate_audio":true}`)
	snapshot, err := s.AcceptVideoQuote(context.Background(), key, body, quote.Token, now)
	require.NoError(t, err)
	require.Equal(t, quote.TotalPrice, snapshot.TotalPrice())
	wrong := []byte(`{"model":"google/veo3.1/text-to-video","prompt":"offline fixture","aspect_ratio":"16:9","duration":8,"resolution":"720p","generate_audio":false}`)
	_, err = s.AcceptVideoQuote(context.Background(), key, wrong, quote.Token, now)
	require.Error(t, err)
}

func TestVideoSettingsStudioParameterParity(t *testing.T) {
	raw, err := os.ReadFile("testdata/atlas_video_settings_requests.json")
	require.NoError(t, err)
	var samples []struct {
		Model string          `json:"model"`
		Field string          `json:"field"`
		Body  json.RawMessage `json:"body"`
	}
	require.NoError(t, json.Unmarshal(raw, &samples))
	require.Len(t, samples, 172)
	for _, sample := range samples {
		t.Run(sample.Model+"/"+sample.Field, func(t *testing.T) {
			_, native, err := SerializeAtlasCloudVideoRequest(sample.Body)
			require.NoError(t, err)
			var expected map[string]any
			require.NoError(t, json.Unmarshal(sample.Body, &expected))
			contract := atlasMediaModel(sample.Model, "video")
			require.NotNil(t, contract)
			for field, schema := range contract.Input.Properties {
				if _, present := expected[field]; !present && schema.Default != nil {
					expected[field] = schema.Default
				}
			}
			expectedJSON, err := json.Marshal(expected)
			require.NoError(t, err)
			require.JSONEq(t, string(expectedJSON), string(native), "gateway must preserve settings and only fill contract defaults")
		})
	}
}
