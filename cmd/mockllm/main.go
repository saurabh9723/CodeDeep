// Command mockllm is a deterministic OpenAI-compatible server used by the
// end-to-end tests. It streams scripted reasoning, text, and tool calls so the
// full agent loop can be verified offline.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

type requestMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  []any           `json:"tool_calls"`
	ToolCallID string          `json:"tool_call_id"`
}

type chatRequest struct {
	Model    string           `json:"model"`
	Messages []requestMessage `json:"messages"`
	Stream   bool             `json:"stream"`
	Tools    []any            `json:"tools"`
}

func (m requestMessage) text() string {
	if len(m.Content) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(m.Content, &s) == nil {
		return s
	}
	return string(m.Content)
}

type server struct {
	scenario string
	delay    time.Duration
	requests atomic.Int32
}

func main() {
	addr := flag.String("addr", "127.0.0.1:18080", "listen address")
	scenario := flag.String("scenario", "basic", "basic, multi, subagent, read, write, text, flaky")
	delay := flag.Duration("delay", 25*time.Millisecond, "delay between streamed chunks")
	flag.Parse()

	s := &server{scenario: *scenario, delay: *delay}
	if *scenario == "xss" {
		seedXSSFixture()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /chat/completions", s.handleChat)
	mux.HandleFunc("POST /v1/chat/completions", s.handleChat)
	mux.HandleFunc("GET /models", s.handleModels)
	mux.HandleFunc("GET /v1/models", s.handleModels)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})

	log.Printf("mockllm listening on http://%s (scenario=%s)", *addr, *scenario)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Fatal(err)
	}
}

func seedXSSFixture() {
	const body = "<img src=x onerror=\"window.__xss=4\">\n<b>raw html</b>\n"
	if err := os.MkdirAll(".tmp", 0o755); err != nil {
		log.Printf("xss fixture: %v", err)
		return
	}
	if err := os.WriteFile(filepath.Join(".tmp", "xss.html"), []byte(body), 0o644); err != nil {
		log.Printf("xss fixture: %v", err)
	}
}

func (s *server) handleModels(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"object": "list",
		"data": []map[string]string{
			{"id": "deepseek-chat", "object": "model"},
			{"id": "deepseek-reasoner", "object": "model"},
			{"id": "mock-model", "object": "model"},
		},
	})
}

func (s *server) handleChat(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":{"message":"bad json"}}`, http.StatusBadRequest)
		return
	}
	system := ""
	if len(req.Messages) > 0 && req.Messages[0].Role == "system" {
		system = req.Messages[0].text()
	}
	last := requestMessage{}
	if len(req.Messages) > 0 {
		last = req.Messages[len(req.Messages)-1]
	}
	roles := make([]string, 0, len(req.Messages))
	for _, m := range req.Messages {
		roles = append(roles, m.Role)
	}
	log.Printf("request model=%s stream=%v tools=%d roles=%s", req.Model, req.Stream, len(req.Tools), strings.Join(roles, ","))

	// The first two attempts are rate limited so the client retry policy and
	// its Retry-After handling can be exercised end to end.
	if s.scenario == "flaky" {
		if n := s.requests.Add(1); n <= 2 {
			log.Printf("  flaky: 429 for attempt %d", n)
			w.Header().Set("Retry-After", "0")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":{"message":"mock rate limit","type":"rate_limit"}}`)
			return
		}
	}

	if strings.Contains(system, "You summarize conversations") {
		s.respondSummary(w, req)
		return
	}

	plan := s.plan(req, system, last)
	log.Printf("  plan=%s", plan.kind)

	if !req.Stream {
		writeJSON(w, map[string]any{
			"id":      "chatcmpl-mock",
			"object":  "chat.completion",
			"created": time.Now().Unix(),
			"model":   req.Model,
			"choices": []any{map[string]any{
				"index": 0,
				"message": map[string]any{
					"role":    "assistant",
					"content": plan.text,
				},
				"finish_reason": "stop",
			}},
			"usage": usageFor(req, plan.text),
		})
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	emit := func(delta map[string]any, finish any) {
		chunk := map[string]any{
			"id":      "chatcmpl-mock",
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   req.Model,
			"choices": []any{map[string]any{
				"index":         0,
				"delta":         delta,
				"finish_reason": finish,
			}},
		}
		writeSSE(w, flusher, chunk)
		time.Sleep(s.delay)
	}

	emit(map[string]any{"role": "assistant", "content": ""}, nil)
	for _, part := range splitWords(plan.reasoning) {
		emit(map[string]any{"reasoning_content": part}, nil)
	}
	for _, part := range splitWords(plan.text) {
		emit(map[string]any{"content": part}, nil)
	}
	for i, call := range plan.calls {
		half := len(call.args) / 2
		emit(map[string]any{"tool_calls": []any{map[string]any{
			"index": i,
			"id":    call.id,
			"type":  "function",
			"function": map[string]any{
				"name":      call.name,
				"arguments": call.args[:half],
			},
		}}}, nil)
		emit(map[string]any{"tool_calls": []any{map[string]any{
			"index": i,
			"function": map[string]any{
				"arguments": call.args[half:],
			},
		}}}, nil)
	}

	finish := "stop"
	if len(plan.calls) > 0 {
		finish = "tool_calls"
	}
	emit(map[string]any{}, finish)
	usage := usageFor(req, plan.text)
	writeSSE(w, flusher, map[string]any{
		"id":      "chatcmpl-mock",
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   req.Model,
		"choices": []any{},
		"usage":   usage,
	})
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

type toolCall struct {
	id   string
	name string
	args string
}

type plan struct {
	kind      string
	reasoning string
	text      string
	calls     []toolCall
}

func (s *server) plan(req chatRequest, system string, last requestMessage) plan {
	if strings.Contains(system, "You are a subagent inside CodeDeep") {
		return plan{
			kind: "subagent-report",
			text: "Subagent report: the module is codedeep and it builds cleanly.",
		}
	}
	if last.Role == "tool" {
		output := strings.TrimSpace(last.text())
		if len(output) > 200 {
			output = output[:200]
		}
		text := "All done. The tool returned:\n\n```\n" + output + "\n```\n\nLet me know if you want another change."
		return plan{kind: "final", text: text}
	}

	switch s.scenario {
	case "flaky":
		return plan{
			kind: "flaky",
			text: "Recovered after the rate limit. The client retried the request and streamed this answer.",
		}
	case "text":
		return plan{
			kind:      "text",
			reasoning: "The user wants a direct answer, so no tools are needed.",
			text:      "Here is a direct answer without using any tools. The mock server streamed this text token by token.",
		}
	case "read":
		return plan{
			kind:      "read",
			reasoning: "I should read the module file before making claims.",
			text:      "Let me read the module definition.",
			calls: []toolCall{{
				id:   "call_read_1",
				name: "read_file",
				args: `{"path":"go.mod","limit":5}`,
			}},
		}
	case "write":
		return plan{
			kind:      "write",
			reasoning: "I will create a small file to prove write access works.",
			text:      "Creating a demo file.",
			calls: []toolCall{{
				id:   "call_write_1",
				name: "write_file",
				args: `{"path":"mock-output.txt","content":"written by the mock model\n"}`,
			}},
		}
	case "multi":
		return plan{
			kind:      "multi",
			reasoning: "Two independent commands can run one after the other.",
			text:      "Running two commands.",
			calls: []toolCall{
				{id: "call_multi_1", name: "bash", args: `{"command":"echo first"}`},
				{id: "call_multi_2", name: "bash", args: `{"command":"echo second"}`},
			},
		}
	case "subagent":
		return plan{
			kind:      "subagent",
			reasoning: "This is a self-contained investigation, so I will delegate it.",
			text:      "Delegating the repository check to a subagent.",
			calls: []toolCall{{
				id:   "call_agent_1",
				name: "agent",
				args: `{"description":"inspect module","prompt":"Inspect the repository and report the Go module name."}`,
			}},
		}
	case "xss":
		return plan{
			kind:      "xss",
			reasoning: "I will test markup handling and read a file with HTML.",
			text:      "Here is **bold**, *italic*, `inline <code>`, and a [safe link](https://example.com).\n\n<img src=x onerror=\"window.__xss=2\">\n\n<script>window.__xss=3</script>\n\n- one\n- two\n\n```html\n<b>code block</b>\n```\n",
			calls: []toolCall{{
				id:   "call_xss_1",
				name: "read_file",
				args: `{"path":".tmp/xss.html"}`,
			}},
		}
	case "mcp":
		return plan{
			kind:      "mcp",
			reasoning: "I will call the MCP echo tool.",
			text:      "Calling the MCP echo tool.",
			calls: []toolCall{{
				id:   "call_mcp_1",
				name: "mcp__mock__echo",
				args: `{"text":"hello from mcp"}`,
			}},
		}
	case "mcp-ro":
		return plan{
			kind:      "mcp-ro",
			reasoning: "I will call the read-only MCP tool.",
			text:      "Calling the read-only MCP tool.",
			calls: []toolCall{{
				id:   "call_mcp_ro_1",
				name: "mcp__mock__workspace_info",
				args: `{}`,
			}},
		}
	default:
		return plan{
			kind:      "basic",
			reasoning: "I need to verify the shell works before summarizing.",
			text:      "Let me check the shell environment first.",
			calls: []toolCall{{
				id:   "call_bash_1",
				name: "bash",
				args: `{"command":"echo mock-tool-ok"}`,
			}},
		}
	}
}

func (s *server) respondSummary(w http.ResponseWriter, req chatRequest) {
	text := "Summary: the user asked the agent to verify the workspace. The agent ran a shell command and reported the result. No files were modified."
	writeJSON(w, map[string]any{
		"id":      "chatcmpl-mock-summary",
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   req.Model,
		"choices": []any{map[string]any{
			"index": 0,
			"message": map[string]any{
				"role":    "assistant",
				"content": text,
			},
			"finish_reason": "stop",
		}},
		"usage": usageFor(req, text),
	})
}

func usageFor(req chatRequest, completion string) map[string]any {
	prompt := 0
	for _, m := range req.Messages {
		prompt += 4 + len(m.text())/4
	}
	completionTokens := len(completion)/4 + 8
	return map[string]any{
		"prompt_tokens":           prompt,
		"completion_tokens":       completionTokens,
		"total_tokens":            prompt + completionTokens,
		"prompt_cache_hit_tokens": 0,
	}
}

func writeSSE(w http.ResponseWriter, flusher http.Flusher, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "data: %s\n\n", data)
	flusher.Flush()
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// splitWords approximates token streaming while keeping spaces intact.
func splitWords(s string) []string {
	if s == "" {
		return nil
	}
	words := strings.SplitAfter(s, " ")
	out := make([]string, 0, len(words))
	for _, w := range words {
		if w != "" {
			out = append(out, w)
		}
	}
	return out
}
