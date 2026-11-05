// Package diagnostics runs build, vet, or lint commands after file edits.
package diagnostics

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"codedeep/internal/config"
	"codedeep/internal/proc"
	"codedeep/internal/tools"
	"codedeep/internal/util"
)

const maxOutput = 16 * 1024

// Outcome is the output of one diagnostic command.
type Outcome struct {
	Pattern string
	Command string
	Output  string
	IsError bool
	Elapsed time.Duration
}

type Runner struct {
	cfg  *config.Config
	root string
}

func New(cfg *config.Config, root string) *Runner {
	return &Runner{cfg: cfg, root: root}
}

func (r *Runner) Enabled() bool {
	return r != nil && r.cfg != nil && len(r.cfg.Diagnostics) > 0
}

// RunAfterChange executes every rule matching the changed workspace-relative
// path. Diagnostics are best effort: a failing command reports its output but
// never fails the edit.
func (r *Runner) RunAfterChange(ctx context.Context, rel string) []Outcome {
	if !r.Enabled() {
		return nil
	}
	rel = filepath.ToSlash(strings.TrimSpace(rel))
	if rel == "" {
		return nil
	}
	abs := filepath.Join(r.root, filepath.FromSlash(rel))
	timeout := time.Duration(r.cfg.DiagnosticTimeout) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	var outcomes []Outcome
	for _, rule := range r.cfg.Diagnostics {
		if !match(rule.Pattern, rel) {
			continue
		}
		command := strings.NewReplacer("{file}", rel, "{abs}", abs).Replace(rule.Command)
		start := time.Now()
		output, err := r.exec(ctx, command, timeout)
		out := Outcome{
			Pattern: rule.Pattern,
			Command: command,
			Output:  output,
			IsError: err != nil,
			Elapsed: time.Since(start),
		}
		if err != nil && strings.TrimSpace(out.Output) == "" {
			out.Output = err.Error()
		}
		outcomes = append(outcomes, out)
	}
	return outcomes
}

func match(pattern, rel string) bool {
	pattern = strings.TrimSpace(filepath.ToSlash(pattern))
	if pattern == "" {
		return false
	}
	if !strings.Contains(pattern, "/") {
		return tools.Match(pattern, filepath.Base(rel))
	}
	return tools.Match(pattern, rel)
}

func (r *Runner) exec(ctx context.Context, command string, timeout time.Duration) (string, error) {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	shell, shellArgs := tools.ShellCommand(r.cfg.Shell, command)
	cmd := exec.CommandContext(runCtx, shell, shellArgs...)
	cmd.Dir = r.root
	cmd.Env = os.Environ()
	proc.Prepare(cmd)
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(util.TruncateMiddle(string(out), maxOutput))
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		err = errors.New("diagnostics timed out")
	}
	if err != nil && text == "" {
		text = err.Error()
	}
	return text, err
}
