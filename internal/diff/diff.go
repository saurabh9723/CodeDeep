// Package diff produces unified diffs for edit previews and checkpoints.
package diff

import (
	"fmt"
	"strings"
)

const maxDPCells = 250_000

type opKind byte

const (
	opEqual  opKind = ' '
	opDelete opKind = '-'
	opInsert opKind = '+'
)

type op struct {
	kind opKind
	text string
	aPos int
	bPos int
}

// SplitLines splits text into lines without a trailing empty line, so a file
// that ends in a newline does not gain a phantom line.
func SplitLines(text string) []string {
	if text == "" {
		return nil
	}
	lines := strings.Split(text, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// Line is one line of a structured diff. Op is ' ' (unchanged), '-' (removed
// from the old text), or '+' (added in the new text).
type Line struct {
	Op   string `json:"op"`
	Text string `json:"text"`
}

// Lines returns a line-by-line diff between oldText and newText. Unlike
// Unified, it keeps every line and emits no hunk headers, which makes it
// suitable for rendering side-by-side comparisons in a UI.
func Lines(oldText, newText string) []Line {
	if oldText == newText {
		return nil
	}
	ops := diffLines(SplitLines(oldText), SplitLines(newText))
	out := make([]Line, 0, len(ops))
	for _, o := range ops {
		out = append(out, Line{Op: string(o.kind), Text: o.text})
	}
	return out
}

// Unified renders a unified diff between oldText and newText. It returns an
// empty string when the inputs are identical.
func Unified(path, oldText, newText string, contextLines int) string {
	if oldText == newText {
		return ""
	}
	if contextLines <= 0 {
		contextLines = 3
	}
	a := SplitLines(oldText)
	b := SplitLines(newText)
	ops := diffLines(a, b)

	var hunks [][2]int
	for i := 0; i < len(ops); {
		if ops[i].kind == opEqual {
			i++
			continue
		}
		start := i
		end := i
		for j := i + 1; j < len(ops); j++ {
			if ops[j].kind != opEqual {
				end = j
				continue
			}
			if j-end > 2*contextLines {
				break
			}
		}
		hunks = append(hunks, [2]int{start, end})
		i = end + 1
	}
	if len(hunks) == 0 {
		return ""
	}

	name := path
	if name == "" {
		name = "file"
	}
	name = strings.ReplaceAll(name, "\\", "/")

	var out strings.Builder
	if len(a) == 0 {
		out.WriteString("--- /dev/null\n")
	} else {
		fmt.Fprintf(&out, "--- a/%s\n", name)
	}
	if len(b) == 0 {
		out.WriteString("+++ /dev/null\n")
	} else {
		fmt.Fprintf(&out, "+++ b/%s\n", name)
	}

	lastEnd := -1
	for _, h := range hunks {
		start := h[0] - contextLines
		if start < 0 {
			start = 0
		}
		if start <= lastEnd {
			start = lastEnd + 1
		}
		end := h[1] + contextLines
		if end >= len(ops) {
			end = len(ops) - 1
		}
		if end < start {
			continue
		}
		lastEnd = end

		aStart, bStart := ops[start].aPos, ops[start].bPos
		aCount, bCount := 0, 0
		for i := start; i <= end; i++ {
			switch ops[i].kind {
			case opEqual:
				aCount++
				bCount++
			case opDelete:
				aCount++
			case opInsert:
				bCount++
			}
		}
		if aCount > 0 {
			aStart++
		}
		if bCount > 0 {
			bStart++
		}
		fmt.Fprintf(&out, "@@ -%d,%d +%d,%d @@\n", aStart, aCount, bStart, bCount)
		for i := start; i <= end; i++ {
			out.WriteByte(byte(ops[i].kind))
			out.WriteString(ops[i].text)
			out.WriteByte('\n')
		}
	}
	return out.String()
}

func diffLines(a, b []string) []op {
	prefix := 0
	for prefix < len(a) && prefix < len(b) && a[prefix] == b[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(a)-prefix && suffix < len(b)-prefix &&
		a[len(a)-1-suffix] == b[len(b)-1-suffix] {
		suffix++
	}

	ops := make([]op, 0, len(a)+len(b))
	aPos, bPos := 0, 0
	appendLine := func(kind opKind, text string) {
		ops = append(ops, op{kind: kind, text: text, aPos: aPos, bPos: bPos})
		switch kind {
		case opEqual:
			aPos++
			bPos++
		case opDelete:
			aPos++
		case opInsert:
			bPos++
		}
	}

	for i := 0; i < prefix; i++ {
		appendLine(opEqual, a[i])
	}
	for _, o := range diffMiddle(a[prefix:len(a)-suffix], b[prefix:len(b)-suffix]) {
		appendLine(o.kind, o.text)
	}
	for i := len(a) - suffix; i < len(a); i++ {
		appendLine(opEqual, a[i])
	}
	return ops
}

func diffMiddle(a, b []string) []op {
	n, m := len(a), len(b)
	if n == 0 {
		out := make([]op, 0, m)
		for _, line := range b {
			out = append(out, op{kind: opInsert, text: line})
		}
		return out
	}
	if m == 0 {
		out := make([]op, 0, n)
		for _, line := range a {
			out = append(out, op{kind: opDelete, text: line})
		}
		return out
	}
	if n*m > maxDPCells {
		return coarseDiff(a, b)
	}

	dp := make([][]int32, n+1)
	for i := range dp {
		dp[i] = make([]int32, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			switch {
			case a[i] == b[j]:
				dp[i][j] = dp[i+1][j+1] + 1
			case dp[i+1][j] >= dp[i][j+1]:
				dp[i][j] = dp[i+1][j]
			default:
				dp[i][j] = dp[i][j+1]
			}
		}
	}

	out := make([]op, 0, n+m)
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			out = append(out, op{kind: opEqual, text: a[i]})
			i++
			j++
		case dp[i+1][j] >= dp[i][j+1]:
			out = append(out, op{kind: opDelete, text: a[i]})
			i++
		default:
			out = append(out, op{kind: opInsert, text: b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		out = append(out, op{kind: opDelete, text: a[i]})
	}
	for ; j < m; j++ {
		out = append(out, op{kind: opInsert, text: b[j]})
	}
	return out
}

// coarseDiff is the fallback for very large rewrites, where a line-level LCS
// would cost too much memory.
func coarseDiff(a, b []string) []op {
	out := make([]op, 0, len(a)+len(b))
	for _, line := range a {
		out = append(out, op{kind: opDelete, text: line})
	}
	for _, line := range b {
		out = append(out, op{kind: opInsert, text: line})
	}
	return out
}
