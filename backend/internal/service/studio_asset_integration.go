package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"go.uber.org/zap"
)

const (
	studioImageCollectorKey = "studio_image_collector"
	studioImageOutputsKey   = "studio_image_outputs"
)

// StudioLibraryStatus is independent of generation and billing success. Queued
// means that private retry sources were persisted, not that archival completed.
type StudioLibraryStatus struct {
	Status       string `json:"status"`
	GenerationID string `json:"generation_id"`
	Code         string `json:"code,omitempty"`
}

func StudioLibraryRequested(c *gin.Context) bool {
	if c == nil || c.Request == nil {
		return false
	}
	values := c.Request.Header.Values("X-Studio-Library")
	return len(values) == 1 && values[0] == "save"
}

// SetStudioImageCollector binds a trusted handler's narrow completion callback
// to this request only. It never becomes an upstream parameter or header.
func SetStudioImageCollector(c *gin.Context, collect func([]StudioImageOutput) *StudioLibraryStatus) {
	if c != nil && StudioLibraryRequested(c) && collect != nil {
		c.Set(studioImageCollectorKey, collect)
	}
}

func studioImageOutputs(c *gin.Context) []StudioImageOutput {
	value, _ := c.Get(studioImageOutputsKey)
	outputs, _ := value.([]StudioImageOutput)
	return outputs
}

// CompleteStudioImages receives only final, already parsed successful outputs.
// AtlasCloud calls this from the handler after its existing settlement gate.
func CompleteStudioImages(c *gin.Context, outputs []StudioImageOutput) *StudioLibraryStatus {
	if c == nil || len(outputs) == 0 {
		return nil
	}
	c.Set(studioImageOutputsKey, outputs)
	if !StudioLibraryRequested(c) {
		return nil
	}
	value, _ := c.Get(studioImageCollectorKey)
	collect, _ := value.(func([]StudioImageOutput) *StudioLibraryStatus)
	if collect == nil {
		return nil
	}
	return collect(outputs)
}

func studioImageJSONOutputs(body []byte) []StudioImageOutput {
	var outputs []StudioImageOutput
	for _, item := range gjson.GetBytes(body, "data").Array() {
		output := studioImageSource(item.Get("url").String(), item.Get("b64_json").String(),
			firstNonEmpty(item.Get("output_format").String(), gjson.GetBytes(body, "output_format").String()))
		if item.Get("url").String() != "" || item.Get("b64_json").String() != "" {
			outputs = append(outputs, output)
		}
	}
	return outputs
}

func studioImageSource(url, encoded, format string) StudioImageOutput {
	output := StudioImageOutput{URL: url}
	if strings.HasPrefix(url, "data:image/") {
		if head, data, ok := strings.Cut(url, ";base64,"); ok {
			output.MIME, encoded, output.URL = strings.TrimPrefix(head, "data:"), data, ""
		}
	}
	if encoded != "" {
		if data, err := base64.StdEncoding.DecodeString(encoded); err == nil {
			output.Bytes, output.URL = data, ""
		}
	}
	if output.MIME == "" && format != "" {
		output.MIME = openAIImageOutputMIMEType(format)
	}
	return output
}

func studioResponsesImageOutputs(results []openAIResponsesImageResult) []StudioImageOutput {
	outputs := make([]StudioImageOutput, 0, len(results))
	for _, result := range results {
		outputs = append(outputs, studioImageSource("", result.Result, result.OutputFormat))
	}
	return outputs
}

func withStudioLibraryStatus(body []byte, status *StudioLibraryStatus) []byte {
	if status == nil {
		return body
	}
	encoded, err := json.Marshal(status)
	if err != nil {
		return body
	}
	updated, err := sjson.SetRawBytes(body, "library", encoded)
	if err != nil {
		return body
	}
	return updated
}

func studioSSEDataEnvelope(raw string, payload []byte) string {
	var compact bytes.Buffer
	if json.Compact(&compact, payload) == nil {
		payload = compact.Bytes()
	}
	var out strings.Builder
	replaced := false
	for _, line := range strings.SplitAfter(raw, "\n") {
		if _, data := extractOpenAISSEDataLine(strings.TrimRight(line, "\r\n")); data {
			if !replaced {
				out.WriteString("data: ")
				out.Write(payload)
				out.WriteByte('\n')
				replaced = true
			}
		} else {
			out.WriteString(line)
		}
	}
	return out.String()
}

// Install only at startup using the existing dynamic private-storage resolver.
func (s *OpenAIGatewayService) SetStudioAssetStorage(resolve ImageStorageResolver) {
	if s == nil {
		return
	}
	if repo, ok := s.usageBillingRepo.(StudioAssetRepository); ok {
		s.studioAssets = NewStudioAssetService(repo, resolve)
	}
}

func (s *OpenAIGatewayService) StudioAssets() *StudioAssetService {
	if s == nil {
		return nil
	}
	return s.studioAssets
}

func (s *OpenAIGatewayService) StartStudioAssetWorker() {
	if s == nil {
		return
	}
	s.studioAssetRuntimeMu.Lock()
	defer s.studioAssetRuntimeMu.Unlock()
	if s.studioAssets == nil || s.studioAssetCancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	s.studioAssetCancel, s.studioAssetDone = cancel, done
	go func() {
		defer close(done)
		// A single serial loop owns both queues; a slow tick never overlaps the next.
		for ctx.Err() == nil {
			s.processStudioAssetQueues(ctx)
			timer := time.NewTimer(15 * time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
}

func (s *OpenAIGatewayService) processStudioAssetQueues(ctx context.Context) {
	// At most one fixed-size warning per queue per polling cycle. Raw errors
	// can contain private sources or credentials and must never be logged here.
	reportFailure := func(queue string, err error) {
		if err == nil || ctx.Err() != nil {
			return
		}
		logger.FromContext(ctx).Warn("studio_asset.worker_process_failed",
			zap.String("queue", queue),
			zap.String("error_code", "studio_asset_unavailable"),
		)
	}
	reportFailure("archive", s.studioAssets.ProcessPending(ctx))
	if ctx.Err() != nil {
		return
	}
	reportFailure("cleanup", s.studioAssets.ProcessCleanup(ctx))
}

func (s *OpenAIGatewayService) StopStudioAssetWorker() {
	if s == nil {
		return
	}
	s.studioAssetRuntimeMu.Lock()
	defer s.studioAssetRuntimeMu.Unlock()
	if s.studioAssetCancel != nil {
		s.studioAssetCancel()
		<-s.studioAssetDone
		s.studioAssetCancel, s.studioAssetDone = nil, nil
	}
}
