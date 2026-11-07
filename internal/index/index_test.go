package index

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codedeep/internal/tools"
)

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

func newTestIndex(t *testing.T, root string) *Index {
	t.Helper()
	return New(root, Options{RefreshInterval: time.Nanosecond})
}

func TestIndexGoSymbolsAndSearch(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "internal/sample/sample.go", `package sample

const Limit = 10

var ErrBad = error(nil)

type Registry struct {
	Name string
}

type Store interface {
	Get() string
}

func NewRegistry(name string) *Registry {
	return &Registry{Name: name}
}

func (r *Registry) Get(key string) (string, bool) {
	return key, true
}
`)
	idx := newTestIndex(t, root)
	files, symbols, _, err := idx.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if files != 1 {
		t.Fatalf("files = %d, want 1", files)
	}
	if symbols < 6 {
		t.Fatalf("symbols = %d, want at least 6", symbols)
	}

	got, err := idx.SearchSymbols("Registry.Get", "", "", 10)
	if err != nil {
		t.Fatalf("SearchSymbols: %v", err)
	}
	if len(got) == 0 || got[0].Kind != "method" || got[0].Parent != "Registry" {
		t.Fatalf("expected Registry.Get method first, got %#v", got)
	}
	if !strings.Contains(got[0].Signature, "func (r *Registry) Get") {
		t.Fatalf("signature = %q", got[0].Signature)
	}

	got, _ = idx.SearchSymbols("limit", "", "const", 10)
	if len(got) != 1 || got[0].Name != "Limit" || got[0].Line != 3 {
		t.Fatalf("const lookup = %#v", got)
	}

	got, _ = idx.SearchSymbols("store", "", "", 10)
	if len(got) == 0 || got[0].Kind != "interface" {
		t.Fatalf("interface lookup = %#v", got)
	}

	got, _ = idx.SearchSymbols("", "internal/sample", "", 10)
	if len(got) == 0 {
		t.Fatal("expected symbols for path prefix")
	}
	for _, sym := range got {
		if !strings.HasPrefix(sym.File, "internal/sample/") {
			t.Fatalf("path filter leaked %s", sym.File)
		}
	}
}

func TestIndexSkipsIgnoredDirsAndNoise(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "main.go", "package main\n\nfunc main() {}\n")
	writeFile(t, root, "node_modules/pkg/index.js", "export function ignored() {}\n")
	writeFile(t, root, ".hidden/secret.go", "package hidden\n\nfunc Hidden() {}\n")
	writeFile(t, root, "package-lock.json", "{}\n")
	writeFile(t, root, "app.min.js", "function minified(){}\n")
	writeFile(t, root, "logo.png", string([]byte{0, 1, 2, 3}))

	idx := newTestIndex(t, root)
	files, symbols, _, err := idx.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if files != 1 {
		t.Fatalf("files = %d, want 1", files)
	}
	if symbols != 1 {
		t.Fatalf("symbols = %d, want 1", symbols)
	}
}

func TestIndexPythonJavaScriptMarkdown(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "app.py", `class Service:
    def __init__(self, name):
        self.name = name

    async def fetch(self, url):
        return url

def helper(value):
    return value
`)
	writeFile(t, root, "web/app.js", `export class Panel {
}

export function render(node, opts) {
}

const load = (url) => fetch(url);
`)
	writeFile(t, root, "README.md", "# Project\n\n## Usage\n\n````\n# not a heading\n````\n")

	idx := newTestIndex(t, root)
	got, _ := idx.SearchSymbols("fetch", "", "", 5)
	if len(got) == 0 || got[0].Kind != "method" || got[0].Parent != "Service" {
		t.Fatalf("python method = %#v", got)
	}
	got, _ = idx.SearchSymbols("render", "", "func", 5)
	if len(got) == 0 || got[0].File != "web/app.js" {
		t.Fatalf("js function = %#v", got)
	}
	got, _ = idx.SearchSymbols("usage", "", "heading", 5)
	if len(got) == 0 || got[0].Name != "Usage" || got[0].Line != 3 {
		t.Fatalf("markdown heading = %#v", got)
	}
}

func TestIndexFuzzySearch(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "symbols.go", `package sample

func NewFileSystem() {}

func ResolvePath() {}
`)
	idx := newTestIndex(t, root)
	got, _ := idx.SearchSymbols("nfs", "", "", 5)
	if len(got) == 0 || got[0].Name != "NewFileSystem" {
		t.Fatalf("fuzzy lookup = %#v", got)
	}
	got, _ = idx.SearchSymbols("res", "", "", 5)
	if len(got) == 0 || got[0].Name != "ResolvePath" {
		t.Fatalf("prefix lookup = %#v", got)
	}
}

func TestMapRankingBudgetAndQuery(t *testing.T) {
	root := t.TempDir()
	var big strings.Builder
	big.WriteString("package big\n\n")
	for i := 0; i < 30; i++ {
		big.WriteString("func Helper")
		big.WriteString(strings.Repeat("X", i%3+1))
		big.WriteString("() {}\n")
	}
	writeFile(t, root, "alpha/big.go", big.String())
	writeFile(t, root, "beta/widget.go", "package beta\n\nfunc Gadget() {}\n")

	idx := newTestIndex(t, root)
	res, err := idx.Map(MapOptions{MaxTokens: 400})
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	if res.Files != 2 || res.ShownFiles == 0 {
		t.Fatalf("map counts = %#v", res)
	}
	if res.Tokens > 460 {
		t.Fatalf("map exceeded budget: %d tokens", res.Tokens)
	}

	res, err = idx.Map(MapOptions{Query: "Gadget", MaxTokens: 400})
	if err != nil {
		t.Fatalf("Map query: %v", err)
	}
	text := res.Text
	beta := strings.Index(text, "beta/widget.go")
	alpha := strings.Index(text, "alpha/big.go")
	if beta < 0 || (alpha >= 0 && beta > alpha) {
		t.Fatalf("query ranking wrong:\n%s", text)
	}
}

func TestPromptSectionAndCache(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "main.go", "package main\n\nfunc BuildThing() {}\n")
	idx := newTestIndex(t, root)

	if got := idx.Prompt("build thing", 0); got != "" {
		t.Fatalf("Prompt with zero budget = %q", got)
	}
	first := idx.Prompt("build thing", 600)
	if !strings.Contains(first, "# Codebase map") || !strings.Contains(first, "BuildThing") {
		t.Fatalf("prompt missing map content:\n%s", first)
	}
	if second := idx.Prompt("build thing", 600); second != first {
		t.Fatal("prompt cache returned different text for the same input")
	}

	writeFile(t, root, "main.go", "package main\n\nfunc BuildThing() {}\n\nfunc AddedLater() {}\n")
	updated := idx.Prompt("addedlater", 600)
	if !strings.Contains(updated, "AddedLater") {
		t.Fatalf("prompt did not refresh:\n%s", updated)
	}
}

func TestRegisteredTools(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.go", "package a\n\nfunc Alpha() {}\n")
	reg := tools.NewRegistry()
	RegisterTools(reg, newTestIndex(t, root))

	find, ok := reg.Get("find_symbol")
	if !ok {
		t.Fatal("find_symbol not registered")
	}
	res, err := find.Run(context.Background(), json.RawMessage(`{"name":"alpha"}`))
	if err != nil {
		t.Fatalf("find_symbol: %v", err)
	}
	if !strings.Contains(res.Output, "a.go:3  func Alpha()") {
		t.Fatalf("find_symbol output = %q", res.Output)
	}

	mapSpec, ok := reg.Get("repo_map")
	if !ok {
		t.Fatal("repo_map not registered")
	}
	res, err = mapSpec.Run(context.Background(), json.RawMessage(`{"query":"alpha","max_tokens":300}`))
	if err != nil {
		t.Fatalf("repo_map: %v", err)
	}
	if !strings.Contains(res.Output, "a.go") {
		t.Fatalf("repo_map output = %q", res.Output)
	}
}

func TestIndexRefreshesChangedFiles(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.go", "package a\n\nfunc First() {}\n")
	idx := newTestIndex(t, root)
	if _, _, _, err := idx.Stats(); err != nil {
		t.Fatalf("Stats: %v", err)
	}
	writeFile(t, root, "a.go", "package a\n\nfunc First() {}\n\nfunc Second() {}\n")
	got, _ := idx.SearchSymbols("Second", "", "", 5)
	if len(got) != 1 {
		t.Fatalf("expected refreshed symbol, got %#v", got)
	}
	os.Remove(filepath.Join(root, "a.go"))
	got, _ = idx.SearchSymbols("Second", "", "", 5)
	if len(got) != 0 {
		t.Fatalf("expected removed file to disappear, got %#v", got)
	}
}
