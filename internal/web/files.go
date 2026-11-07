package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"codedeep/internal/approval"
	"codedeep/internal/editor"
	"codedeep/internal/util"
)

type fileEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Dir  bool   `json:"dir"`
	Size int64  `json:"size,omitempty"`
}

// handleFiles lists one directory of the workspace for the file picker.
func (s *Server) handleFiles(w http.ResponseWriter, r *http.Request) {
	rel := strings.TrimSpace(r.URL.Query().Get("path"))
	dir := s.App.FS.Root
	if rel != "" {
		resolved, err := s.App.FS.Resolve(rel)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if !withinRoot(s.App.FS.Root, resolved) {
			writeError(w, http.StatusForbidden, "path is outside the workspace")
			return
		}
		dir = resolved
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, os.ErrNotExist) {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	out := make([]fileEntry, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() && util.IgnoredDir(name) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		abs := filepath.Join(dir, name)
		path := s.App.FS.Rel(abs)
		out = append(out, fileEntry{
			Name: name,
			Path: filepath.ToSlash(path),
			Dir:  entry.IsDir(),
			Size: info.Size(),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Dir != out[j].Dir {
			return out[i].Dir
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	writeJSON(w, http.StatusOK, out)
}

// handleFileSearch walks the workspace and returns files whose relative path
// contains the query. It powers @-mention completion.
func (s *Server) handleFileSearch(w http.ResponseWriter, r *http.Request) {
	query := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	if query == "" {
		writeJSON(w, http.StatusOK, []fileEntry{})
		return
	}
	const maxResults = 50
	const maxVisited = 20000
	root := s.App.FS.Root
	visited := 0
	out := make([]fileEntry, 0, maxResults)
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		visited++
		if visited > maxVisited {
			return fs.SkipAll
		}
		if d.IsDir() {
			if p != root && util.IgnoredDir(d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if !strings.Contains(strings.ToLower(rel), query) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		out = append(out, fileEntry{Name: d.Name(), Path: rel, Size: info.Size()})
		if len(out) >= maxResults {
			return fs.SkipAll
		}
		return nil
	})
	if err != nil && !errors.Is(err, fs.SkipAll) {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// handleCompact summarizes older messages on demand.
func (s *Server) handleCompact(w http.ResponseWriter, r *http.Request) {
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
	if !s.App.Cfg.APIKeyPresent() {
		writeError(w, http.StatusPreconditionRequired, "an API key is required to compact")
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
	removed, err := ag.CompactNow(context.Background(), sess)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"removed":  removed,
		"usage":    sess.Usage,
		"messages": len(sess.Messages),
	})
}

func withinRoot(root, abs string) bool {
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// handleOpen launches the configured editor for a workspace file.
func (s *Server) handleOpen(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
		Line int    `json:"line"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	ref := strings.TrimSpace(req.Path)
	if req.Line > 0 {
		ref = fmt.Sprintf("%s:%d", ref, req.Line)
	}
	target, err := editor.Resolve(s.App.Cfg.WorkDir, ref)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := editor.Open(s.App.Cfg, target); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"opened": target.Path,
		"line":   target.Line,
	})
}
