package agent

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codedeep/internal/session"
	"codedeep/internal/tools"
)

func TestCheckpointCapturesTurnDiff(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "note.txt"), []byte("old\n"), 0o644); err != nil {
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
	if len(sess.Checkpoints) != 1 {
		t.Fatalf("checkpoints = %+v", sess.Checkpoints)
	}
	cp := sess.Checkpoints[0]
	if !strings.Contains(cp.Diff, "-old") || !strings.Contains(cp.Diff, "+new") {
		t.Errorf("checkpoint diff = %q", cp.Diff)
	}
	if cp.Added != 1 || cp.Removed != 1 {
		t.Errorf("diff stats = +%d/-%d, want +1/-1", cp.Added, cp.Removed)
	}

	saved, err := a.sessions.Load(sess.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(saved.Checkpoints) != 1 || saved.Checkpoints[0].Diff != cp.Diff {
		t.Errorf("diff was not persisted: %+v", saved.Checkpoints)
	}
}

func TestCheckpointCapturesEveryFileInAtomicPatch(t *testing.T) {
	root := t.TempDir()
	for name, content := range map[string]string{"a.txt": "a0\n", "b.txt": "b0\n", "c.txt": "c0\n"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	client, _ := newFakeLLM(t, func(n int, w http.ResponseWriter) {
		if n == 1 {
			streamToolCall(w, "call_patch", "apply_patch",
				`{"edits":[{"path":"a.txt","old_string":"a0","new_string":"a1"},`+
					`{"path":"b.txt","old_string":"b0","new_string":"b1"},`+
					`{"path":"c.txt","old_string":"c0","new_string":"c1"}]}`, 10, 6)
			return
		}
		streamText(w, "Patched.", 12, 5)
	})

	cfg := testConfig(t)
	cfg.WorkDir = root
	fs := tools.NewFS(root, false)
	a := newTier1Agent(t, cfg, client, coreRegistry(fs), nil, nil)
	sess := a.sessions.New(cfg.Model, root)

	if err := a.Run(context.Background(), sess, "patch all three"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for name, want := range map[string]string{"a.txt": "a1\n", "b.txt": "b1\n", "c.txt": "c1\n"} {
		data, _ := os.ReadFile(filepath.Join(root, name))
		if string(data) != want {
			t.Errorf("%s = %q, want %q", name, data, want)
		}
	}
	if len(sess.Checkpoints) != 1 {
		t.Fatalf("checkpoints = %+v", sess.Checkpoints)
	}
	cp := sess.Checkpoints[0]
	if len(cp.Files) != 3 {
		t.Fatalf("checkpoint files = %d, want 3: %+v", len(cp.Files), cp.Files)
	}
	for _, want := range []string{"a.txt", "b.txt", "c.txt", "-a0", "+a1", "-c0", "+c1"} {
		if !strings.Contains(cp.Diff, want) {
			t.Errorf("checkpoint diff missing %q:\n%s", want, cp.Diff)
		}
	}

	restored, err := a.UndoAt(sess, 0)
	if err != nil {
		t.Fatalf("UndoAt: %v", err)
	}
	if len(restored) != 3 {
		t.Errorf("restored = %v, want 3 files", restored)
	}
	for name, want := range map[string]string{"a.txt": "a0\n", "b.txt": "b0\n", "c.txt": "c0\n"} {
		data, _ := os.ReadFile(filepath.Join(root, name))
		if string(data) != want {
			t.Errorf("after undo %s = %q, want %q", name, data, want)
		}
	}
}

func TestCheckpointDiffCoversCreatedFile(t *testing.T) {
	root := t.TempDir()
	client, _ := newFakeLLM(t, func(n int, w http.ResponseWriter) {
		if n == 1 {
			streamToolCall(w, "call_w", "write_file", `{"path":"made.txt","content":"fresh\n"}`, 10, 4)
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
	if len(sess.Checkpoints) != 1 {
		t.Fatalf("checkpoints = %+v", sess.Checkpoints)
	}
	cp := sess.Checkpoints[0]
	if !strings.Contains(cp.Diff, "+fresh") {
		t.Errorf("diff for a created file = %q", cp.Diff)
	}
	if cp.Added != 1 || cp.Removed != 0 {
		t.Errorf("diff stats = +%d/-%d, want +1/-0", cp.Added, cp.Removed)
	}
}

func TestUndoAtRollsBackThroughCheckpoints(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("v0\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	turn := 0
	client, _ := newFakeLLM(t, func(n int, w http.ResponseWriter) {
		if n%2 == 1 {
			turn++
			switch turn {
			case 1:
				streamToolCall(w, "c1", "write_file", `{"path":"a.txt","content":"v1\n"}`, 10, 4)
			case 2:
				streamToolCall(w, "c2", "write_file", `{"path":"a.txt","content":"v2\n"}`, 10, 4)
			default:
				streamToolCalls(w, []toolCallSpec{
					{id: "c3", name: "write_file", args: `{"path":"a.txt","content":"v3\n"}`},
					{id: "c4", name: "write_file", args: `{"path":"b.txt","content":"bee\n"}`},
				})
			}
			return
		}
		streamText(w, "Done.", 12, 5)
	})

	cfg := testConfig(t)
	cfg.WorkDir = root
	fs := tools.NewFS(root, false)
	a := newTier1Agent(t, cfg, client, coreRegistry(fs), nil, nil)
	sess := a.sessions.New(cfg.Model, root)

	for i := 0; i < 3; i++ {
		if err := a.Run(context.Background(), sess, "mutate"); err != nil {
			t.Fatalf("Run %d: %v", i+1, err)
		}
	}
	if data, _ := os.ReadFile(filepath.Join(root, "a.txt")); string(data) != "v3\n" {
		t.Fatalf("a.txt = %q, want v3", data)
	}
	if _, err := os.Stat(filepath.Join(root, "b.txt")); err != nil {
		t.Fatalf("b.txt was not created: %v", err)
	}
	if len(sess.Checkpoints) != 3 {
		t.Fatalf("checkpoints = %d, want 3", len(sess.Checkpoints))
	}

	files, err := a.UndoAt(sess, 1)
	if err != nil {
		t.Fatalf("UndoAt: %v", err)
	}
	if len(files) != 2 {
		t.Errorf("reverted = %v, want a.txt and b.txt", files)
	}
	if data, _ := os.ReadFile(filepath.Join(root, "a.txt")); string(data) != "v1\n" {
		t.Errorf("a.txt after rollback = %q, want v1", data)
	}
	if _, err := os.Stat(filepath.Join(root, "b.txt")); !os.IsNotExist(err) {
		t.Errorf("b.txt should have been removed, stat err = %v", err)
	}
	if len(sess.Checkpoints) != 1 {
		t.Errorf("checkpoints after rollback = %d, want 1", len(sess.Checkpoints))
	}

	saved, err := a.sessions.Load(sess.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(saved.Checkpoints) != 1 || saved.Checkpoints[0].Turn != 1 {
		t.Errorf("persisted checkpoints = %+v, want only turn 1", saved.Checkpoints)
	}
}

func TestUndoAtRejectsBadIndex(t *testing.T) {
	cfg := testConfig(t)
	a := newTier1Agent(t, cfg, nil, nil, nil, nil)
	sess := a.sessions.New(cfg.Model, cfg.WorkDir)
	sess.Checkpoints = []session.Checkpoint{
		{Turn: 1, Files: []session.FileSnapshot{{Path: "x.txt", Existed: true, Content: "x\n"}}},
		{Turn: 2, Files: []session.FileSnapshot{{Path: "y.txt", Existed: true, Content: "y\n"}}},
	}
	if _, err := a.UndoAt(sess, -1); err == nil || !strings.Contains(err.Error(), "out of range") {
		t.Errorf("UndoAt(-1) = %v, want out-of-range error", err)
	}
	if _, err := a.UndoAt(sess, 2); err == nil || !strings.Contains(err.Error(), "out of range") {
		t.Errorf("UndoAt(2) = %v, want out-of-range error", err)
	}
	if len(sess.Checkpoints) != 2 {
		t.Errorf("bad-index calls must not touch checkpoints, got %d", len(sess.Checkpoints))
	}
}

func TestUndoAtEmptyCheckpoint(t *testing.T) {
	cfg := testConfig(t)
	a := newTier1Agent(t, cfg, nil, nil, nil, nil)
	sess := a.sessions.New(cfg.Model, cfg.WorkDir)
	sess.Checkpoints = []session.Checkpoint{
		{Turn: 1},
		{Turn: 2, Files: []session.FileSnapshot{{Path: "y.txt", Existed: true, Content: "y\n"}}},
	}
	if _, err := a.UndoAt(sess, 0); err == nil || !strings.Contains(err.Error(), "no file changes") {
		t.Errorf("UndoAt on empty checkpoint = %v, want no-file-changes error", err)
	}
	if len(sess.Checkpoints) != 2 {
		t.Errorf("empty checkpoint must be kept, got %d", len(sess.Checkpoints))
	}
}
