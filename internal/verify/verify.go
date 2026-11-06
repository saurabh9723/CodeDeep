// Package verify runs the configured end-of-turn verification command.
package verify

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"codedeep/internal/config"
	"codedeep/internal/proc"
	"codedeep/internal/tools"
	"codedeep/internal/util"
)

const (
	maxOutput      = 16 << 10
	defaultTimeout = 300 * time.Second
	maxTimeout     = 900 * time.Second
)

// Result is the outcome of one verification run.
type Result struct {
	Command string
	Output  string
	IsError bool
	Elapsed time.Duration
}

// Runner executes the verify command in a workspace.
type Runner struct {
	cfg  *config.Config
	root string
}

func New(cfg *config.Config, root string) *Runner {
	return &Runner{cfg: cfg, root: root}
}

// Enabled reports whether a verification command is configured.
func (r *Runner) Enabled() bool {
	return r != nil && r.cfg != nil && strings.TrimSpace(r.cfg.Verify.Command) != ""
}

// Command returns the configured command.
func (r *Runner) Command() string {
	if r == nil || r.cfg == nil {
		return ""
	}
	return strings.TrimSpace(r.cfg.Verify.Command)
}

// Run executes the verification command and captures its combined output.
func (r *Runner) Run(ctx context.Context) Result {
	command := r.Command()
	if command == "" {
		return Result{Command: command, Output: "no verification command configured", IsError: true}
	}
	timeout := time.Duration(r.cfg.Verify.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	if timeout > maxTimeout {
		timeout = maxTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	shell, args := tools.ShellCommand(r.cfg.Shell, command)
	cmd := exec.CommandContext(runCtx, shell, args...)
	cmd.Dir = r.root
	cmd.Env = os.Environ()
	proc.Prepare(cmd)

	start := time.Now()
	out, err := cmd.CombinedOutput()
	elapsed := time.Since(start)
	text := strings.TrimSpace(util.TruncateMiddle(string(out), maxOutput))
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		err = fmt.Errorf("verification timed out after %s", timeout)
	}
	if err != nil && text == "" {
		text = err.Error()
	}
	return Result{Command: command, Output: text, IsError: err != nil, Elapsed: elapsed}
}
