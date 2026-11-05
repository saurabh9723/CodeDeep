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

func TestBuildBodyIncludesReasoningEffort(t *testing.T) {
	c := NewClient("http://example.test", "k")
	payload, err := c.buildBody(ChatRequest{
		Model:           "m",
		ReasoningEffort: " high ",
		Messages:        []Message{{Role: "user", Content: "hi"}},
	}, true)
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["reasoning_effort"] != "high" {
		t.Errorf("reasoning_effort = %#v", body["reasoning_effort"])
	}

	payload, err = c.buildBody(ChatRequest{Model: "m", Messages: []Message{{Role: "user", Content: "hi"}}}, true)
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}
	body = nil
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := body["reasoning_effort"]; ok {
		t.Error("reasoning_effort should be omitted when unset")
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
