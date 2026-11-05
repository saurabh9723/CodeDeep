package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTestFile(t *testing.T, root, rel, content string) string {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", rel, err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
	return p
}

func callTool(t *testing.T, fn func(context.Context, json.RawMessage) (Result, error), args any) (Result, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return fn(context.Background(), raw)
}

func TestMatch(t *testing.T) {
	cases := []struct {
		pattern string
		name    string
		want    bool
	}{
		{"*.go", "main.go", true},
		{"*.go", "a/b.go", false},
		{"**/*.go", "main.go", true},
		{"**/*.go", "a/b/c.go", true},
		{"src/**/*.ts", "src/a/b/x.ts", true},
		{"src/**/*.ts", "lib/a/x.ts", false},
		{"a/**", "a/b/c", true},
		{"a/**", "a", true},
		{"**", "any/nested/path", true},
		{"a/?.txt", "a/b.txt", true},
		{"a/?.txt", "a/bc.txt", false},
		{"a/*.txt", "a/b/c.txt", false},
		{"./a/b.go", "a/b.go", true},
	}
	for _, c := range cases {
		if got := Match(c.pattern, c.name); got != c.want {
			t.Errorf("Match(%q, %q) = %v, want %v", c.pattern, c.name, got, c.want)
		}
	}
}

func TestGlobSkipsIgnoredDirs(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "main.go", "package main\n")
	writeTestFile(t, root, "sub/x.go", "package sub\n")
	writeTestFile(t, root, "node_modules/dep/y.go", "package dep\n")
	writeTestFile(t, root, ".git/hooks/z.go", "package z\n")
	writeTestFile(t, root, "vendor/v.go", "package v\n")

	f := NewFS(root, false)
	res, err := callTool(t, f.globFiles, map[string]any{"pattern": "**/*.go"})
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	out := res.Output
	for _, want := range []string{"main.go", "sub/x.go"} {
		if !strings.Contains(out, want) {
			t.Errorf("glob output missing %q:\n%s", want, out)
		}
	}
	for _, unwanted := range []string{"node_modules", ".git", "vendor"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("glob output should skip %q:\n%s", unwanted, out)
		}
	}
}

func TestGlobNoMatchesAndLimits(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		writeTestFile(t, root, name, "x\n")
	}
	f := NewFS(root, false)

	res, err := callTool(t, f.globFiles, map[string]any{"pattern": "*.md"})
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if !strings.Contains(res.Output, "No files matched") {
		t.Errorf("expected no-match message, got %q", res.Output)
	}

	res, err = callTool(t, f.globFiles, map[string]any{"pattern": "*.txt", "max_results": 2})
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if lines := strings.Count(res.Output, ".txt"); lines != 2 {
		t.Errorf("expected 2 results, got %d:\n%s", lines, res.Output)
	}
	if !strings.Contains(res.Output, "stopped after 2 results") {
		t.Errorf("expected limit notice, got %q", res.Output)
	}
}

func TestGrepSkipsIgnoredDirsAndBinaries(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "a.txt", "hello needle\n")
	writeTestFile(t, root, "b.md", "nothing here\n")
	writeTestFile(t, root, "node_modules/n.txt", "needle\n")
	writeTestFile(t, root, "binary.bin", "needle\x00binary\n")

	f := NewFS(root, false)
	res, err := callTool(t, f.grepFiles, map[string]any{"pattern": "needle"})
	if err != nil {
		t.Fatalf("grep: %v", err)
	}
	out := res.Output
	if !strings.Contains(out, "a.txt:1: hello needle") {
		t.Errorf("grep missing expected match:\n%s", out)
	}
	for _, unwanted := range []string{"node_modules", "binary.bin"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("grep should skip %q:\n%s", unwanted, out)
		}
	}
}

func TestGrepFilterAndIgnoreCase(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "a.txt", "Hello Needle\n")
	writeTestFile(t, root, "b.md", "nothing\n")
	writeTestFile(t, root, "c.md", "hello NEEDLE\n")

	f := NewFS(root, false)
	res, err := callTool(t, f.grepFiles, map[string]any{
		"pattern":     "needle",
		"glob":        "*.md",
		"ignore_case": true,
	})
	if err != nil {
		t.Fatalf("grep: %v", err)
	}
	if !strings.Contains(res.Output, "c.md:1: hello NEEDLE") {
		t.Errorf("grep missing filtered match:\n%s", res.Output)
	}
	if strings.Contains(res.Output, "a.txt") {
		t.Errorf("grep glob filter failed:\n%s", res.Output)
	}
}

func TestGrepInvalidRegex(t *testing.T) {
	root := t.TempDir()
	f := NewFS(root, false)
	if _, err := callTool(t, f.grepFiles, map[string]any{"pattern": "("}); err == nil {
		t.Fatal("expected invalid regex error")
	}
}

func TestEditFileCRLFFallback(t *testing.T) {
	root := t.TempDir()
	path := writeTestFile(t, root, "note.txt", "line one\r\nline two\r\n")
	f := NewFS(root, false)

	res, err := callTool(t, f.editFile, map[string]any{
		"path":       "note.txt",
		"old_string": "line one\nline two",
		"new_string": "changed\nline two",
	})
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if !strings.Contains(res.Output, "1 replacement") {
		t.Errorf("unexpected edit output: %q", res.Output)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); got != "changed\r\nline two\r\n" {
		t.Errorf("CRLF file content = %q", got)
	}
}

func TestEditFileAmbiguousAndReplaceAll(t *testing.T) {
	root := t.TempDir()
	path := writeTestFile(t, root, "dup.txt", "dup dup\n")
	f := NewFS(root, false)

	if _, err := callTool(t, f.editFile, map[string]any{
		"path":       "dup.txt",
		"old_string": "dup",
		"new_string": "x",
	}); err == nil || !strings.Contains(err.Error(), "2 times") {
		t.Fatalf("expected ambiguous-match error, got %v", err)
	}

	res, err := callTool(t, f.editFile, map[string]any{
		"path":        "dup.txt",
		"old_string":  "dup",
		"new_string":  "x",
		"replace_all": true,
	})
	if err != nil {
		t.Fatalf("edit replace_all: %v", err)
	}
	if !strings.Contains(res.Output, "2 replacement") {
		t.Errorf("unexpected output: %q", res.Output)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "x x\n" {
		t.Errorf("content = %q", data)
	}
}

func TestEditFileMissingString(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "a.txt", "hello\n")
	f := NewFS(root, false)
	_, err := callTool(t, f.editFile, map[string]any{
		"path":       "a.txt",
		"old_string": "goodbye",
		"new_string": "hi",
	})
	if err == nil || !strings.Contains(err.Error(), "was not found") {
		t.Fatalf("expected not-found error, got %v", err)
	}
}

func TestResolveStaysInsideWorkspace(t *testing.T) {
	root := t.TempDir()
	f := NewFS(root, false)
	if _, err := f.Resolve("../escape.txt"); err == nil || !strings.Contains(err.Error(), "outside the workspace") {
		t.Fatalf("expected escape error, got %v", err)
	}
	if _, err := callTool(t, f.readFile, map[string]any{"path": "../escape.txt"}); err == nil {
		t.Fatal("read_file should reject paths outside the workspace")
	}

	loose := NewFS(root, true)
	if _, err := loose.Resolve("../escape.txt"); err != nil {
		t.Fatalf("allowOutside should permit escape path: %v", err)
	}
}

func TestReadFileOffsetAndLimit(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "lines.txt", "one\ntwo\nthree\n")
	f := NewFS(root, false)

	res, err := callTool(t, f.readFile, map[string]any{"path": "lines.txt", "offset": 2, "limit": 1})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(res.Output, "     2→two") {
		t.Errorf("offset read missing line 2:\n%s", res.Output)
	}
	if strings.Contains(res.Output, "one") {
		t.Errorf("offset read should not include line 1:\n%s", res.Output)
	}
	if !strings.Contains(res.Output, "more lines available") {
		t.Errorf("expected truncation notice:\n%s", res.Output)
	}

	if _, err := callTool(t, f.readFile, map[string]any{"path": "lines.txt", "offset": 99}); err == nil || !strings.Contains(err.Error(), "beyond the end") {
		t.Fatalf("expected beyond-end error, got %v", err)
	}
}

func TestReadFileRejectsBinaryAndDirectories(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "bin.dat", "abc\x00def")
	os.MkdirAll(filepath.Join(root, "dir"), 0o755)
	f := NewFS(root, false)

	if _, err := callTool(t, f.readFile, map[string]any{"path": "bin.dat"}); err == nil || !strings.Contains(err.Error(), "binary") {
		t.Fatalf("expected binary error, got %v", err)
	}
	if _, err := callTool(t, f.readFile, map[string]any{"path": "dir"}); err == nil || !strings.Contains(err.Error(), "directory") {
		t.Fatalf("expected directory error, got %v", err)
	}
}

func TestWriteFileOutput(t *testing.T) {
	root := t.TempDir()
	f := NewFS(root, false)

	res, err := callTool(t, f.writeFile, map[string]any{"path": "notes.txt", "content": "hello"})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if res.Output != "created notes.txt (5 bytes)" {
		t.Errorf("create output = %q", res.Output)
	}

	res, err = callTool(t, f.writeFile, map[string]any{"path": "notes.txt", "content": "hello"})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if res.Output != "updated notes.txt (5 bytes)" {
		t.Errorf("update output = %q", res.Output)
	}

	data, _ := os.ReadFile(filepath.Join(root, "notes.txt"))
	if string(data) != "hello" {
		t.Errorf("content = %q", data)
	}
}

func TestWriteFileCreatesParentsAndRejectsEmptyPath(t *testing.T) {
	root := t.TempDir()
	f := NewFS(root, false)

	if _, err := callTool(t, f.writeFile, map[string]any{"path": "deep/nested/file.txt", "content": "ok"}); err != nil {
		t.Fatalf("write nested: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "deep", "nested", "file.txt")); err != nil {
		t.Fatalf("nested file not created: %v", err)
	}
	if _, err := callTool(t, f.writeFile, map[string]any{"path": "", "content": "x"}); err == nil {
		t.Fatal("expected empty-path error")
	}
}
