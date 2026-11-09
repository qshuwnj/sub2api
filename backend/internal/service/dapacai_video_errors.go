package service

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"
)

type dapacaiVideoFailure struct {
	Code      string `json:"code,omitempty"`
	Message   string `json:"message,omitempty"`
	RequestID string `json:"request_id,omitempty"`
}

// The supplier wraps task-plugin errors as JSON inside message. Only unwrap
// documented error fields; never persist arbitrary response objects or headers.
func parseDapacaiVideoFailure(body []byte, depth int) dapacaiVideoFailure {
	if depth > 4 {
		return dapacaiVideoFailure{}
	}
	var envelope struct {
		dapacaiVideoFailure
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &envelope) != nil {
		return dapacaiVideoFailure{}
	}
	for _, nested := range [][]byte{envelope.Error, []byte(envelope.Message)} {
		if json.Valid(nested) {
			failure := parseDapacaiVideoFailure(nested, depth+1)
			if failure.Code != "" || failure.Message != "" {
				if failure.RequestID == "" {
					failure.RequestID = envelope.RequestID
				}
				return failure
			}
		}
	}
	return envelope.dapacaiVideoFailure
}

var (
	dapacaiErrorURL        = regexp.MustCompile(`(?i)https?://[^\s<>"']+`)
	dapacaiErrorBearer     = regexp.MustCompile(`(?i)\bBearer\s+[^\s,;"']+`)
	dapacaiErrorIdentifier = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)
)

func sanitizeDapacaiVideoDiagnostic(value, credential string) string {
	if credential != "" {
		value = strings.ReplaceAll(value, credential, "[redacted]")
	}
	value = sanitizeErrorMessage(sanitizeUpstreamErrorMessage(value))
	value = dapacaiErrorURL.ReplaceAllString(value, "[redacted-url]")
	value = dapacaiErrorBearer.ReplaceAllString(value, "Bearer [redacted]")
	value = strings.Join(strings.Fields(value), " ")
	runes := []rune(value)
	if len(runes) > 512 {
		value = string(runes[:512]) + "..."
	}
	return value
}

func dapacaiVideoFailureMessage(status int, code string) (int, string) {
	switch status {
	case http.StatusBadRequest:
		switch strings.ToUpper(code) {
		case "INVALID_PROMPT":
			return status, "上游拒绝了视频提示词，请检查提示词长度和内容要求"
		case "INVALID_DURATION":
			return status, "上游拒绝了当前视频时长，请核对该模型支持的秒数"
		case "INVALID_RATIO":
			return status, "上游拒绝了当前视频比例，请核对该模型支持的画面比例"
		case "INVALID_RESOLUTION":
			return status, "上游拒绝了当前视频清晰度，请核对该模型支持的分辨率"
		case "INSUFFICIENT_POINTS":
			return status, "新上游账号额度不足，请联系管理员补充额度"
		case "TASK_NOT_EXIST":
			return status, "新上游没有找到这个视频任务"
		}
		return status, "新上游拒绝了视频请求，具体原因请联系管理员查看上游错误记录"
	case http.StatusUnauthorized, http.StatusForbidden:
		return status, "新上游账号鉴权失败，请检查该账号的 Key 和模型权限"
	case http.StatusPaymentRequired:
		return status, "新上游账号余额不足"
	case http.StatusNotFound:
		return status, "新上游没有找到这个视频任务"
	case http.StatusServiceUnavailable:
		return status, "新上游暂未提供此模型的可用渠道"
	default:
		return http.StatusBadGateway, "Dapacai request failed"
	}
}

func writeDapacaiVideoUpstreamError(c *gin.Context, account *Account, resp *http.Response, target, credential string) error {
	const maxErrorBody = 32 << 10
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody+1))
	failure := dapacaiVideoFailure{}
	if readErr == nil && len(body) <= maxErrorBody {
		failure = parseDapacaiVideoFailure(body, 0)
	}
	failure.Code = sanitizeDapacaiVideoDiagnostic(failure.Code, credential)
	if !dapacaiErrorIdentifier.MatchString(failure.Code) {
		failure.Code = ""
	}
	failure.Message = sanitizeDapacaiVideoDiagnostic(failure.Message, credential)
	if failure.RequestID == "" {
		failure.RequestID = resp.Header.Get("X-Request-ID")
	}
	failure.RequestID = sanitizeDapacaiVideoDiagnostic(failure.RequestID, credential)
	if !dapacaiErrorIdentifier.MatchString(failure.RequestID) {
		failure.RequestID = ""
	}
	status, message := dapacaiVideoFailureMessage(resp.StatusCode, failure.Code)
	if failure.Message == "" {
		failure.Message = message
	}
	detail, _ := json.Marshal(failure)
	setOpsUpstreamError(c, resp.StatusCode, failure.Message, string(detail))
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		ProxyID: opsUpstreamProxyID(account), ProxyName: opsUpstreamProxyName(account),
		Platform: account.Platform, AccountID: account.ID, AccountName: account.Name,
		UpstreamStatusCode: resp.StatusCode, UpstreamRequestID: failure.RequestID,
		UpstreamURL: safeUpstreamURL(target),
		Kind:        "http_error", Reason: failure.Code, Message: failure.Message, Detail: string(detail),
	})
	// Preserve the HTTP mapping and single-submission behavior, including redirects.
	// Client responses contain only allowlisted explanations, never supplier text.
	writeGrokMediaErrorResponse(c, status, "dapacai_upstream_error", message)
	return fmt.Errorf("Dapacai upstream rejected request: status=%d code=%s", resp.StatusCode, failure.Code)
}
