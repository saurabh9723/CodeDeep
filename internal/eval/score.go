package eval

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"codedeep/internal/agent"
	"codedeep/internal/llm"
)

// VerifyRecord is one automatic verification attempt.
type VerifyRecord struct {
	Command string `json:"command,omitempty"`
	Output  string `json:"output,omitempty"`
	IsError bool   `json:"is_error"`
}

// Observation is everything the harness extracts from the NDJSON event stream.
type Observation struct {
	Events          []agent.Event
	EventCounts     map[string]int
	Text            string
	ToolCounts      map[string]int
	ToolErrors      map[string]int
	ToolOutputs     map[string]string
	ModelCalls      int
	ToolCalls       int
	Subagents       int
	SubagentSuccess int
	Retries         int
	Verifies        []VerifyRecord
	Usage           llm.Usage
	CostUSD         float64
	Done            bool
	DoneMessage     string
	Errors          []string
	UserMessages    int
	InvalidLines    int
}

// Metrics are the comparable numbers reported for each run.
type Metrics struct {
	ModelCalls int            `json:"model_calls"`
	ToolCalls  int            `json:"tool_calls"`
	Subagents  int            `json:"subagents"`
	Retries    int            `json:"retries"`
	Usage      llm.Usage      `json:"usage"`
	CostUSD    float64        `json:"cost_usd"`
	DurationMS int64          `json:"duration_ms"`
	ToolCounts map[string]int `json:"tool_counts,omitempty"`
}

func metricsFor(obs Observation, durationMS int64) Metrics {
	return Metrics{
		ModelCalls: obs.ModelCalls,
		ToolCalls:  obs.ToolCalls,
		Subagents:  obs.Subagents,
		Retries:    obs.Retries,
		Usage:      obs.Usage,
		CostUSD:    obs.CostUSD,
		DurationMS: durationMS,
		ToolCounts: obs.ToolCounts,
	}
}

// Observe aggregates typed agent events. When the run finished, the done
// event's session totals win because they include subagent usage exactly once.
func Observe(events []agent.Event) Observation {
	obs := Observation{
		Events:      events,
		EventCounts: map[string]int{},
		ToolCounts:  map[string]int{},
		ToolErrors:  map[string]int{},
		ToolOutputs: map[string]string{},
	}
	var usageSum llm.Usage
	var costSum float64
	var done *agent.Event
	for i := range events {
		ev := events[i]
		obs.EventCounts[ev.Type]++
		switch ev.Type {
		case agent.EventUser:
			obs.UserMessages++
		case agent.EventText:
			if ev.Depth == 0 {
				obs.Text += ev.Text
			}
		case agent.EventUsage:
			obs.ModelCalls++
			if ev.Usage != nil {
				usageSum = usageSum.Add(*ev.Usage)
			}
			costSum += ev.CostUSD
		case agent.EventToolStart:
			obs.ToolCalls++
			obs.ToolCounts[ev.Name]++
		case agent.EventToolEnd:
			if ev.Output != "" {
				if prev := obs.ToolOutputs[ev.Name]; prev != "" {
					obs.ToolOutputs[ev.Name] = prev + "\n" + ev.Output
				} else {
					obs.ToolOutputs[ev.Name] = ev.Output
				}
			}
			if ev.IsError {
				obs.ToolErrors[ev.Name]++
			}
		case agent.EventSubagentStart:
			obs.Subagents++
		case agent.EventSubagentEnd:
			if !ev.IsError {
				obs.SubagentSuccess++
			}
		case agent.EventRetry:
			obs.Retries++
		case agent.EventVerify:
			obs.Verifies = append(obs.Verifies, VerifyRecord{
				Command: ev.Message,
				Output:  ev.Output,
				IsError: ev.IsError,
			})
		case agent.EventError:
			obs.Errors = append(obs.Errors, ev.Message)
		case agent.EventDone:
			done = &events[i]
		}
	}
	if done != nil {
		obs.Done = true
		obs.DoneMessage = done.Message
		if done.TotalUsage != nil {
			obs.Usage = *done.TotalUsage
			obs.CostUSD = done.TotalCost
		} else {
			obs.Usage = usageSum
			obs.CostUSD = costSum
		}
	} else {
		obs.Usage = usageSum
		obs.CostUSD = costSum
	}
	return obs
}

// checkTask applies every hard assertion and budget. The returned warnings are
// non-fatal budget/baseline nudges unless the task marks its budget strict.
func checkTask(task Task, obs Observation, verifier *VerifierResult, workspace string, durationMS int64, exitCode int, timedOut bool) (failures, warnings []string) {
	wantExit := 0
	if task.Expect.ExitCode != nil {
		wantExit = *task.Expect.ExitCode
	}
	if timedOut {
		failures = append(failures, fmt.Sprintf("run timed out after %s", task.Timeout()))
	} else if exitCode != wantExit {
		failures = append(failures, fmt.Sprintf("exit code %d, want %d", exitCode, wantExit))
	}

	for _, want := range task.Expect.TextContains {
		if !strings.Contains(obs.Text, want) {
			failures = append(failures, fmt.Sprintf("text does not contain %q", truncate(want, 120)))
		}
	}
	for _, avoid := range task.Expect.TextNotContains {
		if strings.Contains(obs.Text, avoid) {
			failures = append(failures, fmt.Sprintf("text contains forbidden %q", truncate(avoid, 120)))
		}
	}

	// Raw event assertions.
	for _, name := range task.Expect.Events.Required {
		if obs.EventCounts[name] == 0 {
			failures = append(failures, fmt.Sprintf("event %s: required but missing", name))
		}
	}
	for _, name := range task.Expect.Events.Forbidden {
		if n := obs.EventCounts[name]; n > 0 {
			failures = append(failures, fmt.Sprintf("event %s: forbidden but occurred %d time(s)", name, n))
		}
	}
	for name, min := range task.Expect.Events.Min {
		if obs.EventCounts[name] < min {
			failures = append(failures, fmt.Sprintf("event %s: want at least %d, got %d", name, min, obs.EventCounts[name]))
		}
	}
	for name, max := range task.Expect.Events.Max {
		if obs.EventCounts[name] > max {
			failures = append(failures, fmt.Sprintf("event %s: want at most %d, got %d", name, max, obs.EventCounts[name]))
		}
	}
	if task.Expect.NoErrorEvents {
		if len(obs.Errors) > 0 {
			failures = append(failures, "error events: "+truncate(strings.Join(obs.Errors, "; "), 300))
		}
	}

	// Tool assertions.
	for _, name := range task.Expect.Tools.Required {
		if obs.ToolCounts[name] == 0 {
			failures = append(failures, fmt.Sprintf("tool %s: required but never called", name))
		}
	}
	for _, name := range task.Expect.Tools.Forbidden {
		if n := obs.ToolCounts[name]; n > 0 {
			failures = append(failures, fmt.Sprintf("tool %s: forbidden but called %d time(s)", name, n))
		}
	}
	for name, want := range task.Expect.Tools.Counts {
		if got := obs.ToolCounts[name]; got != want {
			failures = append(failures, fmt.Sprintf("tool %s: want exactly %d calls, got %d", name, want, got))
		}
	}
	for name, min := range task.Expect.Tools.Min {
		if obs.ToolCounts[name] < min {
			failures = append(failures, fmt.Sprintf("tool %s: want at least %d calls, got %d", name, min, obs.ToolCounts[name]))
		}
	}
	for name, max := range task.Expect.Tools.Max {
		if obs.ToolCounts[name] > max {
			failures = append(failures, fmt.Sprintf("tool %s: want at most %d calls, got %d", name, max, obs.ToolCounts[name]))
		}
	}
	if task.Expect.Tools.NoErrors {
		for name, n := range obs.ToolErrors {
			if n > 0 {
				failures = append(failures, fmt.Sprintf("tool %s: returned an error %d time(s)", name, n))
			}
		}
	}
	for name, wants := range task.Expect.Tools.OutputContains {
		output := obs.ToolOutputs[name]
		for _, want := range wants {
			if !strings.Contains(output, want) {
				failures = append(failures, fmt.Sprintf("tool %s output does not contain %q", name, truncate(want, 120)))
			}
		}
	}

	// Subagents.
	if obs.Subagents < task.Expect.Subagents.Min {
		failures = append(failures, fmt.Sprintf("subagents: want at least %d, got %d", task.Expect.Subagents.Min, obs.Subagents))
	}
	if task.Expect.Subagents.Max != nil && obs.Subagents > *task.Expect.Subagents.Max {
		failures = append(failures, fmt.Sprintf("subagents: want at most %d, got %d", *task.Expect.Subagents.Max, obs.Subagents))
	}
	if task.Expect.Subagents.RequireSuccess && obs.SubagentSuccess == 0 {
		failures = append(failures, "subagents: no successful subagent_end event")
	}

	// Retries.
	if obs.Retries < task.Expect.Retries.Min {
		failures = append(failures, fmt.Sprintf("retries: want at least %d, got %d", task.Expect.Retries.Min, obs.Retries))
	}
	if task.Expect.Retries.Max != nil && obs.Retries > *task.Expect.Retries.Max {
		failures = append(failures, fmt.Sprintf("retries: want at most %d, got %d", *task.Expect.Retries.Max, obs.Retries))
	}

	// Automatic verification.
	verifySuccess, verifyFailure := 0, 0
	for _, v := range obs.Verifies {
		if v.IsError {
			verifyFailure++
		} else {
			verifySuccess++
		}
	}
	if len(obs.Verifies) < task.Expect.Verify.Min {
		failures = append(failures, fmt.Sprintf("verify: want at least %d attempt(s), got %d", task.Expect.Verify.Min, len(obs.Verifies)))
	}
	if task.Expect.Verify.Max != nil && len(obs.Verifies) > *task.Expect.Verify.Max {
		failures = append(failures, fmt.Sprintf("verify: want at most %d attempt(s), got %d", *task.Expect.Verify.Max, len(obs.Verifies)))
	}
	if task.Expect.Verify.RequireSuccess && verifySuccess == 0 {
		failures = append(failures, "verify: no successful verification")
	}
	if task.Expect.Verify.RequireFailure && verifyFailure == 0 {
		failures = append(failures, "verify: no failing verification")
	}

	// Files.
	for _, check := range task.Expect.Files {
		if reason := checkFile(workspace, check); reason != "" {
			failures = append(failures, reason)
		}
	}

	// External verifier.
	if verifier != nil {
		want := 0
		if task.Expect.VerifierExit != nil {
			want = *task.Expect.VerifierExit
		}
		switch {
		case verifier.Error != "":
			failures = append(failures, "verifier failed to start: "+verifier.Error)
		case verifier.ExitCode != want:
			tail := truncate(strings.TrimSpace(verifier.Output), 400)
			if tail == "" {
				failures = append(failures, fmt.Sprintf("verifier exit %d, want %d", verifier.ExitCode, want))
			} else {
				failures = append(failures, fmt.Sprintf("verifier exit %d, want %d: %s", verifier.ExitCode, want, tail))
			}
		}
	}

	// Budgets.
	type budgetCheck struct {
		label string
		got   string
		over  bool
	}
	checks := []budgetCheck{
		{"model calls", fmt.Sprint(obs.ModelCalls), task.Budget.MaxModelCalls > 0 && obs.ModelCalls > task.Budget.MaxModelCalls},
		{"tool calls", fmt.Sprint(obs.ToolCalls), task.Budget.MaxToolCalls > 0 && obs.ToolCalls > task.Budget.MaxToolCalls},
		{"tokens", fmt.Sprint(obs.Usage.TotalTokens), task.Budget.MaxTotalTokens > 0 && obs.Usage.TotalTokens > task.Budget.MaxTotalTokens},
		{"cost", fmt.Sprintf("$%.6f", obs.CostUSD), task.Budget.MaxCostUSD > 0 && obs.CostUSD > task.Budget.MaxCostUSD},
		{"duration", fmt.Sprintf("%dms", durationMS), task.Budget.MaxDurationMS > 0 && durationMS > task.Budget.MaxDurationMS},
	}
	limits := map[string]string{
		"model calls": fmt.Sprint(task.Budget.MaxModelCalls),
		"tool calls":  fmt.Sprint(task.Budget.MaxToolCalls),
		"tokens":      fmt.Sprint(task.Budget.MaxTotalTokens),
		"cost":        fmt.Sprintf("$%.6f", task.Budget.MaxCostUSD),
		"duration":    fmt.Sprintf("%dms", task.Budget.MaxDurationMS),
	}
	for _, check := range checks {
		if !check.over {
			continue
		}
		msg := fmt.Sprintf("budget: %s %s exceeds %s", check.label, check.got, limits[check.label])
		if task.Budget.Strict {
			failures = append(failures, msg)
		} else {
			warnings = append(warnings, msg)
		}
	}
	return failures, warnings
}

func checkFile(workspace string, check FileCheck) string {
	path, err := safeJoin(workspace, check.Path)
	if err != nil {
		return "file " + check.Path + ": " + err.Error()
	}
	expected := true
	if check.Exists != nil {
		expected = *check.Exists
	}
	info, statErr := os.Stat(path)
	exists := statErr == nil
	if statErr != nil && !os.IsNotExist(statErr) {
		return fmt.Sprintf("file %s: %v", check.Path, statErr)
	}
	if !expected {
		if exists {
			return fmt.Sprintf("file %s: expected to be absent", check.Path)
		}
		return ""
	}
	if !exists {
		return fmt.Sprintf("file %s: missing", check.Path)
	}
	if info.IsDir() {
		return fmt.Sprintf("file %s: is a directory", check.Path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("file %s: %v", check.Path, err)
	}
	body := string(data)
	if check.Contains != "" && !strings.Contains(body, check.Contains) {
		return fmt.Sprintf("file %s: does not contain %q", check.Path, truncate(check.Contains, 120))
	}
	if check.NotContains != "" && strings.Contains(body, check.NotContains) {
		return fmt.Sprintf("file %s: contains forbidden %q", check.Path, truncate(check.NotContains, 120))
	}
	if check.Equals != "" && body != check.Equals {
		return fmt.Sprintf("file %s: content differs from the expected exact value", check.Path)
	}
	if check.Regex != "" {
		re, err := regexp.Compile(check.Regex)
		if err != nil {
			return fmt.Sprintf("file %s: invalid regex: %v", check.Path, err)
		}
		if !re.MatchString(body) {
			return fmt.Sprintf("file %s: does not match %q", check.Path, check.Regex)
		}
	}
	return ""
}

// medianMetrics summarizes repeated runs with medians for scalar values.
func medianMetrics(runs []RunResult) Metrics {
	if len(runs) == 0 {
		return Metrics{}
	}
	modelCalls := make([]int, 0, len(runs))
	toolCalls := make([]int, 0, len(runs))
	subagents := make([]int, 0, len(runs))
	retries := make([]int, 0, len(runs))
	prompt := make([]int, 0, len(runs))
	completion := make([]int, 0, len(runs))
	total := make([]int, 0, len(runs))
	cacheHit := make([]int, 0, len(runs))
	cacheWrite := make([]int, 0, len(runs))
	cost := make([]float64, 0, len(runs))
	duration := make([]int64, 0, len(runs))
	for _, run := range runs {
		modelCalls = append(modelCalls, run.Metrics.ModelCalls)
		toolCalls = append(toolCalls, run.Metrics.ToolCalls)
		subagents = append(subagents, run.Metrics.Subagents)
		retries = append(retries, run.Metrics.Retries)
		prompt = append(prompt, run.Metrics.Usage.PromptTokens)
		completion = append(completion, run.Metrics.Usage.CompletionTokens)
		total = append(total, run.Metrics.Usage.TotalTokens)
		cacheHit = append(cacheHit, run.Metrics.Usage.PromptCacheHitTokens)
		cacheWrite = append(cacheWrite, run.Metrics.Usage.PromptCacheWriteTokens)
		cost = append(cost, run.Metrics.CostUSD)
		duration = append(duration, run.Metrics.DurationMS)
	}
	return Metrics{
		ModelCalls: medianInt(modelCalls),
		ToolCalls:  medianInt(toolCalls),
		Subagents:  medianInt(subagents),
		Retries:    medianInt(retries),
		Usage: llm.Usage{
			PromptTokens:           medianInt(prompt),
			CompletionTokens:       medianInt(completion),
			TotalTokens:            medianInt(total),
			PromptCacheHitTokens:   medianInt(cacheHit),
			PromptCacheWriteTokens: medianInt(cacheWrite),
		},
		CostUSD:    medianFloat(cost),
		DurationMS: medianInt64(duration),
		ToolCounts: runs[0].Metrics.ToolCounts,
	}
}

func medianInt(values []int) int {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]int(nil), values...)
	sort.Ints(sorted)
	return sorted[len(sorted)/2]
}

func medianInt64(values []int64) int64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]int64(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted[len(sorted)/2]
}

func medianFloat(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	return sorted[len(sorted)/2]
}

func truncate(text string, limit int) string {
	if limit <= 0 || len(text) <= limit {
		return text
	}
	if limit <= 3 {
		return text[:limit]
	}
	return text[:limit-3] + "..."
}
