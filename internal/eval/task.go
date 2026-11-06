// Package eval implements a task-based evaluation harness for CodeDeep.
//
// A task describes a prompt, a deterministic mock trajectory (or a real
// provider for quality runs), fixtures, and hard expectations. The runner
// executes CodeDeep in an isolated workspace and scores the resulting events,
// files, tool calls, costs, and external verifier.
package eval

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"codedeep/internal/mockllm"
)

// Mode selects where model responses come from.
type Mode string

const (
	ModeMock Mode = "mock"
	ModeReal Mode = "real"
)

func ParseMode(raw string) (Mode, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", string(ModeMock):
		return ModeMock, nil
	case string(ModeReal):
		return ModeReal, nil
	default:
		return "", fmt.Errorf("invalid mode %q (use mock or real)", raw)
	}
}

// Task is one evaluation case loaded from <suite>/<name>/task.json.
type Task struct {
	Name               string         `json:"name"`
	Description        string         `json:"description,omitempty"`
	Tags               []string       `json:"tags,omitempty"`
	Prompt             string         `json:"prompt"`
	Skip               bool           `json:"skip,omitempty"`
	SkipReason         string         `json:"skip_reason,omitempty"`
	Mock               Mock           `json:"mock,omitempty"`
	TimeoutSec         int            `json:"timeout_sec,omitempty"`
	TimeoutSecReal     int            `json:"timeout_sec_real,omitempty"`
	FixtureDir         string         `json:"fixture_dir,omitempty"`
	Setup              []SetupFile    `json:"setup,omitempty"`
	Config             map[string]any `json:"config,omitempty"`
	Expect             Expect         `json:"expect,omitempty"`
	Budget             Budget         `json:"budget,omitempty"`
	Verifier           string         `json:"verifier,omitempty"`
	VerifierTimeoutSec int            `json:"verifier_timeout_sec,omitempty"`

	Dir    string `json:"-"`
	Source string `json:"-"`
}

// Mock configures the deterministic provider. Steps can be inline or loaded
// from Script, which is resolved relative to the task directory.
type Mock struct {
	Scenario string         `json:"scenario,omitempty"`
	Script   string         `json:"script,omitempty"`
	Steps    []mockllm.Step `json:"steps,omitempty"`
	DelayMS  int            `json:"delay_ms,omitempty"`
}

// SetupFile is written into the workspace before the run.
type SetupFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	// Mode is an optional octal string such as "0755". Files default to 0644.
	Mode string `json:"mode,omitempty"`
}

// Expect contains the hard pass/fail assertions for a task.
type Expect struct {
	ExitCode        *int           `json:"exit_code,omitempty"`
	Files           []FileCheck    `json:"files,omitempty"`
	TextContains    []string       `json:"text_contains,omitempty"`
	TextNotContains []string       `json:"text_not_contains,omitempty"`
	Tools           ToolExpect     `json:"tools,omitempty"`
	Events          EventExpect    `json:"events,omitempty"`
	Subagents       SubagentExpect `json:"subagents,omitempty"`
	Retries         CountExpect    `json:"retries,omitempty"`
	Verify          VerifyExpect   `json:"verify,omitempty"`
	VerifierExit    *int           `json:"verifier_exit,omitempty"`
	NoErrorEvents   bool           `json:"no_error_events,omitempty"`
}

// FileCheck asserts the state of one workspace file. Exists defaults to true.
type FileCheck struct {
	Path        string `json:"path"`
	Exists      *bool  `json:"exists,omitempty"`
	Contains    string `json:"contains,omitempty"`
	NotContains string `json:"not_contains,omitempty"`
	Equals      string `json:"equals,omitempty"`
	Regex       string `json:"regex,omitempty"`
}

// ToolExpect asserts which tools ran and how many times.
type ToolExpect struct {
	Required       []string            `json:"required,omitempty"`
	Forbidden      []string            `json:"forbidden,omitempty"`
	Counts         map[string]int      `json:"counts,omitempty"`
	Min            map[string]int      `json:"min,omitempty"`
	Max            map[string]int      `json:"max,omitempty"`
	NoErrors       bool                `json:"no_errors,omitempty"`
	OutputContains map[string][]string `json:"output_contains,omitempty"`
}

// EventExpect asserts raw agent event types.
type EventExpect struct {
	Required  []string       `json:"required,omitempty"`
	Forbidden []string       `json:"forbidden,omitempty"`
	Min       map[string]int `json:"min,omitempty"`
	Max       map[string]int `json:"max,omitempty"`
}

// SubagentExpect asserts subagent delegation behavior. A non-nil Max of 0
// means "no subagents", which plain omitempty integers cannot express.
type SubagentExpect struct {
	Min            int  `json:"min,omitempty"`
	Max            *int `json:"max,omitempty"`
	RequireSuccess bool `json:"require_success,omitempty"`
}

// CountExpect bounds an event count.
type CountExpect struct {
	Min int  `json:"min,omitempty"`
	Max *int `json:"max,omitempty"`
}

// VerifyExpect asserts automatic verification events.
type VerifyExpect struct {
	Min            int  `json:"min,omitempty"`
	Max            *int `json:"max,omitempty"`
	RequireSuccess bool `json:"require_success,omitempty"`
	RequireFailure bool `json:"require_failure,omitempty"`
}

// Budget bounds efficiency. Violations are warnings unless Strict is set.
// Real overrides the thresholds for real-model runs, which naturally take
// extra steps that scripted mock trajectories skip.
type Budget struct {
	BudgetLimits
	Real *BudgetLimits `json:"real,omitempty"`
}

// BudgetLimits are the thresholds for one run mode.
type BudgetLimits struct {
	MaxModelCalls  int     `json:"max_model_calls,omitempty"`
	MaxToolCalls   int     `json:"max_tool_calls,omitempty"`
	MaxTotalTokens int     `json:"max_total_tokens,omitempty"`
	MaxCostUSD     float64 `json:"max_cost_usd,omitempty"`
	MaxDurationMS  int64   `json:"max_duration_ms,omitempty"`
	Strict         bool    `json:"strict,omitempty"`
}

// ForMode returns the thresholds enforced for a run mode.
func (b Budget) ForMode(mode Mode) BudgetLimits {
	if mode == ModeReal && b.Real != nil {
		return *b.Real
	}
	return b.BudgetLimits
}

// Discover loads every task under root. A root that is itself a task
// directory is accepted. Hidden directories are ignored.
func Discover(root string) ([]Task, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(abs, "task.json")); err == nil {
		task, err := Load(abs)
		if err != nil {
			return nil, err
		}
		return []Task{task}, nil
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return nil, fmt.Errorf("read suite %s: %w", root, err)
	}
	var tasks []Task
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		dir := filepath.Join(abs, entry.Name())
		if _, err := os.Stat(filepath.Join(dir, "task.json")); err != nil {
			continue
		}
		task, err := Load(dir)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	if len(tasks) == 0 {
		return nil, fmt.Errorf("no tasks found under %s", root)
	}
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].Name < tasks[j].Name })
	seen := map[string]string{}
	for _, task := range tasks {
		if prev, ok := seen[task.Name]; ok {
			return nil, fmt.Errorf("duplicate task name %q in %s and %s", task.Name, prev, task.Source)
		}
		seen[task.Name] = task.Source
	}
	return tasks, nil
}

// Load reads and validates one task directory.
func Load(dir string) (Task, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return Task{}, err
	}
	path := filepath.Join(abs, "task.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return Task{}, fmt.Errorf("read %s: %w", path, err)
	}
	var task Task
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&task); err != nil {
		return Task{}, fmt.Errorf("parse %s: %w", path, err)
	}
	task.Dir = abs
	task.Source = path
	if task.Name == "" {
		task.Name = filepath.Base(abs)
	}
	if task.FixtureDir == "" {
		task.FixtureDir = "files"
	}
	if task.Mock.Script != "" && len(task.Mock.Steps) > 0 {
		return Task{}, fmt.Errorf("%s: mock.script and mock.steps are mutually exclusive", path)
	}
	if task.Mock.Script != "" {
		scriptPath, err := safeJoin(abs, task.Mock.Script)
		if err != nil {
			return Task{}, fmt.Errorf("%s: mock.script: %w", path, err)
		}
		scriptData, err := os.ReadFile(scriptPath)
		if err != nil {
			return Task{}, fmt.Errorf("read mock script %s: %w", scriptPath, err)
		}
		script, err := mockllm.ParseScript(scriptData)
		if err != nil {
			return Task{}, fmt.Errorf("parse mock script %s: %w", scriptPath, err)
		}
		task.Mock.Steps = script.Steps
	}
	if task.Mock.DelayMS < 0 {
		return Task{}, fmt.Errorf("%s: mock.delay_ms must be >= 0", path)
	}
	if task.TimeoutSec < 0 {
		return Task{}, fmt.Errorf("%s: timeout_sec must be >= 0", path)
	}
	if task.TimeoutSec == 0 {
		task.TimeoutSec = defaultTimeoutSec
	}
	if task.TimeoutSec > maxTimeoutSec {
		task.TimeoutSec = maxTimeoutSec
	}
	if task.TimeoutSecReal < 0 {
		return Task{}, fmt.Errorf("%s: timeout_sec_real must be >= 0", path)
	}
	if task.TimeoutSecReal > maxTimeoutSec {
		task.TimeoutSecReal = maxTimeoutSec
	}
	if task.VerifierTimeoutSec < 0 {
		return Task{}, fmt.Errorf("%s: verifier_timeout_sec must be >= 0", path)
	}
	if task.VerifierTimeoutSec == 0 {
		task.VerifierTimeoutSec = 60
	}
	if task.VerifierTimeoutSec > 600 {
		task.VerifierTimeoutSec = 600
	}
	if err := task.validate(); err != nil {
		return Task{}, fmt.Errorf("%s: %w", path, err)
	}
	return task, nil
}

func (t *Task) validate() error {
	if strings.TrimSpace(t.Name) == "" {
		return fmt.Errorf("name is required")
	}
	if strings.TrimSpace(t.Prompt) == "" && !t.Skip {
		return fmt.Errorf("prompt is required")
	}
	if strings.TrimSpace(t.Verifier) == "" && t.Expect.VerifierExit != nil {
		return fmt.Errorf("expect.verifier_exit requires a verifier command")
	}
	if err := validateBudget("budget", t.Budget.BudgetLimits); err != nil {
		return err
	}
	if t.Budget.Real != nil {
		if err := validateBudget("budget.real", *t.Budget.Real); err != nil {
			return err
		}
	}
	for _, key := range []string{"provider", "model", "base_url", "api_key", "sessions_dir", "allow_outside"} {
		if _, ok := t.Config[key]; ok {
			return fmt.Errorf("config.%s is reserved for the harness", key)
		}
	}
	if t.Expect.Subagents.Min < 0 ||
		(t.Expect.Subagents.Max != nil && (*t.Expect.Subagents.Max < 0 || *t.Expect.Subagents.Max < t.Expect.Subagents.Min)) {
		return fmt.Errorf("invalid subagent bounds")
	}
	if err := validateCount("retries", t.Expect.Retries); err != nil {
		return err
	}
	if err := validateCount("verify", CountExpect{Min: t.Expect.Verify.Min, Max: t.Expect.Verify.Max}); err != nil {
		return err
	}
	for name, n := range t.Expect.Tools.Counts {
		if n < 0 {
			return fmt.Errorf("tools.counts[%s] must be >= 0", name)
		}
	}
	for name, n := range t.Expect.Tools.Min {
		if n < 0 {
			return fmt.Errorf("tools.min[%s] must be >= 0", name)
		}
	}
	for name, n := range t.Expect.Tools.Max {
		if n < 0 {
			return fmt.Errorf("tools.max[%s] must be >= 0", name)
		}
	}
	for name, n := range t.Expect.Events.Min {
		if n < 0 {
			return fmt.Errorf("events.min[%s] must be >= 0", name)
		}
	}
	for name, n := range t.Expect.Events.Max {
		if n < 0 {
			return fmt.Errorf("events.max[%s] must be >= 0", name)
		}
	}
	for _, file := range t.Expect.Files {
		if _, err := safeJoin(".", file.Path); err != nil {
			return fmt.Errorf("expect.files: %w", err)
		}
		if file.Regex != "" {
			if _, err := regexp.Compile(file.Regex); err != nil {
				return fmt.Errorf("expect.files[%s].regex: %w", file.Path, err)
			}
		}
	}
	for _, setup := range t.Setup {
		if _, err := safeJoin(".", setup.Path); err != nil {
			return fmt.Errorf("setup: %w", err)
		}
		if setup.Mode != "" {
			var mode uint32
			if _, err := fmt.Sscanf(setup.Mode, "%o", &mode); err != nil {
				return fmt.Errorf("setup[%s].mode %q is not octal", setup.Path, setup.Mode)
			}
		}
	}
	if _, err := safeJoin(".", t.FixtureDir); err != nil {
		return fmt.Errorf("fixture_dir: %w", err)
	}
	return nil
}

func validateBudget(name string, b BudgetLimits) error {
	if b.MaxModelCalls < 0 || b.MaxToolCalls < 0 || b.MaxTotalTokens < 0 ||
		b.MaxCostUSD < 0 || b.MaxDurationMS < 0 {
		return fmt.Errorf("%s values must be >= 0", name)
	}
	return nil
}

func validateCount(name string, c CountExpect) error {
	if c.Min < 0 || (c.Max != nil && (*c.Max < 0 || *c.Max < c.Min)) {
		return fmt.Errorf("invalid %s bounds", name)
	}
	return nil
}

// safeJoin resolves a task-relative path and rejects escapes.
func safeJoin(root, rel string) (string, error) {
	if strings.TrimSpace(rel) == "" {
		return "", fmt.Errorf("empty path")
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("path %q must be relative", rel)
	}
	clean := filepath.Clean(rel)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes the workspace", rel)
	}
	joined := filepath.Join(root, clean)
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	absJoined, err := filepath.Abs(joined)
	if err != nil {
		return "", err
	}
	if absJoined != absRoot && !strings.HasPrefix(absJoined, absRoot+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes the workspace", rel)
	}
	return absJoined, nil
}

// Filter selects tasks for a run.
type Filter struct {
	Pattern         string
	Tags            []string
	SkipTags        []string
	Mode            Mode
	IncludeMockOnly bool
}

// Validate checks the filter pattern before any tasks run.
func (f Filter) Validate() error {
	if f.Pattern != "" {
		if _, err := regexp.Compile(f.Pattern); err != nil {
			return fmt.Errorf("invalid -task pattern: %w", err)
		}
	}
	return nil
}

// Select reports whether a task should run. Pattern and tag mismatches exclude
// a task entirely; explicit skip flags and real-mode mock exclusions appear in
// the report as skipped so the omission is visible.
func (f Filter) Select(task Task) (selected bool, reason string, skipped bool) {
	if f.Pattern != "" {
		re, err := regexp.Compile(f.Pattern)
		if err != nil || !re.MatchString(task.Name) {
			return false, "", false
		}
	}
	if len(f.Tags) > 0 && !hasAnyTag(task.Tags, f.Tags) {
		return false, "", false
	}
	if task.Skip {
		reason := task.SkipReason
		if reason == "" {
			reason = "task is marked skip"
		}
		return true, reason, true
	}
	if hasAnyTag(task.Tags, f.SkipTags) {
		return true, "excluded by -skip-tag", true
	}
	if f.Mode == ModeReal && !f.IncludeMockOnly && hasAnyTag(task.Tags, []string{"mock-only"}) {
		return true, "mock-only task skipped in real mode", true
	}
	return true, "", false
}

func hasAnyTag(have, want []string) bool {
	if len(want) == 0 {
		return false
	}
	set := make(map[string]bool, len(have))
	for _, tag := range have {
		set[strings.ToLower(tag)] = true
	}
	for _, tag := range want {
		if set[strings.ToLower(strings.TrimSpace(tag))] {
			return true
		}
	}
	return false
}

const (
	defaultTimeoutSec     = 90
	defaultRealTimeoutSec = 300
	maxTimeoutSec         = 900
)

// Timeout returns the agent process timeout for a run mode. Mock runs use
// timeout_sec; real runs use timeout_sec_real and default to a longer budget
// because a model call takes far more than a scripted response.
func (t Task) Timeout(mode Mode) time.Duration {
	sec := t.TimeoutSec
	if mode == ModeReal {
		sec = t.TimeoutSecReal
		if sec <= 0 {
			sec = defaultRealTimeoutSec
		}
	} else if sec <= 0 {
		sec = defaultTimeoutSec
	}
	return time.Duration(sec) * time.Second
}
