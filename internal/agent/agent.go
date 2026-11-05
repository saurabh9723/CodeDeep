package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"codedeep/internal/approval"
	"codedeep/internal/config"
	"codedeep/internal/llm"
	"codedeep/internal/session"
	"codedeep/internal/tools"
	"codedeep/internal/util"
)

const MaxSubagentDepth = 2

var approvalCounter atomic.Int64

func newApprovalID() string {
	return fmt.Sprintf("ap-%d-%d", time.Now().UnixNano(), approvalCounter.Add(1))
}

type Options struct {
	Config   *config.Config
	Client   *llm.Client
	Registry *tools.Registry
	Sessions *session.Store
	Approver approval.Approver
	Emitter  Emitter
	Depth    int
}

type Agent struct {
	cfg       *config.Config
	client    *llm.Client
	reg       *tools.Registry
	sessions  *session.Store
	approver  approval.Approver
	emit      Emitter
	depth     int
	autoAllow map[string]bool
	usage     llm.Usage
}

func New(opts Options) *Agent {
	depth := opts.Depth
	if depth < 0 {
		depth = 0
	}
	a := &Agent{
		cfg:       opts.Config,
		client:    opts.Client,
		reg:       opts.Registry.CloneWithout("agent"),
		sessions:  opts.Sessions,
		approver:  opts.Approver,
		emit:      opts.Emitter,
		depth:     depth,
		autoAllow: map[string]bool{},
	}
	a.registerSubagentTool()
	return a
}

// Run executes one user turn, including every tool call the model requests,
// and persists the session as it goes.
func (a *Agent) Run(ctx context.Context, sess *session.Session, prompt string) error {
	if sess == nil {
		return fmt.Errorf("nil session")
	}
	if strings.TrimSpace(prompt) == "" {
		return fmt.Errorf("empty prompt")
	}
	sess.Messages = append(sess.Messages, llm.Message{Role: "user", Content: prompt})
	sess.Turns++
	a.emit.Emit(Event{Type: EventUser, SessionID: sess.ID, Text: prompt})
	a.save(sess)

	system := BuildSystemPrompt(a.cfg, a.depth)
	_, err := a.runLoop(ctx, &sess.Messages, system, sess, a.cfg.MaxSteps)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			a.emit.Emit(Event{
				Type:       EventDone,
				SessionID:  sess.ID,
				Depth:      a.depth,
				Message:    "stopped",
				Usage:      &a.usage,
				TotalUsage: &sess.Usage,
			})
		} else {
			a.emit.Emit(Event{Type: EventError, SessionID: sess.ID, Depth: a.depth, Message: err.Error()})
		}
		a.save(sess)
		return err
	}
	a.save(sess)
	a.emit.Emit(Event{
		Type:       EventDone,
		SessionID:  sess.ID,
		Depth:      a.depth,
		Usage:      &a.usage,
		TotalUsage: &sess.Usage,
	})
	return nil
}

func (a *Agent) runLoop(ctx context.Context, msgs *[]llm.Message, system string, sess *session.Session, maxSteps int) (string, error) {
	if maxSteps <= 0 {
		maxSteps = 60
	}
	lastText := ""
	compactedRetry := false

	for step := 0; step < maxSteps; step++ {
		if err := ctx.Err(); err != nil {
			return lastText, err
		}
		a.maybeCompact(ctx, msgs, system, sess)

		requestMsgs := make([]llm.Message, 0, len(*msgs)+1)
		requestMsgs = append(requestMsgs, llm.Message{Role: "system", Content: system})
		requestMsgs = append(requestMsgs, *msgs...)

		a.emit.Emit(Event{Type: EventAssistantStart, SessionID: sessionID(sess), Depth: a.depth})
		resp, err := a.client.Stream(ctx, llm.ChatRequest{
			Model:       a.cfg.Model,
			Messages:    requestMsgs,
			Tools:       a.reg.Definitions(),
			MaxTokens:   a.cfg.MaxTokens,
			Temperature: a.cfg.Temperature,
		}, func(d llm.Delta) error {
			if d.Reasoning != "" {
				a.emit.Emit(Event{Type: EventReasoning, SessionID: sessionID(sess), Depth: a.depth, Text: d.Reasoning})
			}
			if d.Content != "" {
				a.emit.Emit(Event{Type: EventText, SessionID: sessionID(sess), Depth: a.depth, Text: d.Content})
			}
			return nil
		})
		if err != nil {
			if isContextError(err) && !compactedRetry {
				compactedRetry = true
				estimate := llm.EstimatedTokens(requestMsgs) + len(system)/4
				a.compact(ctx, msgs, sess, 4, estimate)
				continue
			}
			if resp != nil && strings.TrimSpace(resp.Message.Content) != "" {
				*msgs = append(*msgs, llm.Message{
					Role:      "assistant",
					Content:   resp.Message.Content,
					Reasoning: resp.Message.Reasoning,
				})
			}
			a.save(sess)
			return lastText, err
		}
		compactedRetry = false

		usage := resp.Usage
		if usage.TotalTokens == 0 {
			promptEst := llm.EstimatedTokens(requestMsgs)
			completionEst := llm.EstimatedTokens([]llm.Message{resp.Message})
			usage = llm.Usage{
				PromptTokens:     promptEst,
				CompletionTokens: completionEst,
				TotalTokens:      promptEst + completionEst,
			}
		}
		a.usage = a.usage.Add(usage)
		if sess != nil {
			sess.Usage = sess.Usage.Add(usage)
		}
		a.emit.Emit(Event{
			Type:       EventUsage,
			SessionID:  sessionID(sess),
			Depth:      a.depth,
			Usage:      &usage,
			TotalUsage: a.totalUsage(sess),
		})

		assistant := resp.Message
		assistant.Role = "assistant"
		for i := range assistant.ToolCalls {
			if strings.TrimSpace(assistant.ToolCalls[i].ID) == "" {
				assistant.ToolCalls[i].ID = fmt.Sprintf("call_%d_%d", step, i)
			}
		}
		*msgs = append(*msgs, assistant)
		if assistant.Content != "" {
			lastText = assistant.Content
		}
		a.save(sess)

		if len(assistant.ToolCalls) == 0 {
			if assistant.Content == "" && assistant.Reasoning == "" {
				return lastText, fmt.Errorf("the model returned an empty response")
			}
			return lastText, nil
		}

		for i, tc := range assistant.ToolCalls {
			if err := ctx.Err(); err != nil {
				for _, rest := range assistant.ToolCalls[i:] {
					*msgs = append(*msgs, llm.Message{
						Role:       "tool",
						ToolCallID: rest.ID,
						Content:    "Error: cancelled by user.",
					})
				}
				a.save(sess)
				return lastText, err
			}
			out := a.executeTool(ctx, tc, sess)
			*msgs = append(*msgs, llm.Message{
				Role:       "tool",
				ToolCallID: tc.ID,
				Content:    out,
			})
			a.save(sess)
		}
	}

	msg := fmt.Sprintf("stopped after %d steps without finishing", maxSteps)
	a.emit.Emit(Event{Type: EventError, SessionID: sessionID(sess), Depth: a.depth, Message: msg})
	return lastText, fmt.Errorf("%s", msg)
}

func (a *Agent) executeTool(ctx context.Context, tc llm.ToolCall, sess *session.Session) string {
	name := tc.Function.Name
	raw := json.RawMessage(tc.Function.Arguments)
	if len(strings.TrimSpace(string(raw))) == 0 {
		raw = json.RawMessage("{}")
	}
	eventArgs := raw
	if !json.Valid(eventArgs) {
		encoded, _ := json.Marshal(tc.Function.Arguments)
		eventArgs = encoded
	}

	a.emit.Emit(Event{
		Type:      EventToolStart,
		SessionID: sessionID(sess),
		Depth:     a.depth,
		Name:      name,
		Args:      eventArgs,
		CallID:    tc.ID,
	})

	finish := func(output string, isErr bool) string {
		if strings.TrimSpace(output) == "" {
			output = "(no output)"
		}
		modelOutput := util.TruncateMiddle(output, 200*1024)
		a.emit.Emit(Event{
			Type:      EventToolEnd,
			SessionID: sessionID(sess),
			Depth:     a.depth,
			Name:      name,
			Output:    util.TruncateMiddle(modelOutput, 64*1024),
			IsError:   isErr,
			CallID:    tc.ID,
		})
		return modelOutput
	}

	if !json.Valid(raw) {
		return finish(fmt.Sprintf("Error: invalid JSON arguments for %s: %s", name, util.Truncate(tc.Function.Arguments, 300)), true)
	}
	spec, ok := a.reg.Get(name)
	if !ok {
		return finish(fmt.Sprintf("Error: unknown tool %q. Available tools: %s", name, a.toolNames()), true)
	}

	decision, err := a.decide(ctx, spec, tc, sess)
	if err != nil {
		return finish("Error: approval failed: "+err.Error(), true)
	}
	if decision == approval.Deny {
		if a.cfg.Approval == "readonly" {
			return finish("Error: "+name+" is blocked by read-only approval mode. Do not retry.", true)
		}
		return finish("Error: permission denied for "+name+". Do not retry this call; change approach or explain what you need.", true)
	}

	result, runErr := spec.Run(ctx, raw)
	if runErr != nil {
		return finish("Error: "+runErr.Error(), true)
	}
	return finish(result.Output, result.IsError)
}

func (a *Agent) decide(ctx context.Context, spec *tools.Spec, tc llm.ToolCall, sess *session.Session) (approval.Decision, error) {
	if spec.ReadOnly {
		return approval.AllowOnce, nil
	}
	switch a.cfg.Approval {
	case "auto":
		return approval.AllowOnce, nil
	case "readonly":
		return approval.Deny, nil
	}
	if a.autoAllow[spec.Name] {
		return approval.AllowOnce, nil
	}
	if a.approver == nil {
		return approval.Deny, nil
	}
	req := approval.Request{
		ID:       newApprovalID(),
		Tool:     spec.Name,
		Args:     json.RawMessage(tc.Function.Arguments),
		ReadOnly: spec.ReadOnly,
		Depth:    a.depth,
	}
	a.emit.Emit(Event{
		Type:       EventApproval,
		SessionID:  sessionID(sess),
		Depth:      a.depth,
		ApprovalID: req.ID,
		Name:       spec.Name,
		Args:       req.Args,
		ReadOnly:   spec.ReadOnly,
	})
	dec, err := a.approver.Approve(ctx, req)
	if err != nil {
		a.emit.Emit(Event{
			Type:       EventApprovalResult,
			SessionID:  sessionID(sess),
			Depth:      a.depth,
			ApprovalID: req.ID,
			Name:       spec.Name,
			IsError:    true,
			Message:    "error: " + err.Error(),
		})
		return approval.Deny, err
	}
	if dec == approval.AllowAlways {
		a.autoAllow[spec.Name] = true
	}
	a.emit.Emit(Event{
		Type:       EventApprovalResult,
		SessionID:  sessionID(sess),
		Depth:      a.depth,
		ApprovalID: req.ID,
		Name:       spec.Name,
		Message:    dec.String(),
	})
	return dec, nil
}

func (a *Agent) toolNames() string {
	specs := a.reg.List()
	names := make([]string, 0, len(specs))
	for _, s := range specs {
		names = append(names, s.Name)
	}
	return strings.Join(names, ", ")
}

func (a *Agent) save(sess *session.Session) {
	if sess == nil || a.sessions == nil {
		return
	}
	if err := a.sessions.Save(sess); err != nil {
		a.emit.Emit(Event{Type: EventError, SessionID: sess.ID, Depth: a.depth, Message: "session save failed: " + err.Error()})
	}
}

func (a *Agent) totalUsage(sess *session.Session) *llm.Usage {
	if sess != nil {
		return &sess.Usage
	}
	return &a.usage
}

func sessionID(sess *session.Session) string {
	if sess == nil {
		return ""
	}
	return sess.ID
}
