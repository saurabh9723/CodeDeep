package web

import (
	"encoding/base64"
	"fmt"
	"strings"

	"codedeep/internal/llm"
)

const (
	maxImagesPerMessage = 4
	maxImageBytes       = 5 << 20
)

var allowedImageMIME = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/webp": true,
	"image/gif":  true,
}

// validateImages normalizes pasted image attachments: it checks the MIME type,
// decodes (and re-encodes) the payload, and enforces per-image and count limits.
func validateImages(in []llm.Image) ([]llm.Image, error) {
	if len(in) == 0 {
		return nil, nil
	}
	if len(in) > maxImagesPerMessage {
		return nil, fmt.Errorf("too many images: %d (max %d)", len(in), maxImagesPerMessage)
	}
	out := make([]llm.Image, 0, len(in))
	for i, img := range in {
		mime := strings.ToLower(strings.TrimSpace(img.MIME))
		if mime == "" {
			mime = "image/png"
		}
		if !allowedImageMIME[mime] {
			return nil, fmt.Errorf("image %d: unsupported type %q (use png, jpeg, webp, or gif)", i+1, mime)
		}
		data := strings.TrimSpace(img.Data)
		if strings.HasPrefix(data, "data:") {
			if idx := strings.Index(data, ","); idx >= 0 {
				data = data[idx+1:]
			}
		}
		data = strings.TrimSpace(data)
		if data == "" {
			return nil, fmt.Errorf("image %d: empty data", i+1)
		}
		raw, err := base64.StdEncoding.DecodeString(data)
		if err != nil {
			return nil, fmt.Errorf("image %d: invalid base64: %w", i+1, err)
		}
		if len(raw) == 0 {
			return nil, fmt.Errorf("image %d: empty data", i+1)
		}
		if len(raw) > maxImageBytes {
			return nil, fmt.Errorf("image %d: too large (%d KB, max %d KB)", i+1, len(raw)/1024, maxImageBytes/1024)
		}
		out = append(out, llm.Image{MIME: mime, Data: base64.StdEncoding.EncodeToString(raw)})
	}
	return out, nil
}
