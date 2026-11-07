package eval

import (
	"fmt"
	"time"

	"codedeep/internal/agent"
	"codedeep/internal/trace"
)

// saveEvalTrace persists one harness run so the web trace viewer can replay
// it after the temporary workspace is gone.
func saveEvalTrace(opts Options, task Task, run RunResult, events []agent.Event, started time.Time, workspace string, runNumber, runTotal int) (string, error) {
	title := task.Name
	if runTotal > 1 {
		title = fmt.Sprintf("%s run %d/%d", task.Name, runNumber, runTotal)
	}
	status := StatusPassed
	if !run.Passed {
		status = StatusFailed
	}
	if run.HarnessError != "" {
		status = StatusError
	}
	provider, model := providerLabel(opts), opts.Model
	if opts.Mode == ModeMock {
		provider, model = "openai", mockModel
	}
	var verifier *trace.Verifier
	if run.Verifier != nil {
		verifier = &trace.Verifier{
			Command:  run.Verifier.Command,
			ExitCode: run.Verifier.ExitCode,
			Output:   run.Verifier.Output,
		}
		if run.Verifier.Error != "" && verifier.Output == "" {
			verifier.Output = run.Verifier.Error
		}
	}
	passed := run.Passed
	store := &trace.Store{Dir: opts.TraceDir, Max: trace.DefaultMax}
	return store.Save(trace.Trace{
		Meta: trace.Meta{
			Kind:        trace.KindEval,
			Title:       title,
			Created:     started.UTC(),
			DurationMS:  run.Metrics.DurationMS,
			Provider:    provider,
			Model:       model,
			Mode:        string(opts.Mode),
			Status:      status,
			Cwd:         workspace,
			Task:        task.Name,
			Tags:        task.Tags,
			Passed:      &passed,
			Failures:    run.Failures,
			Warnings:    run.Warnings,
			ModelCalls:  run.Metrics.ModelCalls,
			ToolCalls:   run.Metrics.ToolCalls,
			TotalTokens: run.Metrics.Usage.TotalTokens,
			CostUSD:     run.Metrics.CostUSD,
		},
		Prompt:   task.Prompt,
		Verifier: verifier,
		Events:   events,
	})
}
