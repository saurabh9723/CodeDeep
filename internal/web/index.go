package web

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"codedeep/internal/index"
)

type indexResponse struct {
	Query   string         `json:"query"`
	Files   int            `json:"files"`
	Symbols int            `json:"symbols"`
	BuiltAt time.Time      `json:"built_at"`
	Results []index.Symbol `json:"results"`
}

// handleIndexSearch powers symbol search in the web file palette.
func (s *Server) handleIndexSearch(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	kind := strings.TrimSpace(r.URL.Query().Get("kind"))
	prefix := strings.TrimSpace(r.URL.Query().Get("path"))
	limit := 20
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			limit = n
		}
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}

	idx := s.App.Index
	if idx == nil {
		writeJSON(w, http.StatusOK, indexResponse{Query: query})
		return
	}
	files, symbols, built, err := idx.Stats()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var results []index.Symbol
	if query != "" {
		results, err = idx.SearchSymbols(query, prefix, kind, limit)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, indexResponse{
		Query:   query,
		Files:   files,
		Symbols: symbols,
		BuiltAt: built,
		Results: results,
	})
}
