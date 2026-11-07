package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"codedeep/internal/agent"
	"codedeep/internal/app"
	"codedeep/internal/config"
	"codedeep/internal/trace"
)

func traceTestServer(t *testing.T) (http.Handler, *trace.Store) {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{
		Model:       "test-model",
		WorkDir:     t.TempDir(),
		Home:        t.TempDir(),
		SessionsDir: t.TempDir(),
		TracesDir:   dir,
	}
	a, err := app.New(cfg)
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	t.Cleanup(a.Close)
	return New(a).Handler(), trace.New(dir)
}

func saveTrace(t *testing.T, store *trace.Store, tr trace.Trace) string {
	t.Helper()
	id, err := store.Save(tr)
	if err != nil {
		t.Fatalf("save trace: %v", err)
	}
	return id
}

func TestTraceAPIListFilterAndDelete(t *testing.T) {
	h, store := traceTestServer(t)
	passed := true
	base := time.Now().UTC().Add(-time.Minute)
	evalID := saveTrace(t, store, trace.Trace{
		Meta: trace.Meta{
			Kind:    trace.KindEval,
			Title:   "smoke",
			Task:    "smoke",
			Created: base,
			Status:  "passed",
			Tags:    []string{"go", "smoke"},
			Passed:  &passed,
		},
		Prompt: "create ok.txt",
		Events: []agent.Event{{Type: agent.EventUser, Text: "create ok.txt", Time: base.Format(time.RFC3339Nano)}},
	})
	sessionID := saveTrace(t, store, trace.Trace{
		Meta: trace.Meta{
			Kind:      trace.KindSession,
			Title:     "Live chat",
			Created:   base.Add(30 * time.Second),
			Status:    "completed",
			Model:     "test-model",
			SessionID: "sess-1",
		},
		Prompt: "hello",
		Events: []agent.Event{{Type: agent.EventUser, Text: "hello"}},
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/traces", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d: %s", rec.Code, rec.Body.String())
	}
	var list struct {
		Traces []trace.Meta `json:"traces"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list.Traces) != 2 || list.Traces[0].ID != sessionID {
		t.Fatalf("unexpected list: %+v", list.Traces)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/traces?kind=eval", nil))
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode eval list: %v", err)
	}
	if len(list.Traces) != 1 || list.Traces[0].ID != evalID {
		t.Fatalf("kind filter failed: %+v", list.Traces)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/traces?q=go", nil))
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode query list: %v", err)
	}
	if len(list.Traces) != 1 || list.Traces[0].ID != evalID {
		t.Fatalf("search filter failed: %+v", list.Traces)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/traces/"+evalID, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d: %s", rec.Code, rec.Body.String())
	}
	var got trace.Trace
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode trace: %v", err)
	}
	if got.ID != evalID || got.Prompt != "create ok.txt" || len(got.Events) != 1 {
		t.Fatalf("unexpected trace: %+v", got.Meta)
	}
	if got.Events[0].Time == "" {
		t.Fatal("event timestamp was not preserved")
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/traces/missing", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing trace status = %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/traces/"+evalID, nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d: %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/traces/"+evalID, nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("deleted trace still served: %d", rec.Code)
	}
}
