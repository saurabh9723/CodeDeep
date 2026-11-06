package verify

import (
	"context"
	"runtime"
	"strings"
	"testing"

	"codedeep/internal/config"
)

func newRunner(t *testing.T, command string, timeoutSec int) *Runner {
	t.Helper()
	cfg := &config.Config{WorkDir: t.TempDir()}
	cfg.Verify = config.Verify{Command: command, TimeoutSec: timeoutSec}
	return New(cfg, cfg.WorkDir)
}

func TestRunnerDisabledWithoutCommand(t *testing.T) {
	r := newRunner(t, "   ", 0)
	if r.Enabled() {
		t.Error("runner should be disabled without a command")
	}
	if got := New(nil, ""); got.Enabled() {
		t.Error("nil config runner should be disabled")
	}
}

func TestRunnerSucceeds(t *testing.T) {
	r := newRunner(t, "echo verified-ok", 0)
	if !r.Enabled() {
		t.Fatal("runner should be enabled")
	}
	if got := r.Command(); got != "echo verified-ok" {
		t.Errorf("Command() = %q", got)
	}
	res := r.Run(context.Background())
	if res.IsError {
		t.Fatalf("Run failed: %+v", res)
	}
	if !strings.Contains(res.Output, "verified-ok") {
		t.Errorf("output = %q", res.Output)
	}
	if res.Command != "echo verified-ok" {
		t.Errorf("result command = %q", res.Command)
	}
	if res.Elapsed < 0 {
		t.Errorf("elapsed = %v", res.Elapsed)
	}
}

func TestRunnerReportsFailure(t *testing.T) {
	r := newRunner(t, "echo broken && exit 3", 0)
	res := r.Run(context.Background())
	if !res.IsError {
		t.Fatalf("Run succeeded, want failure: %+v", res)
	}
	if !strings.Contains(res.Output, "broken") {
		t.Errorf("output = %q", res.Output)
	}
}

func TestRunnerHonorsTimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix sleep is not guaranteed in the fallback Windows shell")
	}
	r := newRunner(t, "sleep 5", 1)
	res := r.Run(context.Background())
	if !res.IsError {
		t.Fatalf("Run succeeded, want a timeout: %+v", res)
	}
	if !strings.Contains(res.Output, "timed out") {
		t.Errorf("output = %q, want a timeout message", res.Output)
	}
}
