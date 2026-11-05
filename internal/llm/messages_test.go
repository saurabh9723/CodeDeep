package llm

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestToAPIMessagesEncodesImagesAsContentParts(t *testing.T) {
	msgs := []Message{
		{Role: "user", Content: "what is this?", Images: []Image{{MIME: "image/png", Data: "aGVsbG8="}}},
		{Role: "assistant", Content: "a screenshot"},
	}
	out := toAPIMessages(msgs)
	raw, err := json.Marshal(out[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(raw)
	if !strings.Contains(got, `"type":"text"`) || !strings.Contains(got, `"text":"what is this?"`) {
		t.Errorf("missing text part: %s", got)
	}
	if !strings.Contains(got, `"type":"image_url"`) {
		t.Errorf("missing image part: %s", got)
	}
	if !strings.Contains(got, `"url":"data:image/png;base64,aGVsbG8="`) {
		t.Errorf("missing data URL: %s", got)
	}

	plain, err := json.Marshal(out[1])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(plain), `"content":"a screenshot"`) {
		t.Errorf("assistant content should stay a plain string: %s", plain)
	}
}

func TestToAPIMessagesImageOnlyOmitsTextField(t *testing.T) {
	out := toAPIMessages([]Message{{
		Role:   "user",
		Images: []Image{{MIME: "image/jpeg", Data: "aGk="}},
	}})
	raw, err := json.Marshal(out[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(raw)
	if strings.Contains(got, `"type":"text"`) {
		t.Errorf("image-only message should not include an empty text part: %s", got)
	}
	if !strings.Contains(got, `"type":"image_url"`) || !strings.Contains(got, `data:image/jpeg;base64,aGk=`) {
		t.Errorf("missing image part: %s", got)
	}
}

func TestToAPIMessagesDefaultsImageMIME(t *testing.T) {
	out := toAPIMessages([]Message{{
		Role:    "user",
		Content: "hi",
		Images:  []Image{{Data: "aGk="}},
	}})
	raw, _ := json.Marshal(out[0])
	if !strings.Contains(string(raw), "data:image/png;base64,aGk=") {
		t.Errorf("empty MIME should default to image/png: %s", raw)
	}
}

func TestEstimatedTokensCountsImages(t *testing.T) {
	plain := EstimatedTokens([]Message{{Role: "user", Content: "hi"}})
	withImage := EstimatedTokens([]Message{{Role: "user", Content: "hi", Images: []Image{{MIME: "image/png", Data: "aGk="}}}})
	if withImage-plain < 1000 {
		t.Errorf("image should add token estimate, got plain=%d withImage=%d", plain, withImage)
	}
}
