package mockllm

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type completion struct {
	Choices []struct {
		Message struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
}

func chat(t *testing.T, handler http.Handler, system string) completion {
	t.Helper()
	body := map[string]any{
		"model":  "mock-model",
		"stream": false,
		"messages": []map[string]any{
			{"role": "system", "content": system},
			{"role": "user", "content": "go"},
		},
	}
	data, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(data)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var out completion
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

func TestScriptConsumesStepsInOrder(t *testing.T) {
	script, err := ParseScript([]byte(`{
		"name": "two-step",
		"steps": [
			{"reasoning": "read first", "text": "Reading.", "calls": [{"name": "read_file", "args": {"path": "go.mod"}}]},
			{"text": "Done."}
		]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	srv := New(Options{Steps: script.Steps})
	server := httptest.NewServer(srv)
	defer server.Close()

	first := chat(t, srv, "system")
	if len(first.Choices) != 1 || first.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("first finish = %+v", first.Choices)
	}
	calls := first.Choices[0].Message.ToolCalls
	if len(calls) != 1 || calls[0].Function.Name != "read_file" {
		t.Fatalf("first calls = %+v", calls)
	}
	if calls[0].Function.Arguments != `{"path": "go.mod"}` {
		t.Fatalf("arguments = %q", calls[0].Function.Arguments)
	}

	second := chat(t, srv, "system")
	if len(second.Choices[0].Message.ToolCalls) != 0 {
		t.Fatalf("second should have no calls: %+v", second.Choices[0].Message.ToolCalls)
	}
	if second.Choices[0].Message.Content != "Done." {
		t.Fatalf("second content = %q", second.Choices[0].Message.Content)
	}

	third := chat(t, srv, "system")
	if third.Choices[0].Message.Content != "Eval script complete." {
		t.Fatalf("exhausted script content = %q", third.Choices[0].Message.Content)
	}
	if srv.Requests() != 3 {
		t.Fatalf("requests = %d", srv.Requests())
	}
}

func TestSummaryAndCompactionDoNotConsumeSteps(t *testing.T) {
	srv := New(Options{Steps: []Step{{Text: "Only step."}}})
	chat(t, srv, "You summarize conversations for compaction.")
	out := chat(t, srv, "normal system")
	if out.Choices[0].Message.Content != "Only step." {
		t.Fatalf("script step was consumed by the summary request: %+v", out.Choices)
	}
}

func TestLegacyWriteScenarioIncludesToolCall(t *testing.T) {
	srv := New(Options{Scenario: "write"})
	out := chat(t, srv, "system")
	if len(out.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("tool calls = %+v", out.Choices[0].Message.ToolCalls)
	}
	call := out.Choices[0].Message.ToolCalls[0]
	if call.Function.Name != "write_file" || !strings.Contains(call.Function.Arguments, "mock-output.txt") {
		t.Fatalf("call = %+v", call)
	}
	if out.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("finish = %q", out.Choices[0].FinishReason)
	}
}

func TestParseScriptRejectsInvalidSteps(t *testing.T) {
	cases := map[string]string{
		"empty":       `{"steps":[]}`,
		"empty-step":  `{"steps":[{"reasoning":"only reasoning"}]}`,
		"no-name":     `{"steps":[{"calls":[{"args":{}}]}]}`,
		"bad-args":    `{"steps":[{"calls":[{"name":"bash","args":"echo hi"}]}]}`,
		"unknown-key": `{"steps":[{"text":"ok","tools":[]}]}`,
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseScript([]byte(data)); err == nil {
				t.Fatalf("expected error for %s", data)
			}
		})
	}
}

func TestFlakyScenarioRateLimitsFirstTwoRequests(t *testing.T) {
	srv := New(Options{Scenario: "flaky"})
	for i := 1; i <= 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"messages":[]}`))
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("attempt %d status = %d", i, rec.Code)
		}
		if rec.Header().Get("Retry-After") != "0" {
			t.Fatalf("attempt %d missing Retry-After", i)
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"stream":false,"messages":[]}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("third status = %d", rec.Code)
	}
}
