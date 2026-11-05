package tools

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func callPreview(t *testing.T, fn func(context.Context, json.RawMessage) (*Preview, error), args any) (*Preview, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return fn(context.Background(), raw)
}

func TestPreviewWriteNewFile(t *testing.T) {
	root := t.TempDir()
	f := NewFS(root, false)

	p, err := callPreview(t, f.previewWrite, map[string]any{"path": "new.txt", "content": "hello\nworld\n"})
	if err != nil {
		t.Fatalf("previewWrite: %v", err)
	}
	if p == nil || p.Path != "new.txt" {
		t.Fatalf("preview = %+v", p)
	}
	for _, want := range []string{"--- /dev/null", "+++ b/new.txt", "+hello", "+world"} {
		if !strings.Contains(p.Diff, want) {
			t.Errorf("diff missing %q:\n%s", want, p.Diff)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "new.txt")); !os.IsNotExist(err) {
		t.Error("preview must not create the file")
	}
}

func TestPreviewWriteExistingAndUnchanged(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "note.txt", "one\ntwo\nthree\n")
	f := NewFS(root, false)

	p, err := callPreview(t, f.previewWrite, map[string]any{"path": "note.txt", "content": "one\nTWO\nthree\n"})
	if err != nil {
		t.Fatalf("previewWrite: %v", err)
	}
	if p == nil {
		t.Fatal("expected a diff for a changed file")
	}
	for _, want := range []string{"--- a/note.txt", "+++ b/note.txt", "-two", "+TWO"} {
		if !strings.Contains(p.Diff, want) {
			t.Errorf("diff missing %q:\n%s", want, p.Diff)
		}
	}

	p, err = callPreview(t, f.previewWrite, map[string]any{"path": "note.txt", "content": "one\ntwo\nthree\n"})
	if err != nil {
		t.Fatalf("previewWrite unchanged: %v", err)
	}
	if p != nil {
		t.Errorf("unchanged content should have no preview, got %+v", p)
	}
}

func TestPreviewWriteBinaryAndOversize(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "blob.bin", "abc\x00def")
	f := NewFS(root, false)

	p, err := callPreview(t, f.previewWrite, map[string]any{"path": "blob.bin", "content": "text"})
	if err != nil {
		t.Fatalf("previewWrite binary: %v", err)
	}
	if p == nil || !strings.Contains(p.Diff, "binary file") {
		t.Errorf("binary preview = %+v", p)
	}

	big := strings.Repeat("x", maxPreviewBytes+1)
	writeTestFile(t, root, "big.txt", big)
	p, err = callPreview(t, f.previewWrite, map[string]any{"path": "big.txt", "content": "small"})
	if err != nil {
		t.Fatalf("previewWrite oversize: %v", err)
	}
	if p == nil || !strings.Contains(p.Diff, "diff omitted") {
		t.Errorf("oversize preview = %+v", p)
	}
}

func TestPreviewEdit(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "code.go", "package demo\n\nfunc old() {}\n")
	f := NewFS(root, false)

	p, err := callPreview(t, f.previewEdit, map[string]any{
		"path":       "code.go",
		"old_string": "func old() {}",
		"new_string": "func newFunc() {}",
	})
	if err != nil {
		t.Fatalf("previewEdit: %v", err)
	}
	if p == nil || p.Path != "code.go" {
		t.Fatalf("preview = %+v", p)
	}
	for _, want := range []string{"-func old() {}", "+func newFunc() {}"} {
		if !strings.Contains(p.Diff, want) {
			t.Errorf("diff missing %q:\n%s", want, p.Diff)
		}
	}

	data, _ := os.ReadFile(filepath.Join(root, "code.go"))
	if !strings.Contains(string(data), "func old()") {
		t.Error("preview must not modify the file")
	}
}

func TestWriteAndEditReturnChange(t *testing.T) {
	root := t.TempDir()
	f := NewFS(root, false)

	res, err := callTool(t, f.writeFile, map[string]any{"path": "a.txt", "content": "first\n"})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if res.Change == nil || res.Change.Existed || res.Change.Old != "" || res.Change.New != "first\n" {
		t.Fatalf("create change = %+v", res.Change)
	}

	res, err = callTool(t, f.writeFile, map[string]any{"path": "a.txt", "content": "second\n"})
	if err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	if res.Change == nil || !res.Change.Existed || res.Change.Old != "first\n" || res.Change.New != "second\n" {
		t.Fatalf("overwrite change = %+v", res.Change)
	}
	if res.Change.Mode == 0 {
		t.Error("change should record the file mode")
	}

	res, err = callTool(t, f.editFile, map[string]any{
		"path":       "a.txt",
		"old_string": "second",
		"new_string": "third",
	})
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if res.Change == nil || res.Change.Old != "second\n" || res.Change.New != "third\n" {
		t.Fatalf("edit change = %+v", res.Change)
	}
}

func TestWriteBinaryFileSkipsChange(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "blob.bin", "old\x00data")
	f := NewFS(root, false)

	res, err := callTool(t, f.writeFile, map[string]any{"path": "blob.bin", "content": "new\x00data"})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if res.Change != nil {
		t.Errorf("binary overwrite should not be checkpointed: %+v", res.Change)
	}
}

func gitEnv(t *testing.T) []string {
	t.Helper()
	cfg := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(cfg, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_GLOBAL="+cfg,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=CodeDeep Test",
		"GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=CodeDeep Test",
		"GIT_COMMITTER_EMAIL=test@example.com",
	)
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = gitEnv(t)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func initRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	runGit(t, dir, "init")
	writeTestFile(t, dir, "a.txt", "old line\n")
	runGit(t, dir, "add", "a.txt")
	runGit(t, dir, "commit", "-m", "init")
	return dir
}

func TestGitStatusAndDiff(t *testing.T) {
	root := initRepo(t)
	writeTestFile(t, root, "a.txt", "new line\n")
	f := NewFS(root, false)

	res, err := callTool(t, f.gitStatus, map[string]any{})
	if err != nil {
		t.Fatalf("git_status: %v", err)
	}
	if !strings.Contains(res.Output, "a.txt") {
		t.Errorf("git_status missing changed file:\n%s", res.Output)
	}

	res, err = callTool(t, f.gitDiff, map[string]any{})
	if err != nil {
		t.Fatalf("git_diff: %v", err)
	}
	for _, want := range []string{"-old line", "+new line"} {
		if !strings.Contains(res.Output, want) {
			t.Errorf("git_diff missing %q:\n%s", want, res.Output)
		}
	}

	res, err = callTool(t, f.gitDiff, map[string]any{"stat": true})
	if err != nil {
		t.Fatalf("git_diff --stat: %v", err)
	}
	if !strings.Contains(res.Output, "a.txt") || !strings.Contains(res.Output, "1") {
		t.Errorf("diffstat missing file/change:\n%s", res.Output)
	}

	brief := GitBrief(root)
	if !strings.Contains(brief, "uncommitted file") {
		t.Errorf("GitBrief = %q", brief)
	}
}

func TestGitToolsOutsideRepo(t *testing.T) {
	root := t.TempDir()
	f := NewFS(root, false)

	for name, fn := range map[string]func() (Result, error){
		"git_status": func() (Result, error) { return callTool(t, f.gitStatus, map[string]any{}) },
		"git_diff":   func() (Result, error) { return callTool(t, f.gitDiff, map[string]any{}) },
	} {
		res, err := fn()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !strings.Contains(res.Output, "not a git repository") {
			t.Errorf("%s output = %q", name, res.Output)
		}
	}
	if brief := GitBrief(root); brief != "" {
		t.Errorf("GitBrief outside repo = %q, want empty", brief)
	}
}
