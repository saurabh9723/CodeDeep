package web

import (
	"errors"
	"net/http"
	"os"
	"strings"
	"time"

	"codedeep/internal/agent"
	"codedeep/internal/session"
	"codedeep/internal/trace"
)

// saveSessionTrace persists one live chat turn for the trace viewer. Traces are
// best-effort: a failure must never disturb the run or the session.
func (s *Server) saveSessionTrace(run *activeRun, sess *session.Session, prompt string, started time.Time, status string) {
	if s.traces == nil {
		return
	}
	events := run.snapshot()
	if len(events) == 0 {
		return
	}
	modelCalls, toolCalls, tokens, cost := trace.Summarize(events)
	var failures []string
	for _, ev := range events {
		if ev.Type == agent.EventError && ev.Depth == 0 && strings.TrimSpace(ev.Message) != "" {
			failures = append(failures, ev.Message)
		}
	}
	title := strings.TrimSpace(sess.Title)
	if title == "" {
		title = "session " + sess.ID
	}
	model := sess.Model
	if model == "" {
		model = s.App.Cfg.Model
	}
	cwd := sess.Cwd
	if cwd == "" {
		cwd = s.App.Cfg.WorkDir
	}
	_, err := s.traces.Save(trace.Trace{
		Meta: trace.Meta{
			Kind:        trace.KindSession,
			Title:       title,
			Created:     started.UTC(),
			DurationMS:  time.Since(started).Milliseconds(),
			Provider:    s.App.Cfg.Provider,
			Model:       model,
			Status:      status,
			Cwd:         cwd,
			SessionID:   sess.ID,
			Failures:    failures,
			ModelCalls:  modelCalls,
			ToolCalls:   toolCalls,
			TotalTokens: tokens,
			CostUSD:     cost,
		},
		Prompt: prompt,
		Events: events,
	})
	if err != nil {
		s.App.Logf("trace %s: %v", sess.ID, err)
	}
}

func (s *Server) handleListTraces(w http.ResponseWriter, r *http.Request) {
	metas := []trace.Meta{}
	if s.traces != nil {
		list, err := s.traces.List()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		kind := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("kind")))
		query := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
		for _, meta := range list {
			if kind != "" && kind != "all" && meta.Kind != kind {
				continue
			}
			if query != "" && !traceMatches(meta, query) {
				continue
			}
			metas = append(metas, meta)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"traces": metas})
}

func traceMatches(meta trace.Meta, query string) bool {
	haystack := strings.ToLower(strings.Join([]string{
		meta.Title, meta.Task, meta.Model, meta.Provider, meta.SessionID, meta.Status,
		strings.Join(meta.Tags, " "),
	}, " "))
	return strings.Contains(haystack, query)
}

func (s *Server) handleGetTrace(w http.ResponseWriter, r *http.Request) {
	if s.traces == nil {
		writeError(w, http.StatusNotFound, "trace not found")
		return
	}
	tr, err := s.traces.Load(r.PathValue("id"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeError(w, http.StatusNotFound, "trace not found")
		} else {
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, tr)
}

func (s *Server) handleDeleteTrace(w http.ResponseWriter, r *http.Request) {
	if s.traces == nil {
		writeError(w, http.StatusNotFound, "trace not found")
		return
	}
	if err := s.traces.Delete(r.PathValue("id")); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeError(w, http.StatusNotFound, "trace not found")
		} else {
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
