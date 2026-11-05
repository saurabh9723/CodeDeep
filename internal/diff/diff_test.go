package diff

import (
	"strings"
	"testing"
)

func TestSplitLines(t *testing.T) {
	tests := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"a", 1},
		{"a\n", 1},
		{"a\nb\n", 2},
		{"a\n\nb", 3},
		{"\n", 1},
	}
	for _, tc := range tests {
		if got := len(SplitLines(tc.in)); got != tc.want {
			t.Errorf("SplitLines(%q) = %d lines, want %d", tc.in, got, tc.want)
		}
	}
}

func TestUnifiedReplace(t *testing.T) {
	got := Unified("src/a.go", "a\nb\nc\n", "a\nB\nc\n", 3)
	if !strings.Contains(got, "--- a/src/a.go") || !strings.Contains(got, "+++ b/src/a.go") {
		t.Fatalf("missing file headers:\n%s", got)
	}
	if !strings.Contains(got, "@@ -1,3 +1,3 @@") {
		t.Fatalf("missing hunk header:\n%s", got)
	}
	if !strings.Contains(got, "\n-b\n") || !strings.Contains(got, "\n+B\n") {
		t.Fatalf("missing changed lines:\n%s", got)
	}
	if strings.Contains(got, "-a") || strings.Contains(got, "+c") {
		t.Fatalf("unchanged lines were marked:\n%s", got)
	}
}

func TestUnifiedInsertAndDelete(t *testing.T) {
	insert := Unified("f.txt", "a\nc\n", "a\nb\nc\n", 3)
	if !strings.Contains(insert, "@@ -1,2 +1,3 @@") || !strings.Contains(insert, "\n+b\n") {
		t.Fatalf("unexpected insert diff:\n%s", insert)
	}
	del := Unified("f.txt", "a\nb\nc\n", "a\nc\n", 3)
	if !strings.Contains(del, "@@ -1,3 +1,2 @@") || !strings.Contains(del, "\n-b\n") {
		t.Fatalf("unexpected delete diff:\n%s", del)
	}
}

func TestUnifiedNewFile(t *testing.T) {
	got := Unified("notes.txt", "", "one\ntwo\n", 3)
	if !strings.HasPrefix(got, "--- /dev/null\n+++ b/notes.txt\n") {
		t.Fatalf("unexpected headers:\n%s", got)
	}
	if !strings.Contains(got, "@@ -0,0 +1,2 @@") {
		t.Fatalf("unexpected hunk header:\n%s", got)
	}
}

func TestUnifiedNoChange(t *testing.T) {
	if got := Unified("f.txt", "same\n", "same\n", 3); got != "" {
		t.Fatalf("expected no diff, got:\n%s", got)
	}
}

func TestUnifiedSeparateHunks(t *testing.T) {
	var oldLines, newLines []string
	for i := 1; i <= 20; i++ {
		line := "line" + string(rune('a'+i-1))
		oldLines = append(oldLines, line)
		if i == 2 || i == 20 {
			line += "-changed"
		}
		newLines = append(newLines, line)
	}
	got := Unified("f.txt", strings.Join(oldLines, "\n")+"\n", strings.Join(newLines, "\n")+"\n", 1)
	if strings.Count(got, "@@ ") != 2 {
		t.Fatalf("expected two hunks:\n%s", got)
	}
	if !strings.Contains(got, "@@ -1,3 +1,3 @@") || !strings.Contains(got, "@@ -19,2 +19,2 @@") {
		t.Fatalf("unexpected hunk ranges:\n%s", got)
	}
}

func TestUnifiedCoarseFallback(t *testing.T) {
	var a, b []string
	for i := 0; i < 900; i++ {
		a = append(a, "old-"+string(rune('a'+i%26)))
		b = append(b, "new-"+string(rune('a'+i%26)))
	}
	a = append(a, "tail")
	b = append(b, "tail")
	got := Unified("big.txt", strings.Join(a, "\n"), strings.Join(b, "\n"), 3)
	if !strings.Contains(got, "@@") || !strings.Contains(got, "-old-") || !strings.Contains(got, "+new-") {
		t.Fatalf("fallback diff looks wrong:\n%s", got[:minInt(len(got), 400)])
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
