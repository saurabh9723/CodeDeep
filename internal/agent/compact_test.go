package agent

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"codedeep/internal/llm"
	"codedeep/internal/session"
	"codedeep/internal/tools"
)

func history(n int) []llm.Message {
	msgs := make([]llm.Message, 0, n)
	for i := 0; i < n; i++ {
		role := "assistant"
		if i%4 == 0 {
			role = "user"
		}
		msgs = append(msgs, llm.Message{Role: role, Content: "message " + string(rune('a'+i))})
	}
	return msgs
}

func toolHeavyHistory() []llm.Message {
	toolCall := func(id string) llm.ToolCall {
		return llm.ToolCall{ID: id, Type: "function", Function: llm.FunctionCall{Name: "echo", Arguments: "{}"}}
	}
	return []llm.Message{
		{Role: "user", Content: "u0"},
		{Role: "assistant", Content: "a1", ToolCalls: []llm.ToolCall{toolCall("c1")}},
		{Role: "tool", ToolCallID: "c1", Content: "t2"},
		{Role: "assistant", Content: "a3"},
		{Role: "user", Content: "u4"},
		{Role: "assistant", Content: "a5", ToolCalls: []llm.ToolCall{toolCall("c5")}},
		{Role: "tool", ToolCallID: "c5", Content: "t6"},
		{Role: "assistant", Content: "a7", ToolCalls: []llm.ToolCall{toolCall("c7")}},
		{Role: "tool", ToolCallID: "c7", Content: "t8"},
		{Role: "assistant", Content: "a9"},
		{Role: "user", Content: "u10"},
		{Role: "assistant", Content: "a11"},
	}
}

func compactAgent(t *testing.T, client *llm.Client, events *[]Event) *Agent {
	t.Helper()
	cfg := testConfig(t)
	return New(Options{
		Config:   cfg,
		Client:   client,
		Registry: tools.NewRegistry(),
		Emitter: func(ev Event) {
			if events != nil {
				*events = append(*events, ev)
			}
		},
	})
}

func TestCompactKeepsBoundaryAndSummarizes(t *testing.T) {
	client, logs := newFakeLLM(t, func(n int, w http.ResponseWriter) {
		jsonCompletion(w, "summary text")
	})

	var events []Event
	a := compactAgent(t, client, &events)
	msgs := toolHeavyHistory()

	a.compact(context.Background(), &msgs, nil, 4, 5000)

	if len(msgs) != 6 {
		t.Fatalf("compacted length = %d, want 6: %+v", len(msgs), msgs)
	}
	if msgs[0].Role != "user" || !strings.Contains(msgs[0].Content, "Conversation summary") || !strings.Contains(msgs[0].Content, "summary text") {
		t.Errorf("summary message = %+v", msgs[0])
	}
	if msgs[1].Role == "tool" {
		t.Errorf("retained history starts on a tool message: %+v", msgs[1])
	}
	if msgs[1].Content != "a7" || msgs[len(msgs)-1].Content != "a11" {
		t.Errorf("retained tail is wrong: %+v", msgs[1:])
	}

	body := logs.all()[0]
	ms := requestMessages(body)
	if len(ms) != 2 || ms[0]["role"] != "system" {
		t.Fatalf("summarizer request = %+v", ms)
	}
	transcript, _ := ms[1]["content"].(string)
	for _, want := range []string{"[USER]", "u0", "[TOOL RESULT]", "t2"} {
		if !strings.Contains(transcript, want) {
			t.Errorf("transcript missing %q:\n%s", want, transcript)
		}
	}

	ev := findEvent(events, EventCompact)
	if ev == nil || !strings.Contains(ev.Message, "Compacted 7 earlier messages") {
		t.Errorf("compact event = %+v", ev)
	}
}

func TestCompactRefusesToSplitToolPairs(t *testing.T) {
	client, logs := newFakeLLM(t, func(n int, w http.ResponseWriter) {
		jsonCompletion(w, "should not be called")
	})
	a := compactAgent(t, client, nil)

	msgs := []llm.Message{
		{Role: "user", Content: "u0"},
		{Role: "assistant", Content: "a1"},
		{Role: "tool", ToolCallID: "c1", Content: "t2"},
		{Role: "tool", ToolCallID: "c2", Content: "t3"},
		{Role: "tool", ToolCallID: "c3", Content: "t4"},
		{Role: "tool", ToolCallID: "c4", Content: "t5"},
		{Role: "tool", ToolCallID: "c5", Content: "t6"},
	}
	before := len(msgs)
	a.compact(context.Background(), &msgs, nil, 4, 1000)

	if len(msgs) != before {
		t.Errorf("conversation changed despite unsafe cut: %+v", msgs)
	}
	if n := len(logs.all()); n != 0 {
		t.Errorf("summarizer called %d times, want 0", n)
	}
}

func TestCompactFallbackWhenSummaryFails(t *testing.T) {
	client, _ := newFakeLLM(t, func(n int, w http.ResponseWriter) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	var events []Event
	a := compactAgent(t, client, &events)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	msgs := history(12)
	a.compact(ctx, &msgs, nil, 4, 9000)

	if len(msgs) != 5 {
		t.Fatalf("compacted length = %d, want 5", len(msgs))
	}
	if !strings.Contains(msgs[0].Content, "Earlier conversation was omitted") {
		t.Errorf("fallback summary = %q", msgs[0].Content)
	}
	ev := findEvent(events, EventCompact)
	if ev == nil || !strings.Contains(ev.Message, "summary failed") {
		t.Errorf("compact event = %+v", ev)
	}
}

func TestCompactNow(t *testing.T) {
	client, _ := newFakeLLM(t, func(n int, w http.ResponseWriter) {
		jsonCompletion(w, "condensed history")
	})
	cfg := testConfig(t)
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := New(Options{
		Config:   cfg,
		Client:   client,
		Registry: tools.NewRegistry(),
		Sessions: store,
	})
	sess := store.New(cfg.Model, t.TempDir())
	sess.Messages = history(14)

	removed, err := a.CompactNow(context.Background(), sess)
	if err != nil {
		t.Fatalf("CompactNow: %v", err)
	}
	if removed <= 0 {
		t.Errorf("removed = %d, want > 0", removed)
	}
	if len(sess.Messages) >= 14 {
		t.Errorf("messages were not compacted: %d", len(sess.Messages))
	}
	if !strings.Contains(sess.Messages[0].Content, "condensed history") {
		t.Errorf("first message = %q", sess.Messages[0].Content)
	}

	saved, err := store.Load(sess.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(saved.Messages) != len(sess.Messages) {
		t.Errorf("saved messages = %d, in-memory = %d", len(saved.Messages), len(sess.Messages))
	}
	if !strings.Contains(saved.Messages[0].Content, "condensed history") {
		t.Error("compaction was not persisted")
	}
}

func TestCompactNowShortConversation(t *testing.T) {
	a := New(Options{
		Config:   testConfig(t),
		Registry: tools.NewRegistry(),
	})
	sess := &session.Session{Messages: []llm.Message{{Role: "user", Content: "hi"}}}
	if _, err := a.CompactNow(context.Background(), sess); err == nil || !strings.Contains(err.Error(), "too short") {
		t.Fatalf("expected too-short error, got %v", err)
	}
	if _, err := a.CompactNow(context.Background(), nil); err == nil {
		t.Fatal("expected nil-session error")
	}
}

func TestMaybeCompactOnlyAboveThreshold(t *testing.T) {
	client, logs := newFakeLLM(t, func(n int, w http.ResponseWriter) {
		jsonCompletion(w, "summary")
	})
	cfg := testConfig(t)
	a := New(Options{Config: cfg, Client: client, Registry: tools.NewRegistry()})

	small := history(11)
	before := len(small)
	a.maybeCompact(context.Background(), &small, strings.Repeat("sys", 10), nil)
	if len(small) != before || len(logs.all()) != 0 {
		t.Errorf("compaction ran below threshold: len=%d calls=%d", len(small), len(logs.all()))
	}

	cfg.ContextWindow = 1000
	cfg.CompactAt = 0.5
	large := make([]llm.Message, 0, 14)
	for i := 0; i < 14; i++ {
		large = append(large, llm.Message{Role: "user", Content: strings.Repeat("x", 200)})
	}
	a.maybeCompact(context.Background(), &large, "", nil)
	if len(large) >= 14 {
		t.Errorf("expected compaction above threshold, length = %d", len(large))
	}
	if n := len(logs.all()); n != 1 {
		t.Errorf("summarizer calls = %d, want 1", n)
	}
}

func TestIsContextError(t *testing.T) {
	yes := []string{
		"context length exceeded",
		"maximum context window is 128000",
		"too many tokens in request",
		"input exceeds the maximum allowed",
	}
	for _, msg := range yes {
		if !isContextError(errors.New(msg)) {
			t.Errorf("isContextError(%q) = false, want true", msg)
		}
	}
	no := []string{"rate limited", "invalid api key", "connection reset"}
	for _, msg := range no {
		if isContextError(errors.New(msg)) {
			t.Errorf("isContextError(%q) = true, want false", msg)
		}
	}
	if isContextError(nil) {
		t.Error("isContextError(nil) = true")
	}
}
