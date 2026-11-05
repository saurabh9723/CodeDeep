package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"strings"
	"sync"
	"time"

	"codedeep/internal/agent"
	"codedeep/internal/app"
	"codedeep/internal/approval"
	"codedeep/internal/session"
)

//go:embed static/*
var staticFS embed.FS

type Server struct {
	App *app.App
	hub *approvalHub

	mu           sync.Mutex
	active       map[string]bool
	prevApproval string
}

func New(a *app.App) *Server {
	return &Server{
		App:    a,
		hub:    newApprovalHub(),
		active: map[string]bool{},
	}
}

func (s *Server) Handler() http.Handler {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err)
	}
	files := http.FileServer(http.FS(sub))

	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", files))
	mux.HandleFunc("GET /", s.handleIndex)
	mux.HandleFunc("GET /api/bootstrap", s.handleBootstrap)
	mux.HandleFunc("GET /api/sessions", s.handleListSessions)
	mux.HandleFunc("POST /api/sessions", s.handleCreateSession)
	mux.HandleFunc("GET /api/sessions/{id}", s.handleGetSession)
	mux.HandleFunc("DELETE /api/sessions/{id}", s.handleDeleteSession)
	mux.HandleFunc("POST /api/chat", s.handleChat)
	mux.HandleFunc("POST /api/approve", s.handleApprove)
	mux.HandleFunc("POST /api/undo", s.handleUndo)
	mux.HandleFunc("POST /api/plan", s.handlePlan)
	mux.HandleFunc("GET /api/health", s.handleHealth)
	return withHeaders(mux)
}

func withHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; "+
				"script-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'self'")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		http.Error(w, "ui not available", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(data)
}

type toolInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	ReadOnly    bool   `json:"read_only"`
}

func (s *Server) handleBootstrap(w http.ResponseWriter, r *http.Request) {
	metas, err := s.App.Sessions.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if metas == nil {
		metas = []session.Meta{}
	}
	tools := s.App.ToolInfos()
	infos := make([]toolInfo, 0, len(tools))
	for _, t := range tools {
		infos = append(infos, toolInfo{Name: t.Name, Description: t.Description, ReadOnly: t.ReadOnly})
	}
	approval := s.App.Cfg.ApprovalMode()
	writeJSON(w, http.StatusOK, map[string]any{
		"model":           s.App.Cfg.Model,
		"base_url":        s.App.Cfg.BaseURL,
		"workspace":       s.App.Cfg.WorkDir,
		"approval":        approval,
		"api_key_present": s.App.Cfg.APIKeyPresent(),
		"tools":           infos,
		"sessions":        metas,
		"mcp_servers":     s.App.MCPCount(),
		"version":         "0.1.0",
		"compact_at":      s.App.Cfg.CompactAt,
		"context_window":  s.App.Cfg.ContextWindow,
		"plan":            approval == "plan",
		"pricing":         s.App.Cfg.Pricing,
	})
}

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	metas, err := s.App.Sessions.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if metas == nil {
		metas = []session.Meta{}
	}
	writeJSON(w, http.StatusOK, metas)
}

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	sess := s.App.Sessions.New(s.App.Cfg.Model, s.App.Cfg.WorkDir)
	if err := s.App.Sessions.Save(sess); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, sess)
}

func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, err := s.App.Sessions.Load(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	writeJSON(w, http.StatusOK, sess)
}

func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	busy := s.active[id]
	s.mu.Unlock()
	if busy {
		writeError(w, http.StatusConflict, "session is running")
		return
	}
	if err := s.App.Sessions.Delete(id); err != nil {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string `json:"session_id"`
		Prompt    string `json:"prompt"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<20)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	req.Prompt = strings.TrimSpace(req.Prompt)
	if req.Prompt == "" {
		writeError(w, http.StatusBadRequest, "prompt is required")
		return
	}
	if !s.App.Cfg.APIKeyPresent() {
		writeError(w, http.StatusPreconditionRequired, "DEEPSEEK_API_KEY is not set")
		return
	}

	var sess *session.Session
	var err error
	if req.SessionID != "" {
		sess, err = s.App.Sessions.Load(req.SessionID)
		if err != nil {
			writeError(w, http.StatusNotFound, "session not found")
			return
		}
	} else {
		sess = s.App.Sessions.New(s.App.Cfg.Model, s.App.Cfg.WorkDir)
	}

	if !s.acquire(sess.ID) {
		writeError(w, http.StatusConflict, "a run is already active for this session")
		return
	}
	defer s.release(sess.ID)

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming is not supported")
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	var writeMu sync.Mutex
	failed := false
	emit := func(ev agent.Event) {
		writeMu.Lock()
		defer writeMu.Unlock()
		if failed {
			return
		}
		if ev.SessionID == "" {
			ev.SessionID = sess.ID
		}
		data, err := json.Marshal(ev)
		if err != nil {
			return
		}
		if _, err := w.Write(append(data, '\n')); err != nil {
			failed = true
			cancel()
			return
		}
		flusher.Flush()
	}

	emit(agent.Event{Type: "session", SessionID: sess.ID, Message: sess.Title})

	ag := s.App.NewAgent(approval.Func(s.hub.approve), emit)
	if err := ag.Run(ctx, sess, req.Prompt); err != nil && !errors.Is(err, context.Canceled) {
		s.App.Logf("chat %s: %v", sess.ID, err)
	}
}

func (s *Server) handleApprove(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID       string `json:"id"`
		Decision string `json:"decision"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	var decision approval.Decision
	switch req.Decision {
	case "allow":
		decision = approval.AllowOnce
	case "always":
		decision = approval.AllowAlways
	case "deny":
		decision = approval.Deny
	default:
		writeError(w, http.StatusBadRequest, "decision must be allow, always, or deny")
		return
	}
	if req.ID == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	s.hub.resolve(req.ID, decision)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleUndo(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string `json:"session_id"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if req.SessionID == "" {
		writeError(w, http.StatusBadRequest, "session_id is required")
		return
	}
	sess, err := s.App.Sessions.Load(req.SessionID)
	if err != nil {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	if !s.acquire(sess.ID) {
		writeError(w, http.StatusConflict, "a run is already active for this session")
		return
	}
	defer s.release(sess.ID)

	ag := s.App.NewAgent(approval.DenyAll, nil)
	files, err := ag.Undo(sess)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "reverted": files})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"reverted": files})
}

func (s *Server) handlePlan(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled bool `json:"enabled"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	s.mu.Lock()
	if req.Enabled {
		if current := s.App.Cfg.ApprovalMode(); current != "plan" {
			s.prevApproval = current
		}
		s.App.Cfg.SetApproval("plan")
	} else {
		mode := s.prevApproval
		if mode == "" || mode == "plan" {
			mode = "ask"
		}
		s.App.Cfg.SetApproval(mode)
	}
	approvalMode := s.App.Cfg.ApprovalMode()
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"approval": approvalMode,
		"plan":     approvalMode == "plan",
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":    true,
		"model": s.App.Cfg.Model,
		"time":  time.Now().UTC(),
	})
}

func (s *Server) acquire(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active[id] {
		return false
	}
	s.active[id] = true
	return true
}

func (s *Server) release(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.active, id)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

type earlyDecision struct {
	decision approval.Decision
	at       time.Time
}

// approvalHub bridges the agent's blocking Approve call and the browser's
// POST /api/approve request. Early decisions cover the tiny race where the
// client answers before the agent starts waiting.
type approvalHub struct {
	mu      sync.Mutex
	waiters map[string]chan approval.Decision
	early   map[string]earlyDecision
}

func newApprovalHub() *approvalHub {
	return &approvalHub{
		waiters: map[string]chan approval.Decision{},
		early:   map[string]earlyDecision{},
	}
}

func (h *approvalHub) approve(ctx context.Context, req approval.Request) (approval.Decision, error) {
	h.mu.Lock()
	if e, ok := h.early[req.ID]; ok && time.Since(e.at) < time.Minute {
		delete(h.early, req.ID)
		h.mu.Unlock()
		return e.decision, nil
	}
	ch := make(chan approval.Decision, 1)
	h.waiters[req.ID] = ch
	h.mu.Unlock()

	select {
	case <-ctx.Done():
		h.mu.Lock()
		delete(h.waiters, req.ID)
		h.mu.Unlock()
		return approval.Deny, ctx.Err()
	case d := <-ch:
		return d, nil
	}
}

func (h *approvalHub) resolve(id string, d approval.Decision) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for key, e := range h.early {
		if time.Since(e.at) > time.Minute {
			delete(h.early, key)
		}
	}
	if ch, ok := h.waiters[id]; ok {
		delete(h.waiters, id)
		ch <- d
		return
	}
	h.early[id] = earlyDecision{decision: d, at: time.Now()}
}
