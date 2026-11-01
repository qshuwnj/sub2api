package service

import (
	"context"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
)

type imageGenerationTimingKey struct{}

// Timings deliberately exclude prompts, media, URLs, credentials and raw errors.
// Upstream status is observed locally; it is not a provider-side queue timer.
type imageGenerationTiming struct {
	log                     *zap.Logger
	started                 time.Time
	stageStarted            time.Time
	stage                   string
	lastStatus              string
	lastStatusLog           time.Time
	submissionCount         int
	pollCount               int
	referenceUploadCount    int
	submissionDuration      time.Duration
	pollDuration            time.Duration
	referenceUploadDuration time.Duration
}

func newImageGenerationTiming(ctx context.Context, model string, accountID int64) (context.Context, *imageGenerationTiming) {
	now := time.Now()
	trace := &imageGenerationTiming{
		log: logger.FromContext(ctx).With(
			zap.String("component", "image_generation"),
			zap.String("upstream_model", model),
			zap.Int64("account_id", accountID),
		),
		started: now, stageStarted: now, stage: "preflight",
	}
	trace.log.Info("image_generation.stage_started", zap.String("stage", trace.stage), zap.Int64("elapsed_ms", 0))
	return context.WithValue(ctx, imageGenerationTimingKey{}, trace), trace
}

func (t *imageGenerationTiming) transition(stage string) {
	if t.stage == stage {
		return
	}
	t.completeStage()
	t.stage = stage
	t.stageStarted = time.Now()
	t.log.Info("image_generation.stage_started",
		zap.String("stage", stage), zap.Int64("elapsed_ms", time.Since(t.started).Milliseconds()))
}

func (t *imageGenerationTiming) completeStage() {
	t.log.Info("image_generation.stage_completed",
		zap.String("stage", t.stage),
		zap.Int64("stage_ms", time.Since(t.stageStarted).Milliseconds()),
		zap.Int64("elapsed_ms", time.Since(t.started).Milliseconds()))
}

func (t *imageGenerationTiming) observeStatus(status string) {
	switch status {
	case "created", "pending", "queued", "starting", "processing", "running",
		"completed", "succeeded", "success", "failed", "error", "cancelled", "canceled":
	default:
		status = "unknown"
	}
	now := time.Now()
	if status == t.lastStatus && now.Sub(t.lastStatusLog) < 30*time.Second {
		return
	}
	t.lastStatus, t.lastStatusLog = status, now
	t.log.Info("image_generation.upstream_status",
		zap.String("upstream_status", status),
		zap.Int("poll_count", t.pollCount),
		zap.Int64("elapsed_ms", now.Sub(t.started).Milliseconds()))
}

func (t *imageGenerationTiming) finish(returned bool) {
	t.completeStage()
	outcome := "not_completed"
	if returned {
		outcome = "returned"
	}
	t.log.Info("image_generation.finished",
		zap.String("outcome", outcome),
		zap.Int64("total_ms", time.Since(t.started).Milliseconds()),
		zap.Int("submission_count", t.submissionCount),
		zap.Int64("submission_ms", t.submissionDuration.Milliseconds()),
		zap.Int("reference_upload_count", t.referenceUploadCount),
		zap.Int64("reference_upload_ms", t.referenceUploadDuration.Milliseconds()),
		zap.Int("poll_count", t.pollCount),
		zap.Int64("poll_http_ms", t.pollDuration.Milliseconds()))
}

func recordImageUpstreamTiming(ctx context.Context, method, path string, elapsed time.Duration, status int, failed bool) {
	trace, _ := ctx.Value(imageGenerationTimingKey{}).(*imageGenerationTiming)
	if trace == nil {
		return
	}
	operation := ""
	switch {
	case method == "POST" && path == "/api/v1/model/uploadMedia":
		operation = "reference_upload"
		trace.referenceUploadCount++
		trace.referenceUploadDuration += elapsed
	case method == "POST" && path == "/api/v1/model/generateImage":
		operation = "submission"
		trace.submissionCount++
		trace.submissionDuration += elapsed
	case method == "GET":
		trace.pollCount++
		trace.pollDuration += elapsed
		return // Status changes and 30-second heartbeats bound poll log volume.
	default:
		return
	}
	trace.log.Info("image_generation.upstream_request",
		zap.String("operation", operation),
		zap.Int64("request_ms", elapsed.Milliseconds()),
		zap.Int("status_code", status),
		zap.Bool("transport_failed", failed))
}
