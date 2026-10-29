package service

import (
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Exact model contracts; regenerated only from verified official model schemas.
//
//go:embed atlas_media_schemas.json
var atlasMediaSchemaJSON []byte

type atlasMediaSchema struct {
	Type                 string                       `json:"type"`
	Properties           map[string]*atlasMediaSchema `json:"properties"`
	Items                *atlasMediaSchema            `json:"items"`
	Required             []string                     `json:"required"`
	Enum                 []any                        `json:"enum"`
	Const                any                          `json:"const"`
	Default              any                          `json:"default"`
	Minimum              *float64                     `json:"minimum"`
	Maximum              *float64                     `json:"maximum"`
	MinLength            int                          `json:"minLength"`
	MaxLength            int                          `json:"maxLength"`
	MinItems             int                          `json:"minItems"`
	MaxItems             int                          `json:"maxItems"`
	Pattern              string                       `json:"pattern"`
	Format               string                       `json:"format"`
	AllOf                []*atlasMediaSchema          `json:"allOf"`
	If                   *atlasMediaSchema            `json:"if"`
	Then                 *atlasMediaSchema            `json:"then"`
	Else                 *atlasMediaSchema            `json:"else"`
	AdditionalProperties *bool                        `json:"additionalProperties"`
}
type atlasMediaReference struct {
	Array   bool `json:"array"`
	DataURI bool `json:"dataURI"`
}
type atlasMediaDimensions struct {
	Minimum   int    `json:"minimum"`
	Maximum   int    `json:"maximum"`
	Separator string `json:"separator"`
}
type atlasMediaContract struct {
	ID             string                         `json:"id"`
	Media          string                         `json:"media"`
	Mode           string                         `json:"mode"`
	Blocked        string                         `json:"blocked"`
	Input          *atlasMediaSchema              `json:"input"`
	MediaFields    map[string]atlasMediaReference `json:"mediaFields"`
	SizeDimensions *atlasMediaDimensions          `json:"sizeDimensions"`
}

var atlasMediaContracts = func() map[string]*atlasMediaContract {
	var entries []*atlasMediaContract
	if err := json.Unmarshal(atlasMediaSchemaJSON, &entries); err != nil {
		panic(err)
	}
	result := make(map[string]*atlasMediaContract, len(entries))
	for _, entry := range entries {
		if result[entry.ID] != nil {
			panic("duplicate AtlasCloud media contract")
		}
		result[entry.ID] = entry
	}
	return result
}()

func atlasMediaModel(model, media string) *atlasMediaContract {
	entry := atlasMediaContracts[model]
	if entry == nil || entry.Media != media || entry.Blocked != "" || entry.Input == nil {
		return nil
	}
	return entry
}

// Partial conditional schemas constrain only present values; the outer schema
// separately rejects undeclared fields. Do not discard fields while validating.
func validateAtlasMediaValue(value any, schema *atlasMediaSchema, field string, strict bool) error {
	fail := func() error { return fmt.Errorf("invalid AtlasCloud parameter %s", field) }
	if schema == nil {
		return fail()
	}
	if schema.Const != nil && !reflect.DeepEqual(schema.Const, value) {
		return fail()
	}
	if len(schema.Enum) > 0 {
		found := false
		for _, candidate := range schema.Enum {
			if reflect.DeepEqual(candidate, value) {
				found = true
				break
			}
		}
		if !found {
			return fail()
		}
	}
	kind := schema.Type
	if kind == "" && schema.Properties != nil {
		kind = "object"
	}
	if kind == "" && schema.Items != nil {
		kind = "array"
	}
	if kind == "" && (schema.MinItems > 0 || schema.MaxItems > 0) {
		kind = "array"
	}
	// JSON Schema partials apply numeric/string bounds without requiring "type".
	if kind == "" {
		switch value.(type) {
		case float64:
			kind = "number"
		case string:
			kind = "string"
		}
	}
	switch kind {
	case "string":
		text, ok := value.(string)
		if !ok {
			return fail()
		}
		length := utf8.RuneCountInString(text)
		if length < schema.MinLength || schema.MaxLength > 0 && length > schema.MaxLength {
			return fail()
		}
		if schema.Pattern != "" {
			pattern, err := regexp.Compile(schema.Pattern)
			if err != nil || !pattern.MatchString(text) {
				return fail()
			}
		}
		if schema.Format == "uri" && !atlasMediaURI(text) {
			return fail()
		}
	case "integer", "number":
		number, ok := value.(float64)
		if !ok || math.IsNaN(number) || math.IsInf(number, 0) || kind == "integer" && math.Trunc(number) != number {
			return fail()
		}
		if schema.Minimum != nil && number < *schema.Minimum || schema.Maximum != nil && number > *schema.Maximum {
			return fail()
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return fail()
		}
	case "array":
		items, ok := value.([]any)
		if !ok || len(items) < schema.MinItems || schema.MaxItems > 0 && len(items) > schema.MaxItems {
			return fail()
		}
		for index, item := range items {
			if schema.Items != nil {
				if err := validateAtlasMediaValue(item, schema.Items, fmt.Sprintf("%s[%d]", field, index), strict); err != nil {
					return err
				}
			}
		}
	case "object":
		object, ok := value.(map[string]any)
		if !ok {
			return fail()
		}
		for _, key := range schema.Required {
			if child, exists := object[key]; !exists || child == nil {
				return fmt.Errorf("AtlasCloud %s.%s is required", field, key)
			}
		}
		for key, child := range object {
			property := schema.Properties[key]
			if property == nil {
				if strict || schema.AdditionalProperties != nil && !*schema.AdditionalProperties {
					return fmt.Errorf("unsupported AtlasCloud parameter %s.%s", field, key)
				}
				continue
			}
			if err := validateAtlasMediaValue(child, property, field+"."+key, strict); err != nil {
				return err
			}
		}
	}
	for _, rule := range schema.AllOf {
		if err := validateAtlasMediaValue(value, rule, field, false); err != nil {
			return err
		}
	}
	if schema.If != nil {
		rule := schema.Then
		if validateAtlasMediaValue(value, schema.If, field, false) != nil {
			rule = schema.Else
		}
		if rule != nil {
			return validateAtlasMediaValue(value, rule, field, false)
		}
	}
	return nil
}

func atlasMediaHTTPSURL(value string) bool {
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") || !strings.Contains(host, ".") {
		return false
	}
	if ip := net.ParseIP(host); ip != nil && (ip.IsPrivate() || !ip.IsGlobalUnicast() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified()) {
		return false
	}
	return true
}

func atlasMediaReferenceValid(value string, allowData bool) bool {
	if strings.HasPrefix(value, "data:") {
		if !allowData || len(value) > 14*1024*1024 {
			return false
		}
		parts := strings.SplitN(value, ",", 2)
		if len(parts) != 2 {
			return false
		}
		switch parts[0] {
		case "data:image/png;base64", "data:image/jpeg;base64", "data:image/webp;base64":
		default:
			return false
		}
		raw, err := base64.StdEncoding.Strict().DecodeString(parts[1])
		return err == nil && len(raw) > 0
	}
	return atlasMediaHTTPSURL(value)
}

func buildAtlasMediaContractPayload(body []byte, contract *atlasMediaContract, imageEnvelope bool) (map[string]any, error) {
	var input map[string]any
	if contract == nil || contract.Blocked != "" || len(body) > 16*1024*1024 || json.Unmarshal(body, &input) != nil || input == nil {
		return nil, fmt.Errorf("unsupported AtlasCloud model or invalid request")
	}
	if model, ok := input["model"].(string); !ok || model != contract.ID {
		return nil, fmt.Errorf("exact AtlasCloud model ID is required")
	}
	prompt, ok := input["prompt"].(string)
	if !ok || strings.TrimSpace(prompt) == "" {
		return nil, fmt.Errorf("prompt must be a non-empty string")
	}
	if imageEnvelope {
		if n, exists := input["n"]; exists && n != float64(1) {
			return nil, fmt.Errorf("AtlasCloud requires n=1")
		}
		if stream, exists := input["stream"]; exists && stream != false {
			return nil, fmt.Errorf("AtlasCloud requires stream=false")
		}
		if format, exists := input["response_format"]; exists && format != "url" {
			return nil, fmt.Errorf("AtlasCloud requires response_format=url")
		}
		delete(input, "stream")
		delete(input, "response_format")
		if contract.Input.Properties["n"] == nil {
			delete(input, "n")
		}
		if references, exists := input["images_list"]; exists {
			if len(contract.MediaFields) != 1 {
				return nil, fmt.Errorf("use the model's exact image field")
			}
			for key, ref := range contract.MediaFields {
				if _, conflict := input[key]; conflict {
					return nil, fmt.Errorf("conflicting input images")
				}
				if ref.Array {
					input[key] = references
				} else {
					items, ok := references.([]any)
					if !ok || len(items) != 1 {
						return nil, fmt.Errorf("model requires one input image")
					}
					input[key] = items[0]
				}
			}
			delete(input, "images_list")
		}
	}
	for key, schema := range contract.Input.Properties {
		if _, exists := input[key]; !exists && schema.Default != nil {
			input[key] = schema.Default
		}
	}
	for _, flag := range []string{"enable_sync_mode", "enable_base64_output"} {
		if value, exists := input[flag]; exists && value != false {
			return nil, fmt.Errorf("AtlasCloud requires %s=false", flag)
		}
	}
	for _, count := range []string{"n", "num_images"} {
		if value, exists := input[count]; exists && value != float64(1) {
			return nil, fmt.Errorf("AtlasCloud requires %s=1", count)
		}
	}
	if err := validateAtlasMediaValue(input, contract.Input, "request", true); err != nil {
		return nil, err
	}
	if dimensions := contract.SizeDimensions; dimensions != nil {
		size, _ := input["size"].(string)
		parts := strings.Split(size, dimensions.Separator)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid AtlasCloud image dimensions")
		}
		for _, part := range parts {
			edge, err := strconv.Atoi(part)
			if err != nil || edge < dimensions.Minimum || edge > dimensions.Maximum || strconv.Itoa(edge) != part {
				return nil, fmt.Errorf("invalid AtlasCloud image dimensions")
			}
		}
	}
	for key, ref := range contract.MediaFields {
		value, exists := input[key]
		if !exists {
			continue
		}
		items := []any{value}
		if ref.Array {
			items, _ = value.([]any)
		}
		for _, item := range items {
			text, ok := item.(string)
			if !ok || !atlasMediaReferenceValid(text, ref.DataURI || imageEnvelope) {
				return nil, fmt.Errorf("invalid public image reference %s", key)
			}
		}
	}
	return input, nil
}

func buildAtlasMediaVideoPayload(body []byte, contract *atlasMediaContract) (GrokMediaRequestInfo, []byte, error) {
	var info GrokMediaRequestInfo
	payload, err := buildAtlasMediaContractPayload(body, contract, false)
	if err != nil {
		return info, nil, err
	}
	duration := 0
	switch value := payload["duration"].(type) {
	case float64:
		if value == math.Trunc(value) {
			duration = int(value)
		}
	case string:
		duration, _ = strconv.Atoi(value)
	}
	if duration <= 0 || duration > 120 {
		return info, nil, fmt.Errorf("a fixed positive duration is required for AtlasCloud billing")
	}
	if payload["draft"] == true || payload["return_last_frame"] == true || payload["multi_shot"] == true || payload["multi_prompt"] != nil {
		return info, nil, fmt.Errorf("draft, extra-frame and multi-shot outputs require a separate billing contract")
	}
	info.Model, info.Prompt, info.N = contract.ID, payload["prompt"].(string), 1
	info.DurationSeconds = duration
	info.AspectRatio, _ = payload["aspect_ratio"].(string)
	if ratio, ok := payload["ratio"].(string); ok {
		info.AspectRatio = ratio
	}
	info.Resolution, _ = payload["resolution"].(string)
	if info.Resolution == "" {
		info.Resolution = "native"
		if match := regexp.MustCompile(`-(480p|720p|1080p)$`).FindStringSubmatch(contract.ID); len(match) == 2 {
			info.Resolution = match[1]
		}
	}
	info.Resolution = strings.ToLower(info.Resolution)
	for key, ref := range contract.MediaFields {
		if value, exists := payload[key]; exists {
			if ref.Array {
				for _, item := range value.([]any) {
					info.InputImageURLs = append(info.InputImageURLs, item.(string))
				}
			} else {
				info.InputImageURLs = append(info.InputImageURLs, value.(string))
			}
		}
	}
	raw, err := json.Marshal(payload)
	return info, raw, err
}

func atlasMediaImageSize(model, requested string) (string, string) {
	contract := atlasMediaModel(model, "image")
	if contract == nil {
		return "", ""
	}
	size := requested
	if size == "" || size == "auto" {
		size, _ = contract.Input.Properties["size"].defaultString()
	}
	if size != "" {
		return size, normalizeOpenAIImageSizeTier(strings.ReplaceAll(size, "*", "x"))
	}
	if property := contract.Input.Properties["resolution"]; property != nil {
		resolution, _ := property.defaultString()
		switch strings.ToLower(resolution) {
		case "1k":
			return "", ImageBillingSize1K
		case "2k":
			return "", ImageBillingSize2K
		case "4k":
			return "", ImageBillingSize4K
		}
	}
	return "", ""
}

func atlasCloudRequestedImageSize(parsed *OpenAIImagesRequest, model string) (string, string) {
	size, tier := atlasCloudImageSize(model, parsed.Size)
	var body map[string]any
	if json.Unmarshal(parsed.Body, &body) == nil {
		if resolution, ok := body["resolution"].(string); ok && atlasMediaModel(model, "image") != nil {
			switch strings.ToLower(resolution) {
			case "1k":
				tier = ImageBillingSize1K
			case "2k":
				tier = ImageBillingSize2K
			case "4k":
				tier = ImageBillingSize4K
			}
		}
	}
	return size, tier
}

func (s *atlasMediaSchema) defaultString() (string, bool) {
	if s == nil {
		return "", false
	}
	value, ok := s.Default.(string)
	return value, ok
}

func atlasMediaVideoResolution(model, resolution string) (string, bool) {
	contract := atlasMediaModel(model, "video")
	if contract == nil {
		return "", false
	}
	if property := contract.Input.Properties["resolution"]; property != nil {
		for _, candidate := range property.Enum {
			value, ok := candidate.(string)
			if ok && strings.EqualFold(value, resolution) {
				return strings.ToLower(value), true
			}
		}
		if len(property.Enum) == 0 {
			if value, ok := property.defaultString(); ok && strings.EqualFold(value, resolution) {
				return strings.ToLower(value), true
			}
		}
		return "", false
	}
	tier := "native"
	if match := regexp.MustCompile(`-(480p|720p|1080p)$`).FindStringSubmatch(model); len(match) == 2 {
		tier = match[1]
	}
	return tier, strings.EqualFold(resolution, tier)
}

func atlasMediaURI(value string) bool {
	u, err := url.Parse(value)
	return err == nil && u.IsAbs()
}
