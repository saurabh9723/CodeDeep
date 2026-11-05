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

func writeAnthropicSSE(w http.ResponseWriter, events ...map[string]any) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, ev := range events {
		data, _ := json.Marshal(ev)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev["type"], data)
	}
}

func TestAnthropicStreamTextAndToolUse(t *testing.T) {
	var path, apiKey, version string
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		apiKey = r.Header.Get("x-api-key")
		version = r.Header.Get("anthropic-version")
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		writeAnthropicSSE(w,
			map[string]any{"type": "message_start", "message": map[string]any{
				"usage": map[string]any{"input_tokens": 11, "cache_read_input_tokens": 3},
			}},
			map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text"}},
			map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": "Hello"}},
			map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": " world"}},
			map[string]any{"type": "content_block_start", "index": 1, "content_block": map[string]any{
				"type": "tool_use", "id": "toolu_1", "name": "read_file",
			}},
			map[string]any{"type": "content_block_delta", "index": 1, "delta": map[string]any{"type": "input_json_delta", "partial_json": `{"path":`}},
			map[string]any{"type": "content_block_delta", "index": 1, "delta": map[string]any{"type": "input_json_delta", "partial_json": `"a.txt"}`}},
			map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "tool_use"}, "usage": map[string]any{"output_tokens": 7}},
			map[string]any{"type": "message_stop"},
		)
	}))
	defer srv.Close()

	c := NewAnthropicClient(srv.URL, "sk-ant-test")
	var streamed strings.Builder
	resp, err := c.Stream(context.Background(), ChatRequest{
		Model:     "claude-test",
		MaxTokens: 100,
		Messages: []Message{
			{Role: "system", Content: "be brief"},
			{Role: "user", Content: "read it", Images: []Image{{MIME: "image/png", Data: "aGk="}}},
		},
		Tools: []Tool{{Type: "function", Function: FunctionDef{
			Name:        "read_file",
			Description: "Read a file",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`),
		}}},
	}, func(d Delta) error {
		streamed.WriteString(d.Content)
		return nil
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if path != "/v1/messages" || apiKey != "sk-ant-test" || version != anthropicVersion {
		t.Errorf("request target/headers = %q %q %q", path, apiKey, version)
	}
	if streamed.String() != "Hello world" || resp.Message.Content != "Hello world" {
		t.Errorf("text = %q / %q", streamed.String(), resp.Message.Content)
	}
	if len(resp.Message.ToolCalls) != 1 {
		t.Fatalf("tool calls = %#v", resp.Message.ToolCalls)
	}
	tc := resp.Message.ToolCalls[0]
	if tc.ID != "toolu_1" || tc.Function.Name != "read_file" || tc.Function.Arguments != `{"path":"a.txt"}` {
		t.Errorf("tool call = %#v", tc)
	}
	if resp.FinishReason != "tool_calls" {
		t.Errorf("finish reason = %q", resp.FinishReason)
	}
	if resp.Usage.PromptTokens != 11 || resp.Usage.CompletionTokens != 7 || resp.Usage.TotalTokens != 18 || resp.Usage.PromptCacheHitTokens != 3 {
		t.Errorf("usage = %#v", resp.Usage)
	}

	if body["system"] != "be brief" {
		t.Errorf("system = %#v", body["system"])
	}
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages = %#v", body["messages"])
	}
	blocks, _ := msgs[0].(map[string]any)["content"].([]any)
	var sawText, sawImage bool
	for _, raw := range blocks {
		block, _ := raw.(map[string]any)
		switch block["type"] {
		case "text":
			sawText = block["text"] == "read it"
		case "image":
			src, _ := block["source"].(map[string]any)
			sawImage = src["media_type"] == "image/png" && src["data"] == "aGk="
		}
	}
	if !sawText || !sawImage {
		t.Errorf("message blocks = %#v", blocks)
	}
	tools, _ := body["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %#v", body["tools"])
	}
	tool, _ := tools[0].(map[string]any)
	if tool["name"] != "read_file" || tool["input_schema"] == nil {
		t.Errorf("tool declaration = %#v", tool)
	}
}

func TestAnthropicCompleteParsesBlocksAndUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
			"content":[
				{"type":"text","text":"done"},
				{"type":"tool_use","id":"toolu_9","name":"bash","input":{"command":"ls"}}
			],
			"stop_reason":"tool_use",
			"usage":{"input_tokens":5,"output_tokens":3,"cache_read_input_tokens":2}
		}`)
	}))
	defer srv.Close()

	resp, err := NewAnthropicClient(srv.URL, "k").Complete(context.Background(), ChatRequest{
		Model:    "claude-test",
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Message.Content != "done" || len(resp.Message.ToolCalls) != 1 {
		t.Fatalf("response = %#v", resp)
	}
	if resp.Message.ToolCalls[0].Function.Arguments != `{"command":"ls"}` {
		t.Errorf("tool args = %q", resp.Message.ToolCalls[0].Function.Arguments)
	}
	if resp.Usage.PromptTokens != 5 || resp.Usage.CompletionTokens != 3 || resp.Usage.TotalTokens != 8 {
		t.Errorf("usage = %#v", resp.Usage)
	}
	if resp.FinishReason != "tool_calls" {
		t.Errorf("finish = %q", resp.FinishReason)
	}
}

func TestAnthropicBuildsToolResultMessages(t *testing.T) {
	c := NewAnthropicClient("", "k")
	payload, err := c.buildBody(ChatRequest{
		Model: "claude-test",
		Messages: []Message{
			{Role: "user", Content: "run it"},
			{Role: "assistant", ToolCalls: []ToolCall{{ID: "t1", Type: "function", Function: FunctionCall{Name: "bash", Arguments: `{"command":"ls"}`}}}},
			{Role: "tool", ToolCallID: "t1", Content: "a.txt\nb.txt"},
		},
	}, false)
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}
	var body struct {
		Messages []struct {
			Role    string           `json:"role"`
			Content []anthropicBlock `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Messages) != 3 {
		t.Fatalf("messages = %#v", body.Messages)
	}
	if body.Messages[1].Content[0].Type != "tool_use" || body.Messages[1].Content[0].ID != "t1" {
		t.Errorf("assistant block = %#v", body.Messages[1].Content[0])
	}
	last := body.Messages[2]
	if last.Role != "user" || last.Content[0].Type != "tool_result" || last.Content[0].ToolUseID != "t1" || last.Content[0].Content != "a.txt\nb.txt" {
		t.Errorf("tool result = %#v", last)
	}
}

func TestAnthropicExtendedThinkingRequest(t *testing.T) {
	c := NewAnthropicClient("", "k")
	payload, err := c.buildBody(ChatRequest{
		Model:           "claude-test",
		MaxTokens:       1000,
		Temperature:     0.7,
		ReasoningBudget: 2048,
		Messages:        []Message{{Role: "user", Content: "think"}},
	}, true)
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	thinking, _ := body["thinking"].(map[string]any)
	if thinking["type"] != "enabled" || thinking["budget_tokens"] != float64(2048) {
		t.Errorf("thinking = %#v", body["thinking"])
	}
	if body["max_tokens"] != float64(2048+4096) {
		t.Errorf("max_tokens = %#v, want budget+4096", body["max_tokens"])
	}
	if _, ok := body["temperature"]; ok {
		t.Errorf("temperature must be omitted with extended thinking: %#v", body["temperature"])
	}
}

func TestAnthropicEchoesThinkingBlocks(t *testing.T) {
	c := NewAnthropicClient("", "k")
	payload, err := c.buildBody(ChatRequest{
		Model:           "claude-test",
		ReasoningBudget: 2048,
		Messages: []Message{
			{Role: "user", Content: "run it"},
			{Role: "assistant", Reasoning: "list the files", ReasoningSignature: "sig-1", ToolCalls: []ToolCall{{
				ID: "t1", Type: "function", Function: FunctionCall{Name: "bash", Arguments: `{"command":"ls"}`},
			}}},
			{Role: "tool", ToolCallID: "t1", Content: "a.txt"},
		},
	}, false)
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}
	var echo struct {
		Messages []struct {
			Role    string           `json:"role"`
			Content []anthropicBlock `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(payload, &echo); err != nil {
		t.Fatalf("decode: %v", err)
	}
	assistant := echo.Messages[1].Content
	if len(assistant) != 2 || assistant[0].Type != "thinking" {
		t.Fatalf("assistant blocks = %#v", assistant)
	}
	if assistant[0].Thinking != "list the files" || assistant[0].Signature != "sig-1" {
		t.Errorf("thinking block = %#v", assistant[0])
	}
	if assistant[1].Type != "tool_use" {
		t.Errorf("tool block = %#v", assistant[1])
	}

	payload, err = c.buildBody(ChatRequest{
		Model: "claude-test",
		Messages: []Message{
			{Role: "user", Content: "run it"},
			{Role: "assistant", Reasoning: "list the files", ReasoningSignature: "sig-1"},
		},
	}, false)
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}
	if strings.Contains(string(payload), `"type":"thinking"`) {
		t.Error("thinking blocks must not be sent while extended thinking is disabled")
	}
}

func TestAnthropicThinkingParsedFromComplete(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
			"content":[
				{"type":"thinking","thinking":"plan first","signature":"sig-9"},
				{"type":"text","text":"done"}
			],
			"stop_reason":"end_turn",
			"usage":{"input_tokens":4,"output_tokens":2}
		}`)
	}))
	defer srv.Close()

	resp, err := NewAnthropicClient(srv.URL, "k").Complete(context.Background(), ChatRequest{
		Model: "claude-test", Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Message.Reasoning != "plan first" || resp.Message.ReasoningSignature != "sig-9" || resp.Message.Content != "done" {
		t.Errorf("reasoning = %q / sig %q / content %q", resp.Message.Reasoning, resp.Message.ReasoningSignature, resp.Message.Content)
	}
}

func TestAnthropicThinkingStreamedWithSignature(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeAnthropicSSE(w,
			map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "thinking"}},
			map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "thinking_delta", "thinking": "pondering"}},
			map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "signature_delta", "signature": "sig-stream"}},
			map[string]any{"type": "content_block_start", "index": 1, "content_block": map[string]any{"type": "text"}},
			map[string]any{"type": "content_block_delta", "index": 1, "delta": map[string]any{"type": "text_delta", "text": "answer"}},
			map[string]any{"type": "message_stop"},
		)
	}))
	defer srv.Close()

	resp, err := NewAnthropicClient(srv.URL, "k").Stream(context.Background(), ChatRequest{
		Model: "claude-test", Messages: []Message{{Role: "user", Content: "hi"}},
	}, nil)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if resp.Message.Reasoning != "pondering" || resp.Message.ReasoningSignature != "sig-stream" || resp.Message.Content != "answer" {
		t.Errorf("streamed reasoning = %q / sig %q / content %q", resp.Message.Reasoning, resp.Message.ReasoningSignature, resp.Message.Content)
	}
}

func TestAnthropicListModels(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		fmt.Fprint(w, `{"data":[{"id":"claude-b"},{"id":"claude-a"}]}`)
	}))
	defer srv.Close()
	models, err := NewAnthropicClient(srv.URL, "k").ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if path != "/v1/models" || len(models) != 2 || models[0] != "claude-a" || models[1] != "claude-b" {
		t.Errorf("path=%q models=%v", path, models)
	}
}
