// Package analytics aggregates saved sessions into the observability report used
// by the CLI and the web dashboard: token/cost activity over time, prompt-cache
// hit rates, per-model breakdowns, and tool call heatmaps.
package analytics

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"codedeep/internal/config"
	"codedeep/internal/llm"
	"codedeep/internal/session"
)

const (
	DefaultDays = 30
	MaxDays     = 365
	// maxHeatTools bounds the hourly heatmap rows; extra tools still appear in
	// the tool list.
	maxHeatTools = 8
	maxSessions  = 12
	dateLayout   = "2006-01-02"
)

type Options struct {
	Days     int
	Scope    string // "all" or "workspace"
	Now      time.Time
	Location *time.Location
}

// Report is the aggregated observability view. All durations are bucketed in
// the report location; Since is the start of the first included day.
type Report struct {
	Generated time.Time `json:"generated"`
	Since     time.Time `json:"since"`
	Days      int       `json:"days"`
	Scope     string    `json:"scope"`
	// Priced reports whether at least one included model has pricing configured.
	// Cost fields are zero and meaningless when it is false.
	Priced   bool          `json:"priced"`
	Summary  Summary       `json:"summary"`
	Daily    []Day         `json:"daily"`
	Models   []ModelStat   `json:"models"`
	Tools    []ToolStat    `json:"tools"`
	Heatmap  []HeatRow     `json:"heatmap"`
	Sessions []SessionStat `json:"sessions"`
}

type Summary struct {
	Sessions         int     `json:"sessions"`
	Turns            int     `json:"turns"`
	TotalTokens      int     `json:"total_tokens"`
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	CacheHitTokens   int     `json:"cache_hit_tokens"`
	CacheWriteTokens int     `json:"cache_write_tokens"`
	CacheHitRate     float64 `json:"cache_hit_rate"`
	ToolCalls        int     `json:"tool_calls"`
	CostUSD          float64 `json:"cost_usd"`
	ActiveDays       int     `json:"active_days"`
}

type Day struct {
	Date             string  `json:"date"`
	Sessions         int     `json:"sessions"`
	Turns            int     `json:"turns"`
	TotalTokens      int     `json:"total_tokens"`
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	CacheHitTokens   int     `json:"cache_hit_tokens"`
	ToolCalls        int     `json:"tool_calls"`
	CostUSD          float64 `json:"cost_usd"`
}

type ModelStat struct {
	Model        string  `json:"model"`
	Sessions     int     `json:"sessions"`
	Turns        int     `json:"turns"`
	TotalTokens  int     `json:"total_tokens"`
	CacheHitRate float64 `json:"cache_hit_rate"`
	CostUSD      float64 `json:"cost_usd"`
}

type ToolStat struct {
	Name     string  `json:"name"`
	Calls    int     `json:"calls"`
	Tokens   int     `json:"tokens"`
	CostUSD  float64 `json:"cost_usd"`
	Sessions int     `json:"sessions"`
}

// HeatRow is one tool's calls per hour of the day (0-23) in the report location.
type HeatRow struct {
	Name  string `json:"name"`
	Calls []int  `json:"calls"`
}

type SessionStat struct {
	ID           string    `json:"id"`
	Title        string    `json:"title"`
	Model        string    `json:"model"`
	Updated      time.Time `json:"updated"`
	Turns        int       `json:"turns"`
	TotalTokens  int       `json:"total_tokens"`
	CacheHitRate float64   `json:"cache_hit_rate"`
	ToolCalls    int       `json:"tool_calls"`
	CostUSD      float64   `json:"cost_usd"`
}

// Build loads every saved session and aggregates the requested time window.
func Build(cfg *config.Config, opts Options) (Report, error) {
	if cfg == nil {
		return Report{}, fmt.Errorf("nil config")
	}
	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}
	loc := opts.Location
	if loc == nil {
		loc = time.Local
	}
	opts.Days = NormalizeDays(opts.Days)
	opts.Scope = NormalizeScope(opts.Scope)
	now := opts.Now.In(loc)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	since := today.AddDate(0, 0, -(opts.Days - 1))

	store, err := session.NewStore(cfg.SessionsDir)
	if err != nil {
		return Report{}, err
	}
	sessions, err := store.All()
	if err != nil {
		return Report{}, err
	}

	report := Report{
		Generated: now.UTC(),
		Since:     since.UTC(),
		Days:      opts.Days,
		Scope:     opts.Scope,
		Daily:     make([]Day, opts.Days),
	}
	dayIndex := make(map[string]int, opts.Days)
	for i := range report.Daily {
		date := since.AddDate(0, 0, i).Format(dateLayout)
		report.Daily[i].Date = date
		dayIndex[date] = i
	}

	daySessions := make(map[string]map[string]bool)
	activeDays := map[string]bool{}
	modelTotals := map[string]*ModelStat{}
	modelPrompt := map[string]int{}
	modelHit := map[string]int{}
	toolTotals := map[string]*ToolStat{}
	toolSessions := map[string]map[string]bool{}
	heatCalls := map[string][]int{}

	for _, sess := range sessions {
		if opts.Scope == "workspace" && !sameDir(sess.Cwd, cfg.WorkDir) {
			continue
		}
		model := strings.TrimSpace(sess.Model)
		if model == "" {
			model = cfg.Model
		}

		var (
			used      bool
			sessUsage llm.Usage
			sessTurns int
			sessCalls int
			sessCost  float64
			modelSeen bool
		)

		addTool := func(name string, tu llm.ToolUsage, sessionID string, hour int) {
			if name == "" {
				return
			}
			stat := toolTotals[name]
			if stat == nil {
				stat = &ToolStat{Name: name}
				toolTotals[name] = stat
			}
			calls := tu.Calls
			if calls < 0 {
				calls = 0
			}
			stat.Calls += calls
			stat.Tokens += tu.Usage.TotalTokens
			stat.CostUSD += costOfTool(cfg, model, tu)
			if toolSessions[name] == nil {
				toolSessions[name] = map[string]bool{}
			}
			toolSessions[name][sessionID] = true
			row := heatCalls[name]
			if row == nil {
				row = make([]int, 24)
				heatCalls[name] = row
			}
			row[hour] += calls
		}

		addTurn := func(at time.Time, usage llm.Usage, tools map[string]llm.ToolUsage, turns int) {
			date := at.In(loc).Format(dateLayout)
			idx, ok := dayIndex[date]
			if !ok {
				return
			}
			day := &report.Daily[idx]
			day.Turns += turns
			day.TotalTokens += usage.TotalTokens
			day.PromptTokens += usage.PromptTokens
			day.CompletionTokens += usage.CompletionTokens
			day.CacheHitTokens += usage.PromptCacheHitTokens
			calls := 0
			for name, tu := range tools {
				c := tu.Calls
				if c < 0 {
					c = 0
				}
				calls += c
				addTool(name, tu, sess.ID, at.In(loc).Hour())
			}
			day.ToolCalls += calls
			sessCalls += calls
			day.CostUSD += costOf(cfg, model, usage)

			report.Summary.Turns += turns
			report.Summary.TotalTokens += usage.TotalTokens
			report.Summary.PromptTokens += usage.PromptTokens
			report.Summary.CompletionTokens += usage.CompletionTokens
			report.Summary.CacheHitTokens += usage.PromptCacheHitTokens
			report.Summary.CacheWriteTokens += usage.PromptCacheWriteTokens
			report.Summary.ToolCalls += calls

			sessUsage = sessUsage.Add(usage)
			sessTurns += turns
			used = true
			activeDays[date] = true
			if daySessions[date] == nil {
				daySessions[date] = map[string]bool{}
			}
			daySessions[date][sess.ID] = true
			modelPrompt[model] += usage.PromptTokens
			modelHit[model] += usage.PromptCacheHitTokens
			if !modelSeen {
				modelSeen = true
				stat := modelTotals[model]
				if stat == nil {
					stat = &ModelStat{Model: model}
					modelTotals[model] = stat
				}
				stat.Sessions++
			}
		}

		if len(sess.TurnStats) > 0 {
			for _, stat := range sess.TurnStats {
				at := stat.Time
				if at.IsZero() {
					at = sess.Updated
				}
				usage := stat.Usage
				turns := 1
				if usage.TotalTokens == 0 && len(stat.Tools) == 0 {
					turns = 0
				}
				if turns == 0 {
					continue
				}
				addTurn(at, usage, stat.Tools, turns)
			}
		} else {
			// Sessions written before per-turn stats only have totals; attribute
			// them to the last-updated day so old history still counts.
			at := sess.Updated
			if at.IsZero() {
				at = sess.Created
			}
			turns := sess.Turns
			if turns <= 0 {
				turns = countUserMessages(sess.Messages)
			}
			if sess.Usage.TotalTokens == 0 && len(sess.ToolUsage) == 0 {
				continue
			}
			addTurn(at, sess.Usage, sess.ToolUsage, turns)
		}

		if !used {
			continue
		}
		report.Summary.Sessions++
		cost, priced := cfg.CostFor(model, sessUsage)
		if priced {
			report.Priced = true
		}
		sessCost = cost
		report.Summary.CostUSD += sessCost

		if stat := modelTotals[model]; stat != nil {
			stat.Turns += sessTurns
			stat.TotalTokens += sessUsage.TotalTokens
			stat.CostUSD += sessCost
			stat.CacheHitRate = rate(llm.Usage{
				PromptTokens:         modelPrompt[model],
				PromptCacheHitTokens: modelHit[model],
			})
		}

		report.Sessions = append(report.Sessions, SessionStat{
			ID:           sess.ID,
			Title:        sess.Title,
			Model:        model,
			Updated:      sess.Updated,
			Turns:        sessTurns,
			TotalTokens:  sessUsage.TotalTokens,
			CacheHitRate: rate(sessUsage),
			ToolCalls:    sessCalls,
			CostUSD:      sessCost,
		})
	}

	for date, ids := range daySessions {
		if idx, ok := dayIndex[date]; ok {
			report.Daily[idx].Sessions = len(ids)
		}
	}
	report.Summary.ActiveDays = len(activeDays)
	report.Summary.CacheHitRate = rate(llm.Usage{
		PromptTokens:         report.Summary.PromptTokens,
		PromptCacheHitTokens: report.Summary.CacheHitTokens,
	})

	report.Models = make([]ModelStat, 0, len(modelTotals))
	for _, stat := range modelTotals {
		report.Models = append(report.Models, *stat)
	}
	sort.Slice(report.Models, func(i, j int) bool {
		if report.Models[i].CostUSD != report.Models[j].CostUSD {
			return report.Models[i].CostUSD > report.Models[j].CostUSD
		}
		if report.Models[i].TotalTokens != report.Models[j].TotalTokens {
			return report.Models[i].TotalTokens > report.Models[j].TotalTokens
		}
		return report.Models[i].Model < report.Models[j].Model
	})

	report.Tools = make([]ToolStat, 0, len(toolTotals))
	for _, stat := range toolTotals {
		stat.Sessions = len(toolSessions[stat.Name])
		report.Tools = append(report.Tools, *stat)
	}
	sort.Slice(report.Tools, func(i, j int) bool {
		if report.Tools[i].Calls != report.Tools[j].Calls {
			return report.Tools[i].Calls > report.Tools[j].Calls
		}
		if report.Tools[i].Tokens != report.Tools[j].Tokens {
			return report.Tools[i].Tokens > report.Tools[j].Tokens
		}
		return report.Tools[i].Name < report.Tools[j].Name
	})

	report.Heatmap = make([]HeatRow, 0, maxHeatTools)
	for i, stat := range report.Tools {
		if i == maxHeatTools {
			break
		}
		report.Heatmap = append(report.Heatmap, HeatRow{Name: stat.Name, Calls: heatCalls[stat.Name]})
	}

	sort.Slice(report.Sessions, func(i, j int) bool {
		if report.Sessions[i].CostUSD != report.Sessions[j].CostUSD {
			return report.Sessions[i].CostUSD > report.Sessions[j].CostUSD
		}
		if report.Sessions[i].TotalTokens != report.Sessions[j].TotalTokens {
			return report.Sessions[i].TotalTokens > report.Sessions[j].TotalTokens
		}
		return report.Sessions[i].Updated.After(report.Sessions[j].Updated)
	})
	if len(report.Sessions) > maxSessions {
		report.Sessions = report.Sessions[:maxSessions]
	}
	return report, nil
}

// NormalizeDays clamps the reporting window to a sensible range.
func NormalizeDays(days int) int {
	if days <= 0 {
		return DefaultDays
	}
	if days > MaxDays {
		return MaxDays
	}
	return days
}

// NormalizeScope accepts only workspace filtering; everything else means all.
func NormalizeScope(scope string) string {
	if strings.EqualFold(strings.TrimSpace(scope), "workspace") {
		return "workspace"
	}
	return "all"
}

func rate(u llm.Usage) float64 {
	if u.PromptTokens <= 0 {
		return 0
	}
	hit := u.PromptCacheHitTokens
	if hit < 0 {
		hit = 0
	}
	if hit > u.PromptTokens {
		hit = u.PromptTokens
	}
	return float64(hit) / float64(u.PromptTokens)
}

func costOf(cfg *config.Config, model string, u llm.Usage) float64 {
	cost, ok := cfg.CostFor(model, u)
	if !ok {
		return 0
	}
	return cost
}

func costOfTool(cfg *config.Config, model string, tu llm.ToolUsage) float64 {
	return costOf(cfg, model, tu.Usage)
}

func countUserMessages(msgs []llm.Message) int {
	n := 0
	for _, m := range msgs {
		if m.Role == "user" && !strings.HasPrefix(strings.ToLower(strings.TrimSpace(m.Content)), "[conversation summary") {
			n++
		}
	}
	return n
}

func sameDir(a, b string) bool {
	aa, errA := filepath.Abs(a)
	bb, errB := filepath.Abs(b)
	if errA != nil || errB != nil {
		return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
	}
	return strings.EqualFold(filepath.Clean(aa), filepath.Clean(bb))
}
