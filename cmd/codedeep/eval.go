package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"codedeep/internal/eval"
)

func cmdEval(args []string) int {
	fs := flag.NewFlagSet("codedeep eval", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		suite            string
		mode             string
		provider         string
		model            string
		baseURL          string
		pattern          string
		tags             string
		skipTags         string
		includeMockOnly  bool
		repeat           int
		jobs             int
		keep             bool
		verbose          bool
		minPassRate      float64
		baselinePath     string
		updateBaseline   string
		failOnRegression bool
		regressionPct    float64
		jsonOut          bool
		markdownPath     string
		listOnly         bool
		binary           string
		priceIn          float64
		priceOut         float64
		priceCacheHit    float64
		priceCacheWrite  float64
		traceDir         string
		noTraces         bool
	)
	home, _ := os.UserHomeDir()
	if home == "" {
		home = "."
	}
	fs.StringVar(&suite, "suite", "evals/tasks", "task suite directory")
	fs.StringVar(&mode, "mode", "mock", "provider mode: mock or real")
	fs.StringVar(&provider, "provider", "", "real provider: openai, anthropic, gemini, or ollama")
	fs.StringVar(&model, "model", "", "real model name (also used for pricing)")
	fs.StringVar(&baseURL, "base-url", "", "real provider base URL override")
	fs.StringVar(&pattern, "task", "", "only run tasks whose name matches this regexp")
	fs.StringVar(&tags, "tag", "", "comma-separated tags; a task must match at least one")
	fs.StringVar(&skipTags, "skip-tag", "", "comma-separated tags to skip")
	fs.BoolVar(&includeMockOnly, "include-mock-only", false, "run mock-only tasks in real mode")
	fs.IntVar(&repeat, "repeat", 1, "runs per task")
	fs.IntVar(&jobs, "jobs", 1, "parallel task count")
	fs.BoolVar(&keep, "keep", false, "keep temporary workspaces for debugging")
	fs.BoolVar(&verbose, "v", false, "show workspace paths and failed stderr tails")
	fs.Float64Var(&minPassRate, "min-pass-rate", 1, "fraction of repeated runs that must pass")
	fs.StringVar(&baselinePath, "baseline", "", "compare metrics against this baseline JSON")
	fs.StringVar(&updateBaseline, "update-baseline", "", "write this baseline JSON after the run")
	fs.BoolVar(&failOnRegression, "fail-on-regression", false, "exit non-zero when the baseline regresses")
	fs.Float64Var(&regressionPct, "regression-pct", 25, "metric regression threshold in percent")
	fs.BoolVar(&jsonOut, "json", false, "print the report as JSON")
	fs.StringVar(&markdownPath, "md", "", "write a Markdown report to this path")
	fs.BoolVar(&listOnly, "list", false, "list tasks without running them")
	fs.StringVar(&binary, "binary", "", "codedeep binary under test (default: this executable)")
	fs.Float64Var(&priceIn, "price-in", 0, "USD per million input tokens (real mode)")
	fs.Float64Var(&priceOut, "price-out", 0, "USD per million output tokens (real mode)")
	fs.Float64Var(&priceCacheHit, "price-cache-hit", 0, "USD per million cache-read tokens (real mode)")
	fs.Float64Var(&priceCacheWrite, "price-cache-write", 0, "USD per million cache-write tokens (real mode)")
	fs.StringVar(&traceDir, "trace-dir", filepath.Join(home, ".codedeep", "traces"), "directory for persisted run traces")
	fs.BoolVar(&noTraces, "no-traces", false, "do not persist run traces")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	parsedMode, err := eval.ParseMode(mode)
	if err != nil {
		fmt.Fprintln(os.Stderr, "codedeep eval:", err)
		return 2
	}
	provider = normalizeProvider(provider)
	filter := eval.Filter{
		Pattern:         pattern,
		Tags:            splitList(tags),
		SkipTags:        splitList(skipTags),
		Mode:            parsedMode,
		IncludeMockOnly: includeMockOnly,
	}
	if listOnly {
		return listEvalTasks(suite, filter)
	}
	if noTraces {
		traceDir = ""
	}
	opts := eval.Options{
		Suite:              suite,
		Binary:             binary,
		Mode:               parsedMode,
		Provider:           provider,
		Model:              model,
		BaseURL:            baseURL,
		Prices:             eval.Price{InputPerMTok: priceIn, OutputPerMTok: priceOut, CacheHitPerMTok: priceCacheHit, CacheWritePerMTok: priceCacheWrite},
		Pattern:            pattern,
		Tags:               filter.Tags,
		SkipTags:           filter.SkipTags,
		IncludeMockOnly:    includeMockOnly,
		Repeat:             repeat,
		Jobs:               jobs,
		Keep:               keep,
		Verbose:            verbose,
		MinPassRate:        minPassRate,
		BaselinePath:       baselinePath,
		UpdateBaselinePath: updateBaseline,
		FailOnRegression:   failOnRegression,
		RegressionPct:      regressionPct,
		TraceDir:           traceDir,
	}
	ctx, cancel := signalContext(context.Background())
	defer cancel()
	report, runErr := eval.Run(ctx, opts)
	if runErr != nil {
		fmt.Fprintln(os.Stderr, "codedeep eval:", runErr)
		return 2
	}
	if jsonOut {
		if err := eval.WriteJSON(os.Stdout, report); err != nil {
			fmt.Fprintln(os.Stderr, "codedeep eval:", err)
			return 2
		}
	} else {
		eval.WriteHuman(os.Stdout, report, verbose)
	}
	if markdownPath != "" {
		file, err := os.Create(markdownPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "codedeep eval:", err)
			return 2
		}
		writeErr := eval.WriteMarkdown(file, report)
		closeErr := file.Close()
		if writeErr != nil {
			fmt.Fprintln(os.Stderr, "codedeep eval:", writeErr)
			return 2
		}
		if closeErr != nil {
			fmt.Fprintln(os.Stderr, "codedeep eval:", closeErr)
			return 2
		}
	}
	if report.Summary.Errors > 0 || report.HarnessErr != "" {
		return 2
	}
	if report.Summary.Failed > 0 {
		return 1
	}
	if failOnRegression && report.Baseline != nil && report.Baseline.Regressions > 0 {
		return 1
	}
	return 0
}

func listEvalTasks(suite string, filter eval.Filter) int {
	tasks, err := eval.Discover(suite)
	if err != nil {
		fmt.Fprintln(os.Stderr, "codedeep eval:", err)
		return 2
	}
	if err := filter.Validate(); err != nil {
		fmt.Fprintln(os.Stderr, "codedeep eval:", err)
		return 2
	}
	count := 0
	for _, task := range tasks {
		selected, reason, skipped := filter.Select(task)
		if !selected {
			continue
		}
		count++
		status := ""
		if skipped {
			status = "  (skip: " + reason + ")"
		}
		tagText := ""
		if len(task.Tags) > 0 {
			tagText = "  [" + strings.Join(task.Tags, ",") + "]"
		}
		fmt.Printf("%-28s%s%s\n", task.Name, tagText, status)
		if task.Description != "" {
			fmt.Printf("  %s\n", task.Description)
		}
	}
	if count == 0 {
		fmt.Printf("no tasks matched under %s\n", suite)
	}
	return 0
}

func splitList(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func normalizeProvider(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "openai", "deepseek":
		return "openai"
	case "anthropic", "claude":
		return "anthropic"
	case "gemini", "google":
		return "gemini"
	case "ollama":
		return "ollama"
	default:
		return strings.ToLower(strings.TrimSpace(raw))
	}
}
