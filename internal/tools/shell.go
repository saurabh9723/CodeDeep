package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"codedeep/internal/util"
)

type bashArgs struct {
	Command    string `json:"command"`
	TimeoutSec int    `json:"timeout_sec"`
	Background bool   `json:"background"`
}

func (f *FS) runShell(ctx context.Context, shell string, bg BackgroundStarter, raw json.RawMessage) (Result, error) {
	var a bashArgs
	if err := decodeArgs(raw, &a); err != nil {
		return Result{}, err
	}
	if strings.TrimSpace(a.Command) == "" {
		return Result{}, errors.New("bash: command is required")
	}
	if a.Background {
		if bg == nil {
			return Result{}, errors.New("bash: background jobs are not available")
		}
		id, err := bg.Start(a.Command)
		if err != nil {
			return Result{}, err
		}
		return Result{Output: fmt.Sprintf("Started %s in the background: %s\nUse job_output to read its output and job_kill to stop it.", id, a.Command)}, nil
	}
	timeout := a.TimeoutSec
	if timeout <= 0 {
		timeout = 120
	}
	if timeout > 900 {
		timeout = 900
	}

	runCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()

	shellPath, shellArgs := shellFor(shell, a.Command)
	cmd := exec.CommandContext(runCtx, shellPath, shellArgs...)
	cmd.Dir = f.Root
	cmd.Env = os.Environ()

	start := time.Now()
	out, err := cmd.CombinedOutput()
	elapsed := time.Since(start)
	timedOut := errors.Is(runCtx.Err(), context.DeadlineExceeded)

	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else if !timedOut {
			return Result{}, fmt.Errorf("bash: failed to start command: %w", err)
		}
	}
	if timedOut {
		exitCode = -1
	}

	output := util.TruncateMiddle(string(out), 128*1024)
	var b strings.Builder
	fmt.Fprintf(&b, "$ %s\n", a.Command)
	if strings.TrimSpace(output) == "" {
		b.WriteString("(no output)\n")
	} else {
		b.WriteString(output)
		if !strings.HasSuffix(output, "\n") {
			b.WriteString("\n")
		}
	}
	if timedOut {
		fmt.Fprintf(&b, "[timed out after %ds; partial output]", timeout)
	} else {
		fmt.Fprintf(&b, "[exit %d in %s]", exitCode, util.HumanDuration(elapsed))
	}
	return Result{Output: b.String(), IsError: exitCode != 0 || timedOut}, nil
}

// ShellCommand returns the executable and arguments needed to run command
// through the configured shell.
func ShellCommand(configured, command string) (string, []string) {
	return shellFor(configured, command)
}

func shellFor(configured, command string) (string, []string) {
	if configured != "" {
		base := strings.ToLower(filepath.Base(configured))
		switch {
		case strings.Contains(base, "powershell"), strings.Contains(base, "pwsh"):
			return configured, []string{"-NoProfile", "-Command", command}
		case base == "cmd", base == "cmd.exe":
			return configured, []string{"/C", command}
		default:
			return configured, []string{"-c", command}
		}
	}
	if runtime.GOOS == "windows" {
		if p, err := exec.LookPath("bash"); err == nil {
			return p, []string{"-c", command}
		}
		return "cmd.exe", []string{"/C", command}
	}
	return "/bin/sh", []string{"-c", command}
}
