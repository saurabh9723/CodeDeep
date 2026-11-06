package eval

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"codedeep/internal/agent"
	"codedeep/internal/mockllm"
	"codedeep/internal/proc"
	"codedeep/internal/tools"
)

// Options configures a harness run.
type Options struct {
	Suite              string
	Binary             string
	Mode               Mode
	Provider           string
	Model              string
	BaseURL            string
	Prices             Price
	Pattern            string
	Tags               []string
	SkipTags           []string
	IncludeMockOnly    bool
	Repeat             int
	Jobs               int
	Keep               bool
	Verbose            bool
	MinPassRate        float64
	BaselinePath       string
	UpdateBaselinePath string
	FailOnRegression   bool
	RegressionPct      float64
}

// Run discovers, executes, and scores tasks.
func Run(ctx context.Context, opts Options) (Report, error) {
	if opts.Mode == "" {
		opts.Mode = ModeMock
	}
	if opts.Repeat <= 0 {
		opts.Repeat = 1
	}
	if opts.Jobs <= 0 {
		opts.Jobs = 1
	}
	if opts.Jobs > 16 {
		opts.Jobs = 16
	}
	if opts.MinPassRate <= 0 {
		opts.MinPassRate = 1
	}
	if opts.MinPassRate > 1 {
		return Report{}, fmt.Errorf("min pass rate must be between 0 and 1")
	}
	if opts.Suite == "" {
		opts.Suite = filepath.Join("evals", "tasks")
	}
	filter := Filter{
		Pattern:         opts.Pattern,
		Tags:            opts.Tags,
		SkipTags:        opts.SkipTags,
		Mode:            opts.Mode,
		IncludeMockOnly: opts.IncludeMockOnly,
	}
	if err := filter.Validate(); err != nil {
		return Report{}, err
	}
	if opts.Mode == ModeReal {
		if err := CheckRealCredentials(opts.Provider); err != nil {
			return Report{}, err
		}
	}
	binary, err := resolveBinary(opts.Binary)
	if err != nil {
		return Report{}, err
	}
	all, err := Discover(opts.Suite)
	if err != nil {
		return Report{}, err
	}

	type job struct {
		index int
		task  Task
	}
	var tasks []TaskResult
	var jobs []job
	active := 0
	for _, task := range all {
		selected, reason, skipped := filter.Select(task)
		if !selected {
			continue
		}
		idx := len(tasks)
		if skipped {
			tasks = append(tasks, TaskResult{
				Name:        task.Name,
				Description: task.Description,
				Tags:        task.Tags,
				Status:      StatusSkipped,
				SkipReason:  reason,
			})
			continue
		}
		tasks = append(tasks, TaskResult{})
		jobs = append(jobs, job{index: idx, task: task})
		active++
	}
	if active == 0 {
		return Report{}, fmt.Errorf("no runnable tasks selected from %s", opts.Suite)
	}

	runRoot, err := os.MkdirTemp("", "codedeep-eval-*")
	if err != nil {
		return Report{}, err
	}
	if !opts.Keep {
		defer os.RemoveAll(runRoot)
	}
	gocache := filepath.Join(runRoot, "gocache")
	if err := os.MkdirAll(gocache, 0o755); err != nil {
		return Report{}, err
	}

	started := time.Now()
	sem := make(chan struct{}, opts.Jobs)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var harnessErrors []string
	for _, j := range jobs {
		j := j
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				mu.Lock()
				tasks[j.index] = TaskResult{
					Name:     j.task.Name,
					Status:   StatusError,
					Failures: []string{"cancelled before start"},
				}
				mu.Unlock()
				return
			}
			defer func() { <-sem }()
			result := runTask(ctx, opts, j.task, j.index, runRoot, gocache, binary)
			mu.Lock()
			tasks[j.index] = result
			if result.Status == StatusError {
				harnessErrors = append(harnessErrors, result.Name)
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	sortReports(tasks)

	report := Report{
		Generated: time.Now().UTC(),
		Suite:     opts.Suite,
		Mode:      opts.Mode,
		Provider:  providerLabel(opts),
		Model:     opts.Model,
		Tasks:     tasks,
	}
	if opts.Mode == ModeMock {
		report.Provider = "openai"
		report.Model = mockModel
	}
	report.Summary = summarize(tasks, time.Since(started).Milliseconds())
	if len(harnessErrors) > 0 {
		report.HarnessErr = "task setup errors: " + strings.Join(harnessErrors, ", ")
	}
	if opts.BaselinePath != "" {
		base, err := LoadBaseline(opts.BaselinePath)
		if err != nil {
			return report, fmt.Errorf("load baseline: %w", err)
		}
		ApplyBaseline(&report, base, opts.BaselinePath, opts.RegressionPct)
	}
	if opts.UpdateBaselinePath != "" {
		base := NewBaseline(report)
		if err := WriteBaseline(opts.UpdateBaselinePath, base); err != nil {
			return report, fmt.Errorf("write baseline: %w", err)
		}
	}
	return report, nil
}

func providerLabel(opts Options) string {
	if opts.Mode == ModeMock {
		return "openai"
	}
	return nonEmpty(opts.Provider, "openai")
}

func resolveBinary(raw string) (string, error) {
	if raw == "" {
		exe, err := os.Executable()
		if err != nil {
			return "", fmt.Errorf("locate codedeep binary: %w", err)
		}
		raw = exe
	}
	if strings.ContainsAny(raw, `/\`) {
		abs, err := filepath.Abs(raw)
		if err != nil {
			return "", err
		}
		info, err := os.Stat(abs)
		if err != nil {
			return "", fmt.Errorf("codedeep binary: %w", err)
		}
		if info.IsDir() {
			return "", fmt.Errorf("codedeep binary %s is a directory", abs)
		}
		return abs, nil
	}
	path, err := exec.LookPath(raw)
	if err != nil {
		return "", fmt.Errorf("find %s in PATH: %w", raw, err)
	}
	return path, nil
}

func runTask(ctx context.Context, opts Options, task Task, index int, runRoot, gocache, binary string) TaskResult {
	result := TaskResult{
		Name:        task.Name,
		Description: task.Description,
		Tags:        task.Tags,
	}
	taskRoot := filepath.Join(runRoot, fmt.Sprintf("%02d-%s", index+1, safeName(task.Name)))
	if opts.Keep {
		result.Workspace = taskRoot
	}
	for i := 0; i < opts.Repeat; i++ {
		if ctx.Err() != nil {
			break
		}
		run := runOnce(ctx, opts, task, filepath.Join(taskRoot, fmt.Sprintf("run-%d", i+1)), gocache, binary)
		result.Runs = append(result.Runs, run)
	}
	return aggregateTask(opts, result)
}

func aggregateTask(opts Options, result TaskResult) TaskResult {
	runs := result.Runs
	if len(runs) == 0 {
		result.Status = StatusError
		result.Failures = []string{"no runs completed"}
		return result
	}
	passes := 0
	harnessError := false
	var failures, warnings []string
	failureSeen := map[string]bool{}
	warningSeen := map[string]bool{}
	for i, run := range runs {
		if run.HarnessError != "" {
			harnessError = true
			failures = append(failures, "run "+runLabel(i, len(runs))+": setup: "+run.HarnessError)
		}
		if run.Passed {
			passes++
		}
		for _, reason := range run.Failures {
			label := ""
			if len(runs) > 1 {
				label = "run " + runLabel(i, len(runs)) + ": "
			}
			msg := label + reason
			if !failureSeen[msg] {
				failureSeen[msg] = true
				failures = append(failures, msg)
			}
		}
		for _, warning := range run.Warnings {
			if !warningSeen[warning] {
				warningSeen[warning] = true
				warnings = append(warnings, warning)
			}
		}
	}
	result.PassRate = float64(passes) / float64(len(runs))
	result.Metrics = medianMetrics(runs)
	if harnessError {
		result.Status = StatusError
		result.Failures = failures
		result.Warnings = warnings
		return result
	}
	if result.PassRate >= opts.MinPassRate && passes > 0 {
		result.Status = StatusPassed
		result.Passed = true
		if len(failures) > 0 {
			for _, failure := range failures {
				warnings = append(warnings, "flaky: "+failure)
			}
		}
	} else {
		result.Status = StatusFailed
		result.Failures = failures
	}
	result.Warnings = warnings
	return result
}

func runLabel(index, total int) string {
	if total <= 1 {
		return "1"
	}
	return fmt.Sprintf("%d/%d", index+1, total)
}

func runOnce(ctx context.Context, opts Options, task Task, runDir, gocache, binary string) RunResult {
	workspace := filepath.Join(runDir, "workspace")
	home := filepath.Join(runDir, "home")
	sessions := filepath.Join(runDir, "sessions")
	for _, dir := range []string{workspace, home, sessions} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return setupFailure(err.Error())
		}
	}
	if err := prepareWorkspace(task, workspace); err != nil {
		return setupFailure(err.Error())
	}
	approval := effectiveApproval(task)

	mockURL := ""
	mockRequests := 0
	mockLog := &bytes.Buffer{}
	var mockServer *mockllm.Server
	if opts.Mode == ModeMock {
		if task.Mock.Scenario == "" && len(task.Mock.Steps) == 0 {
			return setupFailure("mock mode requires mock.scenario or mock.steps")
		}
		mockServer = mockllm.New(mockllm.Options{
			Scenario: task.Mock.Scenario,
			Steps:    task.Mock.Steps,
			Delay:    time.Duration(task.Mock.DelayMS) * time.Millisecond,
			Logger:   log.New(mockLog, "", 0),
		})
		server := httptest.NewServer(mockServer)
		defer server.Close()
		mockURL = server.URL
	}

	cfgPath := filepath.Join(runDir, "config.json")
	cfgData, err := buildConfig(task, ProviderOptions{
		Mode:     opts.Mode,
		Provider: opts.Provider,
		Model:    opts.Model,
		BaseURL:  opts.BaseURL,
		Prices:   opts.Prices,
	}, mockURL, sessions)
	if err != nil {
		return setupFailure("build config: " + err.Error())
	}
	if err := writeConfig(cfgPath, cfgData); err != nil {
		return setupFailure("write config: " + err.Error())
	}
	envBaseURL := mockURL
	if opts.Mode == ModeReal {
		envBaseURL = opts.BaseURL
	}
	env := buildEnv(EnvOptions{
		Mode:        opts.Mode,
		Provider:    opts.Provider,
		Model:       opts.Model,
		BaseURL:     envBaseURL,
		Approval:    approval,
		SessionsDir: sessions,
		Home:        home,
		GoCache:     gocache,
	})

	runCtx, cancel := context.WithTimeout(ctx, task.Timeout(opts.Mode))
	defer cancel()
	cmd := exec.CommandContext(runCtx, binary,
		"-config", cfgPath,
		"-cwd", workspace,
		"-p", task.Prompt,
		"-json",
	)
	cmd.Dir = workspace
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	proc.Prepare(cmd)
	started := time.Now()
	runErr := cmd.Run()
	durationMS := time.Since(started).Milliseconds()
	timedOut := errors.Is(runCtx.Err(), context.DeadlineExceeded)
	if mockServer != nil {
		mockRequests = mockServer.Requests()
	}
	exitCode := 0
	var startErr error
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			startErr = runErr
		}
	}
	_ = os.WriteFile(filepath.Join(runDir, "stdout.ndjson"), stdout.Bytes(), 0o644)
	_ = os.WriteFile(filepath.Join(runDir, "stderr.log"), stderr.Bytes(), 0o644)
	if mockLog.Len() > 0 {
		_ = os.WriteFile(filepath.Join(runDir, "mock.log"), mockLog.Bytes(), 0o644)
	}
	if startErr != nil {
		run := setupFailure("start codedeep: " + startErr.Error())
		run.StderrTail = truncate(stderr.String(), 2000)
		return run
	}
	events, invalidLines := parseEvents(stdout.Bytes())
	obs := Observe(events)
	obs.InvalidLines = invalidLines

	verifier := runVerifier(ctx, task, workspace, env)
	failures, warnings := checkTask(task, obs, verifier, workspace, durationMS, exitCode, timedOut, opts.Mode)
	if len(events) == 0 {
		failures = append(failures, "no agent events were emitted")
	}
	if invalidLines > 0 {
		warnings = append(warnings, fmt.Sprintf("ignored %d non-JSON stdout line(s)", invalidLines))
	}
	run := RunResult{
		Passed:       len(failures) == 0,
		ExitCode:     exitCode,
		TimedOut:     timedOut,
		Failures:     failures,
		Warnings:     warnings,
		Metrics:      metricsFor(obs, durationMS),
		Text:         truncate(obs.Text, 8000),
		Errors:       obs.Errors,
		Verifies:     truncateVerifyRecords(obs.Verifies),
		Verifier:     verifier,
		MockRequests: mockRequests,
		StderrTail:   truncate(stderr.String(), 4000),
	}
	return run
}

func setupFailure(reason string) RunResult {
	return RunResult{
		Passed:       false,
		HarnessError: reason,
		Failures:     []string{"setup: " + reason},
	}
}

func effectiveApproval(task Task) string {
	if raw, ok := task.Config["approval"].(string); ok && strings.TrimSpace(raw) != "" {
		return strings.ToLower(strings.TrimSpace(raw))
	}
	return "auto"
}

func prepareWorkspace(task Task, workspace string) error {
	fixtures := filepath.Join(task.Dir, task.FixtureDir)
	if err := copyFixtures(fixtures, workspace); err != nil {
		return fmt.Errorf("copy fixtures: %w", err)
	}
	for _, setup := range task.Setup {
		target, err := safeJoin(workspace, setup.Path)
		if err != nil {
			return fmt.Errorf("setup %s: %w", setup.Path, err)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if setup.Mode != "" {
			var parsed uint32
			if _, err := fmt.Sscanf(setup.Mode, "%o", &parsed); err != nil {
				return fmt.Errorf("setup %s: invalid mode %q", setup.Path, setup.Mode)
			}
			mode = os.FileMode(parsed)
		}
		if err := os.WriteFile(target, []byte(setup.Content), mode); err != nil {
			return fmt.Errorf("setup %s: %w", setup.Path, err)
		}
	}
	return nil
}

func copyFixtures(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", src)
	}
	return filepath.WalkDir(src, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		target := filepath.Join(dst, rel)
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("fixtures may not contain symlinks: %s", rel)
		}
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		mode := entry.Type().Perm()
		if mode == 0 {
			mode = 0o644
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, data, mode)
	})
}

func runVerifier(parent context.Context, task Task, workspace string, env []string) *VerifierResult {
	if strings.TrimSpace(task.Verifier) == "" {
		return nil
	}
	timeoutSec := task.VerifierTimeoutSec
	if timeoutSec <= 0 {
		timeoutSec = 60
	}
	ctx, cancel := context.WithTimeout(parent, time.Duration(timeoutSec)*time.Second)
	defer cancel()
	shell, args := tools.ShellCommand("", task.Verifier)
	cmd := exec.CommandContext(ctx, shell, args...)
	cmd.Dir = workspace
	cmd.Env = env
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	proc.Prepare(cmd)
	started := time.Now()
	err := cmd.Run()
	result := &VerifierResult{
		Command:    task.Verifier,
		Output:     truncate(output.String(), 16000),
		DurationMS: time.Since(started).Milliseconds(),
	}
	if ctx.Err() == context.DeadlineExceeded {
		result.Error = "timed out"
		result.ExitCode = -1
		return result
	}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			result.ExitCode = exitErr.ExitCode()
		} else {
			result.Error = err.Error()
			result.ExitCode = -1
		}
		return result
	}
	result.ExitCode = 0
	return result
}

func parseEvents(data []byte) ([]agent.Event, int) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64*1024), 8<<20)
	var events []agent.Event
	invalid := 0
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var ev agent.Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			invalid++
			continue
		}
		events = append(events, ev)
	}
	return events, invalid
}

func truncateVerifyRecords(records []VerifyRecord) []VerifyRecord {
	out := make([]VerifyRecord, 0, len(records))
	for _, record := range records {
		record.Output = truncate(record.Output, 2000)
		out = append(out, record)
	}
	return out
}

func safeName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
		if b.Len() >= 40 {
			break
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "task"
	}
	return out
}
