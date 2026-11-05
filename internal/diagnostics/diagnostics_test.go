package diagnostics

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codedeep/internal/config"
	"codedeep/internal/tools"
)

func cmdShell() bool {
	shell, _ := tools.ShellCommand("", "x")
	return strings.EqualFold(filepath.Base(shell), "cmd.exe")
}

func TestNoRulesIsDisabled(t *testing.T) {
	r := New(&config.Config{}, t.TempDir())
	if r.Enabled() {
		t.Fatal("empty config should disable diagnostics")
	}
	if out := r.RunAfterChange(context.Background(), "main.go"); out != nil {
		t.Fatalf("outcomes = %+v", out)
	}
}

func TestRulesMatchBaseNameAndPath(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{
		DiagnosticTimeout: 5,
		Diagnostics: []config.DiagnosticRule{
			{Pattern: "*.go", Command: "echo base {file}"},
			{Pattern: "src/**/*.go", Command: "echo path {file}"},
			{Pattern: "*.py", Command: "echo python"},
		},
	}
	r := New(cfg, dir)
	out := r.RunAfterChange(context.Background(), "src/pkg/main.go")
	if len(out) != 2 {
		t.Fatalf("outcomes = %d, want 2: %+v", len(out), out)
	}
	if !strings.Contains(out[0].Output, "base src/pkg/main.go") {
		t.Errorf("base-name rule output = %q", out[0].Output)
	}
	if !strings.Contains(out[1].Output, "path src/pkg/main.go") {
		t.Errorf("path rule output = %q", out[1].Output)
	}
	if out[0].IsError || out[1].IsError {
		t.Fatalf("commands should pass: %+v", out)
	}
}

func TestAbsPlaceholderAndFailure(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{
		DiagnosticTimeout: 5,
		Diagnostics: []config.DiagnosticRule{
			{Pattern: "main.go", Command: `echo "{abs}" && exit 2`},
		},
	}
	r := New(cfg, dir)
	out := r.RunAfterChange(context.Background(), "main.go")
	if len(out) != 1 {
		t.Fatalf("outcomes = %+v", out)
	}
	if !strings.Contains(out[0].Output, filepath.Join(dir, "main.go")) {
		t.Errorf("abs placeholder = %q", out[0].Output)
	}
	if !out[0].IsError {
		t.Fatal("failing command should be marked as an error")
	}
}

func TestNoMatchRunsNothing(t *testing.T) {
	cfg := &config.Config{
		Diagnostics: []config.DiagnosticRule{{Pattern: "*.rs", Command: "echo rust"}},
	}
	r := New(cfg, t.TempDir())
	if out := r.RunAfterChange(context.Background(), "main.go"); len(out) != 0 {
		t.Fatalf("unexpected outcomes: %+v", out)
	}
}

func TestDiagnosticsRunInWorkspace(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "marker.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	command := "cat marker.txt"
	if cmdShell() {
		command = "more marker.txt"
	}
	cfg := &config.Config{
		DiagnosticTimeout: 5,
		Diagnostics: []config.DiagnosticRule{
			{Pattern: "*.go", Command: command},
		},
	}
	r := New(cfg, dir)
	out := r.RunAfterChange(context.Background(), "main.go")
	if len(out) != 1 || out[0].IsError || !strings.Contains(out[0].Output, "x") {
		t.Fatalf("outcomes = %+v", out)
	}
}
