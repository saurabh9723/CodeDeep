package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"codedeep/internal/llm"
	"codedeep/internal/tools"
	"codedeep/internal/util"
)

type subagentArgs struct {
	Description string `json:"description"`
	Prompt      string `json:"prompt"`
}

func (a *Agent) registerSubagentTool() {
	if a.depth >= MaxSubagentDepth {
		return
	}
	a.reg.Register(&tools.Spec{
		Name: "agent",
		Description: "Spawn a subagent to handle a self-contained task such as exploring a large area, " +
			"researching several files, or verifying a hypothesis. The subagent has the same tools " +
			"(except this one at the deepest level), works independently, and returns a concise report. " +
			"Provide a complete, self-contained prompt because it does not see this conversation.",
		Params: tools.Object(map[string]any{
			"description": tools.StrProp("Short 3-6 word description of the subagent's task."),
			"prompt":      tools.StrProp("Complete instructions and all context the subagent needs."),
		}, "description", "prompt"),
		Run: a.runSubagentTool,
	})
}

func (a *Agent) runSubagentTool(ctx context.Context, raw json.RawMessage) (tools.Result, error) {
	var args subagentArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return tools.Result{}, fmt.Errorf("invalid arguments: %w", err)
	}
	if strings.TrimSpace(args.Prompt) == "" {
		return tools.Result{}, fmt.Errorf("prompt is required")
	}
	if args.Description == "" {
		args.Description = "subagent"
	}
	depth := a.depth + 1

	a.emitEvent(Event{
		Type:    EventSubagentStart,
		Depth:   depth,
		Name:    args.Description,
		Message: util.Truncate(args.Prompt, 2000),
	})

	child := New(Options{
		Config:     a.cfg,
		Client:     a.client,
		Registry:   a.reg,
		FS:         a.fs,
		Sessions:   a.sessions,
		Approver:   a.approver,
		Emitter:    a.emit,
		Depth:      depth,
		Session:    a.session,
		Checkpoint: a.checkpoint,
		Approval:   a.approval,
		Skills:     a.skills,
		Memory:     a.mem,
		Locks:      a.locks,
	})
	msgs := []llm.Message{{Role: "user", Content: args.Prompt}}

	maxSteps := 30
	if a.cfg.MaxSteps > 0 && a.cfg.MaxSteps < maxSteps {
		maxSteps = a.cfg.MaxSteps
	}
	text, err := child.runLoop(ctx, &msgs, nil, maxSteps)
	a.locks.stats.Lock()
	a.usage = a.usage.Add(child.usage)
	a.turnUsage = a.turnUsage.Add(child.usage)
	if a.session != nil {
		a.session.Usage = a.session.Usage.Add(child.usage)
	}
	a.locks.stats.Unlock()

	isErr := err != nil
	report := text
	if err != nil {
		report = "Subagent failed: " + err.Error()
	}
	if strings.TrimSpace(report) == "" {
		report = "(subagent produced no final report)"
	}
	a.emitEvent(Event{
		Type:    EventSubagentEnd,
		Depth:   depth,
		Name:    args.Description,
		Message: util.Truncate(report, 1600),
		IsError: isErr,
	})

	if err != nil {
		return tools.Result{}, fmt.Errorf("subagent failed: %w", err)
	}
	return tools.Result{Output: report}, nil
}
