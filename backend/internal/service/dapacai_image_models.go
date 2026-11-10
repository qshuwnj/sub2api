package service

// IsDapacaiImageModel matches exact supplier IDs, never GPT/Atlas aliases.
func IsDapacaiImageModel(model string) bool {
	return model == "gpt-image-2.5-cdn" || model == "gpt-image-2.6"
}
