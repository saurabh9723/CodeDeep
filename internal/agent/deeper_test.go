package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"codedeep/internal/config"
	"codedeep/internal/memory"
	"codedeep/internal/session"
	"codedeep/internal/tools"
)

func mutatingRegistry(t *testing.T, root string) *tools.Registry {
	t.Helper()
	reg := tools.NewRegistry()
	reg.Register(&tools.Spec{
		Name:        "mutate",
		Description: "Pretend to write a file.",
		Params:      tools.Object(map[string]any{}),
		Run: func(ctx context.Context, raw json.RawMessage) (tools.Result, error) {
			return tools.Result{
				Output: "wrote out.txt",
				Change: &tools.Change{Path: filepath.Join(root, "out.txt"), New: "hello"},
			}, nil
		},
	})
	return reg
}

func newTestSession(t *testing.T, cfg *config.Config) (*session.Store, *session.Session) {
	t.Helper()
	store, err := session.NewStore(cfg.SessionsDir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return store, store.New(cfg.Model, cfg.WorkDir)
}

func TestTodoWriteUpdatesSessionPlanAndPrompt(t *testing.T) {
	client, logs := newFakeLLM(t, func(n int, w http.ResponseWriter) {
		if n == 1 {
			streamToolCall(w, "call_todo", "todo_write",
				`{"todos":[{"text":"read the code","status":"in_progress"},{"text":"write the fix"},{"text":"run tests","status":"done"}]}`, 20, 8)
			return
		}
		streamText(w, "Plan is set.", 30, 6)
	})
	cfg := testConfig(t)
	store, sess := newTestSession(t, cfg)
	var events []Event
	a := New(Options{
		Config:   cfg,
		Client:   client,
		Registry: tools.NewRegistry(),
		Sessions: store,
		Emitter:  func(ev Event) { events = append(events, ev) },
	})

	if err := a.Run(context.Background(), sess, "do the task"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(sess.Todos) != 3 {
		t.Fatalf("todos = %+v", sess.Todos)
	}
	if sess.Todos[0].Text != "read the code" || sess.Todos[0].Status != "in_progress" {
		t.Errorf("todo[0] = %+v", sess.Todos[0])
	}
	if sess.Todos[2].Status != "completed" {
		t.Errorf("todo[2] status = %q, want completed", sess.Todos[2].Status)
	}

	ev := findEvent(events, EventTodos)
	if ev == nil || len(ev.Todos) != 3 {
		t.Fatalf("todos event = %+v", ev)
	}

	bodies := logs.all()
	if len(bodies) < 2 {
		t.Fatalf("requests = %d, want at least 2", len(bodies))
	}
	second := requestMessages(bodies[1])
	if len(second) == 0 || second[0]["role"] != "system" {
		t.Fatalf("second request missing system prompt: %+v", second)
	}
	system, _ := second[0]["content"].(string)
	if !strings.Contains(system, "# Current plan") || !strings.Contains(system, "read the code") {
		t.Errorf("system prompt does not include the plan:\n%s", system)
	}
	if !strings.Contains(system, "[~] read the code") {
		t.Errorf("system prompt does not mark the active item:\n%s", system)
	}

	reloaded, err := store.Load(sess.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(reloaded.Todos) != 3 {
		t.Errorf("persisted todos = %+v", reloaded.Todos)
	}
}

func TestTodoWriteValidation(t *testing.T) {
	for in, want := range map[string]string{
		"":            "pending",
		"todo":        "pending",
		"in-progress": "in_progress",
		"doing":       "in_progress",
		"done":        "completed",
		"COMPLETE":    "completed",
	} {
		got, err := normalizeTodoStatus(in)
		if err != nil {
			t.Errorf("normalizeTodoStatus(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("normalizeTodoStatus(%q) = %q, want %q", in, got, want)
		}
	}
	if _, err := normalizeTodoStatus("blocked"); err == nil {
		t.Error("normalizeTodoStatus accepted an unknown status")
	}
}

func TestTodoWriteIsRootOnly(t *testing.T) {
	cfg := testConfig(t)
	root := New(Options{Config: cfg, Registry: tools.NewRegistry()})
	if _, ok := root.reg.Get("todo_write"); !ok {
		t.Fatal("root agent is missing todo_write")
	}
	child := New(Options{Config: cfg, Registry: root.reg, Depth: 1})
	if _, ok := child.reg.Get("todo_write"); ok {
		t.Error("subagent should not see todo_write")
	}
}

func TestVerificationGatePassesAfterChanges(t *testing.T) {
	client, logs := newFakeLLM(t, func(n int, w http.ResponseWriter) {
		if n == 1 {
			streamToolCall(w, "call_mutate", "mutate", `{}`, 20, 8)
			return
		}
		streamText(w, "All done.", 30, 6)
	})
	cfg := testConfig(t)
	cfg.Verify = config.Verify{Command: "echo verified-ok"}
	store, sess := newTestSession(t, cfg)
	var events []Event
	a := New(Options{
		Config:   cfg,
		Client:   client,
		Registry: mutatingRegistry(t, cfg.WorkDir),
		Sessions: store,
		Emitter:  func(ev Event) { events = append(events, ev) },
	})

	if err := a.Run(context.Background(), sess, "change a file"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !hasEvent(events, EventVerifyStart) {
		t.Error("verify_start event missing")
	}
	ev := findEvent(events, EventVerify)
	if ev == nil || ev.IsError {
		t.Fatalf("verify event = %+v", ev)
	}
	if ev.Message != "echo verified-ok" || !strings.Contains(ev.Output, "verified-ok") {
		t.Errorf("verify event = %+v", ev)
	}
	if len(logs.all()) != 2 {
		t.Errorf("requests = %d, want 2 (no extra turn after a passing verify)", len(logs.all()))
	}
}

func TestVerificationGateFeedsFailuresBack(t *testing.T) {
	client, logs := newFakeLLM(t, func(n int, w http.ResponseWriter) {
		if n == 1 {
			streamToolCall(w, "call_mutate", "mutate", `{}`, 20, 8)
			return
		}
		streamText(w, "Done.", 30, 6)
	})
	cfg := testConfig(t)
	cfg.Verify = config.Verify{Command: "echo compile-error && exit 1"}
	store, sess := newTestSession(t, cfg)
	var events []Event
	a := New(Options{
		Config:   cfg,
		Client:   client,
		Registry: mutatingRegistry(t, cfg.WorkDir),
		Sessions: store,
		Emitter:  func(ev Event) { events = append(events, ev) },
	})

	err := a.Run(context.Background(), sess, "change a file")
	if err == nil || !strings.Contains(err.Error(), "automatic verification failed 2 times") {
		t.Fatalf("Run error = %v, want the attempt cap", err)
	}
	verifyEvents := 0
	for _, ev := range events {
		if ev.Type == EventVerify {
			verifyEvents++
			if !ev.IsError {
				t.Errorf("verify event should report failure: %+v", ev)
			}
		}
	}
	if verifyEvents != 2 {
		t.Errorf("verify ran %d times, want 2", verifyEvents)
	}
	requests := logs.all()
	if len(requests) != 3 {
		t.Fatalf("requests = %d, want 3 (initial + one retry after feedback)", len(requests))
	}
	msgs := requestMessages(requests[2])
	found := false
	for _, m := range msgs {
		content, _ := m["content"].(string)
		if strings.HasPrefix(content, "[automatic verification failed]") {
			found = true
			if !strings.Contains(content, "compile-error") {
				t.Errorf("feedback is missing the output: %q", content)
			}
		}
	}
	if !found {
		t.Errorf("retry request is missing verification feedback: %+v", msgs)
	}
	found = false
	for _, m := range sess.Messages {
		if m.Role == "user" && strings.HasPrefix(m.Content, "[automatic verification failed]") {
			found = true
		}
	}
	if !found {
		t.Error("verification feedback was not persisted in the session")
	}
}

func TestVerificationSkippedWithoutChanges(t *testing.T) {
	client, _ := newFakeLLM(t, func(n int, w http.ResponseWriter) {
		streamText(w, "Nothing to do.", 10, 4)
	})
	cfg := testConfig(t)
	cfg.Verify = config.Verify{Command: "exit 1"}
	store, sess := newTestSession(t, cfg)
	var events []Event
	a := New(Options{
		Config:   cfg,
		Client:   client,
		Registry: tools.NewRegistry(),
		Sessions: store,
		Emitter:  func(ev Event) { events = append(events, ev) },
	})
	if err := a.Run(context.Background(), sess, "just answer"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if hasEvent(events, EventVerifyStart) || hasEvent(events, EventVerify) {
		t.Error("verification ran although no files changed")
	}
}

func TestSystemPromptIncludesMemoryIndex(t *testing.T) {
	cfg := testConfig(t)
	mem := memory.New(cfg.WorkDir, t.TempDir())
	if _, err := mem.Save("project", "build-command", "How to build", "run go build ./..."); err != nil {
		t.Fatal(err)
	}
	a := New(Options{Config: cfg, Registry: tools.NewRegistry(), Memory: mem})
	prompt := a.systemPrompt(nil, nil)
	if !strings.Contains(prompt, "# Project memory") || !strings.Contains(prompt, "build-command [project]") {
		t.Errorf("prompt missing memory index:\n%s", prompt)
	}
}
