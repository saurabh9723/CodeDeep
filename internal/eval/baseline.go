package eval

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"
)

// Baseline stores deterministic per-task metrics for regression comparison.
type Baseline struct {
	Generated time.Time               `json:"generated"`
	Mode      Mode                    `json:"mode"`
	Provider  string                  `json:"provider,omitempty"`
	Model     string                  `json:"model,omitempty"`
	Tasks     map[string]BaselineTask `json:"tasks"`
}

// BaselineTask is one task's recorded outcome.
type BaselineTask struct {
	Passed      bool    `json:"passed"`
	PassRate    float64 `json:"pass_rate"`
	ModelCalls  int     `json:"model_calls"`
	ToolCalls   int     `json:"tool_calls"`
	TotalTokens int     `json:"total_tokens"`
	CostUSD     float64 `json:"cost_usd"`
	DurationMS  int64   `json:"duration_ms"`
}

// NewBaseline captures the current report. Skipped tasks are not recorded.
func NewBaseline(report Report) Baseline {
	base := Baseline{
		Generated: report.Generated,
		Mode:      report.Mode,
		Provider:  report.Provider,
		Model:     report.Model,
		Tasks:     map[string]BaselineTask{},
	}
	for _, task := range report.Tasks {
		if task.Status == StatusSkipped {
			continue
		}
		base.Tasks[task.Name] = BaselineTask{
			Passed:      task.Passed,
			PassRate:    task.PassRate,
			ModelCalls:  task.Metrics.ModelCalls,
			ToolCalls:   task.Metrics.ToolCalls,
			TotalTokens: task.Metrics.Usage.TotalTokens,
			CostUSD:     task.Metrics.CostUSD,
			DurationMS:  task.Metrics.DurationMS,
		}
	}
	return base
}

// WriteBaseline saves a baseline as indented JSON.
func WriteBaseline(path string, base Baseline) error {
	data, err := json.MarshalIndent(base, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// LoadBaseline reads a baseline file.
func LoadBaseline(path string) (*Baseline, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var base Baseline
	if err := json.Unmarshal(data, &base); err != nil {
		return nil, fmt.Errorf("parse baseline %s: %w", path, err)
	}
	if base.Tasks == nil {
		base.Tasks = map[string]BaselineTask{}
	}
	return &base, nil
}

const (
	defaultRegressionPct = 25.0
	timeRegressionPct    = 50.0
	// Sub-second tasks fluctuate wildly with process startup, so wall-time
	// warnings also need a meaningful absolute increase.
	timeRegressionFloorMS = 1000
)

// ApplyBaseline annotates tasks and returns the run-level comparison. Metric
// regressions are treated like pass regressions; timing drift is only a
// warning because wall clocks are noisy.
func ApplyBaseline(report *Report, base *Baseline, path string, thresholdPct float64) *BaselineComparison {
	if thresholdPct <= 0 {
		thresholdPct = defaultRegressionPct
	}
	comparison := &BaselineComparison{Path: path}
	if report.Mode != base.Mode {
		comparison.Warnings = append(comparison.Warnings,
			fmt.Sprintf("baseline mode is %s but this run is %s", base.Mode, report.Mode))
	}
	seen := map[string]bool{}
	for i := range report.Tasks {
		task := &report.Tasks[i]
		if task.Status == StatusSkipped {
			continue
		}
		seen[task.Name] = true
		record, ok := base.Tasks[task.Name]
		if !ok {
			continue
		}
		delta := &BaselineDelta{Status: "same"}
		var improved bool
		switch {
		case record.Passed && !task.Passed:
			delta.Status = "regressed"
			delta.Reasons = append(delta.Reasons, "passed in the baseline, fails now")
			comparison.Regressions++
		case !record.Passed && task.Passed:
			delta.Status = "improved"
			delta.Reasons = append(delta.Reasons, "failed in the baseline, passes now")
			improved = true
			comparison.Improvements++
		case task.PassRate < record.PassRate:
			// Repeated real-model runs rarely flip cleanly between pass and
			// fail, so a pass-rate drop is its own regression signal.
			if drop := -pctChange(record.PassRate, task.PassRate); drop > thresholdPct {
				delta.Status = "regressed"
				delta.Reasons = append(delta.Reasons,
					fmt.Sprintf("pass rate %.0f%% -> %.0f%%", record.PassRate*100, task.PassRate*100))
				comparison.Regressions++
			}
		case task.PassRate > record.PassRate:
			if rise := pctChange(record.PassRate, task.PassRate); rise > thresholdPct {
				improved = true
			}
		}
		if delta.Status != "improved" {
			if reason, regressed := metricRegression("model calls", float64(record.ModelCalls), float64(task.Metrics.ModelCalls), thresholdPct, 1, &delta.CallsPct); regressed {
				delta.Reasons = append(delta.Reasons, reason)
				delta.Status = "regressed"
				comparison.Regressions++
			} else if better := metricImprovement(float64(record.ModelCalls), float64(task.Metrics.ModelCalls), thresholdPct, &delta.CallsPct); better {
				improved = true
			}
			if reason, regressed := metricRegression("tool calls", float64(record.ToolCalls), float64(task.Metrics.ToolCalls), thresholdPct, 1, &delta.ToolCallsPct); regressed {
				delta.Reasons = append(delta.Reasons, reason)
				delta.Status = "regressed"
				comparison.Regressions++
			} else if better := metricImprovement(float64(record.ToolCalls), float64(task.Metrics.ToolCalls), thresholdPct, &delta.ToolCallsPct); better {
				improved = true
			}
			if reason, regressed := metricRegression("tokens", float64(record.TotalTokens), float64(task.Metrics.Usage.TotalTokens), thresholdPct, 100, &delta.TokensPct); regressed {
				delta.Reasons = append(delta.Reasons, reason)
				delta.Status = "regressed"
				comparison.Regressions++
			} else if better := metricImprovement(float64(record.TotalTokens), float64(task.Metrics.Usage.TotalTokens), thresholdPct, &delta.TokensPct); better {
				improved = true
			}
			if reason, regressed := metricRegression("cost", record.CostUSD, task.Metrics.CostUSD, thresholdPct, 0.000001, &delta.CostPct); regressed {
				delta.Reasons = append(delta.Reasons, reason)
				delta.Status = "regressed"
				comparison.Regressions++
			} else if better := metricImprovement(record.CostUSD, task.Metrics.CostUSD, thresholdPct, &delta.CostPct); better {
				improved = true
			}
			if record.DurationMS > 0 {
				delta.TimePct = pctChange(float64(record.DurationMS), float64(task.Metrics.DurationMS))
				if delta.TimePct > timeRegressionPct && task.Metrics.DurationMS-record.DurationMS > timeRegressionFloorMS {
					comparison.Warnings = append(comparison.Warnings,
						fmt.Sprintf("%s: wall time +%.0f%% (noise-sensitive)", task.Name, delta.TimePct))
				}
			}
		}
		if delta.Status == "same" && improved {
			delta.Status = "improved"
			comparison.Improvements++
		}
		task.Baseline = delta
	}
	for name := range base.Tasks {
		if !seen[name] {
			comparison.Warnings = append(comparison.Warnings,
				fmt.Sprintf("baseline task %s was not run", name))
		}
	}
	report.Baseline = comparison
	return comparison
}

func metricRegression(label string, base, current float64, threshold, floor float64, pctOut *float64) (string, bool) {
	if base < floor || current < floor {
		if pctOut != nil {
			*pctOut = pctChange(base, current)
		}
		return "", false
	}
	*pctOut = pctChange(base, current)
	if *pctOut > threshold {
		return fmt.Sprintf("%s +%.0f%% (%s -> %s)", label, *pctOut, metricString(base), metricString(current)), true
	}
	return "", false
}

func metricImprovement(base, current float64, threshold float64, pctOut *float64) bool {
	if base <= 0 {
		return false
	}
	*pctOut = pctChange(base, current)
	return *pctOut < -threshold
}

func pctChange(base, current float64) float64 {
	if base == 0 {
		if current > 0 {
			return 100
		}
		return 0
	}
	return math.Round((current-base)/base*1000) / 10
}

func metricString(v float64) string {
	if v >= 1000 {
		return fmt.Sprintf("%.1fk", v/1000)
	}
	return fmt.Sprintf("%.2f", v)
}
