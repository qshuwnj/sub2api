package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const (
	DapacaiEndpointVideoCreate           GrokMediaEndpoint        = "dapacai_video_create"
	DapacaiEndpointVideoStatus           GrokMediaEndpoint        = "dapacai_video_status"
	DapacaiEndpointVideoContent          GrokMediaEndpoint        = "dapacai_video_content"
	OpenAIEndpointCapabilityDapacaiVideo OpenAIEndpointCapability = "dapacai_video"
)

func (e GrokMediaEndpoint) IsDapacaiVideo() bool {
	return e == DapacaiEndpointVideoCreate || e == DapacaiEndpointVideoStatus || e == DapacaiEndpointVideoContent
}

type dapacaiVideoProfile struct {
	durations                                      []int
	ratios, resolutions                            []string
	images, videos, audios                         int
	frames, requiresReference, audioOnly, disabled bool
}

// Public guide + OpenAPI verified 2026-11-06. Never infer arbitrary bare IDs or
// use AtlasCloud contracts, defaults, prices or alternate speed/model variants.
func dapacaiVideoModel(model string) *dapacaiVideoProfile {
	p := &dapacaiVideoProfile{ratios: []string{"16:9", "9:16", "1:1"}, images: 10}
	durationRange := func(min, max int) []int {
		var out []int
		for i := min; i <= max; i++ {
			out = append(out, i)
		}
		return out
	}
	wide := []string{"16:9", "9:16", "1:1", "4:3", "3:4", "21:9"}
	switch model {
	case "seedance-2.0":
		p.durations, p.ratios, p.frames = []int{5, 10, 15}, wide, true
	case "seedance-2.0-plus":
		p.durations, p.resolutions = []int{5, 10, 15}, []string{"480p", "720p", "1080p"}
		p.images, p.videos, p.audios = 9, 3, 3
	case "seedance-2.5-plus":
		p.durations, p.ratios, p.resolutions = durationRange(4, 10), append(wide, "adaptive"), []string{"480p"}
		p.images, p.audios, p.requiresReference, p.audioOnly = 30, 10, true, true
	case "seedance-2.5-plus-1", "seedance-2.5-plus-2", "seedance-2.5-plus-5", "wan-3.0":
		p.durations = durationRange(5, 30)
	case "seedance-2.5-plus-3":
		p.durations, p.resolutions, p.ratios = []int{30}, []string{"720p", "1080p"}, append(p.ratios, "3:4")
	case "seedance-2.5-plus-4":
		p.durations, p.resolutions = []int{15, 30}, []string{"720p"}
		p.ratios, p.images, p.videos, p.audios, p.audioOnly = wide[:5], 9, 3, 3, true
	case "seedance-2.5-plus-6", "seedance-2.5-plus-7":
		p.durations, p.ratios, p.resolutions = durationRange(4, 30), wide, []string{"480p"}
		p.images, p.audios, p.requiresReference, p.audioOnly = 30, 10, true, true
		if model == "seedance-2.5-plus-7" {
			p.resolutions = append(p.resolutions, "720p")
		}
	case "minimax-h3":
		p.durations, p.disabled = durationRange(5, 30), true
	default:
		return nil
	}
	return p
}

func IsDapacaiVideoModel(model string) bool { return dapacaiVideoModel(model) != nil }

func dapacaiAPIBaseValid(base string) bool {
	u, err := url.Parse(base)
	return err == nil && u.Scheme == "https" && u.Host == "api.dapacai.com" &&
		u.User == nil && u.RawQuery == "" && u.Fragment == "" &&
		(u.Path == "" || u.Path == "/" || u.Path == "/v1" || u.Path == "/v1/")
}

var dapacaiTaskID = regexp.MustCompile(`^task_[A-Za-z0-9_-]{1,110}$`)
var dapacaiMediaName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
var dapacaiIPLiteral = regexp.MustCompile(`^[0-9.]+$`)

func dapacaiCompletedStatus(body []byte, task string) bool {
	return gjson.GetBytes(body, "status").String() == "completed" && "dapacai:"+gjson.GetBytes(body, "id").String() == task
}

func dapacaiVideoURL(task string, content bool) (string, error) {
	id, ok := strings.CutPrefix(task, "dapacai:")
	if !ok || !dapacaiTaskID.MatchString(id) {
		return "", errors.New("invalid Dapacai task ID")
	}
	target := "https://api.dapacai.com/v1/videos/" + id
	if content {
		target += "/content"
	}
	return target, nil
}

type dapacaiMaterial struct {
	Type string `json:"type,omitempty"`
	URL  string `json:"url"`
	Name string `json:"name,omitempty"`
}
type dapacaiVideoRequest struct {
	Model      string   `json:"model"`
	Prompt     string   `json:"prompt"`
	Duration   int      `json:"duration"`
	Ratio      string   `json:"ratio"`
	Resolution string   `json:"resolution,omitempty"`
	Images     []string `json:"images,omitempty"`
	Input      *struct {
		Prompt string            `json:"prompt"`
		Media  []dapacaiMaterial `json:"media"`
	} `json:"input,omitempty"`
}

func ParseDapacaiVideoRequest(body []byte) (GrokMediaRequestInfo, error) {
	var info GrokMediaRequestInfo
	var req dapacaiVideoRequest
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if len(body) > 65536 || decoder.Decode(&req) != nil {
		return info, errors.New("invalid Dapacai video JSON")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return info, errors.New("invalid Dapacai video JSON")
	}
	p := dapacaiVideoModel(req.Model)
	if p == nil || p.disabled {
		return info, errors.New("Dapacai video model is not available")
	}
	if !slices.Contains(p.durations, req.Duration) || !slices.Contains(p.ratios, req.Ratio) {
		return info, errors.New("unsupported Dapacai duration or aspect ratio")
	}
	if len(p.resolutions) == 0 && req.Resolution != "" || len(p.resolutions) > 0 && !slices.Contains(p.resolutions, req.Resolution) {
		return info, errors.New("unsupported Dapacai resolution")
	}
	media := make([]dapacaiMaterial, 0, len(req.Images))
	for _, image := range req.Images {
		media = append(media, dapacaiMaterial{Type: "reference_image", URL: image})
	}
	prompt := req.Prompt
	if req.Input != nil {
		if len(req.Images) > 0 {
			return info, errors.New("use images or input.media, not both")
		}
		if req.Input.Prompt != prompt {
			return info, errors.New("nested and top-level prompts must match")
		}
		media = req.Input.Media
	}
	if strings.TrimSpace(prompt) == "" || utf8.RuneCountInString(prompt) > 8000 {
		return info, errors.New("video prompt must contain 1-8000 characters")
	}
	counts, names := map[string]int{}, map[string]bool{}
	for _, item := range media {
		u, err := url.Parse(item.URL)
		if err != nil || u.Scheme != "https" || u.User != nil || u.Fragment != "" || len(item.URL) > 2000 ||
			validatePublicVideoReferenceHost(u.Hostname()) != nil {
			return info, errors.New("Dapacai references require final public HTTPS URLs")
		}
		if item.Name != "" {
			if !dapacaiMediaName.MatchString(item.Name) || names[item.Name] {
				return info, errors.New("invalid or duplicate reference name")
			}
			names[item.Name] = true
		}
		kind := item.Type
		if req.Model == "seedance-2.0-plus" && (kind == "reference_image" || kind == "reference_audio") {
			return info, errors.New("this Dapacai model uses image/audio/video material roles")
		}
		switch kind {
		case "", "image", "reference_image":
			kind = "image"
		case "first_frame", "last_frame":
			if !p.frames {
				return info, errors.New("this Dapacai model does not support start/end frames")
			}
		case "audio", "reference_audio":
			kind = "audio"
		case "video":
		default:
			return info, errors.New("unsupported Dapacai reference type")
		}
		counts[kind]++
		if kind == "image" || kind == "first_frame" || kind == "last_frame" {
			info.InputImageURLs = append(info.InputImageURLs, item.URL)
		}
	}
	if counts["image"] > p.images || counts["video"] > p.videos || counts["audio"] > p.audios ||
		counts["first_frame"] > 1 || counts["last_frame"] > 1 || counts["last_frame"] > counts["first_frame"] ||
		counts["first_frame"] > 0 && (counts["image"] > 0 || counts["video"] > 0 || counts["audio"] > 0) {
		return info, errors.New("unsupported Dapacai reference combination or count")
	}
	if p.requiresReference && len(media) == 0 || !p.audioOnly && counts["audio"] > 0 && counts["video"] == 0 && counts["image"] == 0 {
		return info, errors.New("this Dapacai workflow requires an image or video reference")
	}
	resolution := req.Resolution
	if resolution == "" {
		resolution = "native"
	}
	info.Model, info.Prompt, info.DurationSeconds, info.Resolution, info.AspectRatio = req.Model, prompt, req.Duration, resolution, req.Ratio
	return info, nil
}

func validatePublicVideoReferenceHost(host string) error {
	// The gateway never fetches supplied references here. The existing upload
	// service still validates bytes/ownership; block local/IP literals as well.
	if host == "" || !strings.Contains(host, ".") || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".localhost") ||
		host == "localhost" || strings.ContainsAny(host, ":[]") || dapacaiIPLiteral.MatchString(host) {
		return errors.New("reference host is not public")
	}
	return nil
}

func HasDapacaiVideoPrice(key *APIKey, model, resolution string) bool {
	_, _, ok := dapacaiVideoCardPrice(key, model, resolution)
	return ok
}

func (s *OpenAIGatewayService) ForwardDapacaiVideo(ctx context.Context, c *gin.Context, account *Account, endpoint GrokMediaEndpoint, task string, body []byte) (*OpenAIForwardResult, error) {
	if !endpoint.IsDapacaiVideo() || !account.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityDapacaiVideo) {
		return nil, errors.New("Dapacai video requires its own HTTPS API-key account")
	}
	method, target := http.MethodGet, ""
	var info GrokMediaRequestInfo
	var err error
	if endpoint == DapacaiEndpointVideoCreate {
		info, err = ParseDapacaiVideoRequest(body)
		if err != nil {
			return nil, err
		}
		if account.GetMappedModel(info.Model) != info.Model {
			return nil, errors.New("Dapacai requires the exact upstream model ID")
		}
		method, target = http.MethodPost, "https://api.dapacai.com/v1/videos"
	} else {
		target, err = dapacaiVideoURL(task, endpoint == DapacaiEndpointVideoContent)
		if err != nil {
			return nil, err
		}
	}
	token := strings.TrimSpace(account.GetCredential("api_key"))
	if token == "" {
		return nil, errors.New("Dapacai credentials unavailable")
	}
	started := time.Now()
	ctx, cancel := context.WithTimeout(WithHTTPUpstreamRedirectsDisabled(ctx), 110*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	if endpoint == DapacaiEndpointVideoContent {
		req.Header.Set("Accept", "video/*, application/octet-stream")
		if value := c.GetHeader("Range"); value != "" {
			req.Header.Set("Range", value)
		}
	}
	proxy := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxy = account.Proxy.URL()
	}
	// Exactly one submission: no failover, retry, idempotency or inherited headers.
	resp, err := s.httpUpstream.Do(req, proxy, account.ID, account.Concurrency)
	SetOpsLatencyMs(c, OpsUpstreamLatencyMsKey, time.Since(started).Milliseconds())
	if err != nil {
		return nil, errors.New("Dapacai request interrupted; do not resubmit generation automatically")
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 || resp.StatusCode < 200 {
		// Fail closed on redirects too; never forward account credentials to Location.
		status, message := resp.StatusCode, "Dapacai request failed"
		switch status {
		case 400:
			message = "新上游不接受当前视频参数，请检查模型、时长、比例和参考素材"
		case 401, 403:
			message = "新上游账号鉴权失败，请检查该账号的 Key 和模型权限"
		case 402:
			message = "新上游账号余额不足"
		case 404:
			message = "新上游没有找到这个视频任务"
		case 503:
			message = "新上游暂未提供此模型的可用渠道"
		default:
			status = http.StatusBadGateway
		}
		writeGrokMediaErrorResponse(c, status, "dapacai_upstream_error", message)
		return nil, errors.New("Dapacai upstream rejected request")
	}
	result := &OpenAIForwardResult{ResponseID: task, Model: info.Model, BillingModel: info.Model, UpstreamModel: info.Model,
		VideoResolution: info.Resolution, VideoDurationSeconds: info.DurationSeconds, Duration: time.Since(started), UpstreamEndpoint: "/v1/videos"}
	if endpoint == DapacaiEndpointVideoContent {
		if !strings.HasPrefix(resp.Header.Get("Content-Type"), "video/") && resp.Header.Get("Content-Type") != "application/octet-stream" {
			return nil, errors.New("Dapacai returned no playable video content")
		}
		err = writeGrokMediaContentResponse(c, resp)
		return result, err
	}
	const maxResponse = 2 << 20
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	var data map[string]any
	if err != nil || len(raw) > maxResponse || json.Unmarshal(raw, &data) != nil {
		return nil, errors.New("Dapacai returned invalid video data; do not resubmit automatically")
	}
	id, _ := data["id"].(string)
	if !dapacaiTaskID.MatchString(id) || endpoint != DapacaiEndpointVideoCreate && task != "dapacai:"+id {
		return nil, errors.New("Dapacai task identity mismatch")
	}
	result.ResponseID = "dapacai:" + id
	status, _ := data["status"].(string)
	response := map[string]any{"id": result.ResponseID, "request_id": result.ResponseID, "status": status}
	switch status {
	case "completed":
		if endpoint != DapacaiEndpointVideoCreate {
			result.VideoCount = 1
		}
	case "queued", "in_progress", "unknown":
	case "failed":
		response["error"] = gin.H{"message": "新上游视频生成失败；请查看上游任务记录，勿自动重复提交"}
	default:
		return nil, fmt.Errorf("Dapacai returned unsupported task status")
	}
	if progress, ok := data["progress"].(float64); ok && progress >= 0 && progress <= 100 {
		response["progress"] = progress
	}
	result.DapacaiVideoResponse = response
	return result, nil
}
