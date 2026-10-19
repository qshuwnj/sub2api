package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"

	"github.com/tidwall/gjson"
)

// Temporary media is uploaded only as part of an authorized, priced generation.
// The customer never receives the account token or an unrestricted upload API.
func (s *OpenAIGatewayService) prepareAtlasCloudImageReferences(ctx context.Context, account *Account, token, model string, body []byte) ([]byte, error) {
	contract := atlasMediaModel(model, "image")
	if contract == nil || len(contract.MediaFields) == 0 {
		return body, nil
	}
	var input map[string]any
	if err := json.Unmarshal(body, &input); err != nil {
		return nil, fmt.Errorf("invalid AtlasCloud image payload")
	}
	uploaded := make(map[string]string)
	convert := func(value any) (any, error) {
		text, ok := value.(string)
		if !ok || !strings.HasPrefix(text, "data:") {
			return value, nil
		}
		if url, ok := uploaded[text]; ok {
			return url, nil
		}
		parts := strings.SplitN(text, ",", 2)
		if len(parts) != 2 || !atlasMediaReferenceValid(text, true) {
			return nil, fmt.Errorf("invalid inline reference image")
		}
		raw, err := base64.StdEncoding.Strict().DecodeString(parts[1])
		if err != nil || len(raw) > 10*1024*1024 {
			return nil, fmt.Errorf("reference image exceeds 10 MB or is invalid")
		}
		mime := strings.TrimSuffix(strings.TrimPrefix(parts[0], "data:"), ";base64")
		extension := map[string]string{"image/png": "png", "image/jpeg": "jpg", "image/webp": "webp"}[mime]
		if extension == "" || http.DetectContentType(raw) != mime {
			return nil, fmt.Errorf("reference image content does not match its type")
		}
		var buffer bytes.Buffer
		writer := multipart.NewWriter(&buffer)
		header := make(textproto.MIMEHeader)
		header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="reference.%s"`, extension))
		header.Set("Content-Type", mime)
		part, err := writer.CreatePart(header)
		if err != nil {
			return nil, err
		}
		if _, err := part.Write(raw); err != nil {
			return nil, err
		}
		if err := writer.Close(); err != nil {
			return nil, err
		}
		response, status, err := s.doAtlasCloudImagesRequest(ctx, account, http.MethodPost,
			"https://api.atlascloud.ai/api/v1/model/uploadMedia", token, buffer.Bytes(), writer.FormDataContentType())
		if err != nil || status < 200 || status >= 300 {
			return nil, fmt.Errorf("AtlasCloud reference upload failed; generation was not submitted")
		}
		if code := gjson.GetBytes(response, "code"); code.Exists() && (code.Type != gjson.Number || code.Int() != 200) {
			return nil, fmt.Errorf("AtlasCloud reference upload was rejected")
		}
		url := gjson.GetBytes(response, "data.download_url").String()
		if !atlasMediaHTTPSURL(url) {
			return nil, fmt.Errorf("AtlasCloud returned an invalid reference URL")
		}
		uploaded[text] = url
		return url, nil
	}
	for key, reference := range contract.MediaFields {
		if reference.DataURI {
			continue // This exact model explicitly documents inline media support.
		}
		value, exists := input[key]
		if !exists {
			continue
		}
		if reference.Array {
			items, ok := value.([]any)
			if !ok {
				return nil, fmt.Errorf("invalid reference image list")
			}
			for i := range items {
				item, err := convert(items[i])
				if err != nil {
					return nil, err
				}
				items[i] = item
			}
		} else {
			converted, err := convert(value)
			if err != nil {
				return nil, err
			}
			input[key] = converted
		}
	}
	return json.Marshal(input)
}
