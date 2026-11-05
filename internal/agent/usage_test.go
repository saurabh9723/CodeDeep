package agent

import (
	"context"
	"math"
	"net/http"
	"sync/atomic"
	"testing"

	"codedeep/internal/config"
)

func TestPerToolUsageAttribution(t *testing.T) {
	client, _ := newFakeLLM(t, func(n int, w http.ResponseWriter) {
		if n == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			sseEvent(w, map[string]any{
				"choices": []any{map[string]any{
					"index": 0,
					"delta": map[string]any{
						"role": "assistant",
						"tool_calls": []any{
							map[string]any{
								"index": 0, "id": "c1", "type": "function",
								"function": map[string]any{"name": "echo", "arguments": `{"text":"a"}`},
							},
							map[string]any{
								"index": 1, "id": "c2", "type": "function",
								"function": map[string]any{"name": "echo", "arguments": `{"text":"b"}`},
							},
						},
					},
				}},
			})
			sseFinish(w, "tool_calls", 21, 7)
			return
		}
		streamText(w, "done", 10, 5)
	})

	cfg := testConfig(t)
	cfg.Pricing = map[string]config.Price{
		"test-model": {InputPerMTok: 1, OutputPerMTok: 2},
	}
	var ran atomic.Int32
	var events []Event
	a := newAgentWith(t, cfg, client, echoRegistry(&ran), nil, &events)
	sess := a.sessions.New(cfg.Model, cfg.WorkDir)

	if err := a.Run(context.Background(), sess, "use echo twice"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ran.Load() != 2 {
		t.Fatalf("echo ran %d times, want 2", ran.Load())
	}

	var starts int
	var prompt, completion int
	for _, ev := range events {
		if ev.Type != EventToolStart {
			continue
		}
		starts++
		if ev.Usage == nil {
			t.Fatalf("tool_start %s has no usage", ev.Name)
		}
		prompt += ev.Usage.PromptTokens
		completion += ev.Usage.CompletionTokens
		if ev.Usage.TotalTokens != 14 {
			t.Errorf("per-call total = %d, want 14", ev.Usage.TotalTokens)
		}
		if ev.CostUSD <= 0 {
			t.Errorf("per-call cost = %v, want > 0", ev.CostUSD)
		}
	}
	if starts != 2 || prompt != 21 || completion != 7 {
		t.Fatalf("tool_start usage = %d calls, %d prompt, %d completion", starts, prompt, completion)
	}

	done := findEvent(events, EventDone)
	if done == nil {
		t.Fatal("missing done event")
	}
	tu, ok := done.ToolUsage["echo"]
	if !ok {
		t.Fatalf("done tool usage = %+v", done.ToolUsage)
	}
	if tu.Calls != 2 || tu.Usage.PromptTokens != 21 || tu.Usage.CompletionTokens != 7 || tu.Usage.TotalTokens != 28 {
		t.Fatalf("done tool usage = %+v", tu)
	}
	wantCost := (21*1.0 + 7*2.0) / 1e6
	if math.Abs(tu.CostUSD-wantCost) > 1e-12 {
		t.Errorf("done tool cost = %.12f, want %.12f", tu.CostUSD, wantCost)
	}

	if sess.Usage.TotalTokens != 43 {
		t.Errorf("turn total tokens = %d, want 43", sess.Usage.TotalTokens)
	}
	stored, ok := sess.ToolUsage["echo"]
	if !ok || stored.Calls != 2 || stored.Usage.TotalTokens != 28 {
		t.Fatalf("session tool usage = %+v", sess.ToolUsage)
	}
	saved, err := a.sessions.Load(sess.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if saved.ToolUsage["echo"].Usage.TotalTokens != 28 {
		t.Errorf("persisted tool usage = %+v", saved.ToolUsage)
	}
}
