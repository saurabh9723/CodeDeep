package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"codedeep/internal/agent"
	"codedeep/internal/trace"
)

func TestChatRunSavesSessionTrace(t *testing.T) {
	provider := &scriptedProvider{chunks: []string{"hello from trace"}}
	s := newRunTestServer(t, provider)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/api/chat", "application/json", strings.NewReader(`{"prompt":"trace me"}`))
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	defer resp.Body.Close()
	mustScanEvents(t, resp.Body, agent.EventDone)

	store := trace.New(s.App.Cfg.TracesDir)
	var metas []trace.Meta
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		metas, _ = store.List()
		if len(metas) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(metas) != 1 {
		t.Fatalf("got %d session traces, want 1", len(metas))
	}
	meta := metas[0]
	if meta.Kind != trace.KindSession || meta.Status != "completed" || meta.EventCount == 0 {
		t.Fatalf("unexpected trace meta: %+v", meta)
	}
	if meta.ModelCalls == 0 || meta.SessionID == "" {
		t.Fatalf("trace did not record usage/session: %+v", meta)
	}
	saved, err := store.Load(meta.ID)
	if err != nil {
		t.Fatalf("load trace: %v", err)
	}
	if saved.Prompt != "trace me" || len(saved.Events) == 0 {
		t.Fatalf("unexpected trace body: prompt=%q events=%d", saved.Prompt, len(saved.Events))
	}
	done := false
	for _, ev := range saved.Events {
		if ev.Type == agent.EventDone {
			done = true
		}
	}
	if !done {
		t.Fatal("trace is missing the terminal done event")
	}
}
