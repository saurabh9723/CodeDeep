package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestApplyPatchMultiFile(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "arith.go", "func Add(a, b int) int {\n\treturn a + b\n}\n")
	writeTestFile(t, root, "label.go", "func Label(a, b int) string {\n\treturn fmt.Sprint(Add(a, b))\n}\n")
	writeTestFile(t, root, "stats.go", "func Average(a, b int) float64 {\n\treturn float64(Add(a, b)) / 2\n}\n")
	f := NewFS(root, false)

	res, err := callTool(t, f.applyPatch, map[string]any{
		"edits": []map[string]any{
			{"path": "arith.go", "old_string": "func Add(", "new_string": "func Sum("},
			{"path": "label.go", "old_string": "Add(a, b)", "new_string": "Sum(a, b)"},
			{"path": "stats.go", "old_string": "Add(a, b)", "new_string": "Sum(a, b)"},
		},
	})
	if err != nil {
		t.Fatalf("apply_patch: %v", err)
	}
	for _, rel := range []string{"arith.go", "label.go", "stats.go"} {
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "Add(") || !strings.Contains(string(data), "Sum(") {
			t.Errorf("%s was not patched:\n%s", rel, data)
		}
	}
	if len(res.Changes) != 3 {
		t.Fatalf("changes = %d, want 3", len(res.Changes))
	}
	for _, ch := range res.Changes {
		if !ch.Existed || ch.Old == ch.New || ch.Mode == 0 {
			t.Errorf("incomplete change: %+v", ch)
		}
		if !strings.Contains(ch.Old, "Add(") || !strings.Contains(ch.New, "Sum(") {
			t.Errorf("change content = %+v", ch)
		}
	}
	if !strings.Contains(res.Output, "applied 3 edit(s) to 3 file(s)") {
		t.Errorf("unexpected output:\n%s", res.Output)
	}
}

func TestApplyPatchRepeatedFileHunks(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "dup.txt", "one\ntwo\none\n")
	f := NewFS(root, false)

	res, err := callTool(t, f.applyPatch, map[string]any{
		"edits": []map[string]any{
			{"path": "dup.txt", "old_string": "one", "new_string": "1", "replace_all": true},
			{"path": "dup.txt", "old_string": "two", "new_string": "2"},
		},
	})
	if err != nil {
		t.Fatalf("apply_patch: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(root, "dup.txt"))
	if got := string(data); got != "1\n2\n1\n" {
		t.Errorf("content = %q", got)
	}
	if len(res.Changes) != 1 {
		t.Fatalf("changes = %d, want 1", len(res.Changes))
	}
	ch := res.Changes[0]
	if ch.Old != "one\ntwo\none\n" || ch.New != "1\n2\n1\n" {
		t.Errorf("change = %+v", ch)
	}
}

func TestApplyPatchValidationFailureWritesNothing(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "a.txt", "alpha\n")
	writeTestFile(t, root, "b.txt", "beta\n")
	f := NewFS(root, false)

	res, err := callTool(t, f.applyPatch, map[string]any{
		"edits": []map[string]any{
			{"path": "a.txt", "old_string": "alpha", "new_string": "A"},
			{"path": "b.txt", "old_string": "missing", "new_string": "B"},
		},
	})
	if err == nil {
		t.Fatal("expected apply_patch to reject the unmatched edit")
	}
	if !strings.Contains(err.Error(), "edit 2") || !strings.Contains(err.Error(), "was not found") {
		t.Errorf("error = %v", err)
	}
	if len(res.Changes) != 0 {
		t.Errorf("failed patch returned changes: %+v", res.Changes)
	}
	for rel, want := range map[string]string{"a.txt": "alpha\n", "b.txt": "beta\n"} {
		data, _ := os.ReadFile(filepath.Join(root, rel))
		if string(data) != want {
			t.Errorf("%s = %q, want %q", rel, data, want)
		}
	}

	if _, err := callPreview(t, f.previewPatch, map[string]any{
		"edits": []map[string]any{
			{"path": "a.txt", "old_string": "missing", "new_string": "A"},
		},
	}); err == nil {
		t.Fatal("preview should reject an unmatched edit")
	}
}

func TestApplyPatchRollsBackWhenWriteFails(t *testing.T) {
	root := t.TempDir()
	aPath := writeTestFile(t, root, "a.txt", "alpha\n")
	bPath := writeTestFile(t, root, "b.txt", "beta\n")
	if err := os.Chmod(bPath, 0o444); err != nil {
		t.Skipf("cannot make a read-only file: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(bPath, 0o644) })
	if err := os.WriteFile(bPath, []byte("beta\n"), 0o644); err == nil {
		t.Skip("platform allows writes to read-only files")
	}
	f := NewFS(root, false)

	_, err := callTool(t, f.applyPatch, map[string]any{
		"edits": []map[string]any{
			{"path": "a.txt", "old_string": "alpha", "new_string": "A"},
			{"path": "b.txt", "old_string": "beta", "new_string": "B"},
		},
	})
	if err == nil {
		t.Fatal("expected the read-only target to fail the patch")
	}
	data, _ := os.ReadFile(aPath)
	if got := string(data); got != "alpha\n" {
		t.Errorf("a.txt was not rolled back: %q", got)
	}
}

func TestApplyPatchAmbiguousMatch(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "dup.txt", "dup dup\n")
	f := NewFS(root, false)

	_, err := callTool(t, f.applyPatch, map[string]any{
		"edits": []map[string]any{
			{"path": "dup.txt", "old_string": "dup", "new_string": "x"},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "2 times") {
		t.Fatalf("expected ambiguous-match error, got %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(root, "dup.txt"))
	if string(data) != "dup dup\n" {
		t.Errorf("ambiguous patch modified the file: %q", data)
	}
}

func TestApplyPatchPreviewCombinesFiles(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "a.txt", "alpha\n")
	writeTestFile(t, root, "b.txt", "beta\n")
	f := NewFS(root, false)

	p, err := callPreview(t, f.previewPatch, map[string]any{
		"edits": []map[string]any{
			{"path": "a.txt", "old_string": "alpha", "new_string": "A"},
			{"path": "b.txt", "old_string": "beta", "new_string": "B"},
		},
	})
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if p == nil {
		t.Fatal("preview is nil")
	}
	if p.Path != "a.txt, b.txt" {
		t.Errorf("preview path = %q", p.Path)
	}
	for _, want := range []string{"a.txt", "b.txt", "-alpha", "+A", "-beta", "+B"} {
		if !strings.Contains(p.Diff, want) {
			t.Errorf("preview diff missing %q:\n%s", want, p.Diff)
		}
	}
}

func TestApplyPatchRejectsUnsafeAndMissingPaths(t *testing.T) {
	root := t.TempDir()
	f := NewFS(root, false)
	_, err := callTool(t, f.applyPatch, map[string]any{
		"edits": []map[string]any{
			{"path": "../escape.txt", "old_string": "a", "new_string": "b"},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "outside the workspace") {
		t.Fatalf("expected escape error, got %v", err)
	}
	_, err = callTool(t, f.applyPatch, map[string]any{
		"edits": []map[string]any{
			{"path": "missing.txt", "old_string": "a", "new_string": "b"},
		},
	})
	if err == nil {
		t.Fatal("expected missing-file error")
	}
}

func TestApplyPatchRequiresEdits(t *testing.T) {
	f := NewFS(t.TempDir(), false)
	if _, err := callTool(t, f.applyPatch, map[string]any{}); err == nil || !strings.Contains(err.Error(), "edits is required") {
		t.Fatalf("expected edits-required error, got %v", err)
	}
}
