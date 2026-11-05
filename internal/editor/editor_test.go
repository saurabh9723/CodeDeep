package editor

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"codedeep/internal/config"
)

func TestResolveRelativeAndLine(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "src", "main.go")
	if err := os.WriteFile(file, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	target, err := Resolve(root, "src/main.go:42")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if target.Path != file || target.Line != 42 {
		t.Fatalf("target = %+v", target)
	}
	// The line suffix is not confused with a Windows drive colon or a file
	// that contains colons.
	target, err = Resolve(root, "src/main.go")
	if err != nil || target.Line != 0 {
		t.Fatalf("target = %+v, err = %v", target, err)
	}
}

func TestResolveRootAndMissing(t *testing.T) {
	root := t.TempDir()
	target, err := Resolve(root, "")
	if err != nil {
		t.Fatalf("Resolve root: %v", err)
	}
	if target.Path != root || target.Line != 0 {
		t.Fatalf("target = %+v", target)
	}
	if _, err := Resolve(root, "missing.go:3"); err == nil {
		t.Fatal("missing files should be rejected")
	}
}

func TestResolveRejectsOutside(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(filepath.Dir(root), "outside.txt")
	if err := os.WriteFile(outside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(outside)
	if _, err := Resolve(root, outside); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("expected outside-workspace error, got %v", err)
	}
	if _, err := Resolve(root, "../outside.txt"); err == nil {
		t.Fatal("parent traversal should be rejected")
	}
}

func TestCommandUsesConfiguredEditor(t *testing.T) {
	cfg := &config.Config{
		Editor: &config.Editor{
			Command: "myedit",
			Args:    []string{"--goto", "{file}:{line}", "--wait"},
		},
	}
	command, args, err := Command(cfg, Target{Path: "/work/main.go", Line: 12})
	if err != nil {
		t.Fatal(err)
	}
	if command != "myedit" {
		t.Errorf("command = %q", command)
	}
	want := []string{"--goto", "/work/main.go:12", "--wait"}
	if strings.Join(args, "|") != strings.Join(want, "|") {
		t.Errorf("args = %v", args)
	}
}

func TestCommandAppendsPathWithoutPlaceholder(t *testing.T) {
	cfg := &config.Config{Editor: &config.Editor{Command: "myedit", Args: []string{"--wait"}}}
	_, args, err := Command(cfg, Target{Path: "/work/main.go", Line: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(args) != 2 || args[0] != "--wait" || args[1] != "/work/main.go" {
		t.Fatalf("args = %v", args)
	}
}

func TestCommandFallsBackToEnvEditor(t *testing.T) {
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", "vi -u NONE")
	command, args, err := Command(&config.Config{}, Target{Path: "/work/main.go", Line: 7})
	if err != nil {
		t.Fatal(err)
	}
	if command != "vi" {
		t.Errorf("command = %q", command)
	}
	want := []string{"-u", "NONE", "+7", "/work/main.go"}
	if strings.Join(args, "|") != strings.Join(want, "|") {
		t.Errorf("args = %v", args)
	}
}

func TestCommandFallsBackToVSCode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("LookPath for the code shim is environment dependent on Windows")
	}
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", "")
	path, err := exec.LookPath("code")
	if err != nil {
		t.Skip("code is not installed")
	}
	command, args, err := Command(&config.Config{}, Target{Path: "/work/main.go", Line: 9})
	if err != nil {
		t.Fatal(err)
	}
	if command != path || len(args) != 2 || args[0] != "-g" || args[1] != "/work/main.go:9" {
		t.Fatalf("command = %q args = %v", command, args)
	}
}
