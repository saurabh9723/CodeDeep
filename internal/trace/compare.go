package trace

import (
	"fmt"
	"sort"
	"time"

	"codedeep/internal/agent"
	"codedeep/internal/diff"
)

// maxDiffLines bounds prompt and answer diffs so a huge transcript cannot
// bloat the comparison response.
const maxDiffLines = 400

// IntDelta is a before/after pair and their difference (B - A).
type IntDelta struct {
	A     int64 `json:"a"`
	B     int64 `json:"b"`
	Delta int64 `json:"delta"`
}

// FloatDelta is the float form of IntDelta, used for cost.
type FloatDelta struct {
	A     float64 `json:"a"`
	B     float64 `json:"b"`
	Delta float64 `json:"delta"`
}

// MetricsComparison holds the headline deltas of two traces.
type MetricsComparison struct {
	DurationMS  IntDelta   `json:"duration_ms"`
	ModelCalls  IntDelta   `json:"model_calls"`
	ToolCalls   IntDelta   `json:"tool_calls"`
	TotalTokens IntDelta   `json:"total_tokens"`
	CostUSD     FloatDelta `json:"cost_usd"`
	EventCount  IntDelta   `json:"event_count"`
}

// LineSet is a set difference for free-form strings such as failures.
type LineSet struct {
	OnlyA []string `json:"only_a,omitempty"`
	OnlyB []string `json:"only_b,omitempty"`
	Same  []string `json:"same,omitempty"`
}

// ToolStep is one tool call in a trace's execution sequence.
type ToolStep struct {
	Name     string `json:"name"`
	Depth    int    `json:"depth,omitempty"`
	Failed   bool   `json:"failed,omitempty"`
	OffsetMS int64  `json:"offset_ms,omitempty"`
}

// ToolStepDiff aligns one step from each trace. Op is "same", "added" (only in
// B), or "removed" (only in A).
type ToolStepDiff struct {
	Op string    `json:"op"`
	A  *ToolStep `json:"a,omitempty"`
	B  *ToolStep `json:"b,omitempty"`
}

// ToolComparison is the aligned tool-call sequence of both traces.
type ToolComparison struct {
	A       []ToolStep     `json:"a"`
	B       []ToolStep     `json:"b"`
	Same    int            `json:"same"`
	Added   int            `json:"added"`
	Removed int            `json:"removed"`
	Steps   []ToolStepDiff `json:"steps"`
}

// EventCount is the number of events of one type in each trace.
type EventCount struct {
	Type string `json:"type"`
	A    int    `json:"a"`
	B    int    `json:"b"`
}

// TextDiff is a line diff of the prompt or final answer. Lines are omitted
// when the texts are identical.
type TextDiff struct {
	Changed   bool        `json:"changed"`
	Lines     []diff.Line `json:"lines,omitempty"`
	Truncated bool        `json:"truncated,omitempty"`
}

// Comparison is the full diff of two traces, oriented A -> B.
type Comparison struct {
	A        Meta              `json:"a"`
	B        Meta              `json:"b"`
	SameKind bool              `json:"same_kind"`
	Metrics  MetricsComparison `json:"metrics"`
	Failures LineSet           `json:"failures"`
	Warnings LineSet           `json:"warnings"`
	Tools    ToolComparison    `json:"tools"`
	Events   []EventCount      `json:"events"`
	Prompt   *TextDiff         `json:"prompt,omitempty"`
	Answer   *TextDiff         `json:"answer,omitempty"`
}

// Compare builds a structured comparison of two traces. It never mutates its
// inputs and is safe to call on partially populated traces.
func Compare(a, b Trace) Comparison {
	c := Comparison{A: a.Meta, B: b.Meta, SameKind: a.Kind == b.Kind}
	c.Metrics = metricsComparison(a, b)
	c.Failures = lineSet(a.Failures, b.Failures)
	c.Warnings = lineSet(a.Warnings, b.Warnings)
	c.Tools = compareTools(a, b)
	c.Events = eventCounts(a.Events, b.Events)
	if a.Prompt != "" || b.Prompt != "" {
		c.Prompt = textDiff(a.Prompt, b.Prompt)
	}
	answerA, answerB := finalAnswer(a.Events), finalAnswer(b.Events)
	if answerA != "" || answerB != "" {
		c.Answer = textDiff(answerA, answerB)
	}
	return c
}

func metricsComparison(a, b Trace) MetricsComparison {
	callsA, toolsA, tokensA, costA := metricsOf(a)
	callsB, toolsB, tokensB, costB := metricsOf(b)
	return MetricsComparison{
		DurationMS:  intDelta(a.DurationMS, b.DurationMS),
		ModelCalls:  intDelta(int64(callsA), int64(callsB)),
		ToolCalls:   intDelta(int64(toolsA), int64(toolsB)),
		TotalTokens: intDelta(int64(tokensA), int64(tokensB)),
		CostUSD:     floatDelta(costA, costB),
		EventCount:  intDelta(int64(eventCount(a)), int64(eventCount(b))),
	}
}

// metricsOf prefers the stored summary but reconstructs it for traces that
// were built in memory rather than saved through the store.
func metricsOf(t Trace) (modelCalls, toolCalls, tokens int, cost float64) {
	if t.ModelCalls == 0 && t.ToolCalls == 0 && t.TotalTokens == 0 && t.CostUSD == 0 && len(t.Events) > 0 {
		return Summarize(t.Events)
	}
	return t.ModelCalls, t.ToolCalls, t.TotalTokens, t.CostUSD
}

func eventCount(t Trace) int {
	if t.EventCount > 0 || len(t.Events) == 0 {
		return t.EventCount
	}
	return len(t.Events)
}

func intDelta(a, b int64) IntDelta {
	return IntDelta{A: a, B: b, Delta: b - a}
}

func floatDelta(a, b float64) FloatDelta {
	return FloatDelta{A: a, B: b, Delta: b - a}
}

func lineSet(a, b []string) LineSet {
	setA := stringSet(a)
	setB := stringSet(b)
	var out LineSet
	for _, value := range dedupeStrings(a) {
		if setB[value] {
			out.Same = append(out.Same, value)
		} else {
			out.OnlyA = append(out.OnlyA, value)
		}
	}
	for _, value := range dedupeStrings(b) {
		if !setA[value] {
			out.OnlyB = append(out.OnlyB, value)
		}
	}
	return out
}

func stringSet(values []string) map[string]bool {
	set := make(map[string]bool, len(values))
	for _, value := range values {
		set[value] = true
	}
	return set
}

func dedupeStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func compareTools(a, b Trace) ToolComparison {
	stepsA, stepsB := toolSteps(a), toolSteps(b)
	out := ToolComparison{A: stepsA, B: stepsB}
	key := func(step ToolStep) string { return fmt.Sprintf("%s@%d", step.Name, step.Depth) }
	n, m := len(stepsA), len(stepsB)
	if n*m > 200_000 {
		for i := range stepsA {
			out.Removed++
			out.Steps = append(out.Steps, ToolStepDiff{Op: "removed", A: &stepsA[i]})
		}
		for j := range stepsB {
			out.Added++
			out.Steps = append(out.Steps, ToolStepDiff{Op: "added", B: &stepsB[j]})
		}
		return out
	}

	dp := make([][]int32, n+1)
	for i := range dp {
		dp[i] = make([]int32, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			switch {
			case key(stepsA[i]) == key(stepsB[j]):
				dp[i][j] = dp[i+1][j+1] + 1
			case dp[i+1][j] >= dp[i][j+1]:
				dp[i][j] = dp[i+1][j]
			default:
				dp[i][j] = dp[i][j+1]
			}
		}
	}

	i, j := 0, 0
	for i < n && j < m {
		switch {
		case key(stepsA[i]) == key(stepsB[j]):
			out.Same++
			out.Steps = append(out.Steps, ToolStepDiff{Op: "same", A: &stepsA[i], B: &stepsB[j]})
			i++
			j++
		case dp[i+1][j] >= dp[i][j+1]:
			out.Removed++
			out.Steps = append(out.Steps, ToolStepDiff{Op: "removed", A: &stepsA[i]})
			i++
		default:
			out.Added++
			out.Steps = append(out.Steps, ToolStepDiff{Op: "added", B: &stepsB[j]})
			j++
		}
	}
	for ; i < n; i++ {
		out.Removed++
		out.Steps = append(out.Steps, ToolStepDiff{Op: "removed", A: &stepsA[i]})
	}
	for ; j < m; j++ {
		out.Added++
		out.Steps = append(out.Steps, ToolStepDiff{Op: "added", B: &stepsB[j]})
	}
	return out
}

func toolSteps(t Trace) []ToolStep {
	steps := make([]ToolStep, 0)
	byCall := make(map[string]int)
	for _, ev := range t.Events {
		switch ev.Type {
		case agent.EventToolStart:
			name := ev.Name
			if name == "" {
				name = "tool"
			}
			step := ToolStep{
				Name:     name,
				Depth:    ev.Depth,
				OffsetMS: eventOffset(ev.Time, t.Created),
			}
			if ev.CallID != "" {
				byCall[ev.CallID] = len(steps)
			}
			steps = append(steps, step)
		case agent.EventToolEnd:
			if idx, ok := byCall[ev.CallID]; ok && ev.IsError {
				steps[idx].Failed = true
			}
		}
	}
	return steps
}

func eventOffset(stamp string, start time.Time) int64 {
	t, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil || start.IsZero() {
		return 0
	}
	return t.Sub(start).Milliseconds()
}

func eventCounts(a, b []agent.Event) []EventCount {
	countA := make(map[string]int)
	countB := make(map[string]int)
	for _, ev := range a {
		if ev.Type != "" {
			countA[ev.Type]++
		}
	}
	for _, ev := range b {
		if ev.Type != "" {
			countB[ev.Type]++
		}
	}
	types := make([]string, 0, len(countA)+len(countB))
	for name := range countA {
		types = append(types, name)
	}
	for name := range countB {
		if _, ok := countA[name]; !ok {
			types = append(types, name)
		}
	}
	sort.Slice(types, func(i, j int) bool {
		totalI := countA[types[i]] + countB[types[i]]
		totalJ := countA[types[j]] + countB[types[j]]
		if totalI == totalJ {
			return types[i] < types[j]
		}
		return totalI > totalJ
	})
	out := make([]EventCount, 0, len(types))
	for _, name := range types {
		out = append(out, EventCount{Type: name, A: countA[name], B: countB[name]})
	}
	return out
}

// finalAnswer returns the last top-level text event, which is what the user
// sees as the run's answer.
func finalAnswer(events []agent.Event) string {
	for i := len(events) - 1; i >= 0; i-- {
		ev := events[i]
		if ev.Type == agent.EventText && ev.Depth == 0 && ev.Text != "" {
			return ev.Text
		}
	}
	return ""
}

func textDiff(a, b string) *TextDiff {
	if a == b {
		return &TextDiff{Changed: false}
	}
	lines := diff.Lines(a, b)
	out := &TextDiff{Changed: true}
	if len(lines) > maxDiffLines {
		lines = lines[:maxDiffLines]
		out.Truncated = true
	}
	out.Lines = lines
	return out
}
