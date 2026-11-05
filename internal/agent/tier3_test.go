package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"codedeep/internal/config"
	"codedeep/internal/tools"
)

func TestPreHookBlocksToolCall(t *testing.T) {
	var ran atomic.Int32
	reg := echoRegistry(&ran)
	client, _ := newFakeLLM(t, func(n int, w http.ResponseWriter) {
		if n == 1 {
			streamToolCalls(w, []toolCallSpec{{id: "call_1", name: "echo", args: `{"text":"hi"}`}})
			return
		}
		streamText(w, "I was blocked.", 20, 5)
	})

	cfg := testConfig(t)
	cfg.Hooks.PreTool = []config.Hook{{Command: "exit 1", BlockOnFailure: true}}
	var events []Event
	a := newAgentWith(t, cfg, client, reg, nil, &events)
	sess := a.sessions.New(cfg.Model, cfg.WorkDir)
	if err := a.Run(context.Background(), sess, "run echo"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ran.Load() != 0 {
		t.Fatal("tool ran despite the blocking pre-hook")
	}
	var toolMsg string
	for _, m := range sess.Messages {
		if m.Role == "tool" {
			toolMsg = m.Content
		}
	}
	if !strings.Contains(toolMsg, "blocked by a pre-tool hook") {
		t.Fatalf("tool message = %q", toolMsg)
	}
	hook := findEvent(events, EventHook)
	if hook == nil || !hook.IsError || hook.Name != "pre_tool" || hook.Message != "echo" {
		t.Fatalf("hook event = %+v", hook)
	}
}

func TestPostHookAndDiagnosticsRunAfterChange(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(&tools.Spec{
		Name:        "touch",
		Description: "Pretend to edit a file.",
		Params:      tools.Object(map[string]any{}),
		Run: func(context.Context, json.RawMessage) (tools.Result, error) {
			return tools.Result{
				Output: "edited main.go",
				Change: &tools.Change{Path: "main.go", New: "package main\n"},
			}, nil
		},
	})
	client, _ := newFakeLLM(t, func(n int, w http.ResponseWriter) {
		if n == 1 {
			streamToolCalls(w, []toolCallSpec{{id: "call_1", name: "touch", args: `{}`}})
			return
		}
		streamText(w, "Edited.", 20, 5)
	})

	cfg := testConfig(t)
	cfg.Diagnostics = []config.DiagnosticRule{{Pattern: "*.go", Command: "echo diag-clean"}}
	cfg.Hooks.PostTool = []config.Hook{{Command: "echo post-clean"}}
	var events []Event
	a := newAgentWith(t, cfg, client, reg, nil, &events)
	sess := a.sessions.New(cfg.Model, cfg.WorkDir)
	if err := a.Run(context.Background(), sess, "touch main.go"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var toolMsg string
	for _, m := range sess.Messages {
		if m.Role == "tool" {
			toolMsg = m.Content
		}
	}
	for _, want := range []string{"edited main.go", "diag-clean", "post-clean"} {
		if !strings.Contains(toolMsg, want) {
			t.Errorf("tool message is missing %q: %q", want, toolMsg)
		}
	}

	diag := findEvent(events, EventDiagnostics)
	if diag == nil || diag.Name != "*.go" || diag.Path != "main.go" || diag.IsError {
		t.Fatalf("diagnostics event = %+v", diag)
	}
	hook := findEvent(events, EventHook)
	if hook == nil || hook.Name != "post_tool" || hook.IsError {
		t.Fatalf("hook event = %+v", hook)
	}
}
