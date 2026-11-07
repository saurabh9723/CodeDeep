package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"codedeep/internal/agent"
	"codedeep/internal/trace"
)

func TestTraceCompareAPI(t *testing.T) {
	h, store := traceTestServer(t)
	at := time.Now().UTC().Add(-time.Minute)
	aID := saveTrace(t, store, trace.Trace{
		Meta: trace.Meta{
			Kind:        trace.KindEval,
			Title:       "baseline",
			Task:        "go-bug-fix",
			Created:     at,
			Status:      "failed",
			DurationMS:  1200,
			ModelCalls:  2,
			ToolCalls:   2,
			TotalTokens: 900,
			CostUSD:     0.009,
			Failures:    []string{"tests failing"},
		},
		Prompt: "fix it",
		Events: []agent.Event{
			{Type: agent.EventToolStart, Name: "read_file", CallID: "a1"},
			{Type: agent.EventToolEnd, Name: "read_file", CallID: "a1"},
			{Type: agent.EventToolStart, Name: "edit_file", CallID: "a2"},
			{Type: agent.EventToolEnd, Name: "edit_file", CallID: "a2"},
			{Type: agent.EventText, Text: "could not fix"},
		},
	})
	bID := saveTrace(t, store, trace.Trace{
		Meta: trace.Meta{
			Kind:       trace.KindEval,
			Title:      "candidate",
			Task:       "go-bug-fix",
			Created:    at.Add(time.Minute),
			Status:     "passed",
			DurationMS: 1000,
			ModelCalls: 3,
			ToolCalls:  3,
			Failures:   nil,
		},
		Prompt: "fix it",
		Events: []agent.Event{
			{Type: agent.EventToolStart, Name: "read_file", CallID: "b1"},
			{Type: agent.EventToolEnd, Name: "read_file", CallID: "b1"},
			{Type: agent.EventToolStart, Name: "grep", CallID: "b2"},
			{Type: agent.EventToolEnd, Name: "grep", CallID: "b2"},
			{Type: agent.EventToolStart, Name: "edit_file", CallID: "b3"},
			{Type: agent.EventToolEnd, Name: "edit_file", CallID: "b3"},
			{Type: agent.EventText, Text: "fixed"},
		},
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/traces/compare?a="+aID+"&b="+bID, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("compare status = %d: %s", rec.Code, rec.Body.String())
	}
	var cmp trace.Comparison
	if err := json.Unmarshal(rec.Body.Bytes(), &cmp); err != nil {
		t.Fatalf("decode comparison: %v", err)
	}
	if cmp.A.ID != aID || cmp.B.ID != bID || !cmp.SameKind {
		t.Fatalf("comparison headers = %+v / %+v sameKind=%v", cmp.A, cmp.B, cmp.SameKind)
	}
	if cmp.Metrics.TotalTokens.Delta != -900 || cmp.Metrics.DurationMS.Delta != -200 {
		t.Fatalf("metrics = %+v", cmp.Metrics)
	}
	if cmp.Tools.Added != 1 || cmp.Tools.Removed != 0 || cmp.Tools.Same != 2 {
		t.Fatalf("tool diff = %+v", cmp.Tools)
	}
	if len(cmp.Failures.OnlyA) != 1 || cmp.Failures.OnlyA[0] != "tests failing" {
		t.Fatalf("failures = %+v", cmp.Failures)
	}
	if cmp.Answer == nil || !cmp.Answer.Changed {
		t.Fatalf("answer diff = %+v", cmp.Answer)
	}

	for _, target := range []string{
		"/api/traces/compare?a=" + aID,
		"/api/traces/compare?a=" + aID + "&b=" + aID,
	} {
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s status = %d, want 400", target, rec.Code)
		}
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/traces/compare?a="+aID+"&b=missing", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing trace status = %d, want 404", rec.Code)
	}
}
