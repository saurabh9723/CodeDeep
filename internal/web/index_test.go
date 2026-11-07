package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"codedeep/internal/app"
	"codedeep/internal/config"
	"codedeep/internal/index"
)

func indexTestServer(t *testing.T) http.Handler {
	t.Helper()
	dir := t.TempDir()
	src := "package widget\n\nfunc GadgetWidget() {}\n"
	if err := os.WriteFile(filepath.Join(dir, "widget.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Model:       "test-model",
		WorkDir:     dir,
		Home:        t.TempDir(),
		SessionsDir: t.TempDir(),
		TracesDir:   t.TempDir(),
		RepoMap:     config.RepoMap{MaxFiles: 100, RefreshSec: 1},
	}
	a, err := app.New(cfg)
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	t.Cleanup(a.Close)
	return New(a).Handler()
}

func TestIndexAPIFindsSymbols(t *testing.T) {
	h := indexTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/index?q=gadget", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Files   int            `json:"files"`
		Symbols int            `json:"symbols"`
		Results []index.Symbol `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Files != 1 || body.Symbols != 1 {
		t.Fatalf("stats = %d files / %d symbols", body.Files, body.Symbols)
	}
	if len(body.Results) != 1 {
		t.Fatalf("results = %#v", body.Results)
	}
	got := body.Results[0]
	if got.Name != "GadgetWidget" || got.File != "widget.go" || got.Line != 3 {
		t.Fatalf("result = %#v", got)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/index", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("empty query status = %d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode empty: %v", err)
	}
	if len(body.Results) != 0 || body.Files != 1 {
		t.Fatalf("empty query response = %#v", body)
	}
}
