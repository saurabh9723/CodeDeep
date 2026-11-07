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
	"codedeep/internal/diff"
	"codedeep/internal/hooks"
	"codedeep/internal/index"
	"codedeep/internal/llm"
	"codedeep/internal/memory"
	"codedeep/internal/session"
	"codedeep/internal/skills"
	"codedeep/internal/tools"
	"codedeep/internal/util"
	"codedeep/internal/verify"
)

const (
	MaxSubagentDepth    = 2
	maxCheckpoints      = 20
	maxCheckpointBytes  = 4 << 20
	maxDiffPreviewBytes = 64 << 10
	maxCheckpointDiff   = 64 << 10
	maxParallelTools    = 4
	// maxTurnStats bounds the per-session analytics history. The dashboard
	// windows it by time; older turns are dropped so session files stay small.
	maxTurnStats = 200
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
	// Memory is the shared project note store. It is optional; without it the
	// recall/remember tools are still registered by the app but the prompt
	// gets no memory index.
	Memory *memory.Store
	// Index is the shared codebase symbol index. It adds a query-ranked map to
	// the system prompt; tools are registered separately by the app.
	Index *index.Index
	// Locks, when set, shares one lock set across the whole agent tree so
	// parallel subagents cannot interleave events, approvals, checkpoint
	// writes, or session saves. Subagents inherit their parent's locks.
	Locks *sharedLocks
}

// sharedLocks serialize the parts of an agent tree that multiple goroutines
// can touch when subagents run in parallel.
type sharedLocks struct {
	emit       sync.Mutex
	approval   sync.Mutex
	checkpoint sync.Mutex
	save       sync.Mutex
	stats      sync.Mutex
	todos      sync.Mutex
	verify     sync.Mutex

	turnChanged    bool
	turnVerified   bool
	verifyAttempts int
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
	mem       *memory.Store
	idx       *index.Index
	verifier  *verify.Runner
	locks     *sharedLocks
	depth     int
	approval  string
	autoAllow map[string]bool
	usage     llm.Usage
	turnUsage llm.Usage
	turnTools map[string]llm.ToolUsage

	session    *session.Session
	checkpoint *session.Checkpoint
}

func New(opts Options) *Agent {
	depth := opts.Depth
	if depth < 0 {
		depth = 0
	}
	locks := opts.Locks
	if locks == nil {
		locks = &sharedLocks{}
	}
	exclude := []string{"agent"}
	if depth > 0 {
		// The session plan belongs to the root agent; subagents inherit it in
		// their prompt but do not edit it.
		exclude = append(exclude, "todo_write")
	}
	a := &Agent{
		cfg:        opts.Config,
		client:     opts.Client,
		reg:        opts.Registry.CloneWithout(exclude...),
		fs:         opts.FS,
		sessions:   opts.Sessions,
		approver:   opts.Approver,
		emit:       opts.Emitter,
		hooks:      hooks.New(opts.Config, opts.Config.WorkDir),
		diag:       diagnostics.New(opts.Config, opts.Config.WorkDir),
		skills:     opts.Skills,
		mem:        opts.Memory,
		idx:        opts.Index,
		verifier:   verify.New(opts.Config, opts.Config.WorkDir),
		locks:      locks,
		depth:      depth,
		approval:   opts.Approval,
		autoAllow:  map[string]bool{},
		session:    opts.Session,
		checkpoint: opts.Checkpoint,
	}
	a.registerSubagentTool()
	a.registerTodoTool()
	return a
}

// emitEvent serializes emission because read-only tool calls and subagents
// can run in parallel; the lock set is shared across the agent tree.
func (a *Agent) emitEvent(ev Event) {
	a.locks.emit.Lock()
	defer a.locks.emit.Unlock()
	if ev.Time == "" {
		ev.Time = time.Now().UTC().Format(time.RFC3339Nano)
	}
	a.emit.Emit(ev)
}

// systemPrompt adds the session plan and durable memory index to the base
// prompt so long tasks and earlier notes stay visible after compaction.
func (a *Agent) systemPrompt(sess *session.Session, msgs []llm.Message) string {
	system := BuildSystemPrompt(a.cfg, a.depth, a.skills)
	if sess != nil {
		system += PlanSection(sess.Todos)
	}
	if a.mem != nil {
		system += a.mem.PromptSection(8 << 10)
	}
	if a.idx != nil {
		if section := a.idx.Prompt(latestUserQuery(msgs, sess), a.cfg.RepoMap.PromptTokens); section != "" {
			system += "\n" + section
		}
	}
	return system
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
	a.turnTools = map[string]llm.ToolUsage{}
	turnStarted := time.Now().UTC()
	a.approval = a.cfg.ApprovalMode()
	a.resetVerification()
	sess.Checkpoints = append(sess.Checkpoints, session.Checkpoint{
		Turn: sess.Turns,
		Time: turnStarted,
	})
	a.checkpoint = &sess.Checkpoints[len(sess.Checkpoints)-1]
	defer func() {
		a.finishCheckpoint(sess)
		a.recordTurnStats(sess, turnStarted)
	}()

	a.emitEvent(Event{Type: EventUser, SessionID: sess.ID, Text: prompt, Images: images})
	a.save(sess)

	_, err := a.runLoop(ctx, &sess.Messages, sess, a.cfg.MaxSteps)
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

func (a *Agent) runLoop(ctx context.Context, msgs *[]llm.Message, sess *session.Session, maxSteps int) (string, error) {
	if maxSteps <= 0 {
		maxSteps = 60
	}
	lastText := ""
	compactedRetry := false

	for step := 0; step < maxSteps; step++ {
		if err := ctx.Err(); err != nil {
			return lastText, err
		}
		// Rebuild the prompt each step so plan and memory updates made by tools
		// are visible immediately and survive compaction within the same turn.
		system := a.systemPrompt(a.session, *msgs)
		a.maybeCompact(ctx, msgs, system, sess)

		requestMsgs := make([]llm.Message, 0, len(*msgs)+1)
		requestMsgs = append(requestMsgs, llm.Message{Role: "system", Content: system})
		requestMsgs = append(requestMsgs, *msgs...)

		a.emitEvent(Event{Type: EventAssistantStart, SessionID: sessionID(sess), Depth: a.depth})
		resp, err := a.client.Stream(ctx, llm.ChatRequest{
			Model:           a.cfg.Model,
			Messages:        requestMsgs,
			Tools:           a.reg.Definitions(),
			MaxTokens:       a.cfg.MaxTokens,
			Temperature:     a.cfg.Temperature,
			ReasoningEffort: a.cfg.ReasoningEffort,
			ReasoningBudget: a.cfg.ReasoningBudget,
			PromptCache:     a.cfg.PromptCache,
		}, func(d llm.Delta) error {
			if d.Retry != nil {
				a.emitEvent(Event{Type: EventRetry, SessionID: sessionID(sess), Depth: a.depth, Retry: d.Retry})
			}
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
		callUsage := attributeToolUsage(usage, assistant.ToolCalls)
		a.recordToolUsage(assistant.ToolCalls, callUsage, sess)
		a.save(sess)

		if len(assistant.ToolCalls) == 0 {
			if assistant.Content == "" && assistant.Reasoning == "" {
				return lastText, fmt.Errorf("the model returned an empty response")
			}
			continued, err := a.verifyTurn(ctx, msgs, sess)
			if err != nil {
				return lastText, err
			}
			if continued {
				continue
			}
			return lastText, nil
		}

		outputs := a.executeToolCalls(ctx, assistant.ToolCalls, sess, callUsage)
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
// call order. Batches that contain only read-only tools or subagents run
// concurrently; subagents inherit a shared lock set so their shared state
// (events, approvals, checkpoints, session saves) stays serialized.
func (a *Agent) executeToolCalls(ctx context.Context, calls []llm.ToolCall, sess *session.Session, callUsage []llm.ToolUsage) []string {
	results := make([]string, len(calls))
	if len(calls) < 2 || !a.allParallelizable(calls) {
		for i, tc := range calls {
			if ctx.Err() != nil {
				results[i] = "Error: cancelled by user."
				continue
			}
			results[i] = a.executeTool(ctx, tc, sess, usageAt(callUsage, i))
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
			results[i] = a.executeTool(ctx, tc, sess, usageAt(callUsage, i))
		}(i, tc)
	}
	wg.Wait()
	return results
}

func (a *Agent) allParallelizable(calls []llm.ToolCall) bool {
	for _, tc := range calls {
		spec, ok := a.reg.Get(tc.Function.Name)
		if !ok {
			return false
		}
		if !spec.ReadOnly && spec.Name != "agent" {
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

func (a *Agent) executeTool(ctx context.Context, tc llm.ToolCall, sess *session.Session, callUsage llm.ToolUsage) string {
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

	ev := Event{
		Type:      EventToolStart,
		SessionID: sessionID(sess),
		Depth:     a.depth,
		Name:      name,
		Args:      eventArgs,
		CallID:    tc.ID,
	}
	if callUsage.Usage.TotalTokens > 0 {
		ev.Usage = &callUsage.Usage
		ev.CostUSD = callUsage.CostUSD
	}
	a.emitEvent(ev)

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
		if len(out.Diagnostics) > 0 {
			for _, d := range out.Diagnostics {
				fmt.Fprintf(&b, "%s\n", util.Truncate(formatDiagnostic(d), 500))
			}
		} else if strings.TrimSpace(out.Output) != "" {
			b.WriteString(out.Output)
			if !strings.HasSuffix(out.Output, "\n") {
				b.WriteByte('\n')
			}
		}
	}
	return b.String()
}

func formatDiagnostic(d diagnostics.Diagnostic) string {
	loc := d.Path
	if d.Line > 0 {
		loc += fmt.Sprintf(":%d", d.Line)
		if d.Column > 0 {
			loc += fmt.Sprintf(":%d", d.Column)
		}
	}
	sev := ""
	if d.Severity != "" {
		sev = d.Severity + ": "
	}
	return fmt.Sprintf("%s: %s%s", loc, sev, d.Message)
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
			Type:        EventDiagnostics,
			SessionID:   sessionID(sess),
			Depth:       a.depth,
			Name:        out.Pattern,
			Message:     out.Command,
			Path:        path,
			Output:      util.TruncateMiddle(out.Output, 8*1024),
			IsError:     out.IsError,
			CallID:      tc.ID,
			Annotations: out.Diagnostics,
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
	if spec.ReadOnly || spec.Internal {
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
	dec, err := func() (approval.Decision, error) {
		// Parallel subagents share one approver; serialize prompts so the
		// user answers one at a time.
		a.locks.approval.Lock()
		defer a.locks.approval.Unlock()
		return a.approver.Approve(ctx, req)
	}()
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
	a.markChanged()
	a.locks.checkpoint.Lock()
	cp := a.checkpoint
	if cp == nil {
		a.locks.checkpoint.Unlock()
		return
	}
	for _, f := range cp.Files {
		if f.Path == ch.Path {
			a.locks.checkpoint.Unlock()
			return
		}
	}
	size := 0
	for _, f := range cp.Files {
		size += len(f.Content)
	}
	if size+len(ch.Old) > maxCheckpointBytes {
		a.locks.checkpoint.Unlock()
		return
	}
	cp.Files = append(cp.Files, session.FileSnapshot{
		Path:    ch.Path,
		Existed: ch.Existed,
		Mode:    ch.Mode,
		Content: ch.Old,
	})
	sess := a.session
	a.locks.checkpoint.Unlock()
	if sess != nil {
		a.save(sess)
	}
}

func (a *Agent) finishCheckpoint(sess *session.Session) {
	a.locks.checkpoint.Lock()
	cp := a.checkpoint
	a.checkpoint = nil
	a.locks.checkpoint.Unlock()
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
	if len(cp.Files) > 0 {
		cp.Diff, cp.Added, cp.Removed = a.turnDiff(cp.Files)
	}
	a.save(sess)
}

// turnDiff renders the unified diff between each snapshot's pre-turn content
// and the file's current on-disk content, capped at maxCheckpointDiff bytes.
func (a *Agent) turnDiff(files []session.FileSnapshot) (string, int, int) {
	var b strings.Builder
	for _, f := range files {
		abs := a.snapshotPath(f.Path)
		cur, err := os.ReadFile(abs)
		if err != nil {
			if !os.IsNotExist(err) {
				continue
			}
			cur = nil
		}
		if d := diff.Unified(f.Path, f.Content, string(cur), 3); d != "" {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(d)
		}
		if b.Len() >= maxCheckpointDiff {
			return countDiffLines(util.TruncateMiddle(b.String(), maxCheckpointDiff))
		}
	}
	out := b.String()
	if out == "" {
		return "", 0, 0
	}
	return countDiffLines(out)
}

func (a *Agent) snapshotPath(rel string) string {
	if a.fs != nil {
		if resolved, err := a.fs.Resolve(rel); err == nil {
			return resolved
		}
	}
	return filepath.Join(a.cfg.WorkDir, filepath.FromSlash(rel))
}

// countDiffLines returns the diff text with its added and removed line counts.
func countDiffLines(text string) (string, int, int) {
	added, removed := 0, 0
	for _, line := range strings.Split(text, "\n") {
		switch {
		case strings.HasPrefix(line, "+++") || strings.HasPrefix(line, "---"):
			continue
		case strings.HasPrefix(line, "+"):
			added++
		case strings.HasPrefix(line, "-"):
			removed++
		}
	}
	return text, added, removed
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
	last := &sess.Checkpoints[len(sess.Checkpoints)-1]
	if len(last.Files) == 0 {
		sess.Checkpoints = sess.Checkpoints[:len(sess.Checkpoints)-1]
		a.save(sess)
		return nil, fmt.Errorf("nothing to undo")
	}
	return a.UndoAt(sess, len(sess.Checkpoints)-1)
}

// UndoAt reverts the workspace to its state before the checkpoint at index
// idx (0-based, counting from the oldest kept checkpoint): every snapshot from
// the newest checkpoint down to idx is applied, then those checkpoints are
// dropped. It returns the restored workspace-relative paths.
func (a *Agent) UndoAt(sess *session.Session, idx int) ([]string, error) {
	if sess == nil {
		return nil, fmt.Errorf("nil session")
	}
	if idx < 0 || idx >= len(sess.Checkpoints) {
		return nil, fmt.Errorf("checkpoint %d out of range (1-%d)", idx+1, len(sess.Checkpoints))
	}
	if len(sess.Checkpoints[idx].Files) == 0 {
		return nil, fmt.Errorf("checkpoint %d has no file changes", idx+1)
	}

	restored := make([]string, 0, len(sess.Checkpoints[idx].Files))
	reported := map[string]bool{}
	var errs []error
	// Newer checkpoints are written first and older ones overwrite them, so
	// the oldest snapshot wins: the workspace ends at its pre-turn state.
	for i := len(sess.Checkpoints) - 1; i >= idx; i-- {
		for j := len(sess.Checkpoints[i].Files) - 1; j >= 0; j-- {
			f := sess.Checkpoints[i].Files[j]
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
				if !reported[f.Path] {
					reported[f.Path] = true
					restored = append(restored, f.Path)
				}
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
			if !reported[f.Path] {
				reported[f.Path] = true
				restored = append(restored, f.Path)
			}
		}
	}

	sess.Checkpoints = sess.Checkpoints[:idx]
	a.save(sess)
	if len(errs) > 0 {
		return restored, errors.Join(errs...)
	}
	return restored, nil
}

// attributeToolUsage splits one model response's usage evenly across the tool
// calls it requested, distributing indivisible remainder tokens to the first
// calls so the attributed totals match the response exactly.
func attributeToolUsage(usage llm.Usage, calls []llm.ToolCall) []llm.ToolUsage {
	out := make([]llm.ToolUsage, len(calls))
	if len(calls) == 0 {
		return out
	}
	n := len(calls)
	base := llm.Usage{
		PromptTokens:           usage.PromptTokens / n,
		CompletionTokens:       usage.CompletionTokens / n,
		TotalTokens:            usage.TotalTokens / n,
		PromptCacheHitTokens:   usage.PromptCacheHitTokens / n,
		PromptCacheWriteTokens: usage.PromptCacheWriteTokens / n,
	}
	remPrompt := usage.PromptTokens % n
	remCompletion := usage.CompletionTokens % n
	remTotal := usage.TotalTokens % n
	remCache := usage.PromptCacheHitTokens % n
	remCacheWrite := usage.PromptCacheWriteTokens % n
	for i := range out {
		u := base
		if i < remPrompt {
			u.PromptTokens++
		}
		if i < remCompletion {
			u.CompletionTokens++
		}
		if i < remTotal {
			u.TotalTokens++
		}
		if i < remCache {
			u.PromptCacheHitTokens++
		}
		if i < remCacheWrite {
			u.PromptCacheWriteTokens++
		}
		out[i] = llm.ToolUsage{Calls: 1, Usage: u}
	}
	return out
}

func usageAt(list []llm.ToolUsage, i int) llm.ToolUsage {
	if i >= 0 && i < len(list) {
		return list[i]
	}
	return llm.ToolUsage{Calls: 1}
}

func (a *Agent) recordToolUsage(calls []llm.ToolCall, callUsage []llm.ToolUsage, sess *session.Session) {
	if len(calls) == 0 {
		return
	}
	if a.turnTools == nil {
		a.turnTools = map[string]llm.ToolUsage{}
	}
	for i, tc := range calls {
		u := usageAt(callUsage, i)
		u.CostUSD = a.costOf(u.Usage)
		if i < len(callUsage) {
			callUsage[i] = u
		}
		name := tc.Function.Name
		turn := a.turnTools[name]
		turn.Calls += u.Calls
		turn.Usage = turn.Usage.Add(u.Usage)
		turn.CostUSD = a.costOf(turn.Usage)
		a.turnTools[name] = turn
		if sess != nil {
			if sess.ToolUsage == nil {
				sess.ToolUsage = map[string]llm.ToolUsage{}
			}
			total := sess.ToolUsage[name]
			total.Calls += u.Calls
			total.Usage = total.Usage.Add(u.Usage)
			total.CostUSD = a.costOf(total.Usage)
			sess.ToolUsage[name] = total
		}
	}
}

// recordTurnStats persists one user turn's usage and tool attribution for the
// analytics dashboard. Subagent usage is already folded into turnUsage by the
// time the turn ends; the stats lock also covers that write.
func (a *Agent) recordTurnStats(sess *session.Session, started time.Time) {
	if sess == nil {
		return
	}
	a.locks.stats.Lock()
	usage := a.turnUsage
	tools := make(map[string]llm.ToolUsage, len(a.turnTools))
	for name, u := range a.turnTools {
		tools[name] = u
	}
	a.locks.stats.Unlock()
	if usage.TotalTokens == 0 && len(tools) == 0 {
		return
	}
	stat := session.TurnStat{Turn: sess.Turns, Time: started, Usage: usage}
	if len(tools) > 0 {
		stat.Tools = tools
	}
	sess.TurnStats = append(sess.TurnStats, stat)
	if len(sess.TurnStats) > maxTurnStats {
		sess.TurnStats = append([]session.TurnStat(nil), sess.TurnStats[len(sess.TurnStats)-maxTurnStats:]...)
	}
	a.save(sess)
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
		ToolUsage:  a.turnTools,
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
	// Parallel subagents share one session file; serialize writes.
	a.locks.save.Lock()
	defer a.locks.save.Unlock()
	if err := a.sessions.Save(sess); err != nil {
		a.emitEvent(Event{Type: EventError, SessionID: sess.ID, Depth: a.depth, Message: "session save failed: " + err.Error()})
		return
	}
	// The web transport uses this marker to know which events are already on
	// disk, so a reconnecting client only replays the unsaved tail.
	a.emitEvent(Event{Type: EventSaved, SessionID: sess.ID, Depth: a.depth})
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
