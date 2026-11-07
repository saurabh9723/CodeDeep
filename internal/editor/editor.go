// Package editor opens workspace files in an external editor.
package editor

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"codedeep/internal/config"
	"codedeep/internal/env"
)

// Target is a file (or the workspace root) and an optional 1-based line.
type Target struct {
	Path string
	Line int
}

// Resolve parses ref, which may end in ":<line>", and resolves it inside root.
// An empty ref targets the workspace root.
func Resolve(root, ref string) (Target, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		abs, err := filepath.Abs(root)
		if err != nil {
			return Target{}, err
		}
		return Target{Path: abs}, nil
	}

	line := 0
	if i := strings.LastIndex(ref, ":"); i > 0 {
		if n, err := strconv.Atoi(ref[i+1:]); err == nil && n > 0 && !strings.Contains(ref[i+1:], string(filepath.Separator)) {
			line = n
			ref = ref[:i]
		}
	}

	path := ref
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, filepath.FromSlash(path))
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return Target{}, err
	}
	abs = filepath.Clean(abs)
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return Target{}, err
	}
	rel, err := filepath.Rel(rootAbs, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return Target{}, fmt.Errorf("path is outside the workspace")
	}
	if _, err := os.Stat(abs); err != nil {
		return Target{}, err
	}
	return Target{Path: abs, Line: line}, nil
}

// Open launches the configured editor, falling back to $VISUAL/$EDITOR or a
// VS Code installation on PATH. It returns after the editor starts.
func Open(cfg *config.Config, target Target) error {
	command, args, err := commandFor(cfg, target)
	if err != nil {
		return err
	}
	cmd := exec.Command(command, args...)
	cmd.Dir = cfg.WorkDir
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", command, err)
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

// Command returns the executable and arguments that Open would use. It is
// exported for diagnostics and tests.
func Command(cfg *config.Config, target Target) (string, []string, error) {
	return commandFor(cfg, target)
}

func commandFor(cfg *config.Config, target Target) (string, []string, error) {
	if cfg != nil && cfg.Editor != nil && strings.TrimSpace(cfg.Editor.Command) != "" {
		command := cfg.Editor.Command
		args := expand(cfg.Editor.Args, target)
		if !hasPlaceholder(cfg.Editor.Args, "{file}") {
			args = append(args, target.Path)
		}
		return command, args, nil
	}

	vars := env.Get()
	editor := strings.TrimSpace(vars.Visual)
	if editor == "" {
		editor = strings.TrimSpace(vars.Editor)
	}
	if editor != "" {
		fields := strings.Fields(editor)
		command := fields[0]
		args := append([]string(nil), fields[1:]...)
		if target.Line > 0 {
			args = append(args, "+"+strconv.Itoa(target.Line))
		}
		return command, append(args, target.Path), nil
	}

	if path, err := exec.LookPath("code"); err == nil {
		targetArg := target.Path
		if target.Line > 0 {
			targetArg += ":" + strconv.Itoa(target.Line)
		}
		return path, []string{"-g", targetArg}, nil
	}
	return "", nil, fmt.Errorf("no editor configured; set editor.command/editor.args in the config or $EDITOR")
}

func expand(template []string, target Target) []string {
	out := make([]string, 0, len(template))
	for _, arg := range template {
		arg = strings.ReplaceAll(arg, "{file}", target.Path)
		arg = strings.ReplaceAll(arg, "{line}", strconv.Itoa(target.Line))
		out = append(out, arg)
	}
	return out
}

func hasPlaceholder(template []string, placeholder string) bool {
	for _, arg := range template {
		if strings.Contains(arg, placeholder) {
			return true
		}
	}
	return false
}
