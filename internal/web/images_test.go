package web

import (
	"encoding/base64"
	"strings"
	"testing"

	"codedeep/internal/llm"
)

func TestValidateImagesAcceptsDataURLAndNormalizesMIME(t *testing.T) {
	payload := base64.StdEncoding.EncodeToString([]byte("hello"))
	out, err := validateImages([]llm.Image{{MIME: " IMAGE/PNG ", Data: "data:image/png;base64," + payload}})
	if err != nil {
		t.Fatalf("validateImages: %v", err)
	}
	if len(out) != 1 || out[0].MIME != "image/png" || out[0].Data != payload {
		t.Fatalf("unexpected normalized image: %#v", out)
	}
}

func TestValidateImagesRejectsUnsupportedType(t *testing.T) {
	_, err := validateImages([]llm.Image{{MIME: "image/svg+xml", Data: "aGk="}})
	if err == nil || !strings.Contains(err.Error(), "unsupported type") {
		t.Fatalf("want unsupported type error, got %v", err)
	}
}

func TestValidateImagesRejectsBadBase64(t *testing.T) {
	_, err := validateImages([]llm.Image{{MIME: "image/png", Data: "not-base64!!"}})
	if err == nil || !strings.Contains(err.Error(), "invalid base64") {
		t.Fatalf("want base64 error, got %v", err)
	}
}

func TestValidateImagesRejectsTooMany(t *testing.T) {
	images := make([]llm.Image, maxImagesPerMessage+1)
	for i := range images {
		images[i] = llm.Image{MIME: "image/png", Data: "aGk="}
	}
	_, err := validateImages(images)
	if err == nil || !strings.Contains(err.Error(), "too many images") {
		t.Fatalf("want too many images error, got %v", err)
	}
}

func TestValidateImagesRejectsOversized(t *testing.T) {
	huge := base64.StdEncoding.EncodeToString(make([]byte, maxImageBytes+1))
	_, err := validateImages([]llm.Image{{MIME: "image/png", Data: huge}})
	if err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("want too large error, got %v", err)
	}
}

func TestValidateImagesEmpty(t *testing.T) {
	out, err := validateImages(nil)
	if err != nil || out != nil {
		t.Fatalf("empty input should be a no-op, got %#v %v", out, err)
	}
}
