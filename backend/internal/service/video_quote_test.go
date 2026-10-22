package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

const videoTestKey = "video:v1:WyJnb29nbGUvdmVvMy4xL3RleHQtdG8tdmlkZW8iLCJ0MnYiLHsiYXNwZWN0X3JhdGlvIjoiMTY6OSIsImR1cmF0aW9uIjo4LCJnZW5lcmF0ZV9hdWRpbyI6ZmFsc2UsInJlc29sdXRpb24iOiI3MjBwIn1d"

type videoQuoteKeys struct {
	apiKeyRateLimitLoader
	key *APIKey
}

func (r *videoQuoteKeys) GetByID(context.Context, int64) (*APIKey, error) { return r.key, nil }

type videoQuoteUsers struct {
	UserRepository
	user *User
}

func (r *videoQuoteUsers) GetByID(context.Context, int64) (*User, error) { return r.user, nil }

type videoQuoteGroups struct {
	GroupRepository
	group *Group
}

func (r *videoQuoteGroups) GetByIDLite(context.Context, int64) (*Group, error) { return r.group, nil }

type videoQuoteChannels struct {
	ChannelRepository
	channel *Channel
	err     error
}

func (r *videoQuoteChannels) GetChannelIDByGroupID(context.Context, int64) (int64, error) {
	if r.err != nil {
		return 0, r.err
	}
	if r.channel != nil {
		return r.channel.ID, nil
	}
	return 0, nil
}
func (r *videoQuoteChannels) GetByID(context.Context, int64) (*Channel, error) {
	return r.channel, r.err
}

type videoQuoteAccounts struct {
	AccountRepository
	accounts []Account
}

func (r *videoQuoteAccounts) ListSchedulableByGroupIDAndPlatform(context.Context, int64, string) ([]Account, error) {
	return r.accounts, nil
}

type videoQuoteBilling struct {
	UsageBillingRepository
	ready bool
}

func (r *videoQuoteBilling) VideoQuotedBillingReady() bool { return r.ready }

type videoQuoteRates struct {
	UserGroupRateRepository
	rate *float64
	err  error
}

func (r *videoQuoteRates) GetByUserAndGroup(context.Context, int64, int64) (*float64, error) {
	return r.rate, r.err
}

func videoTestRequest() VideoQuoteRequest {
	var req VideoQuoteRequest
	_ = json.Unmarshal([]byte(`{"model":"google/veo3.1/text-to-video","mode":"t2v","controls":{"aspect_ratio":"16:9","duration":8,"generate_audio":false,"resolution":"720p"}}`), &req)
	return req
}

func videoFixture(t *testing.T) (*OpenAIGatewayService, *APIKey) {
	t.Helper()
	req := videoTestRequest()
	rev, err := VideoContractRevision(req.Model)
	require.NoError(t, err)
	price := 13.331712 // 1.60 * 8.33232, already converted retail tariff.
	g := &Group{ID: 7, Status: StatusActive, Platform: PlatformOpenAI, AllowImageGeneration: true, RateMultiplier: 1}
	g.ModelPricing = []ChannelModelPricing{{Platform: PlatformOpenAI, Models: []string{req.Model}, BillingMode: BillingModeVideo,
		Intervals: []PricingInterval{{TierLabel: videoTestKey, PerRequestPrice: &price}},
		VideoTariff: &VideoTariffMetadata{Version: 1, ContractRevision: rev, Cells: map[string]VideoTariffProvenance{
			videoTestKey: {Source: VideoCalculatorURL, CheckedAt: "2026-10-22T00:00:00Z", UpstreamPrice: "1.60", Policy: VideoRetailPolicy},
		}},
	}}
	u := &User{ID: 3, Status: StatusActive, Balance: 100}
	k := &APIKey{ID: 11, UserID: 3, Status: StatusActive, GroupID: &g.ID, Group: g, User: u}
	s := &OpenAIGatewayService{
		cfg:      &config.Config{JWT: config.JWTConfig{Secret: strings.Repeat("test", 12)}},
		userRepo: &videoQuoteUsers{user: u}, channelService: &ChannelService{groupRepo: &videoQuoteGroups{group: g}, repo: &videoQuoteChannels{}},
		billingCacheService: &BillingCacheService{apiKeyRateLimitLoader: &videoQuoteKeys{key: k}},
		usageBillingRepo:    &videoQuoteBilling{ready: true},
		accountRepo: &videoQuoteAccounts{accounts: []Account{{ID: 30, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
			Status: StatusActive, Schedulable: true, GroupIDs: []int64{7},
			Credentials: map[string]any{"base_url": "https://api.atlascloud.ai", "api_key": "synthetic"}}}},
	}
	return s, k
}

func TestVideoTariffRawDecimalStorageAndQuote(t *testing.T) {
	for _, tc := range []struct {
		price string
		valid bool
	}{
		{"0.100000000000000001", false},
		{"999999999999.12345678", false},
		{"9999999999.12345678", true},
	} {
		t.Run(tc.price, func(t *testing.T) {
			s, key := videoFixture(t)
			raw, err := json.Marshal(key.Group.ModelPricing)
			require.NoError(t, err)
			raw = []byte(strings.Replace(string(raw), `"per_request_price":13.331712`, `"per_request_price":`+tc.price, 1))
			var cards []ChannelModelPricing
			err = json.Unmarshal(raw, &cards)
			if !tc.valid {
				if err == nil {
					_, err = normalizeGroupModelPricing(PlatformOpenAI, cards)
				}
				require.Error(t, err, "the original overprecision decimal must be rejected")
				return
			}
			require.NoError(t, err)
			cards, err = normalizeGroupModelPricing(PlatformOpenAI, cards)
			require.NoError(t, err)
			cloned := (&Channel{ModelPricing: cards}).Clone()
			saved, err := json.Marshal(cloned.ModelPricing)
			require.NoError(t, err)
			var wire []struct {
				Intervals []struct {
					Price json.Number `json:"per_request_price"`
				} `json:"intervals"`
			}
			require.NoError(t, json.Unmarshal(saved, &wire))
			require.Equal(t, tc.price, wire[0].Intervals[0].Price.String())
			require.NoError(t, json.Unmarshal(saved, &key.Group.ModelPricing))
			now := time.Now().UTC()
			quote, err := s.QuoteVideo(context.Background(), key, videoTestRequest(), now)
			require.NoError(t, err)
			require.Equal(t, tc.price, quote.TotalPrice)
			body := []byte(`{"model":"google/veo3.1/text-to-video","prompt":"synthetic","aspect_ratio":"16:9","duration":8,"generate_audio":false,"resolution":"720p"}`)
			snapshot, err := s.AcceptVideoQuote(context.Background(), key, body, quote.Token, now)
			require.NoError(t, err)
			require.Equal(t, tc.price, snapshot.BasePrice())
			require.Equal(t, tc.price, snapshot.TotalPrice())
			replacement := 2.5
			key.Group.ModelPricing[0].Intervals[0].PerRequestPrice = &replacement
			changed, err := s.QuoteVideo(context.Background(), key, videoTestRequest(), now)
			require.NoError(t, err)
			require.Equal(t, "2.50000000", changed.TotalPrice)
		})
	}
}

func TestVideoTariffRawDecimalMutationDoesNotReuseStalePrice(t *testing.T) {
	var interval PricingInterval
	require.NoError(t, json.Unmarshal([]byte(`{"tier_label":"video:v1:synthetic","per_request_price":999999999999.12345678}`), &interval))
	price := 2.5
	interval.PerRequestPrice = &price
	raw, err := json.Marshal(interval)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"per_request_price":2.5`)
}

func TestVideoTariffRawDecimalSameProjectionPointerReplacement(t *testing.T) {
	for name, clone := range map[string]func(ChannelModelPricing) ChannelModelPricing{
		"value copy": func(card ChannelModelPricing) ChannelModelPricing { return card },
		"card clone": func(card ChannelModelPricing) ChannelModelPricing { return card.Clone() },
		"channel clone": func(card ChannelModelPricing) ChannelModelPricing {
			return (&Channel{ModelPricing: []ChannelModelPricing{card}}).Clone().ModelPricing[0]
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, key := videoFixture(t)
			raw, err := json.Marshal(key.Group.ModelPricing)
			require.NoError(t, err)
			raw = []byte(strings.Replace(string(raw), `"per_request_price":13.331712`, `"per_request_price":9999999999.12345678`, 1))
			require.NoError(t, json.Unmarshal(raw, &key.Group.ModelPricing))
			key.Group.ModelPricing[0] = clone(key.Group.ModelPricing[0])
			unchanged, err := json.Marshal(key.Group.ModelPricing)
			require.NoError(t, err)
			require.Contains(t, string(unchanged), `"per_request_price":9999999999.12345678`)
			now := time.Now().UTC()
			oldQuote, err := s.QuoteVideo(context.Background(), key, videoTestRequest(), now)
			require.NoError(t, err)
			require.Equal(t, "9999999999.12345678", oldQuote.TotalPrice)
			replacement := 9999999999.123457
			require.Equal(t, *key.Group.ModelPricing[0].Intervals[0].PerRequestPrice, replacement, "both values have the same binary64 projection")
			key.Group.ModelPricing[0].Intervals[0].PerRequestPrice = &replacement
			saved, err := json.Marshal(key.Group.ModelPricing)
			require.NoError(t, err)
			require.Contains(t, string(saved), `"per_request_price":9999999999.123457`)
			require.NotContains(t, string(saved), `"per_request_price":9999999999.12345678`)
			newQuote, err := s.QuoteVideo(context.Background(), key, videoTestRequest(), now)
			require.NoError(t, err)
			require.Equal(t, "9999999999.12345700", newQuote.TotalPrice)
			require.NoError(t, json.Unmarshal(saved, &key.Group.ModelPricing))
			reloaded, err := s.QuoteVideo(context.Background(), key, videoTestRequest(), now)
			require.NoError(t, err)
			require.Equal(t, newQuote.TotalPrice, reloaded.TotalPrice)
			body := []byte(`{"model":"google/veo3.1/text-to-video","prompt":"synthetic","aspect_ratio":"16:9","duration":8,"generate_audio":false,"resolution":"720p"}`)
			_, err = s.AcceptVideoQuote(context.Background(), key, body, oldQuote.Token, now)
			require.Error(t, err, "the previous exact-price quote must become stale")
			snapshot, err := s.AcceptVideoQuote(context.Background(), key, body, newQuote.Token, now)
			require.NoError(t, err)
			require.Equal(t, "9999999999.12345700", snapshot.TotalPrice())
		})
	}
}

func TestVideoQuoteRejectsAmountOutsideSharedLedgerRange(t *testing.T) {
	for name, multiplied := range map[string]bool{"base": false, "post multiplier": true} {
		t.Run(name, func(t *testing.T) {
			s, key := videoFixture(t)
			price := 10000000000.0
			if multiplied {
				price = 5000000000.0
				key.Group.VideoRateIndependent = true
				key.Group.VideoRateMultiplier = 2
			}
			key.Group.ModelPricing[0].Intervals[0].PerRequestPrice = &price
			_, err := s.QuoteVideo(context.Background(), key, videoTestRequest(), time.Now())
			require.Error(t, err, "numeric(20,10) usage ledger cannot settle this quote")
		})
	}
}

func TestVideoTariffLiteralKeyAndRetail(t *testing.T) {
	key, err := VideoTariffKey(videoTestRequest())
	require.NoError(t, err)
	require.Equal(t, videoTestKey, key)
	for _, tc := range []struct{ supplier, retail string }{
		{"1.60", "13.33171200"}, {"3.20", "26.66342400"}, {"1.514799", "12.62179000"}, {"3.017079", "25.13926769"},
	} {
		got, err := VideoRetailPrice(tc.supplier)
		require.NoError(t, err)
		require.Equal(t, tc.retail, got)
	}
	for _, price := range []string{"", "-1", "NaN", "Infinity", "1e2", "0", "1.1234567890123456789"} {
		_, err := VideoRetailPrice(price)
		require.Error(t, err, price)
	}
}

func TestVideoTariffNativeControlsAndConditionals(t *testing.T) {
	for _, body := range []string{
		`{"duration":"8"}`, `{"duration":-1}`, `{"aspect_ratio":"adaptive"}`, `{"generate_audio":true}`,
		`{"resolution":"1080p","duration":4}`, `{"prompt":"content"}`, `{"seed":12}`, `{"image":"https://assets.example/test.png"}`,
	} {
		req := videoTestRequest()
		var patch map[string]any
		require.NoError(t, json.Unmarshal([]byte(body), &patch))
		for k, v := range patch {
			req.Controls[k] = v
		}
		_, err := VideoTariffKey(req)
		require.Error(t, err, body)
	}
	req := videoTestRequest()
	delete(req.Controls, "generate_audio")
	_, err := VideoTariffKey(req)
	require.Error(t, err, "pinned defaults must be explicit in tariff keys")
	stringDuration := VideoQuoteRequest{Model: "kwaivgi/kling-v2.0-t2v-master", Mode: "t2v", Controls: map[string]any{"aspect_ratio": "16:9", "duration": "5"}}
	key, err := VideoTariffKey(stringDuration)
	require.NoError(t, err)
	require.Equal(t, "video:v1:WyJrd2FpdmdpL2tsaW5nLXYyLjAtdDJ2LW1hc3RlciIsInQydiIseyJhc3BlY3RfcmF0aW8iOiIxNjo5IiwiZHVyYXRpb24iOiI1In1d", key)
	stringDuration.Controls["duration"] = float64(5)
	_, err = VideoTariffKey(stringDuration)
	require.Error(t, err)
}

func TestVideoTariffValidation(t *testing.T) {
	for _, name := range []string{"duplicate cell", "duplicate card", "missing", "negative", "nan", "infinity", "overprecision", "zero", "metadata mismatch", "wildcard", "legacy fallback", "extra provenance"} {
		t.Run(name, func(t *testing.T) {
			_, k := videoFixture(t)
			c := &k.Group.ModelPricing[0]
			switch name {
			case "duplicate cell":
				c.Intervals = append(c.Intervals, c.Intervals[0])
			case "duplicate card":
				k.Group.ModelPricing = append(k.Group.ModelPricing, *c)
			case "missing":
				c.Intervals[0].PerRequestPrice = nil
			case "negative":
				x := -1.0
				c.Intervals[0].PerRequestPrice = &x
			case "nan":
				x := math.NaN()
				c.Intervals[0].PerRequestPrice = &x
			case "infinity":
				x := math.Inf(1)
				c.Intervals[0].PerRequestPrice = &x
			case "overprecision":
				x := 0.123456789
				c.Intervals[0].PerRequestPrice = &x
			case "zero":
				x := 0.0
				c.Intervals[0].PerRequestPrice = &x
			case "metadata mismatch":
				c.VideoTariff.ContractRevision = strings.Repeat("0", 64)
			case "wildcard":
				c.Models = []string{"google/*"}
			case "legacy fallback":
				x := 1.0
				c.PerRequestPrice = &x
			case "extra provenance":
				c.VideoTariff.Cells["wrong"] = c.VideoTariff.Cells[videoTestKey]
			}
			_, err := normalizeGroupModelPricing(PlatformOpenAI, k.Group.ModelPricing)
			require.Error(t, err)
		})
	}
	_, k := videoFixture(t)
	c := &k.Group.ModelPricing[0]
	x := 0.0
	c.Intervals[0].PerRequestPrice = &x
	p := c.VideoTariff.Cells[videoTestKey]
	p.ExplicitFree = true
	c.VideoTariff.Cells[videoTestKey] = p
	_, err := normalizeGroupModelPricing(PlatformOpenAI, k.Group.ModelPricing)
	require.NoError(t, err)
}

func TestVideoQuoteSignedSnapshotAndRevalidation(t *testing.T) {
	s, k := videoFixture(t)
	now := time.Now().UTC().Truncate(time.Second)
	q, err := s.QuoteVideo(context.Background(), k, videoTestRequest(), now)
	require.NoError(t, err)
	require.True(t, q.Enrolled)
	require.Equal(t, "13.33171200", q.TotalPrice)
	require.Equal(t, now.Add(5*time.Minute), q.ExpiresAt)
	raw, err := json.Marshal(q)
	require.NoError(t, err)
	var response map[string]any
	require.NoError(t, json.Unmarshal(raw, &response))
	require.Equal(t, "13.33171200", response["total_price"])
	require.Equal(t, "USD", response["currency"])
	require.Equal(t, "job", response["billing_unit"])
	require.Equal(t, float64(1), response["unit_count"])
	require.NotEmpty(t, response["contract_revision"])
	require.NotContains(t, response, "total")
	body := []byte(`{"model":"google/veo3.1/text-to-video","prompt":"synthetic","duration":8,"aspect_ratio":"16:9","resolution":"720p","generate_audio":false}`)
	snapshot, err := s.AcceptVideoQuote(context.Background(), k, body, q.Token, now)
	require.NoError(t, err)
	require.Equal(t, "13.33171200", snapshot.TotalPrice())
	for _, name := range []string{"tamper", "cross key", "cross group", "expired", "changed tariff", "changed multiplier", "changed body", "key disabled", "user disabled", "group disabled", "permission", "no account", "wrong endpoint account", "account remap", "not ready"} {
		t.Run(name, func(t *testing.T) {
			s, k := videoFixture(t)
			quote, err := s.QuoteVideo(context.Background(), k, videoTestRequest(), now)
			require.NoError(t, err)
			token, b, at := quote.Token, body, now
			switch name {
			case "tamper":
				token += "a"
			case "cross key":
				copy := *k
				copy.ID++
				k = &copy
			case "cross group":
				copy := *k
				id := int64(99)
				copy.GroupID = &id
				k = &copy
			case "expired":
				at = now.Add(5 * time.Minute)
			case "changed tariff":
				x := 1.0
				k.Group.ModelPricing[0].Intervals[0].PerRequestPrice = &x
			case "changed multiplier":
				k.Group.VideoRateIndependent = true
				k.Group.VideoRateMultiplier = 2
			case "changed body":
				b = []byte(strings.ReplaceAll(string(body), `"720p"`, `"1080p"`))
			case "key disabled":
				k.Status = StatusAPIKeyDisabled
			case "user disabled":
				k.User.Status = "disabled"
			case "group disabled":
				k.Group.Status = "disabled"
			case "permission":
				k.Group.AllowImageGeneration = false
			case "no account":
				s.accountRepo.(*videoQuoteAccounts).accounts = nil
			case "wrong endpoint account":
				s.accountRepo.(*videoQuoteAccounts).accounts[0].Credentials["base_url"] = "https://api.openai.com"
			case "account remap":
				s.accountRepo.(*videoQuoteAccounts).accounts[0].Credentials["model_mapping"] = map[string]any{k.Group.ModelPricing[0].Models[0]: "other"}
			case "not ready":
				s.usageBillingRepo.(*videoQuoteBilling).ready = false
			}
			_, err = s.AcceptVideoQuote(context.Background(), k, b, token, at)
			require.Error(t, err)
		})
	}
	x := 1.0
	k.Group.ModelPricing[0].Intervals[0].PerRequestPrice = &x
	require.Equal(t, "13.33171200", snapshot.TotalPrice(), "already accepted snapshots never reprice")
	copy := snapshot.Selection()
	copy.Controls["duration"] = float64(4)
	require.Equal(t, float64(8), snapshot.Selection().Controls["duration"])
}

func TestVideoQuoteReadinessFreshAdmissionAndExactEnrollment(t *testing.T) {
	s, k := videoFixture(t)
	s.usageBillingRepo = nil
	_, err := s.QuoteVideo(context.Background(), k, videoTestRequest(), time.Now())
	require.Error(t, err)
	s, k = videoFixture(t)
	stale := *k
	k.Status = StatusAPIKeyDisabled
	_, err = s.QuoteVideo(context.Background(), &stale, videoTestRequest(), time.Now())
	require.Error(t, err)
	s, k = videoFixture(t)
	k.Group.ModelPricing[0].Models = []string{"google/*"}
	_, err = s.VideoPricingEnrolled(k, videoTestRequest().Model)
	require.Error(t, err, "malformed enrolled wildcard cannot silently become legacy")
	s, k = videoFixture(t)
	k.Group.ModelPricing = nil
	s.usageBillingRepo = nil
	q, err := s.QuoteVideo(context.Background(), k, videoTestRequest(), time.Now())
	require.NoError(t, err)
	require.False(t, q.Enrolled)
	require.Empty(t, q.Token)
	require.Empty(t, q.TotalPrice)
}

func TestVideoQuoteRefreshesUserAndGroupBeyondCachedKey(t *testing.T) {
	for _, kind := range []string{"user", "group"} {
		t.Run(kind, func(t *testing.T) {
			s, k := videoFixture(t)
			cached := *k
			oldUser := *k.User
			oldGroup := *k.Group
			cached.User = &oldUser
			cached.Group = &oldGroup
			if kind == "user" {
				s.userRepo.(*videoQuoteUsers).user.Status = "disabled"
			} else {
				s.channelService.groupRepo.(*videoQuoteGroups).group.AllowImageGeneration = false
			}
			_, err := s.QuoteVideo(context.Background(), &cached, videoTestRequest(), time.Now())
			require.Error(t, err)
		})
	}
}

func TestVideoTariffNumericConditionalAndImageRegression(t *testing.T) {
	var schema atlasMediaSchema
	require.NoError(t, json.Unmarshal([]byte(`{"type":"object","properties":{"duration":{"type":"integer"},"resolution":{"type":"string"}},"allOf":[{"if":{"properties":{"resolution":{"const":"1080p"}}},"then":{"properties":{"duration":{"minimum":8,"maximum":8}}}}]}`), &schema))
	require.Error(t, validateAtlasMediaValue(map[string]any{"duration": float64(4), "resolution": "1080p"}, &schema, "request", true))
	require.NoError(t, validateAtlasMediaValue(map[string]any{"duration": float64(8), "resolution": "1080p"}, &schema, "request", true))
	require.NoError(t, validateAtlasMediaValue(map[string]any{"duration": float64(4), "resolution": "720p"}, &schema, "request", true))
	// A partial without a type must also preserve string/array image constraints.
	var imageSchema atlasMediaSchema
	require.NoError(t, json.Unmarshal([]byte(`{"type":"object","properties":{"prompt":{"type":"string"},"images":{"type":"array","items":{"type":"string"}}},"allOf":[{"properties":{"prompt":{"maxLength":3},"images":{"minItems":1}}}]}`), &imageSchema))
	require.NoError(t, validateAtlasMediaValue(map[string]any{"prompt": "abc", "images": []any{"https://assets.example/a.png"}}, &imageSchema, "image", true))
	require.Error(t, validateAtlasMediaValue(map[string]any{"prompt": "abcd", "images": []any{"https://assets.example/a.png"}}, &imageSchema, "image", true))
	require.Error(t, validateAtlasMediaValue(map[string]any{"prompt": "abc", "images": []any{}}, &imageSchema, "image", true))
}

func TestVideoQuoteFreshAdmissionConstraints(t *testing.T) {
	for _, name := range []string{"expired key", "deleted user", "key reassigned", "group reassigned", "exclusive group", "allowlist", "channel remap", "channel restricted", "channel failure", "expired account", "account mapping absent", "account disabled", "account quota", "account credential absent", "wrong account group", "wrong mode"} {
		t.Run(name, func(t *testing.T) {
			s, k := videoFixture(t)
			req := videoTestRequest()
			now := time.Now()
			switch name {
			case "expired key":
				k.ExpiresAt = &now
			case "deleted user":
				k.User.DeletedAt = &now
			case "key reassigned":
				fresh := *k
				fresh.UserID = 99
				s.billingCacheService.apiKeyRateLimitLoader.(*videoQuoteKeys).key = &fresh
			case "group reassigned":
				fresh := *k
				gid := int64(99)
				fresh.GroupID = &gid
				s.billingCacheService.apiKeyRateLimitLoader.(*videoQuoteKeys).key = &fresh
			case "exclusive group":
				k.Group.IsExclusive = true
			case "allowlist":
				k.Group.ModelAllowlist = GroupModelAllowlist{Enabled: true, Models: []string{"different"}}
			case "channel remap":
				s.channelService.repo.(*videoQuoteChannels).channel = &Channel{ID: 1, Status: StatusActive, GroupIDs: []int64{7}, ModelMapping: map[string]map[string]string{PlatformOpenAI: {req.Model: "other"}}}
			case "channel restricted":
				s.channelService.repo.(*videoQuoteChannels).channel = &Channel{ID: 1, Status: StatusActive, GroupIDs: []int64{7}, RestrictModels: true}
			case "channel failure":
				s.channelService.repo.(*videoQuoteChannels).err = errors.New("database unavailable")
			case "expired account":
				a := &s.accountRepo.(*videoQuoteAccounts).accounts[0]
				a.AutoPauseOnExpired = true
				a.ExpiresAt = &now
			case "account mapping absent":
				s.accountRepo.(*videoQuoteAccounts).accounts[0].Credentials["model_mapping"] = map[string]any{"different": "different"}
			case "account disabled":
				s.accountRepo.(*videoQuoteAccounts).accounts[0].Status = "disabled"
			case "account quota":
				s.accountRepo.(*videoQuoteAccounts).accounts[0].Extra = map[string]any{"quota_limit": 1.0, "quota_used": 2.0}
			case "account credential absent":
				delete(s.accountRepo.(*videoQuoteAccounts).accounts[0].Credentials, "api_key")
			case "wrong account group":
				s.accountRepo.(*videoQuoteAccounts).accounts[0].GroupIDs = []int64{99}
			case "wrong mode":
				req.Mode = "i2v"
			}
			_, err := s.QuoteVideo(context.Background(), k, req, now)
			require.Error(t, err, name)
		})
	}
}

func TestVideoQuoteMultiplierAndMetadataRoundTrip(t *testing.T) {
	s, k := videoFixture(t)
	k.Group.RateMultiplier = 1.5
	k.Group.ImageRateIndependent = true
	k.Group.ImageRateMultiplier = 99
	q, err := s.QuoteVideo(context.Background(), k, videoTestRequest(), time.Now())
	require.NoError(t, err)
	require.Equal(t, "19.99756800", q.TotalPrice)
	rate := 0.75
	s.userGroupRateResolver = &userGroupRateResolver{repo: &videoQuoteRates{rate: &rate}}
	q, err = s.QuoteVideo(context.Background(), k, videoTestRequest(), time.Now())
	require.NoError(t, err)
	require.Equal(t, "9.99878400", q.TotalPrice)
	s.userGroupRateResolver.repo.(*videoQuoteRates).err = errors.New("rate database unavailable")
	_, err = s.QuoteVideo(context.Background(), k, videoTestRequest(), time.Now())
	require.ErrorIs(t, err, ErrVideoQuoteUnavailable)
	s.userGroupRateResolver = nil
	k.Group.VideoRateIndependent = true
	k.Group.VideoRateMultiplier = 2
	q, err = s.QuoteVideo(context.Background(), k, videoTestRequest(), time.Now())
	require.NoError(t, err)
	require.Equal(t, "26.66342400", q.TotalPrice)
	k.Group.VideoRateMultiplier = -1
	_, err = s.QuoteVideo(context.Background(), k, videoTestRequest(), time.Now())
	require.Error(t, err)
	raw, err := json.Marshal(k.Group.ModelPricing)
	require.NoError(t, err)
	var cards []ChannelModelPricing
	require.NoError(t, json.Unmarshal(raw, &cards))
	require.Equal(t, k.Group.ModelPricing[0].VideoTariff, cards[0].VideoTariff)
	copy := cards[0].Clone()
	delete(copy.VideoTariff.Cells, videoTestKey)
	require.Len(t, cards[0].VideoTariff.Cells, 1)
}

func TestVideoQuoteMissingCellProvenanceAndDomain(t *testing.T) {
	s, k := videoFixture(t)
	now := time.Now()
	req := videoTestRequest()
	req.Controls["duration"] = float64(4)
	_, err := s.QuoteVideo(context.Background(), k, req, now)
	require.Error(t, err)
	prov := k.Group.ModelPricing[0].VideoTariff.Cells[videoTestKey]
	prov.SupplierEstimated = true
	k.Group.ModelPricing[0].VideoTariff.Cells[videoTestKey] = prov
	q, err := s.QuoteVideo(context.Background(), k, videoTestRequest(), now)
	require.NoError(t, err)
	require.True(t, q.SupplierEstimated)
	parts := strings.Split(q.Token, ".")
	mac, err := s.quoteMAC(parts[0])
	require.NoError(t, err)
	token := parts[0] + "." + base64.RawURLEncoding.EncodeToString(mac)
	body := []byte(`{"model":"google/veo3.1/text-to-video","prompt":"synthetic","duration":8,"aspect_ratio":"16:9","resolution":"720p","generate_audio":false}`)
	_, err = s.AcceptVideoQuote(context.Background(), k, body, token, now)
	require.ErrorContains(t, err, "signature")
}

func TestVideoTariffStudioContractParity(t *testing.T) {
	raw, err := os.ReadFile("testdata/atlas_video_tariff_contracts.json")
	require.NoError(t, err)
	var cells []struct {
		Selection VideoQuoteRequest `json:"selection"`
		Key       string            `json:"key"`
		Revision  string            `json:"revision"`
	}
	require.NoError(t, json.Unmarshal(raw, &cells))
	require.NotEmpty(t, cells)
	for _, cell := range cells {
		key, err := VideoTariffKey(cell.Selection)
		require.NoError(t, err, cell.Selection.Model)
		require.Equal(t, cell.Key, key, cell.Selection.Model)
		revision, err := VideoContractRevision(cell.Selection.Model)
		require.NoError(t, err)
		require.Equal(t, cell.Revision, revision, cell.Selection.Model)
	}
}

func TestVideoTariffUnsupportedSchemaTypeFailsClosed(t *testing.T) {
	model := videoTestRequest().Model
	original := videoRawContracts[model]
	raw, err := json.Marshal(original)
	require.NoError(t, err)
	var mutated map[string]any
	require.NoError(t, json.Unmarshal(raw, &mutated))
	mutated["input"].(map[string]any)["properties"].(map[string]any)["duration"].(map[string]any)["type"] = "null"
	videoRawContracts[model] = mutated
	t.Cleanup(func() { videoRawContracts[model] = original })
	_, err = VideoTariffKey(videoTestRequest())
	require.Error(t, err)
}

func TestVideoTariffFixedResolutionDefaultPreservesLegacyLookup(t *testing.T) {
	model := "kwaivgi/kling-v3.0-4k/image-to-video"
	got, ok := LookupVideoBillingResolutionForModel(model, "1080P")
	require.True(t, ok)
	require.Equal(t, "1080p", got)
	_, ok = LookupVideoBillingResolutionForModel(model, "4k")
	require.False(t, ok, "model-name 4k never invents a native 4k request value")
}
