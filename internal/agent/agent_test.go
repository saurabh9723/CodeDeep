package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"codedeep/internal/approval"
	"codedeep/internal/config"
	"codedeep/internal/llm"
	"codedeep/internal/session"
	"codedeep/internal/tools"
)

type requestLog struct {
	mu     sync.Mutex
	bodies []map[string]any
}

func (l *requestLog) add(body map[string]any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.bodies = append(l.bodies, body)
}

func (l *requestLog) all() []map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]map[string]any(nil), l.bodies...)
}

// newFakeLLM starts an OpenAI-compatible server. respond receives the 1-based
// request number so tests can script multi-step tool loops.
func newFakeLLM(t *testing.T, respond func(n int, w http.ResponseWriter)) (*llm.Client, *requestLog) {
	t.Helper()
	log := &requestLog{}
	var count atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		log.add(body)
		respond(int(count.Add(1)), w)
	}))
	t.Cleanup(srv.Close)
	return llm.NewClient(srv.URL, "test-key"), log
}

func usageMap(prompt, completion int) map[string]any {
	return map[string]any{
		"prompt_tokens":     prompt,
		"completion_tokens": completion,
		"total_tokens":      prompt + completion,
	}
}

func sseEvent(w http.ResponseWriter, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	fmt.Fprintf(w, "data: %s\n\n", data)
}

func sseFinish(w http.ResponseWriter, reason string, prompt, completion int) {
	sseEvent(w, map[string]any{
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": reason,
		}},
		"usage": usageMap(prompt, completion),
	})
	fmt.Fprint(w, "data: [DONE]\n\n")
}

func streamText(w http.ResponseWriter, text string, prompt, completion int) {
	w.Header().Set("Content-Type", "text/event-stream")
	sseEvent(w, map[string]any{
		"choices": []any{map[string]any{
			"index": 0,
			"delta": map[string]any{"role": "assistant", "content": text},
		}},
	})
	sseFinish(w, "stop", prompt, completion)
}

func streamToolCall(w http.ResponseWriter, id, name, args string, prompt, completion int) {
	w.Header().Set("Content-Type", "text/event-stream")
	half := len(args) / 2
	sseEvent(w, map[string]any{
		"choices": []any{map[string]any{
			"index": 0,
			"delta": map[string]any{
				"role": "assistant",
				"tool_calls": []any{map[string]any{
					"index": 0,
					"id":    id,
					"type":  "function",
					"function": map[string]any{
						"name":      name,
						"arguments": args[:half],
					},
				}},
			},
		}},
	})
	sseEvent(w, map[string]any{
		"choices": []any{map[string]any{
			"index": 0,
			"delta": map[string]any{
				"tool_calls": []any{map[string]any{
					"index":    0,
					"function": map[string]any{"arguments": args[half:]},
				}},
			},
		}},
	})
	sseFinish(w, "tool_calls", prompt, completion)
}

func jsonCompletion(w http.ResponseWriter, content string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": content},
			"finish_reason": "stop",
		}},
		"usage": usageMap(5, 2),
	})
}

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{
		Model:         "test-model",
		MaxTokens:     256,
		Temperature:   0.1,
		ContextWindow: 128000,
		CompactAt:     0.8,
		Approval:      "auto",
		WorkDir:       t.TempDir(),
		MaxSteps:      5,
		SessionsDir:   t.TempDir(),
	}
}

func echoRegistry(ran *atomic.Int32) *tools.Registry {
	reg := tools.NewRegistry()
	reg.Register(&tools.Spec{
		Name:        "echo",
		Description: "Echo text back.",
		Params:      tools.Object(map[string]any{"text": tools.StrProp("Text to echo.")}),
		Run: func(ctx context.Context, raw json.RawMessage) (tools.Result, error) {
			if ran != nil {
				ran.Add(1)
			}
			var args struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return tools.Result{}, err
			}
			return tools.Result{Output: "echo: " + args.Text}, nil
		},
	})
	return reg
}

func newAgentWith(t *testing.T, cfg *config.Config, client *llm.Client, reg *tools.Registry, approver approval.Approver, events *[]Event) *Agent {
	t.Helper()
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return New(Options{
		Config:   cfg,
		Client:   client,
		Registry: reg,
		Sessions: store,
		Approver: approver,
		Emitter: func(ev Event) {
			if events != nil {
				*events = append(*events, ev)
			}
		},
	})
}

func hasEvent(events []Event, typ string) bool {
	for _, ev := range events {
		if ev.Type == typ {
			return true
		}
	}
	return false
}

func findEvent(events []Event, typ string) *Event {
	for i := range events {
		if events[i].Type == typ {
			return &events[i]
		}
	}
	return nil
}

func requestMessages(body map[string]any) []map[string]any {
	raw, _ := body["messages"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func TestAgentRunsToolAndReturnsFinalText(t *testing.T) {
	client, logs := newFakeLLM(t, func(n int, w http.ResponseWriter) {
		if n == 1 {
			streamToolCall(w, "call_1", "echo", `{"text":"hi"}`, 20, 8)
			return
		}
		streamText(w, "All done.", 30, 6)
	})

	cfg := testConfig(t)
	var ran atomic.Int32
	reg := echoRegistry(&ran)
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sess := store.New(cfg.Model, t.TempDir())

	var events []Event
	a := New(Options{
		Config:   cfg,
		Client:   client,
		Registry: reg,
		Sessions: store,
		Emitter:  func(ev Event) { events = append(events, ev) },
	})

	if err := a.Run(context.Background(), sess, "say hi"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := ran.Load(); got != 1 {
		t.Errorf("tool ran %d times, want 1", got)
	}
	if len(sess.Messages) != 4 {
		t.Fatalf("message count = %d, want 4: %+v", len(sess.Messages), sess.Messages)
	}
	if sess.Messages[0].Role != "user" || sess.Messages[0].Content != "say hi" {
		t.Errorf("user message = %+v", sess.Messages[0])
	}
	if sess.Messages[1].Role != "assistant" || len(sess.Messages[1].ToolCalls) != 1 {
		t.Fatalf("assistant tool call = %+v", sess.Messages[1])
	}
	tc := sess.Messages[1].ToolCalls[0]
	if tc.ID != "call_1" || tc.Function.Name != "echo" || tc.Function.Arguments != `{"text":"hi"}` {
		t.Errorf("tool call = %+v", tc)
	}
	if sess.Messages[2].Role != "tool" || sess.Messages[2].ToolCallID != "call_1" || sess.Messages[2].Content != "echo: hi" {
		t.Errorf("tool result = %+v", sess.Messages[2])
	}
	if sess.Messages[3].Content != "All done." {
		t.Errorf("final message = %+v", sess.Messages[3])
	}
	if sess.Usage.TotalTokens != 64 {
		t.Errorf("usage total = %d, want 64", sess.Usage.TotalTokens)
	}

	saved, err := store.Load(sess.ID)
	if err != nil {
		t.Fatalf("Load saved session: %v", err)
	}
	if len(saved.Messages) != 4 {
		t.Errorf("persisted messages = %d", len(saved.Messages))
	}
	if saved.Turns != 1 {
		t.Errorf("turns = %d", saved.Turns)
	}

	for _, typ := range []string{EventUser, EventToolStart, EventToolEnd, EventDone} {
		if !hasEvent(events, typ) {
			t.Errorf("missing event %q", typ)
		}
	}
	end := findEvent(events, EventToolEnd)
	if end == nil || end.Output != "echo: hi" || end.IsError {
		t.Errorf("tool_end event = %+v", end)
	}

	bodies := logs.all()
	if len(bodies) != 2 {
		t.Fatalf("LLM calls = %d, want 2", len(bodies))
	}
	second := requestMessages(bodies[1])
	var toolMsg map[string]any
	for _, m := range second {
		if m["role"] == "tool" {
			toolMsg = m
		}
	}
	if toolMsg == nil || toolMsg["tool_call_id"] != "call_1" || toolMsg["content"] != "echo: hi" {
		t.Errorf("second request did not carry tool result: %+v", second)
	}
	toolDefs, _ := bodies[0]["tools"].([]any)
	if len(toolDefs) == 0 {
		t.Error("first request did not advertise tools")
	}
}

func TestAgentDeniedToolIsReportedToModel(t *testing.T) {
	client, _ := newFakeLLM(t, func(n int, w http.ResponseWriter) {
		if n == 1 {
			streamToolCall(w, "call_d", "echo", `{"text":"no"}`, 10, 4)
			return
		}
		streamText(w, "I could not run it.", 12, 5)
	})

	cfg := testConfig(t)
	cfg.Approval = "ask"
	var ran atomic.Int32
	var events []Event
	a := newAgentWith(t, cfg, client, echoRegistry(&ran), approval.DenyAll, &events)
	sess := a.sessions.New(cfg.Model, t.TempDir())

	if err := a.Run(context.Background(), sess, "try it"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ran.Load() != 0 {
		t.Error("denied tool should not execute")
	}
	if len(sess.Messages) < 3 || !strings.Contains(sess.Messages[2].Content, "permission denied") {
		t.Errorf("tool result = %+v", sess.Messages)
	}
	if !hasEvent(events, EventApproval) {
		t.Error("missing approval request event")
	}
	res := findEvent(events, EventApprovalResult)
	if res == nil || res.Message != "deny" {
		t.Errorf("approval result = %+v", res)
	}
}

func TestAgentReadOnlyToolSkipsApproval(t *testing.T) {
	client, _ := newFakeLLM(t, func(n int, w http.ResponseWriter) {
		if n == 1 {
			streamToolCall(w, "call_ro", "peek", `{}`, 10, 4)
			return
		}
		streamText(w, "Done.", 12, 5)
	})

	cfg := testConfig(t)
	cfg.Approval = "ask"
	var ran atomic.Int32
	reg := tools.NewRegistry()
	reg.Register(&tools.Spec{
		Name:        "peek",
		Description: "Read-only test tool.",
		ReadOnly:    true,
		Params:      tools.Object(map[string]any{}),
		Run: func(ctx context.Context, raw json.RawMessage) (tools.Result, error) {
			ran.Add(1)
			return tools.Result{Output: "peeked"}, nil
		},
	})

	var events []Event
	a := newAgentWith(t, cfg, client, reg, approval.DenyAll, &events)
	sess := a.sessions.New(cfg.Model, t.TempDir())

	if err := a.Run(context.Background(), sess, "peek"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ran.Load() != 1 {
		t.Error("read-only tool should run without approval")
	}
	if hasEvent(events, EventApproval) {
		t.Error("read-only tool should not emit an approval request")
	}
}

func TestAgentReadOnlyModeBlocksWrites(t *testing.T) {
	client, _ := newFakeLLM(t, func(n int, w http.ResponseWriter) {
		if n == 1 {
			streamToolCall(w, "call_w", "echo", `{"text":"x"}`, 10, 4)
			return
		}
		streamText(w, "Blocked.", 12, 5)
	})

	cfg := testConfig(t)
	cfg.Approval = "readonly"
	var ran atomic.Int32
	a := newAgentWith(t, cfg, client, echoRegistry(&ran), nil, nil)
	sess := a.sessions.New(cfg.Model, t.TempDir())

	if err := a.Run(context.Background(), sess, "write"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ran.Load() != 0 {
		t.Error("readonly mode should block non-read-only tools")
	}
	if !strings.Contains(sess.Messages[2].Content, "read-only approval mode") {
		t.Errorf("tool result = %q", sess.Messages[2].Content)
	}
}

func TestAgentUnknownTool(t *testing.T) {
	client, _ := newFakeLLM(t, func(n int, w http.ResponseWriter) {
		if n == 1 {
			streamToolCall(w, "call_x", "missing", `{}`, 10, 4)
			return
		}
		streamText(w, "Moving on.", 12, 5)
	})

	cfg := testConfig(t)
	a := newAgentWith(t, cfg, client, tools.NewRegistry(), nil, nil)
	sess := a.sessions.New(cfg.Model, t.TempDir())

	if err := a.Run(context.Background(), sess, "call missing"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(sess.Messages[2].Content, "unknown tool") {
		t.Errorf("tool result = %q", sess.Messages[2].Content)
	}
}

func TestAgentEmptyResponseIsAnError(t *testing.T) {
	client, _ := newFakeLLM(t, func(n int, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		sseFinish(w, "stop", 3, 0)
	})

	cfg := testConfig(t)
	var events []Event
	a := newAgentWith(t, cfg, client, tools.NewRegistry(), nil, &events)
	sess := a.sessions.New(cfg.Model, t.TempDir())

	err := a.Run(context.Background(), sess, "hello")
	if err == nil || !strings.Contains(err.Error(), "empty response") {
		t.Fatalf("expected empty-response error, got %v", err)
	}
	if !hasEvent(events, EventError) {
		t.Error("missing error event")
	}
}

func TestAgentCancellation(t *testing.T) {
	client, _ := newFakeLLM(t, func(n int, w http.ResponseWriter) {
		streamText(w, "too late", 1, 1)
	})
	cfg := testConfig(t)
	var events []Event
	a := newAgentWith(t, cfg, client, tools.NewRegistry(), nil, &events)
	sess := a.sessions.New(cfg.Model, t.TempDir())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := a.Run(ctx, sess, "stop")
	if err == nil {
		t.Fatal("expected cancellation error")
	}
	done := findEvent(events, EventDone)
	if done == nil || done.Message != "stopped" {
		t.Errorf("done event = %+v", done)
	}
}

func TestAgentMaxSteps(t *testing.T) {
	client, logs := newFakeLLM(t, func(n int, w http.ResponseWriter) {
		streamToolCall(w, fmt.Sprintf("call_%d", n), "echo", `{"text":"loop"}`, 5, 2)
	})

	cfg := testConfig(t)
	cfg.MaxSteps = 2
	var ran atomic.Int32
	a := newAgentWith(t, cfg, client, echoRegistry(&ran), nil, nil)
	sess := a.sessions.New(cfg.Model, t.TempDir())

	err := a.Run(context.Background(), sess, "loop forever")
	if err == nil || !strings.Contains(err.Error(), "stopped after 2 steps") {
		t.Fatalf("expected max-steps error, got %v", err)
	}
	if len(logs.all()) != 2 {
		t.Errorf("LLM calls = %d, want 2", len(logs.all()))
	}
	if ran.Load() != 2 {
		t.Errorf("tool runs = %d, want 2", ran.Load())
	}
}
