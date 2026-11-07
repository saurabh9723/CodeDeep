package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"codedeep/internal/analytics"
	"codedeep/internal/app"
	"codedeep/internal/config"
	"codedeep/internal/llm"
	"codedeep/internal/session"
)

func analyticsTestHandler(t *testing.T) http.Handler {
	t.Helper()
	cfg := &config.Config{
		Model:       "test-model",
		WorkDir:     t.TempDir(),
		Home:        t.TempDir(),
		SessionsDir: t.TempDir(),
		TracesDir:   t.TempDir(),
		Pricing: map[string]config.Price{
			"test-model": {InputPerMTok: 1, OutputPerMTok: 2},
		},
	}
	a, err := app.New(cfg)
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	t.Cleanup(a.Close)

	sess := a.Sessions.New(cfg.Model, cfg.WorkDir)
	sess.Title = "Analytics fixture"
	sess.Turns = 1
	sess.Usage = llm.Usage{PromptTokens: 1000, CompletionTokens: 200, TotalTokens: 1200, PromptCacheHitTokens: 400}
	sess.TurnStats = []session.TurnStat{{
		Turn:  1,
		Time:  time.Now().UTC(),
		Usage: sess.Usage,
		Tools: map[string]llm.ToolUsage{"read_file": {Calls: 2, Usage: llm.Usage{PromptTokens: 300, TotalTokens: 300}}},
	}}
	if err := a.Sessions.Save(sess); err != nil {
		t.Fatalf("save: %v", err)
	}
	return New(a).Handler()
}

func TestAnalyticsEndpointReturnsReport(t *testing.T) {
	h := analyticsTestHandler(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/analytics?days=7&scope=workspace", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var rep analytics.Report
	if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if rep.Days != 7 || rep.Scope != "workspace" {
		t.Errorf("days/scope = %d/%q", rep.Days, rep.Scope)
	}
	if rep.Summary.Sessions != 1 || rep.Summary.Turns != 1 || rep.Summary.TotalTokens != 1200 {
		t.Errorf("summary = %+v", rep.Summary)
	}
	if rep.Summary.ToolCalls != 2 || len(rep.Tools) != 1 || rep.Tools[0].Name != "read_file" {
		t.Errorf("tools = %+v", rep.Tools)
	}
	if !rep.Priced || rep.Summary.CostUSD <= 0 {
		t.Errorf("pricing = %v cost=%v", rep.Priced, rep.Summary.CostUSD)
	}
	if len(rep.Daily) != 7 {
		t.Errorf("daily = %d, want 7", len(rep.Daily))
	}
}

func TestAnalyticsEndpointRejectsBadDays(t *testing.T) {
	h := analyticsTestHandler(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/analytics?days=soon", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}
