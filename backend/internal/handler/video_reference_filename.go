package handler

import (
	"errors"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func videoReferenceStorageSuffix(model, inspectedMIME string) (string, error) {
	if !service.IsDapacaiVideoModel(model) {
		return "", nil
	}
	// Give the object its real format before Save signs the URL. Keep prompt
	// aliases separate and never append an extension to an already signed URL.
	switch inspectedMIME {
	case "image/png":
		return ".png", nil
	case "image/jpeg":
		return ".jpg", nil
	case "image/webp":
		return ".webp", nil
	case "video/mp4":
		return ".mp4", nil
	case "video/webm":
		return ".webm", nil
	case "video/quicktime":
		return ".mov", nil
	case "audio/mpeg":
		return ".mp3", nil
	case "audio/wav":
		return ".wav", nil
	case "audio/ogg":
		return ".ogg", nil
	case "audio/mp4":
		return ".m4a", nil
	case "audio/webm":
		return ".webm", nil
	default:
		return "", errors.New("unsupported reference media format")
	}
}
