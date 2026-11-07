package trace

import (
	"testing"
	"time"

	"codedeep/internal/agent"
	"codedeep/internal/llm"
)

func at(base time.Time, d time.Duration) string {
	return base.Add(d).Format(time.RFC3339Nano)
}

func TestCompareTraces(t *testing.T) {
	start := time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)
	a := Trace{
		Meta: Meta{
			Kind:        KindEval,
			Title:       "baseline",
			Task:        "go-bug-fix",
			Created:     start,
			DurationMS:  1000,
			ModelCalls:  2,
			ToolCalls:   3,
			TotalTokens: 1000,
			CostUSD:     0.010,
			Status:      "failed",
			Failures:    []string{"edit did not apply", "build failed"},
			Warnings:    []string{"slow model"},
		},
		Prompt: "fix the bug",
		Events: []agent.Event{
			{Type: agent.EventUser, Text: "fix the bug", Time: at(start, 0)},
			{Type: agent.EventToolStart, Name: "glob", CallID: "a1", Time: at(start, 100*time.Millisecond)},
			{Type: agent.EventToolEnd, Name: "glob", CallID: "a1", Time: at(start, 150*time.Millisecond)},
			{Type: agent.EventToolStart, Name: "read_file", CallID: "a2", Time: at(start, 200*time.Millisecond)},
			{Type: agent.EventToolEnd, Name: "read_file", CallID: "a2", IsError: true, Time: at(start, 250*time.Millisecond)},
			{Type: agent.EventToolStart, Name: "edit_file", CallID: "a3", Time: at(start, 300*time.Millisecond)},
			{Type: agent.EventText, Text: "I could not fix it.", Time: at(start, 900*time.Millisecond)},
			{Type: agent.EventError, Message: "verification failed", Time: at(start, 950*time.Millisecond)},
		},
	}
	b := Trace{
		Meta: Meta{
			Kind:        KindEval,
			Title:       "candidate",
			Task:        "go-bug-fix",
			Created:     start.Add(time.Hour),
			DurationMS:  800,
			ModelCalls:  3,
			ToolCalls:   3,
			TotalTokens: 900,
			CostUSD:     0.008,
			Status:      "passed",
			Failures:    []string{"build failed"},
		},
		Prompt: "fix the bug",
		Events: []agent.Event{
			{Type: agent.EventUser, Text: "fix the bug"},
			{Type: agent.EventToolStart, Name: "glob", CallID: "b1"},
			{Type: agent.EventToolEnd, Name: "glob", CallID: "b1"},
			{Type: agent.EventToolStart, Name: "grep", CallID: "b2"},
			{Type: agent.EventToolEnd, Name: "grep", CallID: "b2"},
			{Type: agent.EventToolStart, Name: "read_file", CallID: "b3"},
			{Type: agent.EventToolEnd, Name: "read_file", CallID: "b3"},
			{Type: agent.EventToolStart, Name: "edit_file", CallID: "b4"},
			{Type: agent.EventToolEnd, Name: "edit_file", CallID: "b4"},
			{Type: agent.EventText, Text: "Fixed the bug."},
		},
	}

	cmp := Compare(a, b)
	if cmp.A.ID != "" || cmp.B.Title != "candidate" || !cmp.SameKind {
		t.Fatalf("headers = %+v / %+v sameKind=%v", cmp.A, cmp.B, cmp.SameKind)
	}
	if cmp.Metrics.DurationMS.Delta != -200 || cmp.Metrics.ModelCalls.Delta != 1 {
		t.Fatalf("deltas = %+v", cmp.Metrics)
	}
	if cmp.Metrics.ToolCalls.Delta != 0 || cmp.Metrics.TotalTokens.Delta != -100 {
		t.Fatalf("call/token deltas = %+v", cmp.Metrics)
	}
	if cmp.Metrics.CostUSD.Delta > -0.0019 || cmp.Metrics.CostUSD.Delta < -0.0021 {
		t.Fatalf("cost delta = %v", cmp.Metrics.CostUSD.Delta)
	}
	if len(cmp.Failures.OnlyA) != 1 || cmp.Failures.OnlyA[0] != "edit did not apply" ||
		len(cmp.Failures.OnlyB) != 0 || len(cmp.Failures.Same) != 1 {
		t.Fatalf("failure sets = %+v", cmp.Failures)
	}
	if len(cmp.Warnings.OnlyA) != 1 || len(cmp.Warnings.OnlyB) != 0 {
		t.Fatalf("warning sets = %+v", cmp.Warnings)
	}

	if cmp.Tools.Same != 3 || cmp.Tools.Added != 1 || cmp.Tools.Removed != 0 {
		t.Fatalf("tool counts = %+v", cmp.Tools)
	}
	wantOps := []string{"same", "added", "same", "same"}
	if len(cmp.Tools.Steps) != len(wantOps) {
		t.Fatalf("tool steps = %+v", cmp.Tools.Steps)
	}
	for i, want := range wantOps {
		if cmp.Tools.Steps[i].Op != want {
			t.Fatalf("step %d op = %q, want %q", i, cmp.Tools.Steps[i].Op, want)
		}
	}
	if cmp.Tools.Steps[1].B == nil || cmp.Tools.Steps[1].B.Name != "grep" {
		t.Fatalf("added step = %+v", cmp.Tools.Steps[1])
	}
	if !cmp.Tools.A[1].Failed {
		t.Fatalf("failed tool call not marked: %+v", cmp.Tools.A[1])
	}
	if cmp.Tools.A[1].OffsetMS != 200 {
		t.Fatalf("offset = %d, want 200", cmp.Tools.A[1].OffsetMS)
	}

	if cmp.Prompt == nil || cmp.Prompt.Changed {
		t.Fatalf("identical prompt should not be a change: %+v", cmp.Prompt)
	}
	if cmp.Answer == nil || !cmp.Answer.Changed {
		t.Fatalf("answer diff missing: %+v", cmp.Answer)
	}
	var added, removed string
	for _, line := range cmp.Answer.Lines {
		switch line.Op {
		case "+":
			added += line.Text
		case "-":
			removed += line.Text
		}
	}
	if added != "Fixed the bug." || removed != "I could not fix it." {
		t.Fatalf("answer diff = %q / %q", removed, added)
	}

	if len(cmp.Events) == 0 {
		t.Fatal("event counts empty")
	}
	var sawToolStart bool
	for _, count := range cmp.Events {
		if count.Type == agent.EventToolStart {
			sawToolStart = count.A == 3 && count.B == 4
		}
	}
	if !sawToolStart {
		t.Fatalf("tool_start count missing: %+v", cmp.Events)
	}
}

func TestCompareIdenticalAndDerivedMetrics(t *testing.T) {
	start := time.Now().UTC()
	usage := &llm.Usage{TotalTokens: 120}
	events := []agent.Event{
		{Type: agent.EventUser, Text: "hello"},
		{Type: agent.EventToolStart, Name: "read_file"},
		{Type: agent.EventToolEnd, Name: "read_file"},
		{Type: agent.EventUsage, Usage: usage, CostUSD: 0.002},
		{Type: agent.EventText, Text: "hi"},
	}
	tr := Trace{Meta: Meta{Kind: KindSession, Title: "same", Created: start}, Prompt: "hello", Events: events}

	cmp := Compare(tr, tr)
	if cmp.Metrics.ModelCalls.A != 1 || cmp.Metrics.ToolCalls.B != 1 || cmp.Metrics.TotalTokens.A != 120 {
		t.Fatalf("derived metrics = %+v", cmp.Metrics)
	}
	if cmp.Metrics.CostUSD.A != 0.002 || cmp.Metrics.EventCount.A != 5 {
		t.Fatalf("derived cost/events = %+v", cmp.Metrics)
	}
	if cmp.Metrics.DurationMS.Delta != 0 || cmp.Metrics.TotalTokens.Delta != 0 {
		t.Fatalf("identical deltas = %+v", cmp.Metrics)
	}
	if cmp.Tools.Added != 0 || cmp.Tools.Removed != 0 || cmp.Tools.Same != 1 {
		t.Fatalf("identical tools = %+v", cmp.Tools)
	}
	if cmp.Prompt == nil || cmp.Prompt.Changed || cmp.Answer == nil || cmp.Answer.Changed {
		t.Fatalf("identical text changed: prompt=%+v answer=%+v", cmp.Prompt, cmp.Answer)
	}
	found := false
	for _, count := range cmp.Events {
		if count.Type == agent.EventToolStart && count.A == 1 && count.B == 1 {
			found = true
		}
	}
	if !found {
		t.Fatalf("event counts = %+v", cmp.Events)
	}
}
