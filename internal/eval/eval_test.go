package eval

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"codedeep/internal/llm"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverLoadsScriptAndDefaults(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "demo-task")
	writeFile(t, filepath.Join(dir, "task.json"), `{
	  "prompt": "do something",
	  "mock": {"script": "script.json"},
	  "expect": {"files": [{"path": "out.txt", "contains": "ok"}]}
	}`)
	writeFile(t, filepath.Join(dir, "script.json"), `{"steps":[{"text":"done"}]}`)

	tasks, err := Discover(root)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("got %d tasks, want 1", len(tasks))
	}
	task := tasks[0]
	if task.Name != "demo-task" {
		t.Errorf("name = %q, want directory name", task.Name)
	}
	if task.TimeoutSec != 90 {
		t.Errorf("timeout = %d, want default 90", task.TimeoutSec)
	}
	if task.FixtureDir != "files" {
		t.Errorf("fixture dir = %q, want files", task.FixtureDir)
	}
	if len(task.Mock.Steps) != 1 || task.Mock.Steps[0].Text != "done" {
		t.Fatalf("script steps not loaded: %+v", task.Mock.Steps)
	}
}

func TestLoadRejectsUnknownFieldsAndReservedConfig(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "bad")
	writeFile(t, filepath.Join(dir, "task.json"), `{"prompt":"x","typo":true}`)
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("want unknown-field error, got %v", err)
	}

	writeFile(t, filepath.Join(dir, "task.json"), `{"prompt":"x","config":{"base_url":"http://evil"}}`)
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("want reserved-config error, got %v", err)
	}

	writeFile(t, filepath.Join(dir, "task.json"), `{"prompt":"x","expect":{"files":[{"path":"../escape"}]}}`)
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("want escape error, got %v", err)
	}
}

func TestFilterSelect(t *testing.T) {
	mockOnly := Task{Name: "guarded", Tags: []string{"mock-only", "safety"}}
	plain := Task{Name: "plain", Tags: []string{"capability"}}
	skipped := Task{Name: "off", Skip: true, SkipReason: "needs network"}

	cases := []struct {
		name     string
		filter   Filter
		task     Task
		selected bool
		isSkip   bool
	}{
		{"mock real skip", Filter{Mode: ModeReal}, mockOnly, true, true},
		{"mock real included", Filter{Mode: ModeReal, IncludeMockOnly: true}, mockOnly, true, false},
		{"tag match", Filter{Tags: []string{"safety"}}, mockOnly, true, false},
		{"tag mismatch", Filter{Tags: []string{"capability"}}, mockOnly, false, false},
		{"skip tag", Filter{SkipTags: []string{"capability"}}, plain, true, true},
		{"pattern match", Filter{Pattern: "pla"}, plain, true, false},
		{"pattern mismatch", Filter{Pattern: "^other$"}, plain, false, false},
		{"explicit skip", Filter{}, skipped, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			selected, reason, isSkip := tc.filter.Select(tc.task)
			if selected != tc.selected || isSkip != tc.isSkip {
				t.Fatalf("Select = (%v,%q,%v), want (%v,_,%v)", selected, reason, isSkip, tc.selected, tc.isSkip)
			}
			if isSkip && reason == "" {
				t.Fatal("skipped task must explain why")
			}
		})
	}
}

func TestBuildConfigIsolation(t *testing.T) {
	task := Task{
		Name:   "iso",
		Prompt: "x",
		Config: map[string]any{"max_steps": float64(3), "approval": "plan"},
	}
	data, err := buildConfig(task, ProviderOptions{Mode: ModeMock}, "http://127.0.0.1:1234", "/tmp/sessions")
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["provider"] != "openai" || cfg["model"] != mockModel || cfg["base_url"] != "http://127.0.0.1:1234" {
		t.Fatalf("mock connection not forced: %v", cfg)
	}
	if cfg["sessions_dir"] != "/tmp/sessions" || cfg["allow_outside"] != false {
		t.Fatalf("isolation not enforced: %v", cfg)
	}
	if cfg["max_steps"] != float64(3) || cfg["approval"] != "plan" {
		t.Fatalf("task config not merged: %v", cfg)
	}
	pricing, _ := cfg["pricing"].(map[string]any)
	if pricing[mockModel] == nil {
		t.Fatalf("mock pricing missing: %v", cfg["pricing"])
	}

	realData, err := buildConfig(task, ProviderOptions{
		Mode:     ModeReal,
		Provider: "anthropic",
		Model:    "claude-x",
		BaseURL:  "https://example.invalid",
		Prices:   Price{InputPerMTok: 3, OutputPerMTok: 15},
	}, "", "/tmp/sessions")
	if err != nil {
		t.Fatal(err)
	}
	var real map[string]any
	if err := json.Unmarshal(realData, &real); err != nil {
		t.Fatal(err)
	}
	if real["provider"] != "anthropic" || real["model"] != "claude-x" || real["base_url"] != "https://example.invalid" {
		t.Fatalf("real connection not applied: %v", real)
	}
	realPricing, _ := real["pricing"].(map[string]any)
	entry, _ := realPricing["claude-x"].(map[string]any)
	if entry == nil || entry["input_per_mtok"] != float64(3) {
		t.Fatalf("real pricing missing: %v", real["pricing"])
	}
	if _, ok := real["api_key"]; ok {
		t.Fatal("api_key must never be written to the config file")
	}
}

func TestBuildEnvModeIsolation(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "real-secret")
	t.Setenv("OPENAI_API_KEY", "real-secret")
	t.Setenv("CODEAGENT_API_KEY", "keep-secret")
	t.Setenv("CODEAGENT_MODEL", "leaky-model")
	t.Setenv("DEEPSEEK_MODEL", "shell-model")
	t.Setenv("DEEPSEEK_BASE_URL", "https://shell.invalid")
	t.Setenv("CODEAGENT_MCP_CONFIG", "/tmp/mcp.json")
	t.Setenv("KEEP_ME", "yes")

	mockEnv := envMap(buildEnv(EnvOptions{Mode: ModeMock, BaseURL: "http://mock"}))
	if _, ok := mockEnv["DEEPSEEK_API_KEY"]; ok {
		// The mock sets its own placeholder key.
		if mockEnv["DEEPSEEK_API_KEY"] != "eval-key" {
			t.Fatalf("mock leaked a real key: %q", mockEnv["DEEPSEEK_API_KEY"])
		}
	}
	if mockEnv["CODEAGENT_API_KEY"] != "" {
		t.Fatalf("mock env leaked CODEAGENT_API_KEY=%q", mockEnv["CODEAGENT_API_KEY"])
	}
	if mockEnv["CODEAGENT_MODEL"] != mockModel {
		t.Fatalf("mock model = %q, want %q", mockEnv["CODEAGENT_MODEL"], mockModel)
	}
	if mockEnv["CODEAGENT_MCP_CONFIG"] != "" {
		t.Fatalf("mock env kept MCP config: %q", mockEnv["CODEAGENT_MCP_CONFIG"])
	}

	realEnv := envMap(buildEnv(EnvOptions{Mode: ModeReal, Provider: "openai", Home: "/tmp/home"}))
	if realEnv["CODEAGENT_API_KEY"] != "keep-secret" {
		t.Fatalf("real mode must preserve CODEAGENT_API_KEY, got %q", realEnv["CODEAGENT_API_KEY"])
	}
	if realEnv["CODEAGENT_MODEL"] != "" {
		t.Fatalf("real mode leaked inherited model %q", realEnv["CODEAGENT_MODEL"])
	}
	if realEnv["DEEPSEEK_MODEL"] != "" || realEnv["DEEPSEEK_BASE_URL"] != "" {
		t.Fatalf("real mode leaked provider connection overrides: %v", realEnv)
	}
	if realEnv["CODEAGENT_MCP_CONFIG"] != "" || realEnv["CODEAGENT_CONFIG"] != "" {
		t.Fatal("real mode must clear external config paths")
	}
	if realEnv["KEEP_ME"] != "yes" {
		t.Fatal("unrelated environment variables should survive")
	}
}

func TestCheckRealCredentials(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("CODEAGENT_API_KEY", "")
	if err := CheckRealCredentials("anthropic"); err == nil || !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Fatalf("want actionable anthropic error, got %v", err)
	}
	if err := CheckRealCredentials("ollama"); err != nil {
		t.Fatalf("ollama needs no key: %v", err)
	}
	t.Setenv("DEEPSEEK_API_KEY", "x")
	if err := CheckRealCredentials("openai"); err != nil {
		t.Fatalf("openai with DEEPSEEK_API_KEY: %v", err)
	}
}

func envMap(env []string) map[string]string {
	out := make(map[string]string, len(env))
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			out[key] = value
		}
	}
	return out
}

func TestCheckTaskFailuresAndBudgets(t *testing.T) {
	workspace := t.TempDir()
	writeFile(t, filepath.Join(workspace, "ok.txt"), "expected\n")

	task := Task{
		Name:       "checks",
		TimeoutSec: 90,
		Expect: Expect{
			Files:         []FileCheck{{Path: "ok.txt", Contains: "expected"}, {Path: "missing.txt"}},
			Tools:         ToolExpect{Required: []string{"read_file"}, Counts: map[string]int{"write_file": 1}},
			Subagents:     SubagentExpect{Min: 1},
			Verify:        VerifyExpect{Min: 2, RequireFailure: true},
			NoErrorEvents: true,
		},
		Budget: Budget{BudgetLimits: BudgetLimits{MaxModelCalls: 2}},
	}
	obs := Observation{
		ModelCalls:  5,
		ToolCounts:  map[string]int{},
		EventCounts: map[string]int{},
		Errors:      []string{"boom"},
		Verifies:    []VerifyRecord{{Command: "test"}},
	}
	failures, warnings := checkTask(task, obs, nil, workspace, 10, 0, false, ModeMock)
	joined := strings.Join(failures, "\n")
	for _, want := range []string{"missing.txt", "read_file", "write_file", "subagents", "verify", "error events"} {
		if !strings.Contains(joined, want) {
			t.Errorf("failures missing %q:\n%s", want, joined)
		}
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "budget: model calls") {
		t.Fatalf("want advisory budget warning, got %v", warnings)
	}

	task.Budget.Strict = true
	failures, warnings = checkTask(task, obs, nil, workspace, 10, 0, false, ModeMock)
	if len(warnings) != 0 || !strings.Contains(strings.Join(failures, "\n"), "budget: model calls") {
		t.Fatalf("strict budget should fail, warnings=%v failures=%v", warnings, failures)
	}

	failures, _ = checkTask(Task{Name: "timeout", TimeoutSec: 1}, Observation{EventCounts: map[string]int{}}, nil, workspace, 10, 1, true, ModeMock)
	if !strings.Contains(strings.Join(failures, "\n"), "timed out") {
		t.Fatalf("timeout not reported: %v", failures)
	}
}

func TestRealModeUsesRelaxedBudgetAndTimeout(t *testing.T) {
	task := Task{
		Name: "mode-aware",
		Budget: Budget{
			BudgetLimits: BudgetLimits{MaxModelCalls: 2, MaxToolCalls: 1, Strict: true},
			Real:         &BudgetLimits{MaxModelCalls: 5, MaxToolCalls: 4, Strict: true},
		},
	}
	obs := Observation{ModelCalls: 4, ToolCalls: 3, ToolCounts: map[string]int{}, EventCounts: map[string]int{}}
	workspace := t.TempDir()

	failures, _ := checkTask(task, obs, nil, workspace, 10, 0, false, ModeMock)
	if len(failures) != 2 {
		t.Fatalf("mock budget should fail both limits, got %v", failures)
	}
	failures, _ = checkTask(task, obs, nil, workspace, 10, 0, false, ModeReal)
	if len(failures) != 0 {
		t.Fatalf("real budget should allow extra steps, got %v", failures)
	}

	obs.ModelCalls, obs.ToolCalls = 6, 5
	failures, _ = checkTask(task, obs, nil, workspace, 10, 0, false, ModeReal)
	if len(failures) != 2 {
		t.Fatalf("real budget must still catch runaway runs, got %v", failures)
	}

	if got := task.Timeout(ModeMock); got != 90*time.Second {
		t.Fatalf("mock default timeout = %s, want 1m30s", got)
	}
	if got := task.Timeout(ModeReal); got != 300*time.Second {
		t.Fatalf("real default timeout = %s, want 5m0s", got)
	}
	task.TimeoutSec, task.TimeoutSecReal = 120, 420
	if got := task.Timeout(ModeMock); got != 120*time.Second {
		t.Fatalf("mock timeout = %s, want 2m0s", got)
	}
	if got := task.Timeout(ModeReal); got != 420*time.Second {
		t.Fatalf("real timeout = %s, want 7m0s", got)
	}
}

func TestLoadRealOverrides(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "real")
	writeFile(t, filepath.Join(dir, "task.json"), `{
	  "prompt": "x",
	  "timeout_sec": 60,
	  "timeout_sec_real": 420,
	  "budget": {
	    "max_model_calls": 2,
	    "strict": true,
	    "real": {"max_model_calls": 10, "max_tool_calls": 8}
	  }
	}`)
	task, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := task.Timeout(ModeReal); got != 420*time.Second {
		t.Fatalf("real timeout = %s, want 7m0s", got)
	}
	mockBudget := task.Budget.ForMode(ModeMock)
	if mockBudget.MaxModelCalls != 2 || !mockBudget.Strict {
		t.Fatalf("mock budget = %+v", mockBudget)
	}
	realBudget := task.Budget.ForMode(ModeReal)
	if realBudget.MaxModelCalls != 10 || realBudget.MaxToolCalls != 8 || realBudget.Strict {
		t.Fatalf("real budget = %+v", realBudget)
	}

	writeFile(t, filepath.Join(dir, "task.json"), `{"prompt":"x","timeout_sec_real":-1}`)
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "timeout_sec_real") {
		t.Fatalf("want negative timeout error, got %v", err)
	}
	writeFile(t, filepath.Join(dir, "task.json"), `{"prompt":"x","budget":{"real":{"max_tool_calls":-1}}}`)
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "budget.real") {
		t.Fatalf("want negative budget error, got %v", err)
	}
}

func TestBaselineDetectsPassAndMetricRegressions(t *testing.T) {
	base := Baseline{
		Mode: ModeMock,
		Tasks: map[string]BaselineTask{
			"was-passing":  {Passed: true, PassRate: 1, ModelCalls: 2, ToolCalls: 1, TotalTokens: 1000, CostUSD: 0.001},
			"costly":       {Passed: true, PassRate: 1, ModelCalls: 10, ToolCalls: 5, TotalTokens: 5000, CostUSD: 0.01},
			"fast":         {Passed: true, PassRate: 1, ModelCalls: 1, TotalTokens: 10, DurationMS: 100},
			"slow":         {Passed: true, PassRate: 1, ModelCalls: 1, TotalTokens: 10, DurationMS: 1000},
			"flaky-worse":  {Passed: false, PassRate: 2.0 / 3, ModelCalls: 2, ToolCalls: 1, TotalTokens: 1000},
			"flaky-better": {Passed: false, PassRate: 0, ModelCalls: 2, ToolCalls: 1, TotalTokens: 1000},
		},
	}
	report := Report{
		Mode: ModeMock,
		Tasks: []TaskResult{
			{Name: "was-passing", Status: StatusFailed, Metrics: Metrics{ModelCalls: 2, ToolCalls: 1}},
			{Name: "costly", Status: StatusPassed, Passed: true, PassRate: 1, Metrics: Metrics{ModelCalls: 20, ToolCalls: 5,
				Usage: llm.Usage{TotalTokens: 5500}, CostUSD: 0.01}},
			{Name: "fast", Status: StatusPassed, Passed: true, PassRate: 1, Metrics: Metrics{ModelCalls: 1,
				Usage: llm.Usage{TotalTokens: 10}, DurationMS: 400}},
			{Name: "slow", Status: StatusPassed, Passed: true, PassRate: 1, Metrics: Metrics{ModelCalls: 1,
				Usage: llm.Usage{TotalTokens: 10}, DurationMS: 2600}},
			{Name: "flaky-worse", Status: StatusFailed, PassRate: 1.0 / 3, Metrics: Metrics{ModelCalls: 2, ToolCalls: 1,
				Usage: llm.Usage{TotalTokens: 1000}}},
			{Name: "flaky-better", Status: StatusFailed, PassRate: 1.0 / 3, Metrics: Metrics{ModelCalls: 2, ToolCalls: 1,
				Usage: llm.Usage{TotalTokens: 1000}}},
		},
	}
	comparison := ApplyBaseline(&report, &base, "base.json", 25)
	if comparison.Regressions != 3 {
		t.Fatalf("want pass, metric, and pass-rate regressions, got %+v", comparison)
	}
	if report.Tasks[4].Baseline == nil || report.Tasks[4].Baseline.Status != "regressed" ||
		!strings.Contains(strings.Join(report.Tasks[4].Baseline.Reasons, " "), "pass rate 67% -> 33%") {
		t.Fatalf("pass-rate regression not annotated: %+v", report.Tasks[4].Baseline)
	}
	if report.Tasks[5].Baseline == nil || report.Tasks[5].Baseline.Status != "improved" {
		t.Fatalf("pass-rate improvement not annotated: %+v", report.Tasks[5].Baseline)
	}
	if report.Tasks[0].Baseline == nil || report.Tasks[0].Baseline.Status != "regressed" {
		t.Fatalf("pass regression not annotated: %+v", report.Tasks[0].Baseline)
	}
	if !strings.Contains(strings.Join(report.Tasks[1].Baseline.Reasons, " "), "model calls") {
		t.Fatalf("metric regression reason missing: %+v", report.Tasks[1].Baseline)
	}
	timeWarnings := 0
	for _, warning := range comparison.Warnings {
		if strings.Contains(warning, "wall time") {
			timeWarnings++
			if !strings.Contains(warning, "slow") {
				t.Fatalf("sub-second timing noise should not warn: %s", warning)
			}
		}
	}
	if timeWarnings != 1 {
		t.Fatalf("want exactly one meaningful time warning, got %v", comparison.Warnings)
	}
}

func TestRunTinySuiteEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}
	root := t.TempDir()
	binary := filepath.Join(root, "codedeep")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	moduleRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	build := exec.Command("go", "build", "-o", binary, "./cmd/codedeep")
	build.Dir = moduleRoot
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build codedeep: %v\n%s", err, out)
	}

	suite := filepath.Join(root, "suite", "smoke")
	writeFile(t, filepath.Join(suite, "task.json"), `{
	  "name": "smoke",
	  "prompt": "create ok.txt",
	  "mock": {"steps": [
	    {"text": "writing", "calls": [{"name": "write_file", "args": {"path": "ok.txt", "content": "ok\n"}}]},
	    {"text": "created ok.txt"}
	  ]},
	  "expect": {
	    "files": [{"path": "ok.txt", "contains": "ok"}],
	    "tools": {"required": ["write_file"], "counts": {"write_file": 1}},
	    "text_contains": ["created ok.txt"],
	    "no_error_events": true
	  },
	  "budget": {"max_model_calls": 2, "max_tool_calls": 1, "strict": true}
	}`)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	report, err := Run(ctx, Options{
		Suite:       filepath.Join(root, "suite"),
		Binary:      binary,
		Mode:        ModeMock,
		Repeat:      1,
		Jobs:        1,
		MinPassRate: 1,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Summary.Total != 1 || report.Summary.Passed != 1 || report.Summary.Failed != 0 {
		t.Fatalf("unexpected summary: %+v\nfailures: %v", report.Summary, report.Tasks[0].Failures)
	}
	if report.Tasks[0].Metrics.ModelCalls != 2 || report.Tasks[0].Metrics.ToolCalls != 1 {
		t.Fatalf("unexpected metrics: %+v", report.Tasks[0].Metrics)
	}
}
