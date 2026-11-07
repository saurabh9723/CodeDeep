package llm

import (
	"context"
	"io"
	"strings"
	"testing"
)

func textToolTools() []Tool {
	return []Tool{
		{Type: "function", Function: FunctionDef{Name: "read_file"}},
		{Type: "function", Function: FunctionDef{Name: "edit_file"}},
	}
}

func TestRecoverTextToolCalls(t *testing.T) {
	tools := textToolTools()
	cases := []struct {
		name     string
		content  string
		wantOK   bool
		want     int
		wantArgs string
		leftover string
	}{
		{
			name:     "bare object",
			content:  `{"name": "read_file", "arguments": {"path": "hello.txt"}}`,
			wantOK:   true,
			want:     1,
			wantArgs: `{"path": "hello.txt"}`,
		},
		{
			name:    "fenced json",
			content: "```json\n{\"name\": \"read_file\", \"arguments\": {\"path\": \"hello.txt\"}}\n```",
			wantOK:  true,
			want:    1,
		},
		{
			name:     "wrapped with prose leftover",
			content:  "I'll read it.\n<tool_call>{\"name\": \"read_file\", \"arguments\": {\"path\": \"a\"}}</tool_call>",
			wantOK:   true,
			want:     1,
			leftover: "I'll read it.",
		},
		{
			name:    "array of calls",
			content: `[{"name": "read_file", "arguments": {"path": "a"}}, {"name": "edit_file", "arguments": {"path": "b", "old": "x", "new": "y"}}]`,
			wantOK:  true,
			want:    2,
		},
		{
			name:    "string encoded arguments",
			content: `{"name": "read_file", "arguments": "{\"path\": \"hello.txt\"}"}`,
			wantOK:  true,
			want:    1,
		},
		{
			name:    "parameters alias",
			content: `{"name": "read_file", "parameters": {"path": "hello.txt"}}`,
			wantOK:  true,
			want:    1,
		},
		{
			name:     "prose around bare json recovered",
			content:  `Here you go: {"name": "read_file", "arguments": {}}`,
			wantOK:   true,
			want:     1,
			leftover: "Here you go:",
		},
		{
			name:     "several objects then summary",
			content:  "{\"name\": \"read_file\", \"arguments\": {\"path\": \"a\"}}\n{\"name\": \"read_file\", \"arguments\": {\"path\": \"b\"}}\n\n# Summary\n- done",
			wantOK:   true,
			want:     2,
			leftover: "# Summary\n- done",
		},
		{
			name:    "unknown tool rejected",
			content: `{"name": "delete_everything", "arguments": {}}`,
			wantOK:  false,
		},
		{
			name:    "plain prose rejected",
			content: "The file says hello.",
			wantOK:  false,
		},
		{
			name:    "malformed arguments rejected",
			content: `{"name": "read_file", "arguments": "{not json}"}`,
			wantOK:  false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls, leftover, ok := recoverTextToolCalls(tc.content, tools)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !tc.wantOK {
				return
			}
			if len(calls) != tc.want {
				t.Fatalf("calls = %d, want %d", len(calls), tc.want)
			}
			if tc.wantArgs != "" && calls[0].Function.Arguments != tc.wantArgs {
				t.Fatalf("arguments = %q, want %q", calls[0].Function.Arguments, tc.wantArgs)
			}
			if leftover != tc.leftover {
				t.Fatalf("leftover = %q, want %q", leftover, tc.leftover)
			}
		})
	}
}

func TestParseStreamRecoversBareJSONToolCall(t *testing.T) {
	stream := "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"{\\\"name\\\": \\\"read_file\\\", \\\"arguments\\\": {\\\"path\\\": \\\"hello.txt\\\"}}\"},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	client := NewClient("http://example.invalid", "")
	resp, err := client.parseStream(context.Background(), io.NopCloser(strings.NewReader(stream)), nil, textToolTools())
	if err != nil {
		t.Fatalf("parseStream: %v", err)
	}
	if len(resp.Message.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(resp.Message.ToolCalls))
	}
	if got := resp.Message.ToolCalls[0].Function.Name; got != "read_file" {
		t.Fatalf("name = %q, want read_file", got)
	}
	if resp.Message.Content != "" {
		t.Fatalf("content = %q, want empty", resp.Message.Content)
	}
	if resp.FinishReason != "tool_calls" {
		t.Fatalf("finish reason = %q, want tool_calls", resp.FinishReason)
	}
}

func TestParseStreamKeepsPlainAnswer(t *testing.T) {
	stream := "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"All done.\"},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	client := NewClient("http://example.invalid", "")
	resp, err := client.parseStream(context.Background(), io.NopCloser(strings.NewReader(stream)), nil, textToolTools())
	if err != nil {
		t.Fatalf("parseStream: %v", err)
	}
	if len(resp.Message.ToolCalls) != 0 {
		t.Fatalf("tool calls = %d, want 0", len(resp.Message.ToolCalls))
	}
	if resp.Message.Content != "All done." {
		t.Fatalf("content = %q, want %q", resp.Message.Content, "All done.")
	}
}
