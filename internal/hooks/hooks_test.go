package hooks

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codedeep/internal/config"
	"codedeep/internal/tools"
)

func cmdShell() bool {
	shell, _ := tools.ShellCommand("", "x")
	return strings.EqualFold(filepath.Base(shell), "cmd.exe")
}

func TestRunPreMatchesTools(t *testing.T) {
	cfg := &config.Config{
		Hooks: config.Hooks{PreTool: []config.Hook{
			{Command: "echo matched", TimeoutSec: 5, Tools: []string{"bash"}},
			{Command: "echo skipped", TimeoutSec: 5, Tools: []string{"write_file"}},
		}},
	}
	r := New(cfg, t.TempDir())
	blocked, out := r.RunPre(context.Background(), "bash", json.RawMessage(`{"command":"ls"}`), "sess-1")
	if blocked {
		t.Fatal("hook should not block")
	}
	if len(out) != 1 {
		t.Fatalf("outcomes = %d, want 1", len(out))
	}
	if out[0].Phase != "pre_tool" || out[0].Tool != "bash" || !strings.Contains(out[0].Output, "matched") {
		t.Fatalf("unexpected outcome: %+v", out[0])
	}
	if out[0].IsError {
		t.Fatalf("hook should succeed: %+v", out[0])
	}
}

func TestPreHookBlocksOnFailure(t *testing.T) {
	cfg := &config.Config{
		Hooks: config.Hooks{PreTool: []config.Hook{
			{Command: "exit 3", TimeoutSec: 5, BlockOnFailure: true},
		}},
	}
	r := New(cfg, t.TempDir())
	blocked, out := r.RunPre(context.Background(), "bash", nil, "")
	if !blocked {
		t.Fatal("failing hook with block_on_failure should block")
	}
	if len(out) != 1 || !out[0].IsError || !out[0].Blocked {
		t.Fatalf("unexpected outcomes: %+v", out)
	}
}

func TestPostHooksRunOnErrorAndNeverBlock(t *testing.T) {
	cfg := &config.Config{
		Hooks: config.Hooks{PostTool: []config.Hook{
			{Command: "exit 1", TimeoutSec: 5, BlockOnFailure: true},
		}},
	}
	r := New(cfg, t.TempDir())
	out := r.RunPost(context.Background(), "bash", nil, "", "boom", true)
	if len(out) != 1 || !out[0].IsError || out[0].Blocked {
		t.Fatalf("unexpected outcomes: %+v", out)
	}
}

func TestHookReceivesPayloadAndEnvironment(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{
		Hooks: config.Hooks{PreTool: []config.Hook{
			{Command: "echo $CODEDEEP_TOOL > env.txt", TimeoutSec: 5},
		}},
	}
	if cmdShell() {
		cfg.Hooks.PreTool[0].Command = "echo %CODEDEEP_TOOL% > env.txt"
	}
	r := New(cfg, dir)
	blocked, out := r.RunPre(context.Background(), "edit_file", json.RawMessage(`{"path":"a.go"}`), "sess-9")
	if blocked || len(out) != 1 || out[0].IsError {
		t.Fatalf("hook failed: %+v", out)
	}
	data, err := os.ReadFile(filepath.Join(dir, "env.txt"))
	if err != nil {
		t.Fatalf("env file: %v", err)
	}
	if strings.TrimSpace(string(data)) != "edit_file" {
		t.Fatalf("CODEDEEP_TOOL = %q", strings.TrimSpace(string(data)))
	}
}

func TestHookReadsJSONPayloadFromStdin(t *testing.T) {
	dir := t.TempDir()
	command := "cat > payload.json"
	if cmdShell() {
		command = "more > payload.json"
	}
	cfg := &config.Config{
		Hooks: config.Hooks{PreTool: []config.Hook{{Command: command, TimeoutSec: 5}}},
	}
	r := New(cfg, dir)
	blocked, out := r.RunPre(context.Background(), "bash", json.RawMessage(`{"command":"echo hi"}`), "sess-2")
	if blocked || len(out) != 1 || out[0].IsError {
		t.Fatalf("hook failed: %+v", out)
	}
	data, err := os.ReadFile(filepath.Join(dir, "payload.json"))
	if err != nil {
		t.Fatalf("payload file: %v", err)
	}
	var payload Payload
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("payload JSON: %v (%q)", err, string(data))
	}
	if payload.Event != "pre_tool" || payload.Tool != "bash" || payload.SessionID != "sess-2" || payload.WorkDir != dir {
		t.Fatalf("payload = %+v", payload)
	}
	if !strings.Contains(string(payload.Args), "echo hi") {
		t.Fatalf("payload args = %s", payload.Args)
	}
}

func TestHookTimeout(t *testing.T) {
	command := "sleep 5"
	if cmdShell() {
		command = "ping -n 6 127.0.0.1 >nul"
	}
	cfg := &config.Config{
		Hooks: config.Hooks{PreTool: []config.Hook{{Command: command, TimeoutSec: 1, BlockOnFailure: true}}},
	}
	r := New(cfg, t.TempDir())
	start := time.Now()
	blocked, out := r.RunPre(context.Background(), "bash", nil, "")
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Fatalf("hook was not cancelled: %s", elapsed)
	}
	if !blocked || len(out) != 1 || !out[0].IsError {
		t.Fatalf("timeout should block: %+v", out)
	}
	if !strings.Contains(strings.ToLower(out[0].Output), "timed out") {
		t.Fatalf("output = %q", out[0].Output)
	}
}

func TestZeroTimeoutUsesDefault(t *testing.T) {
	cfg := &config.Config{Hooks: config.Hooks{PreTool: []config.Hook{{Command: "echo ok"}}}}
	r := New(cfg, t.TempDir())
	blocked, out := r.RunPre(context.Background(), "bash", nil, "")
	if blocked || len(out) != 1 || out[0].IsError {
		t.Fatalf("zero timeout should fall back to the default: %+v", out)
	}
}

func TestEmptyConfigIsInert(t *testing.T) {
	r := New(&config.Config{}, t.TempDir())
	if r.HasPre() || r.HasPost() {
		t.Fatal("no hooks should be configured")
	}
	blocked, out := r.RunPre(context.Background(), "bash", nil, "")
	if blocked || out != nil {
		t.Fatalf("expected no outcomes, got %+v", out)
	}
	if got := r.RunPost(context.Background(), "bash", nil, "", "", false); got != nil {
		t.Fatalf("expected no outcomes, got %+v", got)
	}
}
