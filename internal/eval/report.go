package eval

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

const (
	StatusPassed  = "passed"
	StatusFailed  = "failed"
	StatusSkipped = "skipped"
	StatusError   = "error"
)

// VerifierResult is the outcome of a task's external verification command.
type VerifierResult struct {
	Command    string `json:"command"`
	ExitCode   int    `json:"exit_code"`
	Output     string `json:"output,omitempty"`
	DurationMS int64  `json:"duration_ms"`
	Error      string `json:"error,omitempty"`
}

// RunResult is one execution of a task (repeats produce multiple runs).
type RunResult struct {
	Passed       bool            `json:"passed"`
	ExitCode     int             `json:"exit_code"`
	TimedOut     bool            `json:"timed_out,omitempty"`
	Failures     []string        `json:"failures,omitempty"`
	Warnings     []string        `json:"warnings,omitempty"`
	Metrics      Metrics         `json:"metrics"`
	Text         string          `json:"text,omitempty"`
	Errors       []string        `json:"errors,omitempty"`
	Verifies     []VerifyRecord  `json:"verifies,omitempty"`
	Verifier     *VerifierResult `json:"verifier,omitempty"`
	MockRequests int             `json:"mock_requests,omitempty"`
	StderrTail   string          `json:"stderr_tail,omitempty"`
	HarnessError string          `json:"harness_error,omitempty"`
}

// TaskResult aggregates repeated runs of one task.
type TaskResult struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Tags        []string       `json:"tags,omitempty"`
	Status      string         `json:"status"`
	Passed      bool           `json:"passed"`
	PassRate    float64        `json:"pass_rate"`
	SkipReason  string         `json:"skip_reason,omitempty"`
	Runs        []RunResult    `json:"runs,omitempty"`
	Failures    []string       `json:"failures,omitempty"`
	Warnings    []string       `json:"warnings,omitempty"`
	Metrics     Metrics        `json:"metrics"`
	Baseline    *BaselineDelta `json:"baseline,omitempty"`
	Workspace   string         `json:"workspace,omitempty"`
}

// BaselineDelta compares one task against its baseline.
type BaselineDelta struct {
	Status       string   `json:"status"`
	Reasons      []string `json:"reasons,omitempty"`
	TokensPct    float64  `json:"tokens_pct,omitempty"`
	CostPct      float64  `json:"cost_pct,omitempty"`
	CallsPct     float64  `json:"calls_pct,omitempty"`
	ToolCallsPct float64  `json:"tool_calls_pct,omitempty"`
	TimePct      float64  `json:"time_pct,omitempty"`
}

// BaselineComparison summarizes the run against a baseline file.
type BaselineComparison struct {
	Path         string   `json:"path"`
	Regressions  int      `json:"regressions"`
	Improvements int      `json:"improvements"`
	Warnings     []string `json:"warnings,omitempty"`
}

// Summary is the run-level rollup.
type Summary struct {
	Total       int     `json:"total"`
	Passed      int     `json:"passed"`
	Failed      int     `json:"failed"`
	Skipped     int     `json:"skipped"`
	Errors      int     `json:"errors"`
	Flaky       int     `json:"flaky,omitempty"`
	ModelCalls  int     `json:"model_calls"`
	ToolCalls   int     `json:"tool_calls"`
	TotalTokens int     `json:"total_tokens"`
	CostUSD     float64 `json:"cost_usd"`
	DurationMS  int64   `json:"duration_ms"`
}

// Report is the complete machine-readable result of a harness run.
type Report struct {
	Generated  time.Time           `json:"generated"`
	Suite      string              `json:"suite"`
	Mode       Mode                `json:"mode"`
	Provider   string              `json:"provider,omitempty"`
	Model      string              `json:"model,omitempty"`
	Tasks      []TaskResult        `json:"tasks"`
	Summary    Summary             `json:"summary"`
	Baseline   *BaselineComparison `json:"baseline,omitempty"`
	HarnessErr string              `json:"harness_error,omitempty"`
}

func summarize(tasks []TaskResult, durationMS int64) Summary {
	summary := Summary{Total: len(tasks), DurationMS: durationMS}
	for _, task := range tasks {
		switch task.Status {
		case StatusPassed:
			summary.Passed++
			if task.PassRate < 1 {
				summary.Flaky++
			}
		case StatusFailed:
			summary.Failed++
		case StatusSkipped:
			summary.Skipped++
		case StatusError:
			summary.Errors++
		}
		if task.Status == StatusSkipped {
			continue
		}
		summary.ModelCalls += task.Metrics.ModelCalls
		summary.ToolCalls += task.Metrics.ToolCalls
		summary.TotalTokens += task.Metrics.Usage.TotalTokens
		summary.CostUSD += task.Metrics.CostUSD
	}
	return summary
}

// WriteJSON encodes a report with stable indentation.
func WriteJSON(w io.Writer, report Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(report)
}

// WriteHuman renders a terminal scorecard.
func WriteHuman(w io.Writer, report Report, verbose bool) {
	fmt.Fprintf(w, "CodeDeep evals · %s mode · %d task(s)\n", report.Mode, len(report.Tasks))
	if report.Provider != "" || report.Model != "" {
		label := strings.TrimSpace(report.Provider + " " + report.Model)
		fmt.Fprintf(w, "provider %s\n", label)
	}
	if report.Suite != "" {
		fmt.Fprintf(w, "suite    %s\n", report.Suite)
	}
	fmt.Fprintln(w)
	for _, task := range report.Tasks {
		line := fmt.Sprintf("%-4s %-28s", strings.ToUpper(task.Status), task.Name)
		if task.Status == StatusSkipped {
			fmt.Fprintf(w, "%s  %s\n", line, task.SkipReason)
			continue
		}
		if task.PassRate > 0 && task.PassRate < 1 {
			line += fmt.Sprintf(" %d/%d ", int(task.PassRate*float64(len(task.Runs))+0.5), len(task.Runs))
		} else {
			line += "       "
		}
		line += fmt.Sprintf("%3d calls  %3d tools  %8s tok  %9s  %s",
			task.Metrics.ModelCalls,
			task.Metrics.ToolCalls,
			humanTokens(task.Metrics.Usage.TotalTokens),
			humanCost(task.Metrics.CostUSD),
			humanDuration(time.Duration(task.Metrics.DurationMS)*time.Millisecond),
		)
		fmt.Fprintln(w, strings.TrimRight(line, " "))
		for _, reason := range task.Failures {
			fmt.Fprintf(w, "     - %s\n", reason)
		}
		for _, warning := range task.Warnings {
			fmt.Fprintf(w, "     ! %s\n", warning)
		}
		if task.Baseline != nil && len(task.Baseline.Reasons) > 0 {
			for _, reason := range task.Baseline.Reasons {
				fmt.Fprintf(w, "     ~ baseline: %s\n", reason)
			}
		}
		if verbose && task.Workspace != "" {
			fmt.Fprintf(w, "     workspace: %s\n", task.Workspace)
			for i, run := range task.Runs {
				if !run.Passed && run.StderrTail != "" {
					fmt.Fprintf(w, "     run %d stderr: %s\n", i+1, indentLines(run.StderrTail, "       "))
				}
			}
		}
	}
	summary := report.Summary
	fmt.Fprintln(w)
	fmt.Fprintf(w, "%d passed · %d failed · %d skipped · %d errors",
		summary.Passed, summary.Failed, summary.Skipped, summary.Errors)
	if summary.Flaky > 0 {
		fmt.Fprintf(w, " · %d flaky", summary.Flaky)
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "%d model calls · %d tools · %s tokens · %s · %s\n",
		summary.ModelCalls,
		summary.ToolCalls,
		humanTokens(summary.TotalTokens),
		humanCost(summary.CostUSD),
		humanDuration(time.Duration(summary.DurationMS)*time.Millisecond),
	)
	if report.Baseline != nil {
		fmt.Fprintf(w, "baseline %s: %d regression(s), %d improvement(s)\n",
			report.Baseline.Path, report.Baseline.Regressions, report.Baseline.Improvements)
		for _, warning := range report.Baseline.Warnings {
			fmt.Fprintf(w, "  ! %s\n", warning)
		}
	}
	if report.HarnessErr != "" {
		fmt.Fprintf(w, "harness: %s\n", report.HarnessErr)
	}
}

// WriteMarkdown renders a report suitable for CI summaries or a release note.
func WriteMarkdown(w io.Writer, report Report) error {
	fmt.Fprintf(w, "# CodeDeep evaluation report\n\n")
	fmt.Fprintf(w, "- Mode: **%s**\n", report.Mode)
	if report.Provider != "" || report.Model != "" {
		fmt.Fprintf(w, "- Provider: `%s`\n", strings.TrimSpace(report.Provider+" "+report.Model))
	}
	fmt.Fprintf(w, "- Suite: `%s`\n", report.Suite)
	fmt.Fprintf(w, "- Generated: %s\n\n", report.Generated.Format(time.RFC3339))
	summary := report.Summary
	fmt.Fprintf(w, "**%d passed, %d failed, %d skipped, %d errors** · %d model calls · %d tools · %s tokens · %s · %s\n\n",
		summary.Passed, summary.Failed, summary.Skipped, summary.Errors,
		summary.ModelCalls, summary.ToolCalls,
		humanTokens(summary.TotalTokens), humanCost(summary.CostUSD),
		humanDuration(time.Duration(summary.DurationMS)*time.Millisecond))
	fmt.Fprintln(w, "| Task | Status | Pass rate | Calls | Tools | Tokens | Cost | Time |")
	fmt.Fprintln(w, "| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: |")
	for _, task := range report.Tasks {
		status := task.Status
		if task.Status == StatusSkipped {
			fmt.Fprintf(w, "| %s | %s | — | — | — | — | — | — |\n", task.Name, status)
			continue
		}
		fmt.Fprintf(w, "| %s | %s | %.0f%% | %d | %d | %s | %s | %s |\n",
			task.Name, status, task.PassRate*100,
			task.Metrics.ModelCalls, task.Metrics.ToolCalls,
			humanTokens(task.Metrics.Usage.TotalTokens),
			humanCost(task.Metrics.CostUSD),
			humanDuration(time.Duration(task.Metrics.DurationMS)*time.Millisecond))
	}
	fmt.Fprintln(w)
	for _, task := range report.Tasks {
		if len(task.Failures) == 0 && len(task.Warnings) == 0 {
			continue
		}
		fmt.Fprintf(w, "## %s\n\n", task.Name)
		for _, reason := range task.Failures {
			fmt.Fprintf(w, "- FAIL: %s\n", reason)
		}
		for _, warning := range task.Warnings {
			fmt.Fprintf(w, "- WARN: %s\n", warning)
		}
		fmt.Fprintln(w)
	}
	if report.Baseline != nil {
		fmt.Fprintf(w, "## Baseline\n\n- Regressions: %d\n- Improvements: %d\n", report.Baseline.Regressions, report.Baseline.Improvements)
		for _, warning := range report.Baseline.Warnings {
			fmt.Fprintf(w, "- %s\n", warning)
		}
	}
	return nil
}

func humanTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fm", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return fmt.Sprint(n)
	}
}

func humanCost(v float64) string {
	if v == 0 {
		return "$0"
	}
	if v < 0.01 {
		return fmt.Sprintf("$%.6f", v)
	}
	return fmt.Sprintf("$%.4f", v)
}

func humanDuration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	return fmt.Sprintf("%.2fs", d.Seconds())
}

func indentLines(text, prefix string) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	return strings.Join(lines, "\n"+prefix)
}

// sortReports orders failed tasks first so the terminal shows actionable work.
func sortReports(tasks []TaskResult) {
	order := map[string]int{StatusError: 0, StatusFailed: 1, StatusSkipped: 2, StatusPassed: 3}
	sort.SliceStable(tasks, func(i, j int) bool {
		return order[tasks[i].Status] < order[tasks[j].Status]
	})
}
