package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"codedeep/internal/approval"
	"codedeep/internal/config"
	"codedeep/internal/diagnostics"
	"codedeep/internal/hooks"
	"codedeep/internal/llm"
	"codedeep/internal/session"
	"codedeep/internal/skills"
	"codedeep/internal/tools"
	"codedeep/internal/util"
)

const (
	MaxSubagentDepth    = 2
	maxCheckpoints      = 20
	maxCheckpointBytes  = 4 << 20
	maxDiffPreviewBytes = 64 << 10
	maxParallelTools    = 4
)

var approvalCounter atomic.Int64

func newApprovalID() string {
	return fmt.Sprintf("ap-%d-%d", time.Now().UnixNano(), approvalCounter.Add(1))
}

type Options struct {
	Config     *config.Config
	Client     llm.Provider
	Registry   *tools.Registry
	FS         *tools.FS
	Sessions   *session.Store
	Approver   approval.Approver
	Emitter    Emitter
	Depth      int
	Session    *session.Session
	Checkpoint *session.Checkpoint
	Approval   string
	Skills     *skills.Catalog
}

type Agent struct {
	cfg       *config.Config
	client    llm.Provider
	reg       *tools.Registry
	fs        *tools.FS
	sessions  *session.Store
	approver  approval.Approver
	emit      Emitter
	hooks     *hooks.Runner
	diag      *diagnostics.Runner
	skills    *skills.Catalog
	emitMu    sync.Mutex
	depth     int
	approval  string
	autoAllow map[string]bool
	usage     llm.Usage
	turnUsage llm.Usage

	session    *session.Session
	checkpoint *session.Checkpoint
	cpMu       sync.Mutex
}

func New(opts Options) *Agent {
	depth := opts.Depth
	if depth < 0 {
		depth = 0
	}
	a := &Agent{
		cfg:        opts.Config,
		client:     opts.Client,
		reg:        opts.Registry.CloneWithout("agent"),
		fs:         opts.FS,
		sessions:   opts.Sessions,
		approver:   opts.Approver,
		emit:       opts.Emitter,
		hooks:      hooks.New(opts.Config, opts.Config.WorkDir),
		diag:       diagnostics.New(opts.Config, opts.Config.WorkDir),
		skills:     opts.Skills,
		depth:      depth,
		approval:   opts.Approval,
		autoAllow:  map[string]bool{},
		session:    opts.Session,
		checkpoint: opts.Checkpoint,
	}
	a.registerSubagentTool()
	return a
}

// emitEvent serializes emission because read-only tool calls can run in
// parallel.
func (a *Agent) emitEvent(ev Event) {
	a.emitMu.Lock()
	defer a.emitMu.Unlock()
	a.emit.Emit(ev)
}

// Run executes one user turn, including every tool call the model requests,
// and persists the session as it goes.
func (a *Agent) Run(ctx context.Context, sess *session.Session, prompt string) error {
	return a.RunWithImages(ctx, sess, prompt, nil)
}

// RunWithImages is Run with user image attachments.
func (a *Agent) RunWithImages(ctx context.Context, sess *session.Session, prompt string, images []llm.Image) error {
	if sess == nil {
		return fmt.Errorf("nil session")
	}
	if strings.TrimSpace(prompt) == "" && len(images) == 0 {
		return fmt.Errorf("empty prompt")
	}
	prompt = strings.TrimSpace(prompt)
	sess.Messages = append(sess.Messages, llm.Message{Role: "user", Content: prompt, Images: images})
	sess.Turns++
	a.session = sess
	a.turnUsage = llm.Usage{}
	a.approval = a.cfg.ApprovalMode()
	sess.Checkpoints = append(sess.Checkpoints, session.Checkpoint{
		Turn: sess.Turns,
		Time: time.Now().UTC(),
	})
	a.checkpoint = &sess.Checkpoints[len(sess.Checkpoints)-1]
	defer a.finishCheckpoint(sess)

	a.emitEvent(Event{Type: EventUser, SessionID: sess.ID, Text: prompt, Images: images})
	a.save(sess)

	system := BuildSystemPrompt(a.cfg, a.depth, a.skills)
	_, err := a.runLoop(ctx, &sess.Messages, system, sess, a.cfg.MaxSteps)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			ev := a.doneEvent(sess, "stopped")
			a.emitEvent(ev)
		} else {
			a.emitEvent(Event{Type: EventError, SessionID: sess.ID, Depth: a.depth, Message: err.Error()})
		}
		a.save(sess)
		return err
	}
	a.save(sess)
	a.emitEvent(a.doneEvent(sess, ""))
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

		a.emitEvent(Event{Type: EventAssistantStart, SessionID: sessionID(sess), Depth: a.depth})
		resp, err := a.client.Stream(ctx, llm.ChatRequest{
			Model:       a.cfg.Model,
			Messages:    requestMsgs,
			Tools:       a.reg.Definitions(),
			MaxTokens:   a.cfg.MaxTokens,
			Temperature: a.cfg.Temperature,
		}, func(d llm.Delta) error {
			if d.Reasoning != "" {
				a.emitEvent(Event{Type: EventReasoning, SessionID: sessionID(sess), Depth: a.depth, Text: d.Reasoning})
			}
			if d.Content != "" {
				a.emitEvent(Event{Type: EventText, SessionID: sessionID(sess), Depth: a.depth, Text: d.Content})
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
		a.turnUsage = a.turnUsage.Add(usage)
		if sess != nil {
			sess.Usage = sess.Usage.Add(usage)
		}
		total := a.totalUsage(sess)
		a.emitEvent(Event{
			Type:       EventUsage,
			SessionID:  sessionID(sess),
			Depth:      a.depth,
			Usage:      &usage,
			TotalUsage: total,
			CostUSD:    a.costOf(usage),
			TotalCost:  a.costOf(*total),
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

		outputs := a.executeToolCalls(ctx, assistant.ToolCalls, sess)
		for i, tc := range assistant.ToolCalls {
			*msgs = append(*msgs, llm.Message{
				Role:       "tool",
				ToolCallID: tc.ID,
				Content:    outputs[i],
			})
		}
		a.save(sess)
		if err := ctx.Err(); err != nil {
			return lastText, err
		}
	}

	msg := fmt.Sprintf("stopped after %d steps without finishing", maxSteps)
	a.emitEvent(Event{Type: EventError, SessionID: sessionID(sess), Depth: a.depth, Message: msg})
	return lastText, fmt.Errorf("%s", msg)
}

// executeToolCalls runs a model turn's tool calls and returns the results in
// call order. Batches that contain only read-only tools run concurrently.
func (a *Agent) executeToolCalls(ctx context.Context, calls []llm.ToolCall, sess *session.Session) []string {
	results := make([]string, len(calls))
	if len(calls) < 2 || !a.allReadOnly(calls) {
		for i, tc := range calls {
			if ctx.Err() != nil {
				results[i] = "Error: cancelled by user."
				continue
			}
			results[i] = a.executeTool(ctx, tc, sess)
		}
		return results
	}

	sem := make(chan struct{}, maxParallelTools)
	var wg sync.WaitGroup
	for i, tc := range calls {
		wg.Add(1)
		go func(i int, tc llm.ToolCall) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				results[i] = "Error: cancelled by user."
				return
			}
			defer func() { <-sem }()
			if ctx.Err() != nil {
				results[i] = "Error: cancelled by user."
				return
			}
			results[i] = a.executeTool(ctx, tc, sess)
		}(i, tc)
	}
	wg.Wait()
	return results
}

func (a *Agent) allReadOnly(calls []llm.ToolCall) bool {
	for _, tc := range calls {
		spec, ok := a.reg.Get(tc.Function.Name)
		if !ok || !spec.ReadOnly {
			return false
		}
		args := strings.TrimSpace(tc.Function.Arguments)
		if args == "" {
			args = "{}"
		}
		if !json.Valid([]byte(args)) {
			return false
		}
	}
	return true
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

	a.emitEvent(Event{
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
		a.emitEvent(Event{
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

	preview := a.previewTool(ctx, spec, raw)
	if preview != nil {
		a.emitEvent(Event{
			Type:      EventDiff,
			SessionID: sessionID(sess),
			Depth:     a.depth,
			Name:      name,
			Path:      preview.Path,
			Diff:      util.TruncateMiddle(preview.Diff, maxDiffPreviewBytes),
			CallID:    tc.ID,
		})
	}

	decision, err := a.decide(ctx, spec, tc, sess, preview)
	if err != nil {
		return finish("Error: approval failed: "+err.Error(), true)
	}
	if decision == approval.Deny {
		switch a.approval {
		case "plan":
			return finish("Error: "+name+" is blocked by plan mode. Use read-only tools to investigate and present a plan instead.", true)
		case "readonly":
			return finish("Error: "+name+" is blocked by read-only approval mode. Do not retry.", true)
		}
		return finish("Error: permission denied for "+name+". Do not retry this call; change approach or explain what you need.", true)
	}

	var prefix strings.Builder
	if a.hooks.HasPre() {
		blocked, outcomes := a.hooks.RunPre(ctx, name, raw, sessionID(sess))
		a.emitHookOutcomes(tc, sess, outcomes)
		for _, out := range outcomes {
			if strings.TrimSpace(out.Output) == "" {
				continue
			}
			if out.IsError {
				fmt.Fprintf(&prefix, "[pre-hook failed: %s] %s\n", out.Command, out.Output)
			} else {
				fmt.Fprintf(&prefix, "[pre-hook] %s\n", out.Output)
			}
		}
		if blocked {
			msg := "Error: " + name + " was blocked by a pre-tool hook."
			if strings.TrimSpace(prefix.String()) != "" {
				msg += "\n" + strings.TrimSpace(prefix.String())
			}
			return finish(msg, true)
		}
	}

	result, runErr := spec.Run(ctx, raw)
	if runErr != nil {
		a.runPostHooks(ctx, tc, raw, sess, "Error: "+runErr.Error(), true)
		return finish(prefix.String()+"Error: "+runErr.Error(), true)
	}
	if result.Change != nil {
		a.recordChange(result.Change)
	}

	output := prefix.String() + result.Output
	if result.Change != nil && a.diag.Enabled() {
		outcomes := a.diag.RunAfterChange(ctx, result.Change.Path)
		a.emitDiagnosticOutcomes(tc, sess, result.Change.Path, outcomes)
		output += formatDiagnostics(outcomes)
	}
	output += a.runPostHooks(ctx, tc, raw, sess, output, result.IsError)
	return finish(output, result.IsError)
}

func (a *Agent) runPostHooks(ctx context.Context, tc llm.ToolCall, raw json.RawMessage, sess *session.Session, output string, isErr bool) string {
	if !a.hooks.HasPost() {
		return ""
	}
	outcomes := a.hooks.RunPost(ctx, tc.Function.Name, raw, sessionID(sess), output, isErr)
	a.emitHookOutcomes(tc, sess, outcomes)
	var b strings.Builder
	for _, out := range outcomes {
		switch {
		case out.IsError:
			fmt.Fprintf(&b, "[post-hook failed: %s] %s\n", out.Command, out.Output)
		case strings.TrimSpace(out.Output) != "":
			fmt.Fprintf(&b, "[post-hook] %s\n", out.Output)
		}
	}
	return b.String()
}

func formatDiagnostics(outcomes []diagnostics.Outcome) string {
	if len(outcomes) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n[diagnostics]\n")
	for _, out := range outcomes {
		status := "ok"
		if out.IsError {
			status = "failed"
		}
		fmt.Fprintf(&b, "$ %s (%s)\n", out.Command, status)
		if strings.TrimSpace(out.Output) != "" {
			b.WriteString(out.Output)
			if !strings.HasSuffix(out.Output, "\n") {
				b.WriteByte('\n')
			}
		}
	}
	return b.String()
}

func (a *Agent) emitHookOutcomes(tc llm.ToolCall, sess *session.Session, outcomes []hooks.Outcome) {
	for _, out := range outcomes {
		text := out.Output
		if out.Command != "" {
			text = "$ " + out.Command + "\n" + text
		}
		a.emitEvent(Event{
			Type:      EventHook,
			SessionID: sessionID(sess),
			Depth:     a.depth,
			Name:      out.Phase,
			Message:   tc.Function.Name,
			Output:    util.TruncateMiddle(text, 8*1024),
			IsError:   out.IsError,
			CallID:    tc.ID,
		})
	}
}

func (a *Agent) emitDiagnosticOutcomes(tc llm.ToolCall, sess *session.Session, path string, outcomes []diagnostics.Outcome) {
	for _, out := range outcomes {
		a.emitEvent(Event{
			Type:      EventDiagnostics,
			SessionID: sessionID(sess),
			Depth:     a.depth,
			Name:      out.Pattern,
			Message:   out.Command,
			Path:      path,
			Output:    util.TruncateMiddle(out.Output, 8*1024),
			IsError:   out.IsError,
			CallID:    tc.ID,
		})
	}
}

func (a *Agent) previewTool(ctx context.Context, spec *tools.Spec, raw json.RawMessage) *tools.Preview {
	if spec == nil || spec.Preview == nil {
		return nil
	}
	switch a.approval {
	case "readonly", "plan":
		return nil
	}
	p, err := spec.Preview(ctx, raw)
	if err != nil || p == nil || strings.TrimSpace(p.Diff) == "" {
		return nil
	}
	return p
}

func (a *Agent) decide(ctx context.Context, spec *tools.Spec, tc llm.ToolCall, sess *session.Session, preview *tools.Preview) (approval.Decision, error) {
	rawArgs := tc.Function.Arguments
	if strings.TrimSpace(rawArgs) == "" {
		rawArgs = "{}"
	}
	rule, ruleSet := a.evaluateRules(spec.Name, json.RawMessage(rawArgs))
	if ruleSet && rule == approval.Deny {
		return approval.Deny, nil
	}
	if spec.ReadOnly {
		return approval.AllowOnce, nil
	}
	switch a.approval {
	case "auto":
		return approval.AllowOnce, nil
	case "readonly", "plan":
		return approval.Deny, nil
	}
	if ruleSet && rule == approval.AllowOnce {
		return approval.AllowOnce, nil
	}
	key := allowKey(spec.Name, rawArgs)
	if a.autoAllow[key] {
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
	if preview != nil {
		req.Path = preview.Path
		req.Diff = util.TruncateMiddle(preview.Diff, maxDiffPreviewBytes)
	}
	a.emitEvent(Event{
		Type:       EventApproval,
		SessionID:  sessionID(sess),
		Depth:      a.depth,
		ApprovalID: req.ID,
		Name:       spec.Name,
		Args:       req.Args,
		ReadOnly:   spec.ReadOnly,
		Path:       req.Path,
		Diff:       req.Diff,
	})
	dec, err := a.approver.Approve(ctx, req)
	if err != nil {
		a.emitEvent(Event{
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
		a.autoAllow[key] = true
	}
	a.emitEvent(Event{
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

func (a *Agent) recordChange(ch *tools.Change) {
	if ch == nil {
		return
	}
	a.cpMu.Lock()
	cp := a.checkpoint
	if cp == nil {
		a.cpMu.Unlock()
		return
	}
	for _, f := range cp.Files {
		if f.Path == ch.Path {
			a.cpMu.Unlock()
			return
		}
	}
	size := 0
	for _, f := range cp.Files {
		size += len(f.Content)
	}
	if size+len(ch.Old) > maxCheckpointBytes {
		a.cpMu.Unlock()
		return
	}
	cp.Files = append(cp.Files, session.FileSnapshot{
		Path:    ch.Path,
		Existed: ch.Existed,
		Mode:    ch.Mode,
		Content: ch.Old,
	})
	sess := a.session
	a.cpMu.Unlock()
	if sess != nil {
		a.save(sess)
	}
}

func (a *Agent) finishCheckpoint(sess *session.Session) {
	a.cpMu.Lock()
	cp := a.checkpoint
	a.checkpoint = nil
	a.cpMu.Unlock()
	a.session = nil
	if sess == nil || cp == nil {
		return
	}
	if len(cp.Files) == 0 {
		for i := len(sess.Checkpoints) - 1; i >= 0; i-- {
			if &sess.Checkpoints[i] == cp {
				sess.Checkpoints = append(sess.Checkpoints[:i], sess.Checkpoints[i+1:]...)
				break
			}
		}
	}
	if len(sess.Checkpoints) > maxCheckpoints {
		sess.Checkpoints = append([]session.Checkpoint(nil), sess.Checkpoints[len(sess.Checkpoints)-maxCheckpoints:]...)
	}
	a.save(sess)
}

// Undo restores the files captured by the most recent checkpoint and drops the
// checkpoint. It returns the restored workspace-relative paths.
func (a *Agent) Undo(sess *session.Session) ([]string, error) {
	if sess == nil {
		return nil, fmt.Errorf("nil session")
	}
	if len(sess.Checkpoints) == 0 {
		return nil, fmt.Errorf("nothing to undo")
	}
	cp := sess.Checkpoints[len(sess.Checkpoints)-1]
	if len(cp.Files) == 0 {
		sess.Checkpoints = sess.Checkpoints[:len(sess.Checkpoints)-1]
		a.save(sess)
		return nil, fmt.Errorf("nothing to undo")
	}

	restored := make([]string, 0, len(cp.Files))
	var errs []error
	for i := len(cp.Files) - 1; i >= 0; i-- {
		f := cp.Files[i]
		abs := f.Path
		if a.fs != nil {
			resolved, err := a.fs.Resolve(f.Path)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			abs = resolved
		} else {
			abs = filepath.Join(a.cfg.WorkDir, filepath.FromSlash(f.Path))
		}
		if !f.Existed {
			if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
				errs = append(errs, fmt.Errorf("remove %s: %w", f.Path, err))
				continue
			}
			restored = append(restored, f.Path)
			continue
		}
		mode := os.FileMode(f.Mode)
		if mode == 0 {
			mode = 0o644
		}
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			errs = append(errs, fmt.Errorf("restore %s: %w", f.Path, err))
			continue
		}
		if err := os.WriteFile(abs, []byte(f.Content), mode); err != nil {
			errs = append(errs, fmt.Errorf("restore %s: %w", f.Path, err))
			continue
		}
		restored = append(restored, f.Path)
	}

	sess.Checkpoints = sess.Checkpoints[:len(sess.Checkpoints)-1]
	a.save(sess)
	if len(errs) > 0 {
		return restored, errors.Join(errs...)
	}
	return restored, nil
}

func (a *Agent) costOf(u llm.Usage) float64 {
	cost, ok := a.cfg.UsageCost(u)
	if !ok {
		return 0
	}
	return cost
}

func (a *Agent) doneEvent(sess *session.Session, message string) Event {
	total := a.totalUsage(sess)
	ev := Event{
		Type:       EventDone,
		SessionID:  sessionID(sess),
		Depth:      a.depth,
		Message:    message,
		Usage:      &a.usage,
		TotalUsage: total,
		CostUSD:    a.costOf(a.turnUsage),
		TotalCost:  a.costOf(*total),
	}
	if sess != nil {
		ev.Checkpoints = len(sess.Checkpoints)
	}
	return ev
}

func (a *Agent) save(sess *session.Session) {
	if sess == nil || a.sessions == nil {
		return
	}
	if err := a.sessions.Save(sess); err != nil {
		a.emitEvent(Event{Type: EventError, SessionID: sess.ID, Depth: a.depth, Message: "session save failed: " + err.Error()})
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
