package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func writeGeminiSSE(w http.ResponseWriter, chunks ...map[string]any) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, chunk := range chunks {
		data, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", data)
	}
}

func TestGeminiStreamTextAndUsage(t *testing.T) {
	var path, query, apiKey string
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		query = r.URL.RawQuery
		apiKey = r.Header.Get("x-goog-api-key")
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		writeGeminiSSE(w,
			map[string]any{"candidates": []any{map[string]any{
				"content": map[string]any{"role": "model", "parts": []any{map[string]any{"text": "Hi"}}},
			}}},
			map[string]any{
				"candidates": []any{map[string]any{
					"content":      map[string]any{"parts": []any{map[string]any{"text": " there"}, map[string]any{"text": "thinking...", "thought": true}}},
					"finishReason": "STOP",
				}},
				"usageMetadata": map[string]any{
					"promptTokenCount": 8, "candidatesTokenCount": 4, "totalTokenCount": 12, "cachedContentTokenCount": 2,
				},
			},
		)
	}))
	defer srv.Close()

	c := NewGeminiClient(srv.URL, "gem-key")
	var text, reasoning strings.Builder
	resp, err := c.Stream(context.Background(), ChatRequest{
		Model:     "gemini-2.5-pro",
		MaxTokens: 256,
		Messages: []Message{
			{Role: "system", Content: "be brief"},
			{Role: "user", Content: "show me", Images: []Image{{MIME: "image/jpeg", Data: "aGk="}}},
		},
	}, func(d Delta) error {
		text.WriteString(d.Content)
		reasoning.WriteString(d.Reasoning)
		return nil
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if path != "/v1beta/models/gemini-2.5-pro:streamGenerateContent" || query != "alt=sse" || apiKey != "gem-key" {
		t.Errorf("target = %q?%s key=%q", path, query, apiKey)
	}
	if resp.Message.Content != "Hi there" || text.String() != "Hi there" {
		t.Errorf("content = %q / streamed %q", resp.Message.Content, text.String())
	}
	if resp.Message.Reasoning != "thinking..." || reasoning.String() != "thinking..." {
		t.Errorf("reasoning = %q / streamed %q", resp.Message.Reasoning, reasoning.String())
	}
	if resp.Usage.PromptTokens != 8 || resp.Usage.CompletionTokens != 4 || resp.Usage.TotalTokens != 12 || resp.Usage.PromptCacheHitTokens != 2 {
		t.Errorf("usage = %#v", resp.Usage)
	}
	if resp.FinishReason != "stop" {
		t.Errorf("finish = %q", resp.FinishReason)
	}

	sys, _ := body["systemInstruction"].(map[string]any)
	parts, _ := sys["parts"].([]any)
	if len(parts) != 1 || parts[0].(map[string]any)["text"] != "be brief" {
		t.Errorf("systemInstruction = %#v", body["systemInstruction"])
	}
	contents, _ := body["contents"].([]any)
	if len(contents) != 1 {
		t.Fatalf("contents = %#v", body["contents"])
	}
	userParts, _ := contents[0].(map[string]any)["parts"].([]any)
	var sawText, sawImage bool
	for _, raw := range userParts {
		part, _ := raw.(map[string]any)
		if part["text"] == "show me" {
			sawText = true
		}
		if inline, ok := part["inlineData"].(map[string]any); ok && inline["mimeType"] == "image/jpeg" && inline["data"] == "aGk=" {
			sawImage = true
		}
	}
	if !sawText || !sawImage {
		t.Errorf("user parts = %#v", userParts)
	}
}

func TestGeminiStreamFunctionCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeGeminiSSE(w, map[string]any{
			"candidates": []any{map[string]any{
				"content": map[string]any{"role": "model", "parts": []any{map[string]any{
					"functionCall": map[string]any{"name": "bash", "args": map[string]any{"command": "ls"}},
				}}},
				"finishReason": "STOP",
			}},
		})
	}))
	defer srv.Close()

	resp, err := NewGeminiClient(srv.URL, "k").Stream(context.Background(), ChatRequest{
		Model:    "gemini-2.5-pro",
		Messages: []Message{{Role: "user", Content: "list files"}},
	}, nil)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if len(resp.Message.ToolCalls) != 1 {
		t.Fatalf("tool calls = %#v", resp.Message.ToolCalls)
	}
	tc := resp.Message.ToolCalls[0]
	if tc.Function.Name != "bash" || tc.Function.Arguments != `{"command":"ls"}` || tc.ID == "" {
		t.Errorf("tool call = %#v", tc)
	}
	if resp.FinishReason != "tool_calls" {
		t.Errorf("finish = %q", resp.FinishReason)
	}
}

func TestGeminiBuildsToolDeclarationsAndFunctionResponses(t *testing.T) {
	c := NewGeminiClient("", "k")
	payload, err := c.buildBody(ChatRequest{
		Model: "gemini-2.5-pro",
		Messages: []Message{
			{Role: "user", Content: "run it"},
			{Role: "assistant", ToolCalls: []ToolCall{{ID: "t1", Type: "function", Function: FunctionCall{Name: "bash", Arguments: `{"command":"ls"}`}}}},
			{Role: "tool", ToolCallID: "t1", Content: "a.txt"},
		},
		Tools: []Tool{{Type: "function", Function: FunctionDef{
			Name:        "bash",
			Description: "Run a command",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"command":{"type":"string","default":""}},"additionalProperties":false}`),
		}}},
	})
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}
	var body struct {
		Contents []geminiContent `json:"contents"`
		Tools    []geminiTool    `json:"tools"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Contents) != 3 {
		t.Fatalf("contents = %#v", body.Contents)
	}
	if body.Contents[1].Role != "model" || body.Contents[1].Parts[0].FunctionCall == nil {
		t.Errorf("assistant content = %#v", body.Contents[1])
	}
	fr := body.Contents[2].Parts[0].FunctionResponse
	if fr == nil || fr.Name != "bash" || fr.Response["result"] != "a.txt" {
		t.Errorf("function response = %#v", fr)
	}
	if len(body.Tools) != 1 || len(body.Tools[0].FunctionDeclarations) != 1 {
		t.Fatalf("tools = %#v", body.Tools)
	}
	params, _ := body.Tools[0].FunctionDeclarations[0].Parameters.(map[string]any)
	if params["type"] != "OBJECT" {
		t.Errorf("schema type should be uppercase: %#v", params)
	}
	if _, exists := params["additionalProperties"]; exists {
		t.Errorf("additionalProperties should be dropped: %#v", params)
	}
	props := params["properties"].(map[string]any)
	command := props["command"].(map[string]any)
	if command["type"] != "STRING" {
		t.Errorf("nested schema type = %#v", command)
	}
	if _, exists := command["default"]; exists {
		t.Errorf("default should be dropped: %#v", command)
	}
}

func TestGeminiComplete(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		fmt.Fprint(w, `{
			"candidates":[{"content":{"role":"model","parts":[{"text":"done"}]},"finishReason":"STOP"}],
			"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":2,"totalTokenCount":9}
		}`)
	}))
	defer srv.Close()
	resp, err := NewGeminiClient(srv.URL, "k").Complete(context.Background(), ChatRequest{
		Model: "gemini-2.5-pro", Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if path != "/v1beta/models/gemini-2.5-pro:generateContent" {
		t.Errorf("path = %q", path)
	}
	if resp.Message.Content != "done" || resp.Usage.TotalTokens != 9 || resp.FinishReason != "stop" {
		t.Errorf("response = %#v", resp)
	}
}

func TestGeminiListModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"models":[{"name":"models/gemini-b"},{"name":"models/gemini-a"}]}`)
	}))
	defer srv.Close()
	models, err := NewGeminiClient(srv.URL, "k").ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 2 || models[0] != "gemini-a" || models[1] != "gemini-b" {
		t.Errorf("models = %v", models)
	}
}

func TestNewProviderSelectsAdapter(t *testing.T) {
	if _, ok := NewProvider("anthropic", "", "").(*AnthropicClient); !ok {
		t.Error("anthropic provider should use AnthropicClient")
	}
	if _, ok := NewProvider("gemini", "", "").(*GeminiClient); !ok {
		t.Error("gemini provider should use GeminiClient")
	}
	for _, kind := range []string{"openai", "ollama", "", "deepseek"} {
		if _, ok := NewProvider(kind, "", "").(*Client); !ok {
			t.Errorf("%q provider should use the OpenAI-compatible Client", kind)
		}
	}
}
