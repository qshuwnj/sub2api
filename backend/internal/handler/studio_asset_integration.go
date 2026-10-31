package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"time"

	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const studioImageGenerationKey = "studio_image_generation_id"

type studioImageCollection struct {
	generationID string
	attempted    int
	warning      bool
	metadata     json.RawMessage
}

func (h *OpenAIGatewayHandler) prepareStudioImages(c *gin.Context, parsed *service.OpenAIImagesRequest, metadata json.RawMessage) {
	if !service.StudioLibraryRequested(c) || parsed == nil {
		return
	}
	generationID := c.GetString(studioImageGenerationKey)
	if generationID == "" {
		generationID = "image_" + uuid.NewString()
		c.Set(studioImageGenerationKey, generationID)
	}
	state := &studioImageCollection{generationID: generationID, metadata: studioImageMetadata(metadata, parsed)}
	c.Set("studio_image_collection", state)
	service.SetStudioImageCollector(c, func(outputs []service.StudioImageOutput) *service.StudioLibraryStatus {
		result := &service.OpenAIForwardResult{StudioImageOutputs: outputs}
		return h.collectStudioImages(c, result, parsed)
	})
}

func studioImageMetadata(body json.RawMessage, parsed *service.OpenAIImagesRequest) json.RawMessage {
	metadata := map[string]any{}
	var raw map[string]any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if decoder.Decode(&raw) == nil {
		for _, key := range []string{"aspect_ratio", "resolution", "duration", "width", "height", "seed"} {
			switch value := raw[key].(type) {
			case string:
				metadata[key] = value
			case json.Number:
				metadata[key] = value
			case bool:
				metadata[key] = value
			}
		}
	}
	if len(metadata) == 0 && parsed.Size != "" {
		metadata["resolution"] = parsed.Size
	}
	encoded, _ := json.Marshal(metadata)
	return encoded
}

// Collection is a detached, bounded DB-only operation. It cannot submit media,
// settle usage or change receipts, and never writes a second HTTP response.
func (h *OpenAIGatewayHandler) collectStudioImages(c *gin.Context, result *service.OpenAIForwardResult, parsed *service.OpenAIImagesRequest) *service.StudioLibraryStatus {
	value, exists := c.Get("studio_image_collection")
	state, ok := value.(*studioImageCollection)
	if !exists || !ok || result == nil || parsed == nil || !service.StudioLibraryRequested(c) {
		return nil
	}
	key, authenticated := middleware2.GetAPIKeyFromContext(c)
	if !authenticated || key == nil || key.UserID <= 0 || key.ID <= 0 {
		state.warning = true
	} else {
		library := h.gatewayService.StudioAssets()
		ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), 10*time.Second)
		defer cancel()
		for index := state.attempted; index < len(result.StudioImageOutputs); index++ {
			if library == nil || library.CollectImage(ctx,
				service.StudioAssetOwner{UserID: key.UserID, APIKeyID: key.ID},
				state.generationID, index, parsed.Model, parsed.Prompt, state.metadata, result.StudioImageOutputs[index]) != nil {
				state.warning = true
			}
		}
	}
	state.attempted = len(result.StudioImageOutputs)
	status := &service.StudioLibraryStatus{Status: "queued", GenerationID: state.generationID}
	if state.warning {
		status.Status, status.Code = "warning", "studio_asset_unavailable"
	}
	c.Set("studio_library_status", status)
	return status
}
