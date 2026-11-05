// Package hooks runs user-configured shell commands around tool calls.
package hooks

import (
	"bytes"
	"context"
	"encoding/json"
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

const maxOutput = 32 * 1024

// Payload is written to the hook's stdin as a single JSON object.
type Payload struct {
	Event     string          `json:"event"`
	Tool      string          `json:"tool"`
	Args      json.RawMessage `json:"args,omitempty"`
	SessionID string          `json:"session_id,omitempty"`
	WorkDir   string          `json:"cwd"`
	Result    string          `json:"result,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

// Outcome records one hook invocation for events and error reporting.
type Outcome struct {
	Phase   string
	Tool    string
	Command string
	Output  string
	IsError bool
	Blocked bool
	Elapsed time.Duration
}

// Runner executes hooks for one workspace.
type Runner struct {
	cfg  *config.Config
	root string
}

func New(cfg *config.Config, root string) *Runner {
	return &Runner{cfg: cfg, root: root}
}

func (r *Runner) HasPre() bool  { return r != nil && r.cfg != nil && len(r.cfg.Hooks.PreTool) > 0 }
func (r *Runner) HasPost() bool { return r != nil && r.cfg != nil && len(r.cfg.Hooks.PostTool) > 0 }

// RunPre executes matching pre-tool hooks. The boolean is true when a hook
// failed and asked to block the tool call.
func (r *Runner) RunPre(ctx context.Context, tool string, args json.RawMessage, sessionID string) (bool, []Outcome) {
	return r.run(ctx, "pre_tool", r.cfg.Hooks.PreTool, tool, args, sessionID, "", false)
}

// RunPost executes matching post-tool hooks. Failures are reported but never
// block or undo the tool.
func (r *Runner) RunPost(ctx context.Context, tool string, args json.RawMessage, sessionID, result string, isErr bool) []Outcome {
	_, outcomes := r.run(ctx, "post_tool", r.cfg.Hooks.PostTool, tool, args, sessionID, result, isErr)
	return outcomes
}

func (r *Runner) run(ctx context.Context, phase string, list []config.Hook, tool string, args json.RawMessage, sessionID, result string, isErr bool) (bool, []Outcome) {
	if r == nil || r.cfg == nil || len(list) == 0 {
		return false, nil
	}
	payload := Payload{
		Event:     phase,
		Tool:      tool,
		Args:      args,
		SessionID: sessionID,
		WorkDir:   r.root,
		Result:    util.TruncateMiddle(result, maxOutput),
		IsError:   isErr,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return false, []Outcome{{Phase: phase, Tool: tool, Output: "encode payload: " + err.Error(), IsError: true}}
	}

	var outcomes []Outcome
	blocked := false
	for _, h := range list {
		if !hookMatches(h, tool) {
			continue
		}
		start := time.Now()
		output, runErr := r.exec(ctx, phase, tool, h, data, sessionID)
		out := Outcome{
			Phase:   phase,
			Tool:    tool,
			Command: h.Command,
			Output:  output,
			IsError: runErr != nil,
			Elapsed: time.Since(start),
		}
		if runErr != nil && phase == "pre_tool" && h.BlockOnFailure {
			blocked = true
			out.Blocked = true
			out.Output = strings.TrimSpace(out.Output + "\n" + runErr.Error())
			outcomes = append(outcomes, out)
			break
		}
		outcomes = append(outcomes, out)
	}
	return blocked, outcomes
}

func hookMatches(h config.Hook, tool string) bool {
	if len(h.Tools) == 0 {
		return true
	}
	for _, name := range h.Tools {
		if name == tool {
			return true
		}
	}
	return false
}

func (r *Runner) exec(ctx context.Context, phase, tool string, h config.Hook, payload []byte, sessionID string) (string, error) {
	timeout := h.TimeoutSec
	if timeout <= 0 {
		timeout = 10
	}
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()

	shell, shellArgs := tools.ShellCommand(r.cfg.Shell, h.Command)
	cmd := exec.CommandContext(runCtx, shell, shellArgs...)
	cmd.Dir = r.root
	cmd.Env = append(os.Environ(),
		"CODEDEEP_HOOK_EVENT="+phase,
		"CODEDEEP_TOOL="+tool,
		"CODEDEEP_SESSION_ID="+sessionID,
		"CODEDEEP_WORKDIR="+r.root,
	)
	cmd.Stdin = bytes.NewReader(payload)
	proc.Prepare(cmd)

	start := time.Now()
	out, err := cmd.CombinedOutput()
	elapsed := time.Since(start)
	text := util.TruncateMiddle(string(out), maxOutput)
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		err = fmt.Errorf("hook timed out after %ds", timeout)
	}
	if err != nil {
		if text == "" {
			text = err.Error()
		}
		if el, ok := err.(*exec.ExitError); ok {
			err = fmt.Errorf("exit %d after %s: %s", el.ExitCode(), util.HumanDuration(elapsed), strings.TrimSpace(text))
		}
	}
	return strings.TrimSpace(text), err
}
