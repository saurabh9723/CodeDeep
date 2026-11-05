package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"codedeep/internal/approval"
	"codedeep/internal/config"
	"codedeep/internal/llm"
	"codedeep/internal/tools"
)

type toolCallSpec struct {
	id, name, args string
}

func streamToolCalls(w http.ResponseWriter, calls []toolCallSpec) {
	w.Header().Set("Content-Type", "text/event-stream")
	deltas := make([]any, 0, len(calls))
	for i, c := range calls {
		deltas = append(deltas, map[string]any{
			"index": i,
			"id":    c.id,
			"type":  "function",
			"function": map[string]any{
				"name":      c.name,
				"arguments": c.args,
			},
		})
	}
	sseEvent(w, map[string]any{
		"choices": []any{map[string]any{
			"index": 0,
			"delta": map[string]any{"role": "assistant", "tool_calls": deltas},
		}},
	})
	sseFinish(w, "tool_calls", 12, 6)
}

func TestReadOnlyToolBatchRunsInParallel(t *testing.T) {
	var started sync.WaitGroup
	started.Add(2)
	release := make(chan struct{})
	go func() {
		started.Wait()
		close(release)
	}()

	names := make([]string, 0, 2)
	var namesMu sync.Mutex
	reg := tools.NewRegistry()
	for _, name := range []string{"slow_a", "slow_b"} {
		name := name
		reg.Register(&tools.Spec{
			Name:        name,
			Description: "Slow read-only tool.",
			Params:      tools.Object(map[string]any{}),
			ReadOnly:    true,
			Run: func(ctx context.Context, _ json.RawMessage) (tools.Result, error) {
				namesMu.Lock()
				names = append(names, name)
				namesMu.Unlock()
				started.Done()
				select {
				case <-release:
					return tools.Result{Output: name}, nil
				case <-time.After(1500 * time.Millisecond):
					return tools.Result{Output: "timeout: " + name, IsError: true}, nil
				case <-ctx.Done():
					return tools.Result{}, ctx.Err()
				}
			},
		})
	}

	client, _ := newFakeLLM(t, func(n int, w http.ResponseWriter) {
		if n == 1 {
			streamToolCalls(w, []toolCallSpec{
				{id: "call_a", name: "slow_a", args: `{}`},
				{id: "call_b", name: "slow_b", args: `{}`},
			})
			return
		}
		streamText(w, "Both finished.", 20, 5)
	})

	cfg := testConfig(t)
	var events []Event
	a := newAgentWith(t, cfg, client, reg, nil, &events)
	sess := a.sessions.New(cfg.Model, cfg.WorkDir)

	done := make(chan error, 1)
	go func() { done <- a.Run(context.Background(), sess, "run both") }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v (read-only tools did not overlap)", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("read-only tools did not run concurrently")
	}

	var toolMsgs []string
	for _, m := range sess.Messages {
		if m.Role == "tool" {
			toolMsgs = append(toolMsgs, m.Content)
		}
	}
	if len(toolMsgs) != 2 || toolMsgs[0] != "slow_a" || toolMsgs[1] != "slow_b" {
		t.Errorf("tool results should be appended in call order, got %v", toolMsgs)
	}
	starts, ends := 0, 0
	for _, ev := range events {
		switch ev.Type {
		case EventToolStart:
			starts++
		case EventToolEnd:
			ends++
		}
	}
	if starts != 2 || ends != 2 {
		t.Errorf("tool events = %d start, %d end", starts, ends)
	}
}

func TestWriteToolBatchStaysSequential(t *testing.T) {
	var mu sync.Mutex
	var order []string
	reg := tools.NewRegistry()
	for _, name := range []string{"write_a", "write_b"} {
		name := name
		reg.Register(&tools.Spec{
			Name:        name,
			Description: "Mutating tool.",
			Params:      tools.Object(map[string]any{}),
			Run: func(ctx context.Context, _ json.RawMessage) (tools.Result, error) {
				mu.Lock()
				order = append(order, name)
				mu.Unlock()
				return tools.Result{Output: name}, nil
			},
		})
	}

	client, _ := newFakeLLM(t, func(n int, w http.ResponseWriter) {
		if n == 1 {
			streamToolCalls(w, []toolCallSpec{
				{id: "call_a", name: "write_a", args: `{}`},
				{id: "call_b", name: "write_b", args: `{}`},
			})
			return
		}
		streamText(w, "Done.", 10, 4)
	})

	cfg := testConfig(t)
	cfg.Approval = "auto"
	var events []Event
	a := newAgentWith(t, cfg, client, reg, nil, &events)
	sess := a.sessions.New(cfg.Model, cfg.WorkDir)
	if err := a.Run(context.Background(), sess, "write"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(order) != 2 || order[0] != "write_a" || order[1] != "write_b" {
		t.Errorf("mutating tools should execute sequentially in call order, got %v", order)
	}
}

func countingTool(name string, ran *atomic.Int32) *tools.Spec {
	return &tools.Spec{
		Name:        name,
		Description: "Test tool.",
		Params:      tools.Object(map[string]any{}),
		Run: func(context.Context, json.RawMessage) (tools.Result, error) {
			ran.Add(1)
			return tools.Result{Output: name + " ok"}, nil
		},
	}
}

func TestPermissionDenyOverridesAutoApproval(t *testing.T) {
	client, _ := newFakeLLM(t, func(n int, w http.ResponseWriter) {
		if n == 1 {
			streamToolCall(w, "call_1", "bash", `{"command":"rm -rf build"}`, 10, 5)
			return
		}
		streamText(w, "Done.", 10, 4)
	})
	cfg := testConfig(t)
	cfg.Approval = "auto"
	cfg.Permissions = []config.PermissionRule{
		{Tool: "bash", Action: "deny", Command: "rm *"},
	}
	var ran atomic.Int32
	reg := tools.NewRegistry()
	reg.Register(countingTool("bash", &ran))
	a := newAgentWith(t, cfg, client, reg, nil, nil)
	sess := a.sessions.New(cfg.Model, cfg.WorkDir)

	if err := a.Run(context.Background(), sess, "clean up"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ran.Load() != 0 {
		t.Error("deny rule must block the tool even in auto mode")
	}
	var result string
	for _, m := range sess.Messages {
		if m.Role == "tool" {
			result = m.Content
		}
	}
	if !strings.Contains(result, "permission denied") {
		t.Errorf("tool result = %q", result)
	}
}

func TestPermissionAllowBypassesAsk(t *testing.T) {
	client, _ := newFakeLLM(t, func(n int, w http.ResponseWriter) {
		if n == 1 {
			streamToolCall(w, "call_1", "bash", `{"command":"go test ./..."}`, 10, 5)
			return
		}
		streamText(w, "Tests pass.", 10, 4)
	})
	cfg := testConfig(t)
	cfg.Approval = "ask"
	cfg.Permissions = []config.PermissionRule{
		{Tool: "bash", Action: "allow", Command: "go test*"},
	}
	var ran atomic.Int32
	reg := tools.NewRegistry()
	reg.Register(countingTool("bash", &ran))
	// A nil approver denies every prompt, so the allow rule must resolve the
	// call without asking.
	a := newAgentWith(t, cfg, client, reg, nil, nil)
	sess := a.sessions.New(cfg.Model, cfg.WorkDir)

	if err := a.Run(context.Background(), sess, "test"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ran.Load() != 1 {
		t.Errorf("allow rule did not run the tool (ran %d)", ran.Load())
	}
}

func TestAlwaysAllowIsNarrowedToThePath(t *testing.T) {
	client, _ := newFakeLLM(t, func(n int, w http.ResponseWriter) {
		switch n {
		case 1:
			streamToolCalls(w, []toolCallSpec{
				{id: "call_a", name: "write_file", args: `{"path":"a.txt","content":"a"}`},
				{id: "call_b", name: "write_file", args: `{"path":"b.txt","content":"b"}`},
			})
		case 2:
			streamToolCall(w, "call_a2", "write_file", `{"path":"a.txt","content":"a2"}`, 10, 4)
		default:
			streamText(w, "Done.", 10, 4)
		}
	})
	cfg := testConfig(t)
	cfg.Approval = "ask"
	var ran atomic.Int32
	reg := tools.NewRegistry()
	reg.Register(countingTool("write_file", &ran))

	var prompts atomic.Int32
	approver := approval.Func(func(context.Context, approval.Request) (approval.Decision, error) {
		prompts.Add(1)
		return approval.AllowAlways, nil
	})
	a := newAgentWith(t, cfg, client, reg, approver, nil)
	sess := a.sessions.New(cfg.Model, cfg.WorkDir)

	if err := a.Run(context.Background(), sess, "write files"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ran.Load() != 3 {
		t.Errorf("tool ran %d times, want 3", ran.Load())
	}
	if prompts.Load() != 2 {
		t.Errorf("approval prompts = %d, want 2 (a.txt, b.txt; repeat of a.txt is auto)", prompts.Load())
	}
}

func TestPermissionPathGlobMatchesRelativeAndAbsolute(t *testing.T) {
	workDir := t.TempDir()
	cases := []struct {
		pattern string
		value   string
		want    bool
	}{
		{"src/**", "src/main.go", true},
		{"src/**", "src/nested/deep.go", true},
		{"src/**", "docs/readme.md", false},
		{"*.go", "src/main.go", true},
		{"*.go", "src/main.py", false},
		{"src/*.go", "src/main.go", true},
		{"src/*.go", "src/nested/main.go", false},
	}
	for _, tc := range cases {
		if got := matchPathRule(workDir, tc.pattern, tc.value); got != tc.want {
			t.Errorf("matchPathRule(%q, %q) = %v, want %v", tc.pattern, tc.value, got, tc.want)
		}
	}
	abs := workDir + "/src/main.go"
	if !matchPathRule(workDir, "src/**", abs) {
		t.Errorf("absolute path inside the workspace should match, got false for %s", abs)
	}
}

func TestRunWithImagesSendsMultimodalContent(t *testing.T) {
	client, log := newFakeLLM(t, func(n int, w http.ResponseWriter) {
		streamText(w, "I see the screenshot.", 20, 6)
	})
	cfg := testConfig(t)
	var events []Event
	a := newAgentWith(t, cfg, client, tools.NewRegistry(), nil, &events)
	sess := a.sessions.New(cfg.Model, cfg.WorkDir)
	img := llm.Image{MIME: "image/png", Data: "aGVsbG8="}

	if err := a.RunWithImages(context.Background(), sess, "what is this?", []llm.Image{img}); err != nil {
		t.Fatalf("RunWithImages: %v", err)
	}
	if len(sess.Messages) == 0 || sess.Messages[0].Role != "user" || len(sess.Messages[0].Images) != 1 {
		t.Fatalf("session should record the user image, got %#v", sess.Messages)
	}

	body := log.all()[0]
	rawMsgs, _ := body["messages"].([]any)
	var content []any
	for _, raw := range rawMsgs {
		m, _ := raw.(map[string]any)
		if m["role"] == "user" {
			content, _ = m["content"].([]any)
			break
		}
	}
	if content == nil {
		t.Fatalf("user message content should be a multimodal array, got %#v", rawMsgs)
	}
	var sawText, sawImage bool
	for _, raw := range content {
		part, _ := raw.(map[string]any)
		switch part["type"] {
		case "text":
			if part["text"] == "what is this?" {
				sawText = true
			}
		case "image_url":
			u, _ := part["image_url"].(map[string]any)["url"].(string)
			if u != "data:image/png;base64,aGVsbG8=" {
				t.Errorf("image url = %q", u)
			}
			sawImage = true
		}
	}
	if !sawText || !sawImage {
		t.Errorf("multimodal parts missing: text=%v image=%v (%#v)", sawText, sawImage, content)
	}

	for _, ev := range events {
		if ev.Type == EventUser && len(ev.Images) != 1 {
			t.Errorf("user event should carry the image, got %#v", ev.Images)
		}
	}
}

func TestRunWithImagesAllowsImageOnlyPrompt(t *testing.T) {
	client, _ := newFakeLLM(t, func(n int, w http.ResponseWriter) {
		streamText(w, "A diagram.", 10, 4)
	})
	cfg := testConfig(t)
	a := newAgentWith(t, cfg, client, tools.NewRegistry(), nil, nil)
	sess := a.sessions.New(cfg.Model, cfg.WorkDir)
	img := llm.Image{MIME: "image/jpeg", Data: "aGk="}
	if err := a.RunWithImages(context.Background(), sess, "  ", []llm.Image{img}); err != nil {
		t.Fatalf("image-only message should be accepted: %v", err)
	}
	if len(sess.Messages) == 0 || sess.Messages[0].Content != "" || len(sess.Messages[0].Images) != 1 {
		t.Fatalf("unexpected first message: %#v", sess.Messages)
	}
}

func TestSubagentsRunInParallel(t *testing.T) {
	var started atomic.Int32
	release := make(chan struct{})
	var probesMu sync.Mutex
	var probes []string
	reg := tools.NewRegistry()
	reg.Register(&tools.Spec{
		Name:        "sync_probe",
		Description: "Read-only probe that blocks until two are running.",
		Params:      tools.Object(map[string]any{}),
		ReadOnly:    true,
		Run: func(ctx context.Context, _ json.RawMessage) (tools.Result, error) {
			if started.Add(1) == 2 {
				close(release)
			}
			select {
			case <-release:
				probesMu.Lock()
				probes = append(probes, "probe ok")
				probesMu.Unlock()
				return tools.Result{Output: "probe ok"}, nil
			case <-time.After(2500 * time.Millisecond):
				probesMu.Lock()
				probes = append(probes, "probe timed out")
				probesMu.Unlock()
				return tools.Result{Output: "probe timed out", IsError: true}, nil
			case <-ctx.Done():
				return tools.Result{}, ctx.Err()
			}
		},
	})

	// Request map: 1 = parent spawns two subagents; 2,3 = each subagent's
	// probe turn (both must start before either can finish, so their numbers
	// are stable); 4,5 = subagent final turns; 6 = parent final turn.
	client, _ := newFakeLLM(t, func(n int, w http.ResponseWriter) {
		switch n {
		case 1:
			streamToolCalls(w, []toolCallSpec{
				{id: "sub_a", name: "agent", args: `{"description":"probe a","prompt":"run the sync probe"}`},
				{id: "sub_b", name: "agent", args: `{"description":"probe b","prompt":"run the sync probe"}`},
			})
		case 2, 3:
			streamToolCalls(w, []toolCallSpec{{id: "probe_1", name: "sync_probe", args: `{}`}})
		case 4, 5:
			streamText(w, "probe finished", 8, 2)
		default:
			streamText(w, "Both subagents finished.", 20, 5)
		}
	})

	cfg := testConfig(t)
	var events []Event
	a := newAgentWith(t, cfg, client, reg, nil, &events)
	sess := a.sessions.New(cfg.Model, cfg.WorkDir)

	done := make(chan error, 1)
	go func() { done <- a.Run(context.Background(), sess, "spawn two subagents") }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("run stalled")
	}

	// The probes only succeed when both subagents are executing at the same
	// time; a sequential run leaves a timeout behind.
	probesMu.Lock()
	results := append([]string(nil), probes...)
	probesMu.Unlock()
	if len(results) != 2 {
		t.Fatalf("probe runs = %d, want 2", len(results))
	}
	for _, r := range results {
		if r != "probe ok" {
			t.Errorf("subagents ran sequentially: probe result %q", r)
		}
	}
	subReports, starts, ends := 0, 0, 0
	for _, m := range sess.Messages {
		if m.Role == "tool" && m.Content == "probe finished" {
			subReports++
		}
	}
	for _, ev := range events {
		switch ev.Type {
		case EventSubagentStart:
			if ev.Depth != 1 {
				t.Errorf("subagent start depth = %d, want 1", ev.Depth)
			}
			starts++
		case EventSubagentEnd:
			ends++
		}
	}
	if subReports != 2 {
		t.Errorf("subagent reports = %d, want 2", subReports)
	}
	if starts != 2 || ends != 2 {
		t.Errorf("subagent events = %d start, %d end, want 2/2", starts, ends)
	}
}
