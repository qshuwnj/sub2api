package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const (
	AtlasCloudEndpointVideoCreate           GrokMediaEndpoint        = "atlascloud_video_create"
	AtlasCloudEndpointVideoStatus           GrokMediaEndpoint        = "atlascloud_video_status"
	OpenAIEndpointCapabilityAtlasCloudVideo OpenAIEndpointCapability = "atlascloud_video"
	atlasCloudGeminiVideoModel                                       = "google/gemini-omni-1.1-flash/text-to-video"
	atlasCloudMinimaxVideoModel                                      = "minimax/h3-developer/text-to-video"
	atlasCloudGrokVideoModel                                         = "xai/grok-imagine-video/text-to-video"
)

func (e GrokMediaEndpoint) IsAtlasCloudVideo() bool {
	return e == AtlasCloudEndpointVideoCreate || e == AtlasCloudEndpointVideoStatus
}

func IsAtlasCloudGenericVideoModel(model string) bool {
	model = strings.TrimSpace(model)
	return strings.Contains(model, "/") && strings.HasSuffix(model, "/text-to-video") &&
		model != atlasCloudGeminiVideoModel && model != atlasCloudMinimaxVideoModel && model != atlasCloudGrokVideoModel
}

func IsAtlasCloudVideoModel(model string) bool {
	return model == atlasCloudGeminiVideoModel || model == atlasCloudMinimaxVideoModel || model == atlasCloudGrokVideoModel ||
		IsAtlasCloudGenericVideoModel(model) || atlasMediaModel(model, "video") != nil
}

func atlasCloudAPIBaseValid(base string) bool {
	u, err := url.Parse(strings.TrimSpace(base))
	return err == nil && u.Scheme == "https" && u.Host == "api.atlascloud.ai" &&
		u.User == nil && u.RawQuery == "" && u.Fragment == "" &&
		(u.Path == "" || u.Path == "/" || strings.TrimRight(u.Path, "/") == "/api/v1")
}

func ParseAtlasCloudVideoRequest(body []byte) (GrokMediaRequestInfo, error) {
	info, _, err := buildAtlasCloudVideoPayload(body)
	return info, err
}

// SerializeAtlasCloudVideoRequest is the single native payload boundary used
// both to bind an attempt's fingerprint and to send its one supplier request.
func SerializeAtlasCloudVideoRequest(body []byte) (GrokMediaRequestInfo, []byte, error) {
	return buildAtlasCloudVideoPayload(body)
}

// VideoTransportError deliberately contains no supplier body or credentials.
// Only an explicit non-accepting HTTP response permits releasing an unknown ID.
type VideoTransportError struct {
	DefinitiveRejection bool
}

func (e *VideoTransportError) Error() string {
	if e.DefinitiveRejection {
		return "video supplier rejected the submission without accepting a task"
	}
	return "video supplier acceptance or outcome is unknown; recover the existing attempt, do not resubmit"
}

type VideoSupplierResult struct {
	TaskID     string
	State      string
	ResultURLs []string
}

func (s *OpenAIGatewayService) requestQuotedAtlasVideo(ctx context.Context, account *Account, taskID string, native []byte) (*VideoSupplierResult, error) {
	unknown := &VideoTransportError{}
	if account == nil || !account.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityAtlasCloudVideo) ||
		strings.TrimSpace(account.GetCredential("api_key")) == "" || s.httpUpstream == nil {
		return nil, ErrVideoAttemptUnavailable
	}
	const base = "https://api.atlascloud.ai/api/v1"
	method, target := http.MethodPost, base+"/model/generateVideo"
	if taskID != "" {
		if !strings.HasPrefix(taskID, "atlascloud:") {
			return nil, ErrVideoAttemptIneligible
		}
		id := strings.TrimPrefix(taskID, "atlascloud:")
		if id == "" || validateUpstreamPathSegment("video task", id) != nil {
			return nil, ErrVideoAttemptIneligible
		}
		method, target, native = http.MethodGet, base+atlasCloudPollPath+url.PathEscape(id), nil
	}
	// A 307/308 redirect is another POST, not safe video recovery.
	ctx, cancel := context.WithTimeout(WithHTTPUpstreamRedirectsDisabled(ctx), time.Minute)
	defer cancel()
	raw, code, err := s.doAtlasCloudImagesRequest(ctx, account, method, target, strings.TrimSpace(account.GetCredential("api_key")), native)
	// A truncated/rejected connection or body cannot prove no task was created.
	if err != nil {
		return nil, unknown
	}
	var known *VideoSupplierResult
	if gjson.ValidBytes(raw) {
		value := atlasCloudValue(raw, "id")
		id := strings.TrimSpace(value.String())
		if value.Type == gjson.String && id != "" && validateUpstreamPathSegment("video task", id) == nil {
			candidate := "atlascloud:" + id
			if taskID == "" || taskID == candidate {
				known = &VideoSupplierResult{TaskID: candidate}
			}
		}
	}
	if code < 200 || code >= 300 {
		if known != nil {
			// Even an error HTTP response can carry evidence that a task
			// exists. Save that identity; it is not a definitive rejection.
			return known, unknown
		}
		// 408 is a timeout, not proof of non-acceptance. Other statuses not in
		// this conservative explicit-rejection set retain the reservation.
		// Malformed error documents or task-like fields are ambiguous too.
		definitive := method == http.MethodPost &&
			gjson.ValidBytes(raw) && gjson.ParseBytes(raw).IsObject() && !atlasCloudValue(raw, "id").Exists() &&
			!atlasCloudValue(raw, "status").Exists() &&
			(code == 400 || code == 401 || code == 403 || code == 404 || code == 405 ||
				code == 413 || code == 415 || code == 422 || code == 429)
		return nil, &VideoTransportError{DefinitiveRejection: definitive}
	}
	if !gjson.ValidBytes(raw) {
		return nil, unknown
	}
	value := atlasCloudValue(raw, "id")
	id := strings.TrimSpace(value.String())
	if value.Exists() && value.Type != gjson.String {
		return nil, unknown
	}
	if id != "" && validateUpstreamPathSegment("video task", id) != nil {
		return nil, unknown
	}
	if taskID == "" {
		if id == "" {
			return nil, unknown
		}
		taskID = "atlascloud:" + id
	} else if id != "" && taskID != "atlascloud:"+id {
		return nil, unknown
	}
	result := &VideoSupplierResult{TaskID: taskID}
	if v := gjson.GetBytes(raw, "code"); v.Exists() && (v.Type != gjson.Number || v.Int() != 200) {
		return result, unknown
	}
	switch strings.ToLower(atlasCloudValue(raw, "status").String()) {
	case "completed", "succeeded", "success":
		result.ResultURLs = atlasCloudOutputURLs(raw)
		if len(result.ResultURLs) != 1 {
			return result, unknown
		}
		result.State = VideoAttemptCompleted
	case "failed", "error", "timeout", "cancelled", "canceled":
		result.State = VideoAttemptFailed
	case "created", "pending", "queued", "starting", "processing", "running":
		result.State = VideoAttemptProcessing
	default:
		return result, unknown
	}
	return result, nil
}

func definitiveVideoRejection(err error) bool {
	var outcome *VideoTransportError
	return errors.As(err, &outcome) && outcome.DefinitiveRejection
}

func buildAtlasCloudVideoPayload(body []byte) (GrokMediaRequestInfo, []byte, error) {
	var info GrokMediaRequestInfo
	id := gjson.GetBytes(body, "model").String()
	if id != atlasCloudGeminiVideoModel && id != atlasCloudMinimaxVideoModel && id != atlasCloudGrokVideoModel {
		if contract := atlasMediaModel(id, "video"); contract != nil {
			return buildAtlasMediaVideoPayload(body, contract)
		}
		if !IsAtlasCloudGenericVideoModel(id) {
			return info, nil, fmt.Errorf("unsupported AtlasCloud video model")
		}
	}
	if !gjson.ValidBytes(body) || !gjson.ParseBytes(body).IsObject() {
		return info, nil, fmt.Errorf("request body must be a JSON object")
	}
	model, prompt := gjson.GetBytes(body, "model"), gjson.GetBytes(body, "prompt")
	if model.Type != gjson.String || !IsAtlasCloudVideoModel(model.String()) {
		return info, nil, fmt.Errorf("unsupported AtlasCloud video model")
	}
	if prompt.Type != gjson.String || strings.TrimSpace(prompt.String()) == "" {
		return info, nil, fmt.Errorf("prompt must be a non-empty string")
	}
	info.Model, info.Prompt, info.N = model.String(), prompt.String(), 1
	fields := []string{"model", "prompt", "duration", "resolution", "aspect_ratio"}
	minDuration, maxDuration, defaultDuration := 1, 15, 8
	defaultRatio, defaultResolution := "16:9", "720p"
	ratios := []string{"1:1", "16:9", "9:16", "4:3", "3:4", "3:2", "2:3"}
	resolutions := []string{"480p", "720p"}
	switch info.Model {
	case atlasCloudGeminiVideoModel:
		fields = append(fields, "thinking_level", "seed")
		minDuration, maxDuration, defaultDuration = 3, 10, 10
		ratios = []string{"16:9", "9:16"}
		resolutions = []string{"360p", "720p", "1080p", "4k"}
		if utf8.RuneCountInString(info.Prompt) > 20000 {
			return info, nil, fmt.Errorf("prompt exceeds 20000 characters")
		}
	case atlasCloudMinimaxVideoModel:
		fields = append(fields, "ratio", "prompt_expansion")
		minDuration, defaultRatio, defaultResolution = 4, "1:1", "768P"
		ratios = []string{"21:9", "16:9", "4:3", "1:1", "3:4", "9:16"}
		resolutions = []string{"480P", "768P", "1440p-sr", "4k-sr"}
	case atlasCloudGrokVideoModel:
		resolutions = []string{"480p", "720p"}
		if utf8.RuneCountInString(info.Prompt) > 32000 {
			return info, nil, fmt.Errorf("prompt exceeds 32000 characters")
		}
	default:
		if !IsAtlasCloudGenericVideoModel(info.Model) {
			return info, nil, fmt.Errorf("unsupported AtlasCloud video model")
		}
		if utf8.RuneCountInString(info.Prompt) > 32000 {
			return info, nil, fmt.Errorf("prompt exceeds 32000 characters")
		}
		resolutions = []string{"480p", "720p", "1080p", "4k"}
	}
	if err := atlasCloudCheckFields(body, fields...); err != nil {
		return info, nil, err
	}
	duration := defaultDuration
	if v := gjson.GetBytes(body, "duration"); v.Exists() {
		if v.Type != gjson.Number || v.Float() != float64(v.Int()) || v.Int() < int64(minDuration) || v.Int() > int64(maxDuration) {
			return info, nil, fmt.Errorf("duration must be an integer between %d and %d", minDuration, maxDuration)
		}
		duration = int(v.Int())
	}
	ratioKey := "aspect_ratio"
	if info.Model == atlasCloudMinimaxVideoModel && gjson.GetBytes(body, "ratio").Exists() {
		if gjson.GetBytes(body, "aspect_ratio").Exists() {
			return info, nil, fmt.Errorf("use either ratio or aspect_ratio, not both")
		}
		ratioKey = "ratio"
	}
	ratio, err := atlasCloudOption(body, ratioKey, defaultRatio, ratios...)
	if err != nil {
		return info, nil, err
	}
	var resolution string
	if IsAtlasCloudGenericVideoModel(info.Model) {
		value := gjson.GetBytes(body, "resolution")
		if !value.Exists() {
			resolution = defaultResolution
		} else if value.Type != gjson.String || strings.TrimSpace(value.String()) == "" ||
			len(value.String()) > 32 {
			return info, nil, fmt.Errorf("resolution must be a non-empty string")
		} else {
			resolution = value.String()
		}
	} else {
		resolution, err = atlasCloudOption(body, "resolution", defaultResolution, resolutions...)
		if err != nil {
			return info, nil, err
		}
	}
	info.AspectRatio, info.Resolution, info.DurationSeconds = ratio, strings.ToLower(resolution), duration
	payload := map[string]any{"model": info.Model, "prompt": info.Prompt, "duration": duration, "resolution": resolution}
	if info.Model == atlasCloudMinimaxVideoModel {
		payload["ratio"] = ratio
		v := gjson.GetBytes(body, "prompt_expansion")
		if v.Exists() && v.Type != gjson.True && v.Type != gjson.False {
			return info, nil, fmt.Errorf("prompt_expansion must be boolean")
		}
		payload["prompt_expansion"] = v.Bool()
	} else {
		payload["aspect_ratio"] = ratio
	}
	if info.Model == atlasCloudGeminiVideoModel {
		thinking, err := atlasCloudOption(body, "thinking_level", "default", "default", "high", "low")
		if err != nil {
			return info, nil, err
		}
		payload["thinking_level"] = thinking
		seed := gjson.GetBytes(body, "seed")
		if seed.Exists() {
			if seed.Type != gjson.Number || seed.Float() != float64(seed.Int()) {
				return info, nil, fmt.Errorf("seed must be an integer")
			}
			payload["seed"] = seed.Int()
		} else {
			payload["seed"] = -1
		}
	}
	raw, err := json.Marshal(payload)
	return info, raw, err
}

func (s *OpenAIGatewayService) ForwardAtlasCloudVideo(ctx context.Context, c *gin.Context, account *Account, endpoint GrokMediaEndpoint, taskID string, body []byte) (*OpenAIForwardResult, error) {
	if !endpoint.IsAtlasCloudVideo() || !account.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityAtlasCloudVideo) {
		return nil, fmt.Errorf("AtlasCloud video requires an AtlasCloud OpenAI API-key account")
	}
	const base = "https://api.atlascloud.ai/api/v1"
	method, target := http.MethodGet, base+atlasCloudPollPath
	var info GrokMediaRequestInfo
	var err error
	if endpoint == AtlasCloudEndpointVideoCreate {
		info, body, err = buildAtlasCloudVideoPayload(body)
		if err != nil {
			return nil, err
		}
		if account.GetMappedModel(info.Model) != info.Model {
			return nil, fmt.Errorf("AtlasCloud videos require the exact upstream model ID")
		}
		method, target = http.MethodPost, base+"/model/generateVideo"
	} else {
		if !strings.HasPrefix(taskID, "atlascloud:") {
			return nil, fmt.Errorf("invalid AtlasCloud task ID")
		}
		id := strings.TrimPrefix(taskID, "atlascloud:")
		if id == "" || validateUpstreamPathSegment("AtlasCloud task ID", id) != nil {
			return nil, fmt.Errorf("invalid AtlasCloud task ID")
		}
		target += url.PathEscape(id)
	}
	token := strings.TrimSpace(account.GetCredential("api_key"))
	if token == "" {
		return nil, fmt.Errorf("AtlasCloud credentials unavailable")
	}
	started := time.Now()
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	raw, statusCode, err := s.doAtlasCloudImagesRequest(ctx, account, method, target, token, body)
	SetOpsLatencyMs(c, OpsUpstreamLatencyMsKey, time.Since(started).Milliseconds())
	if err != nil {
		return nil, fmt.Errorf("AtlasCloud request failed; do not automatically resubmit")
	}
	if statusCode < 200 || statusCode >= 300 || !gjson.ValidBytes(raw) {
		return nil, fmt.Errorf("AtlasCloud rejected the video request")
	}
	if code := gjson.GetBytes(raw, "code"); code.Exists() && (code.Type != gjson.Number || code.Int() != 200) {
		return nil, fmt.Errorf("AtlasCloud video request failed")
	}
	if endpoint == AtlasCloudEndpointVideoCreate {
		id := strings.TrimSpace(atlasCloudValue(raw, "id").String())
		if id == "" || validateUpstreamPathSegment("AtlasCloud task ID", id) != nil {
			return nil, fmt.Errorf("AtlasCloud returned an invalid task ID")
		}
		taskID = "atlascloud:" + id
	}
	result := &OpenAIForwardResult{ResponseID: taskID, Model: info.Model, BillingModel: info.Model,
		UpstreamModel: info.Model, VideoResolution: info.Resolution, VideoDurationSeconds: info.DurationSeconds,
		Duration: time.Since(started), UpstreamEndpoint: strings.TrimPrefix(target, "https://api.atlascloud.ai")}
	status := strings.ToLower(atlasCloudValue(raw, "status").String())
	response := gin.H{"id": taskID, "request_id": taskID}
	switch status {
	case "completed", "succeeded", "success":
		outputs := atlasCloudOutputURLs(raw)
		if len(outputs) != 1 {
			return nil, fmt.Errorf("AtlasCloud returned an invalid video output")
		}
		result.VideoCount = 1
		response["status"], response["outputs"], response["video"] = "completed", outputs, gin.H{"url": outputs[0]}
	case "failed", "error", "timeout":
		response["status"], response["error"] = "failed", gin.H{"message": "AtlasCloud video generation failed"}
	case "created", "pending", "queued", "starting", "processing", "running":
		response["status"] = "processing"
	default:
		return nil, fmt.Errorf("AtlasCloud returned an unknown video status")
	}
	result.AtlasCloudVideoResponse = response
	return result, nil
}
