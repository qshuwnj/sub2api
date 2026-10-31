package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func settingsCalculatorFixture(t *testing.T, estimated bool) (*videoPriceCalculator, *atomic.Int32) {
	t.Helper()
	calls := &atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		require.Equal(t, "/api/v1/model/calculate", r.URL.Path)
		require.Empty(t, r.Header.Get("Authorization"))
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		require.Equal(t, videoTestRequest().Model, body["model"])
		for _, field := range []string{"prompt", "negative_prompt", "image", "images", "api_key"} {
			require.NotContains(t, body, field)
		}
		price := "1.6"
		if body["generate_audio"] == true {
			price = "3.2"
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"code":200,"data":{"price":%q,"estimated":%t}}`, price, estimated)
	}))
	t.Cleanup(server.Close)
	client := server.Client()
	client.Transport = videoCalculatorTestTransport{target: server.URL, next: client.Transport}
	return newVideoPriceCalculator(client), calls
}

type videoCalculatorTestTransport struct {
	target string
	next   http.RoundTripper
}

func (r videoCalculatorTestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	url := *req.URL
	target, _ := http.NewRequest(http.MethodPost, r.target, nil)
	url.Scheme, url.Host = target.URL.Scheme, target.URL.Host
	clone.URL = &url
	return r.next.RoundTrip(clone)
}

func TestVideoAdvancedSettingsCalculatorPreservesDefaultRetailAndBindsActualControls(t *testing.T) {
	s, key := videoFixture(t)
	s.videoCalculator, _ = settingsCalculatorFixture(t, false)
	price := 9.12345678
	key.Group.ModelPricing[0].Intervals[0].PerRequestPrice = &price
	key.Group.RateMultiplier = 1.5
	req := videoTestRequest()
	req.Controls["generate_audio"] = true
	now := time.Now()
	quote, err := s.QuoteVideo(context.Background(), key, req, now)
	require.NoError(t, err)
	require.Equal(t, "27.37037034", quote.TotalPrice)
	require.False(t, quote.SupplierEstimated)
	require.Equal(t, "calculator_settings", quote.PriceSource)
	body := []byte(`{"model":"google/veo3.1/text-to-video","prompt":"private scene","aspect_ratio":"16:9","duration":8,"resolution":"720p","generate_audio":true}`)
	snapshot, err := s.AcceptVideoQuote(context.Background(), key, body, quote.Token, now)
	require.NoError(t, err)
	require.Equal(t, "18.24691356", snapshot.BasePrice())
	wrong := []byte(`{"model":"google/veo3.1/text-to-video","prompt":"private scene","aspect_ratio":"16:9","duration":8,"resolution":"720p","generate_audio":false}`)
	_, err = s.AcceptVideoQuote(context.Background(), key, wrong, quote.Token, now)
	require.Error(t, err)
	defaultQuote, err := s.QuoteVideo(context.Background(), key, videoTestRequest(), now)
	require.NoError(t, err)
	require.Equal(t, "13.68518517", defaultQuote.TotalPrice)
	require.Equal(t, price, *key.Group.ModelPricing[0].Intervals[0].PerRequestPrice)
	price = 10
	_, err = s.AcceptVideoQuote(context.Background(), key, body, quote.Token, now)
	require.Error(t, err, "editing the existing retail baseline invalidates the advanced quote")
}

func TestVideoAdvancedSettingsConfiguredRuleUsesSupplierRatioAndHonestEstimateFlag(t *testing.T) {
	for _, estimated := range []bool{false, true} {
		t.Run(fmt.Sprint(estimated), func(t *testing.T) {
			s, key := videoFixture(t)
			var calls *atomic.Int32
			s.videoCalculator, calls = settingsCalculatorFixture(t, estimated)
			key.Group.ModelPricing = nil
			price := 0.25
			key.Group.VideoPrice720P = &price
			key.Group.RateMultiplier = 1.5
			req := videoTestRequest()
			req.Controls["generate_audio"] = true
			now := time.Now()
			quote, err := s.QuoteVideo(context.Background(), key, req, now)
			require.NoError(t, err)
			require.Equal(t, "6.00000000", quote.TotalPrice)
			require.Equal(t, estimated, quote.SupplierEstimated)
			_, err = s.AcceptVideoQuote(context.Background(), key, fixedVideoBody(t, req), quote.Token, now)
			require.NoError(t, err)
			require.EqualValues(t, 2, calls.Load(), "acceptance reuses exact cached public evidence")
			quote, err = s.QuoteVideo(context.Background(), key, videoTestRequest(), now)
			require.NoError(t, err)
			require.Equal(t, "3.00000000", quote.TotalPrice)
			require.Equal(t, estimated, quote.SupplierEstimated)
		})
	}
}

type videoCalculatorRoundTrip func(*http.Request) (*http.Response, error)

func (f videoCalculatorRoundTrip) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestVideoCalculatorInvalidEvidenceCannotEnableAdvancedGeneration(t *testing.T) {
	for _, response := range []string{
		`{"code":200,"data":{"price":"0"}}`,
		`{"code":200,"data":{"price":"-1"}}`,
		`{"code":200,"data":{"price":"1e5"}}`,
		`{"code":200,"data":{"price":1.6}}`,
		`{"code":200,"data":{"price":"1.6","estimated":"false"}}`,
		`{"code":200,"data":{"price":"1.6","estimated":null}}`,
		`{"code":400,"msg":"not found"}`,
		`<html>Too many requests</html>`,
		strings.Repeat("x", 65537),
	} {
		t.Run(response[:min(len(response), 80)], func(t *testing.T) {
			s, key := videoFixture(t)
			var calls int
			s.videoCalculator = newVideoPriceCalculator(&http.Client{Transport: videoCalculatorRoundTrip(func(req *http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(response))}, nil
			})})
			req := videoTestRequest()
			req.Controls["generate_audio"] = true
			for range 2 {
				quote, err := s.QuoteVideo(context.Background(), key, req, time.Now())
				require.ErrorIs(t, err, ErrVideoQuoteUnavailable, "supplier failures are temporary price lookup errors, not unsupported model settings")
				require.Nil(t, quote)
			}
			require.Equal(t, 1, calls, "negative cache prevents calculator retry storms")
			quote, err := s.QuoteVideo(context.Background(), key, videoTestRequest(), time.Now())
			require.NoError(t, err, "calculator failure must not disable an exact configured default tariff")
			require.Equal(t, "13.33171200", quote.TotalPrice)
		})
	}
}

func TestVideoAdvancedSettingsDoesNotExpandExplicitFreeCellToUnpricedSettings(t *testing.T) {
	s, key := videoFixture(t)
	s.videoCalculator, _ = settingsCalculatorFixture(t, false)
	zero := 0.0
	key.Group.ModelPricing[0].Intervals[0].PerRequestPrice = &zero
	evidence := key.Group.ModelPricing[0].VideoTariff.Cells[videoTestKey]
	evidence.ExplicitFree = true
	key.Group.ModelPricing[0].VideoTariff.Cells[videoTestKey] = evidence
	req := videoTestRequest()
	req.Controls["generate_audio"] = true
	quote, err := s.QuoteVideo(context.Background(), key, req, time.Now())
	require.Error(t, err)
	require.Nil(t, quote)
}

func TestVideoAdvancedSettingsAllStudioSettingsCanLockTheirActualSerializedControls(t *testing.T) {
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
			s, key := videoFixture(t)
			key.Group.ModelPricing = nil
			flat := 0.25
			key.Group.VideoPrice480P, key.Group.VideoPrice720P, key.Group.VideoPrice1080P = &flat, &flat, &flat
			s.videoCalculator = newVideoPriceCalculator(&http.Client{Transport: videoCalculatorRoundTrip(func(req *http.Request) (*http.Response, error) {
				var body map[string]any
				require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
				for field := range body {
					require.False(t, field == "prompt" || field == "negative_prompt")
					_, media := atlasMediaModel(sample.Model, "video").MediaFields[field]
					require.False(t, media)
				}
				price := "1"
				if videoLegacySettings(atlasMediaModel(sample.Model, "video"), body) != nil {
					price = "2"
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(
					fmt.Sprintf(`{"code":200,"data":{"price":%q}}`, price)))}, nil
			})})
			_, native, err := SerializeAtlasCloudVideoRequest(sample.Body)
			require.NoError(t, err)
			var body map[string]any
			require.NoError(t, json.Unmarshal(native, &body))
			contract := atlasMediaModel(sample.Model, "video")
			controls := map[string]any{}
			for field, value := range body {
				if field == "model" || field == "prompt" || field == "negative_prompt" {
					continue
				}
				if _, media := contract.MediaFields[field]; !media {
					controls[field] = value
				}
			}
			req := VideoQuoteRequest{Model: sample.Model, Mode: contract.Mode, Controls: controls}
			now := time.Now()
			quote, err := s.QuoteVideo(context.Background(), key, req, now)
			require.NoError(t, err)
			require.True(t, quote.Enrolled)
			require.False(t, quote.EstimateOnly)
			snapshot, err := s.AcceptVideoQuote(context.Background(), key, sample.Body, quote.Token, now)
			require.NoError(t, err)
			require.Equal(t, quote.TotalPrice, snapshot.TotalPrice())
			require.Equal(t, sample.Model, snapshot.Selection().Model)
		})
	}
}

func TestVideoCalculatorRateLimitProtectsOtherUncachedSettingsWithoutDisablingCachedEvidence(t *testing.T) {
	calls := 0
	calculator := newVideoPriceCalculator(&http.Client{Transport: videoCalculatorRoundTrip(func(req *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"code":200,"data":{"price":"1.6"}}`))}, nil
		}
		return &http.Response{StatusCode: 429, Body: io.NopCloser(strings.NewReader("rate limited"))}, nil
	})})
	req := videoTestRequest()
	_, err := calculator.lookup(context.Background(), req)
	require.NoError(t, err)
	changed := videoTestRequest()
	changed.Controls["generate_audio"] = true
	_, err = calculator.lookup(context.Background(), changed)
	require.Error(t, err)
	changed.Controls["duration"] = 4
	_, err = calculator.lookup(context.Background(), changed)
	require.Error(t, err)
	require.Equal(t, 2, calls, "an upstream 429 must back off across distinct settings, not only one cache key")
	value, err := calculator.lookup(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, "1.6", value.Price.String())
}

func TestVideoCalculatorRejectsRedirectAndDoesNotLeakContentOrCredentials(t *testing.T) {
	calls := 0
	calculator := newVideoPriceCalculator(&http.Client{Transport: videoCalculatorRoundTrip(func(req *http.Request) (*http.Response, error) {
		calls++
		require.Equal(t, VideoCalculatorURL, req.URL.String())
		require.Equal(t, http.MethodPost, req.Method)
		require.Empty(t, req.Header.Get("Authorization"))
		require.Empty(t, req.Header.Get("Cookie"))
		return &http.Response{StatusCode: http.StatusTemporaryRedirect,
			Header: http.Header{"Location": {"https://untrusted.example/collect"}},
			Body:   io.NopCloser(strings.NewReader("")), Request: req}, nil
	})})
	_, err := calculator.lookup(context.Background(), videoTestRequest())
	require.ErrorIs(t, err, ErrVideoQuoteUnavailable)
	require.Equal(t, 1, calls)
}
