// Package mockllm implements a deterministic OpenAI-compatible provider for
// tests and the evaluation harness. It supports turn-by-turn scripts so tasks
// can describe exact tool trajectories without changing this package.
package mockllm

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Step is one scripted assistant turn. Steps are consumed in order across all
// chat completion requests for a server, including nested subagent requests.
type Step struct {
	Reasoning string `json:"reasoning,omitempty"`
	Text      string `json:"text,omitempty"`
	Calls     []Call `json:"calls,omitempty"`
}

// Call is a scripted tool call. Args is embedded verbatim in the provider
// response, so it must be a JSON object.
type Call struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}

// Script is the file format accepted by ParseScript. A bare array of steps is
// also accepted.
type Script struct {
	Name  string `json:"name,omitempty"`
	Steps []Step `json:"steps"`
}

// Options configures a Server.
type Options struct {
	Scenario string
	Steps    []Step
	Delay    time.Duration
	Logger   *log.Logger
}

// Server is an http.Handler serving the OpenAI chat completions API.
type Server struct {
	scenario string
	delay    time.Duration
	logger   *log.Logger

	mu       sync.Mutex
	steps    []Step
	next     int
	requests atomic.Int32
	mux      *http.ServeMux
}

// New returns a mock provider with the given options.
func New(opts Options) *Server {
	logger := opts.Logger
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	steps := make([]Step, len(opts.Steps))
	copy(steps, opts.Steps)
	s := &Server{
		scenario: opts.Scenario,
		delay:    opts.Delay,
		logger:   logger,
		steps:    steps,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /chat/completions", s.handleChat)
	mux.HandleFunc("POST /v1/chat/completions", s.handleChat)
	mux.HandleFunc("GET /models", s.handleModels)
	mux.HandleFunc("GET /v1/models", s.handleModels)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	s.mux = mux
	return s
}

// Handler exposes the HTTP routes for httptest or embedding.
func (s *Server) Handler() http.Handler { return s.mux }

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// Requests reports how many chat completion requests the server handled.
func (s *Server) Requests() int { return int(s.requests.Load()) }

// ListenAndServe starts a blocking HTTP server.
func ListenAndServe(addr string, opts Options) error {
	s := New(opts)
	logger := opts.Logger
	if logger == nil {
		logger = log.Default()
	}
	logger.Printf("mockllm listening on http://%s (scenario=%s, steps=%d)", addr, opts.Scenario, len(opts.Steps))
	return http.ListenAndServe(addr, s)
}

// ParseScript reads a script from JSON. Both a {"steps": [...]} object and a
// bare [...] array are accepted. Invalid or ambiguous scripts are rejected so a
// task cannot silently pass with no scripted behavior.
func ParseScript(data []byte) (Script, error) {
	var script Script
	trimmed := strings.TrimSpace(string(data))
	if strings.HasPrefix(trimmed, "[") {
		if err := decodeStrict(data, &script.Steps); err != nil {
			return Script{}, fmt.Errorf("parse script steps: %w", err)
		}
	} else {
		if err := decodeStrict(data, &script); err != nil {
			return Script{}, fmt.Errorf("parse script: %w", err)
		}
	}
	if err := ValidateSteps(script.Steps); err != nil {
		return Script{}, err
	}
	return script, nil
}

// ValidateSteps checks that a script cannot produce an empty assistant turn
// and that every tool call has a name and valid JSON object arguments.
func ValidateSteps(steps []Step) error {
	if len(steps) == 0 {
		return fmt.Errorf("script has no steps")
	}
	for i, step := range steps {
		if strings.TrimSpace(step.Text) == "" && len(step.Calls) == 0 {
			return fmt.Errorf("step %d has neither text nor tool calls", i+1)
		}
		for j, call := range step.Calls {
			if strings.TrimSpace(call.Name) == "" {
				return fmt.Errorf("step %d call %d is missing a name", i+1, j+1)
			}
			if len(call.Args) > 0 {
				var obj map[string]any
				if err := json.Unmarshal(call.Args, &obj); err != nil {
					return fmt.Errorf("step %d call %d args must be a JSON object: %w", i+1, j+1, err)
				}
			}
		}
	}
	return nil
}

func decodeStrict(data []byte, v any) error {
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("unexpected trailing JSON")
	}
	return nil
}

// SeedXSSFixture writes a small HTML file used by the xss scenario.
func SeedXSSFixture(dir string) error {
	const body = "<img src=x onerror=\"window.__xss=4\">\n<b>raw html</b>\n"
	tmp := filepath.Join(dir, ".tmp")
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(tmp, "xss.html"), []byte(body), 0o644)
}

type requestMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  []any           `json:"tool_calls"`
	ToolCallID string          `json:"tool_call_id"`
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

type chatRequest struct {
	Model    string           `json:"model"`
	Messages []requestMessage `json:"messages"`
	Stream   bool             `json:"stream"`
	Tools    []any            `json:"tools"`
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"object": "list",
		"data": []map[string]string{
			{"id": "deepseek-chat", "object": "model"},
			{"id": "deepseek-reasoner", "object": "model"},
			{"id": "mock-model", "object": "model"},
		},
	})
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":{"message":"bad json"}}`, http.StatusBadRequest)
		return
	}
	s.requests.Add(1)
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
	s.logger.Printf("request model=%s stream=%v tools=%d roles=%s", req.Model, req.Stream, len(req.Tools), strings.Join(roles, ","))

	// The first two attempts are rate limited so the client retry policy and
	// its Retry-After handling can be exercised end to end.
	if s.scenario == "flaky" {
		if n := s.requests.Load(); n <= 2 {
			s.logger.Printf("  flaky: 429 for attempt %d", n)
			w.Header().Set("Retry-After", "0")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":{"message":"mock rate limit","type":"rate_limit"}}`)
			return
		}
	}

	// Compaction summaries are internal housekeeping and must not consume a
	// scripted task step.
	if strings.Contains(system, "You summarize conversations") {
		s.respondSummary(w, req)
		return
	}

	p := s.plan(req, system, last)
	s.logger.Printf("  plan=%s", p.kind)

	if !req.Stream {
		msg := map[string]any{"role": "assistant", "content": p.text}
		finish := "stop"
		if len(p.calls) > 0 {
			calls := make([]any, 0, len(p.calls))
			for _, c := range p.calls {
				calls = append(calls, map[string]any{
					"id":   c.id,
					"type": "function",
					"function": map[string]any{
						"name":      c.name,
						"arguments": c.args,
					},
				})
			}
			msg["tool_calls"] = calls
			finish = "tool_calls"
		}
		writeJSON(w, map[string]any{
			"id":      "chatcmpl-mock",
			"object":  "chat.completion",
			"created": time.Now().Unix(),
			"model":   req.Model,
			"choices": []any{map[string]any{
				"index":         0,
				"message":       msg,
				"finish_reason": finish,
			}},
			"usage": usageFor(req, p.text),
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
		if s.delay > 0 {
			time.Sleep(s.delay)
		}
	}

	emit(map[string]any{"role": "assistant", "content": ""}, nil)
	for _, part := range splitWords(p.reasoning) {
		emit(map[string]any{"reasoning_content": part}, nil)
	}
	for _, part := range splitWords(p.text) {
		emit(map[string]any{"content": part}, nil)
	}
	for i, call := range p.calls {
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
	if len(p.calls) > 0 {
		finish = "tool_calls"
	}
	emit(map[string]any{}, finish)
	usage := usageFor(req, p.text)
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

func (s *Server) plan(req chatRequest, system string, last requestMessage) plan {
	if len(s.steps) > 0 {
		return s.scriptPlan()
	}
	if strings.Contains(system, "You are a subagent inside CodeDeep") {
		return plan{
			kind: "subagent-report",
			text: "Subagent report: the module is codedeep and it builds cleanly.",
		}
	}
	if s.scenario == "memory" && last.Role == "tool" && strings.HasPrefix(strings.TrimSpace(last.text()), "Saved memory") {
		return plan{
			kind:      "memory-recall",
			reasoning: "The note was saved, so I will read it back to confirm it is durable.",
			text:      "Saved the note; checking it through recall.",
			calls: []toolCall{{
				id:   "call_recall_1",
				name: "recall",
				args: `{"query":"mock build command"}`,
			}},
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
	case "todo":
		return plan{
			kind:      "todo",
			reasoning: "This is a multi-step task, so I will record a short plan first.",
			text:      "Writing the plan.",
			calls: []toolCall{{
				id:   "call_todo_1",
				name: "todo_write",
				args: `{"todos":[{"text":"inspect the workspace","status":"completed"},` +
					`{"text":"write the mock build command","status":"in_progress"},` +
					`{"text":"report the result"}]}`,
			}},
		}
	case "memory":
		return plan{
			kind:      "memory",
			reasoning: "The build command is durable project knowledge, so I will remember it.",
			text:      "Saving a durable note.",
			calls: []toolCall{{
				id:   "call_remember_1",
				name: "remember",
				args: `{"name":"mock-build-command","description":"how the mock project builds",` +
					`"body":"Run go build ./... to build the mock project."}`,
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

func (s *Server) scriptPlan() plan {
	s.mu.Lock()
	defer s.mu.Unlock()
	stepNo := s.next + 1
	if s.next >= len(s.steps) {
		return plan{
			kind: "script-final",
			text: "Eval script complete.",
		}
	}
	step := s.steps[s.next]
	s.next++
	p := plan{kind: "script", reasoning: step.Reasoning, text: step.Text}
	for i, call := range step.Calls {
		args := strings.TrimSpace(string(call.Args))
		if args == "" {
			args = "{}"
		}
		p.calls = append(p.calls, toolCall{
			id:   fmt.Sprintf("call_script_%d_%d", stepNo, i+1),
			name: call.Name,
			args: args,
		})
	}
	return p
}

func (s *Server) respondSummary(w http.ResponseWriter, req chatRequest) {
	text := "Summary: the user asked the agent to verify the workspace. The agent ran a shell command and reported the result. No files were modified."
	writeJSON(w, map[string]any{
		"id":      "chatcmpl-mock-summary",
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   req.Model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": text},
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
	// Report some cache reads/writes so analytics and pricing paths are
	// exercised by the mock instead of always showing a 0% hit rate.
	return map[string]any{
		"prompt_tokens":             prompt,
		"completion_tokens":         completionTokens,
		"total_tokens":              prompt + completionTokens,
		"prompt_cache_hit_tokens":   prompt / 2,
		"prompt_cache_write_tokens": prompt / 10,
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
