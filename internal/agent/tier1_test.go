package agent

import (
	"context"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codedeep/internal/approval"
	"codedeep/internal/config"
	"codedeep/internal/llm"
	"codedeep/internal/session"
	"codedeep/internal/tools"
)

func newTier1Agent(t *testing.T, cfg *config.Config, client *llm.Client, reg *tools.Registry, approver approval.Approver, events *[]Event) *Agent {
	t.Helper()
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	fs := tools.NewFS(cfg.WorkDir, false)
	if reg == nil {
		reg = tools.NewRegistry()
	}
	return New(Options{
		Config:   cfg,
		Client:   client,
		Registry: reg,
		FS:       fs,
		Sessions: store,
		Approver: approver,
		Emitter: func(ev Event) {
			if events != nil {
				*events = append(*events, ev)
			}
		},
	})
}

func coreRegistry(fs *tools.FS) *tools.Registry {
	reg := tools.NewRegistry()
	tools.RegisterCore(reg, fs, "")
	return reg
}

func eventIndex(events []Event, typ string) int {
	for i := range events {
		if events[i].Type == typ {
			return i
		}
	}
	return -1
}

func TestAgentEmitsDiffBeforeApproval(t *testing.T) {
	client, _ := newFakeLLM(t, func(n int, w http.ResponseWriter) {
		if n == 1 {
			streamToolCall(w, "call_w", "write_file", `{"path":"note.txt","content":"hello\n"}`, 10, 4)
			return
		}
		streamText(w, "Written.", 12, 5)
	})

	cfg := testConfig(t)
	cfg.Approval = "ask"
	fs := tools.NewFS(cfg.WorkDir, false)
	var events []Event
	a := newTier1Agent(t, cfg, client, coreRegistry(fs), approval.Func(func(context.Context, approval.Request) (approval.Decision, error) {
		return approval.AllowOnce, nil
	}), &events)
	sess := a.sessions.New(cfg.Model, cfg.WorkDir)

	if err := a.Run(context.Background(), sess, "write a note"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	di, ai := eventIndex(events, EventDiff), eventIndex(events, EventApproval)
	if di < 0 || ai < 0 {
		t.Fatalf("missing diff or approval event: %+v", events)
	}
	if di > ai {
		t.Error("diff must be emitted before the approval request")
	}
	d := events[di]
	if d.Path != "note.txt" || !strings.Contains(d.Diff, "+hello") {
		t.Errorf("diff event = %+v", d)
	}
	if ev := events[ai]; ev.Path != "note.txt" || !strings.Contains(ev.Diff, "+hello") {
		t.Errorf("approval event should carry the preview: %+v", ev)
	}

	data, err := os.ReadFile(filepath.Join(cfg.WorkDir, "note.txt"))
	if err != nil || string(data) != "hello\n" {
		t.Fatalf("file content = %q, err %v", data, err)
	}
	if len(sess.Checkpoints) != 1 || len(sess.Checkpoints[0].Files) != 1 {
		t.Fatalf("checkpoints = %+v", sess.Checkpoints)
	}
	if f := sess.Checkpoints[0].Files[0]; f.Existed || f.Path != "note.txt" {
		t.Errorf("snapshot = %+v", f)
	}
}

func TestUndoRestoresFile(t *testing.T) {
	root := t.TempDir()
	original := filepath.Join(root, "note.txt")
	if err := os.WriteFile(original, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	client, _ := newFakeLLM(t, func(n int, w http.ResponseWriter) {
		if n == 1 {
			streamToolCall(w, "call_w", "write_file", `{"path":"note.txt","content":"new\n"}`, 10, 4)
			return
		}
		streamText(w, "Written.", 12, 5)
	})

	cfg := testConfig(t)
	cfg.WorkDir = root
	fs := tools.NewFS(root, false)
	a := newTier1Agent(t, cfg, client, coreRegistry(fs), nil, nil)
	sess := a.sessions.New(cfg.Model, root)

	if err := a.Run(context.Background(), sess, "update note"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if data, _ := os.ReadFile(original); string(data) != "new\n" {
		t.Fatalf("file was not updated: %q", data)
	}
	if len(sess.Checkpoints) != 1 {
		t.Fatalf("checkpoints = %+v", sess.Checkpoints)
	}

	files, err := a.Undo(sess)
	if err != nil {
		t.Fatalf("Undo: %v", err)
	}
	if len(files) != 1 || files[0] != "note.txt" {
		t.Errorf("reverted = %v", files)
	}
	if data, _ := os.ReadFile(original); string(data) != "old\n" {
		t.Errorf("file after undo = %q", data)
	}
	if len(sess.Checkpoints) != 0 {
		t.Errorf("checkpoint was not dropped: %+v", sess.Checkpoints)
	}

	saved, err := a.sessions.Load(sess.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(saved.Checkpoints) != 0 {
		t.Errorf("persisted checkpoints = %+v", saved.Checkpoints)
	}
}

func TestUndoRemovesCreatedFile(t *testing.T) {
	root := t.TempDir()
	client, _ := newFakeLLM(t, func(n int, w http.ResponseWriter) {
		if n == 1 {
			streamToolCall(w, "call_w", "write_file", `{"path":"deep/created.txt","content":"x"}`, 10, 4)
			return
		}
		streamText(w, "Created.", 12, 5)
	})

	cfg := testConfig(t)
	cfg.WorkDir = root
	fs := tools.NewFS(root, false)
	a := newTier1Agent(t, cfg, client, coreRegistry(fs), nil, nil)
	sess := a.sessions.New(cfg.Model, root)

	if err := a.Run(context.Background(), sess, "create a file"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	target := filepath.Join(root, "deep", "created.txt")
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("file was not created: %v", err)
	}
	if _, err := a.Undo(sess); err != nil {
		t.Fatalf("Undo: %v", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("created file should have been removed, stat err = %v", err)
	}
}

func TestUndoWithoutCheckpoints(t *testing.T) {
	cfg := testConfig(t)
	a := newTier1Agent(t, cfg, nil, nil, nil, nil)
	sess := a.sessions.New(cfg.Model, cfg.WorkDir)
	if _, err := a.Undo(sess); err == nil || !strings.Contains(err.Error(), "nothing to undo") {
		t.Fatalf("expected nothing-to-undo error, got %v", err)
	}
}

func TestCheckpointDedupesPerPath(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "note.txt"), []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	client, _ := newFakeLLM(t, func(n int, w http.ResponseWriter) {
		switch n {
		case 1:
			streamToolCall(w, "call_1", "write_file", `{"path":"note.txt","content":"one\n"}`, 10, 4)
		case 2:
			streamToolCall(w, "call_2", "write_file", `{"path":"note.txt","content":"two\n"}`, 10, 4)
		default:
			streamText(w, "Done.", 12, 5)
		}
	})

	cfg := testConfig(t)
	cfg.WorkDir = root
	fs := tools.NewFS(root, false)
	a := newTier1Agent(t, cfg, client, coreRegistry(fs), nil, nil)
	sess := a.sessions.New(cfg.Model, root)

	if err := a.Run(context.Background(), sess, "rewrite twice"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(sess.Checkpoints) != 1 || len(sess.Checkpoints[0].Files) != 1 {
		t.Fatalf("checkpoints = %+v", sess.Checkpoints)
	}
	if f := sess.Checkpoints[0].Files[0]; f.Content != "old\n" {
		t.Errorf("snapshot should keep the original content: %+v", f)
	}
	if _, err := a.Undo(sess); err != nil {
		t.Fatalf("Undo: %v", err)
	}
	if data, _ := os.ReadFile(filepath.Join(root, "note.txt")); string(data) != "old\n" {
		t.Errorf("undo restored %q, want original", data)
	}
}

func TestPlanModeBlocksWrites(t *testing.T) {
	root := t.TempDir()
	client, _ := newFakeLLM(t, func(n int, w http.ResponseWriter) {
		if n == 1 {
			streamToolCall(w, "call_w", "write_file", `{"path":"blocked.txt","content":"x"}`, 10, 4)
			return
		}
		streamText(w, "Here is the plan.", 12, 5)
	})

	cfg := testConfig(t)
	cfg.WorkDir = root
	cfg.Approval = "plan"
	fs := tools.NewFS(root, false)
	var events []Event
	a := newTier1Agent(t, cfg, client, coreRegistry(fs), nil, &events)
	sess := a.sessions.New(cfg.Model, root)

	if err := a.Run(context.Background(), sess, "plan the change"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "blocked.txt")); !os.IsNotExist(err) {
		t.Error("plan mode must not create files")
	}
	var toolResult string
	for _, m := range sess.Messages {
		if m.Role == "tool" {
			toolResult = m.Content
		}
	}
	if !strings.Contains(toolResult, "blocked by plan mode") {
		t.Errorf("tool result = %q", toolResult)
	}
	if hasEvent(events, EventDiff) {
		t.Error("plan mode should not emit diffs")
	}
	if hasEvent(events, EventApproval) {
		t.Error("plan mode should not ask for approval")
	}
}

func TestUsageEventsCarryEstimatedCost(t *testing.T) {
	client, _ := newFakeLLM(t, func(n int, w http.ResponseWriter) {
		streamText(w, "hi", 1000, 500)
	})

	cfg := testConfig(t)
	cfg.Pricing = map[string]config.Price{
		"test-model": {InputPerMTok: 1, OutputPerMTok: 2},
	}
	var events []Event
	a := newTier1Agent(t, cfg, client, nil, nil, &events)
	sess := a.sessions.New(cfg.Model, cfg.WorkDir)

	if err := a.Run(context.Background(), sess, "hello"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	usage := findEvent(events, EventUsage)
	if usage == nil {
		t.Fatal("missing usage event")
	}
	const want = 0.002
	if math.Abs(usage.CostUSD-want) > 1e-9 {
		t.Errorf("step cost = %v, want %v", usage.CostUSD, want)
	}
	if math.Abs(usage.TotalCost-want) > 1e-9 {
		t.Errorf("total cost = %v, want %v", usage.TotalCost, want)
	}
	done := findEvent(events, EventDone)
	if done == nil || math.Abs(done.TotalCost-want) > 1e-9 {
		t.Errorf("done event = %+v", done)
	}
	if done != nil && math.Abs(done.CostUSD-want) > 1e-9 {
		t.Errorf("turn cost = %v, want %v", done.CostUSD, want)
	}
}

func TestTurnCostResetsAcrossTurns(t *testing.T) {
	client, _ := newFakeLLM(t, func(n int, w http.ResponseWriter) {
		streamText(w, "ok", 1000, 500)
	})

	cfg := testConfig(t)
	cfg.Pricing = map[string]config.Price{
		"test-model": {InputPerMTok: 1, OutputPerMTok: 2},
	}
	var events []Event
	a := newTier1Agent(t, cfg, client, nil, nil, &events)
	sess := a.sessions.New(cfg.Model, cfg.WorkDir)

	for i := 0; i < 2; i++ {
		if err := a.Run(context.Background(), sess, "hello"); err != nil {
			t.Fatalf("Run %d: %v", i, err)
		}
	}
	var dones []Event
	for _, ev := range events {
		if ev.Type == EventDone {
			dones = append(dones, ev)
		}
	}
	if len(dones) != 2 {
		t.Fatalf("done events = %d", len(dones))
	}
	if math.Abs(dones[0].CostUSD-0.002) > 1e-9 || math.Abs(dones[1].CostUSD-0.002) > 1e-9 {
		t.Errorf("turn costs = %v, %v; want 0.002 each", dones[0].CostUSD, dones[1].CostUSD)
	}
	if math.Abs(dones[1].TotalCost-0.004) > 1e-9 {
		t.Errorf("session cost = %v, want 0.004", dones[1].TotalCost)
	}
}

func TestSubagentUsageCountsTowardSession(t *testing.T) {
	client, _ := newFakeLLM(t, func(n int, w http.ResponseWriter) {
		switch n {
		case 1:
			streamToolCall(w, "call_a", "agent", `{"description":"probe","prompt":"look around"}`, 10, 5)
		case 2:
			streamToolCall(w, "call_e", "echo", `{"text":"hi"}`, 20, 5)
		case 3:
			streamText(w, "subagent report", 30, 5)
		default:
			streamText(w, "final answer", 40, 5)
		}
	})

	cfg := testConfig(t)
	a := newTier1Agent(t, cfg, client, echoRegistry(nil), nil, nil)
	sess := a.sessions.New(cfg.Model, cfg.WorkDir)

	if err := a.Run(context.Background(), sess, "delegate"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sess.Usage.PromptTokens != 100 || sess.Usage.CompletionTokens != 20 {
		t.Errorf("session usage = %+v, want all parent and child calls", sess.Usage)
	}
}

func TestSessionCheckpointRoundTrip(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sess := store.New("m", t.TempDir())
	sess.Checkpoints = []session.Checkpoint{{
		Turn: 1,
		Files: []session.FileSnapshot{{
			Path:    "a.txt",
			Existed: true,
			Mode:    0o600,
			Content: "old",
		}},
	}}
	if err := store.Save(sess); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := store.Load(sess.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded.Checkpoints) != 1 || len(loaded.Checkpoints[0].Files) != 1 {
		t.Fatalf("checkpoints = %+v", loaded.Checkpoints)
	}
	f := loaded.Checkpoints[0].Files[0]
	if f.Path != "a.txt" || !f.Existed || f.Mode != 0o600 || f.Content != "old" {
		t.Errorf("snapshot = %+v", f)
	}
}
