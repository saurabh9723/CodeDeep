package analytics

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"codedeep/internal/config"
	"codedeep/internal/llm"
	"codedeep/internal/session"
)

func testConfig(t *testing.T, workspace, sessionsDir string) *config.Config {
	t.Helper()
	return &config.Config{
		Model:       "model-a",
		WorkDir:     workspace,
		SessionsDir: sessionsDir,
		Pricing: map[string]config.Price{
			"model-a": {InputPerMTok: 1, OutputPerMTok: 2, CacheHitPerMTok: 0.5, CacheWritePerMTok: 1},
			"model-b": {InputPerMTok: 2, OutputPerMTok: 1},
		},
	}
}

func usage(prompt, completion, hit, write int) llm.Usage {
	return llm.Usage{
		PromptTokens:           prompt,
		CompletionTokens:       completion,
		TotalTokens:            prompt + completion,
		PromptCacheHitTokens:   hit,
		PromptCacheWriteTokens: write,
	}
}

func writeSession(t *testing.T, dir string, sess *session.Session) {
	t.Helper()
	data, err := json.Marshal(sess)
	if err != nil {
		t.Fatalf("marshal session: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, sess.ID+".json"), data, 0o644); err != nil {
		t.Fatalf("write session: %v", err)
	}
}

func day(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func fixture(t *testing.T) (*config.Config, *session.Store) {
	t.Helper()
	workspace := t.TempDir()
	sessionsDir := t.TempDir()
	cfg := testConfig(t, workspace, sessionsDir)
	store, err := session.NewStore(sessionsDir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	d1 := day(2026, time.November, 1)
	d2 := day(2026, time.November, 2)
	writeSession(t, sessionsDir, &session.Session{
		ID:      "a",
		Title:   "Build feature",
		Model:   "model-a",
		Cwd:     workspace,
		Created: d1,
		Updated: d2,
		Turns:   2,
		TurnStats: []session.TurnStat{
			{
				Turn:  1,
				Time:  d1.Add(10 * time.Hour),
				Usage: usage(1000, 200, 400, 100),
				Tools: map[string]llm.ToolUsage{
					"read_file": {Calls: 2, Usage: usage(500, 100, 200, 0)},
					"bash":      {Calls: 1, Usage: usage(300, 50, 0, 0)},
				},
			},
			{
				Turn:  2,
				Time:  d2.Add(11 * time.Hour),
				Usage: usage(500, 100, 100, 0),
				Tools: map[string]llm.ToolUsage{
					"read_file": {Calls: 1, Usage: usage(250, 50, 50, 0)},
				},
			},
		},
	})
	writeSession(t, sessionsDir, &session.Session{
		ID:        "b",
		Title:     "Other repo",
		Model:     "model-b",
		Cwd:       t.TempDir(),
		Created:   d1,
		Updated:   d1.Add(12 * time.Hour),
		Turns:     2,
		Usage:     usage(200, 50, 0, 0),
		ToolUsage: map[string]llm.ToolUsage{"bash": {Calls: 3, Usage: usage(200, 50, 0, 0)}},
	})
	return cfg, store
}

func build(t *testing.T, cfg *config.Config, opts Options) Report {
	t.Helper()
	if opts.Now.IsZero() {
		opts.Now = time.Date(2026, time.November, 3, 12, 0, 0, 0, time.UTC)
	}
	if opts.Location == nil {
		opts.Location = time.UTC
	}
	rep, err := Build(cfg, opts)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return rep
}

func TestBuildAggregatesActivity(t *testing.T) {
	cfg, _ := fixture(t)
	rep := build(t, cfg, Options{Days: 30, Scope: "all"})

	if rep.Summary.Sessions != 2 || rep.Summary.Turns != 4 {
		t.Fatalf("summary sessions/turns = %d/%d, want 2/4", rep.Summary.Sessions, rep.Summary.Turns)
	}
	if rep.Summary.TotalTokens != 2050 || rep.Summary.PromptTokens != 1700 || rep.Summary.CompletionTokens != 350 {
		t.Errorf("tokens = %+v", rep.Summary)
	}
	if rep.Summary.CacheHitTokens != 500 || rep.Summary.CacheWriteTokens != 100 {
		t.Errorf("cache tokens = %d/%d", rep.Summary.CacheHitTokens, rep.Summary.CacheWriteTokens)
	}
	if diff := rep.Summary.CacheHitRate - 500.0/1700.0; math.Abs(diff) > 1e-9 {
		t.Errorf("cache hit rate = %v, want %v", rep.Summary.CacheHitRate, 500.0/1700.0)
	}
	if rep.Summary.ToolCalls != 7 {
		t.Errorf("tool calls = %d, want 7", rep.Summary.ToolCalls)
	}
	if rep.Summary.ActiveDays != 2 {
		t.Errorf("active days = %d, want 2", rep.Summary.ActiveDays)
	}
	if !rep.Priced {
		t.Error("report should be priced")
	}
	if diff := rep.Summary.CostUSD - 0.0023; math.Abs(diff) > 1e-12 {
		t.Errorf("cost = %v, want 0.0023", rep.Summary.CostUSD)
	}

	byDate := map[string]Day{}
	for _, d := range rep.Daily {
		byDate[d.Date] = d
	}
	d1 := byDate["2026-11-01"]
	if d1.TotalTokens != 1450 || d1.Turns != 3 || d1.Sessions != 2 || d1.ToolCalls != 6 {
		t.Errorf("day 1 = %+v", d1)
	}
	d2 := byDate["2026-11-02"]
	if d2.TotalTokens != 600 || d2.Turns != 1 || d2.Sessions != 1 || d2.ToolCalls != 1 {
		t.Errorf("day 2 = %+v", d2)
	}

	tools := map[string]ToolStat{}
	for _, tool := range rep.Tools {
		tools[tool.Name] = tool
	}
	if got := tools["read_file"]; got.Calls != 3 || got.Tokens != 900 || got.Sessions != 1 {
		t.Errorf("read_file = %+v", got)
	}
	if got := tools["bash"]; got.Calls != 4 || got.Tokens != 600 || got.Sessions != 2 {
		t.Errorf("bash = %+v", got)
	}

	models := map[string]ModelStat{}
	for _, m := range rep.Models {
		models[m.Model] = m
	}
	if got := models["model-a"]; got.Sessions != 1 || got.Turns != 2 || got.TotalTokens != 1800 {
		t.Errorf("model-a = %+v", got)
	} else if diff := got.CacheHitRate - 500.0/1500.0; math.Abs(diff) > 1e-9 {
		t.Errorf("model-a cache rate = %v, want %v", got.CacheHitRate, 500.0/1500.0)
	}
	if got := models["model-b"]; got.Sessions != 1 || got.Turns != 2 || got.TotalTokens != 250 {
		t.Errorf("model-b = %+v", got)
	} else if got.CacheHitRate != 0 {
		t.Errorf("model-b cache rate = %v, want 0", got.CacheHitRate)
	}

	if len(rep.Sessions) != 2 || rep.Sessions[0].ID != "a" {
		t.Fatalf("sessions = %+v", rep.Sessions)
	}
	if rep.Sessions[0].TotalTokens != 1800 || rep.Sessions[0].Turns != 2 {
		t.Errorf("session a = %+v", rep.Sessions[0])
	}
}

func TestHeatmapBucketsByHour(t *testing.T) {
	cfg, _ := fixture(t)
	rep := build(t, cfg, Options{Days: 30})
	rows := map[string][]int{}
	for _, row := range rep.Heatmap {
		if len(row.Calls) != 24 {
			t.Fatalf("%s has %d hour buckets", row.Name, len(row.Calls))
		}
		rows[row.Name] = row.Calls
	}
	read := rows["read_file"]
	if read[10] != 2 || read[11] != 1 {
		t.Errorf("read_file hours = %v", read)
	}
	bash := rows["bash"]
	if bash[10] != 1 || bash[12] != 3 {
		t.Errorf("bash hours = %v", bash)
	}
}

func TestWorkspaceScopeFiltersSessions(t *testing.T) {
	cfg, _ := fixture(t)
	rep := build(t, cfg, Options{Days: 30, Scope: "workspace"})
	if rep.Scope != "workspace" {
		t.Errorf("scope = %q", rep.Scope)
	}
	if rep.Summary.Sessions != 1 || rep.Summary.TotalTokens != 1800 {
		t.Errorf("workspace summary = %+v", rep.Summary)
	}
	if len(rep.Sessions) != 1 || rep.Sessions[0].ID != "a" {
		t.Errorf("workspace sessions = %+v", rep.Sessions)
	}
}

func TestDateRangeExcludesOldTurns(t *testing.T) {
	cfg, _ := fixture(t)
	rep := build(t, cfg, Options{
		Days: 1,
		Now:  time.Date(2026, time.November, 2, 12, 0, 0, 0, time.UTC),
	})
	if rep.Summary.Sessions != 1 || rep.Summary.Turns != 1 || rep.Summary.TotalTokens != 600 {
		t.Fatalf("range summary = %+v", rep.Summary)
	}
	if rep.Summary.ToolCalls != 1 || len(rep.Tools) != 1 || rep.Tools[0].Name != "read_file" {
		t.Errorf("range tools = %+v", rep.Tools)
	}
	if diff := rep.Summary.CacheHitRate - 0.2; math.Abs(diff) > 1e-9 {
		t.Errorf("range cache rate = %v, want 0.2", rep.Summary.CacheHitRate)
	}
	if len(rep.Heatmap) != 1 || rep.Heatmap[0].Calls[11] != 1 {
		t.Errorf("range heatmap = %+v", rep.Heatmap)
	}
}

func TestLegacySessionFallsBackToTotals(t *testing.T) {
	workspace := t.TempDir()
	sessionsDir := t.TempDir()
	cfg := testConfig(t, workspace, sessionsDir)
	updated := day(2026, time.November, 1).Add(12 * time.Hour)
	writeSession(t, sessionsDir, &session.Session{
		ID:        "legacy",
		Title:     "Old session",
		Model:     "model-b",
		Cwd:       workspace,
		Created:   updated.Add(-time.Hour),
		Updated:   updated,
		Turns:     2,
		Usage:     usage(200, 50, 0, 0),
		ToolUsage: map[string]llm.ToolUsage{"bash": {Calls: 3, Usage: usage(200, 50, 0, 0)}},
	})
	rep := build(t, cfg, Options{Days: 30})
	if rep.Summary.Sessions != 1 || rep.Summary.Turns != 2 || rep.Summary.TotalTokens != 250 {
		t.Fatalf("legacy summary = %+v", rep.Summary)
	}
	if len(rep.Tools) != 1 || rep.Tools[0].Calls != 3 {
		t.Errorf("legacy tools = %+v", rep.Tools)
	}
	if len(rep.Heatmap) != 1 || rep.Heatmap[0].Calls[12] != 3 {
		t.Errorf("legacy heatmap = %+v", rep.Heatmap)
	}
	if len(rep.Daily) != 30 || rep.Daily[27].Date != "2026-11-01" || rep.Daily[27].TotalTokens != 250 {
		t.Errorf("legacy daily = %+v", rep.Daily)
	}
}

func TestEmptyReportZeroFillsDays(t *testing.T) {
	cfg := testConfig(t, t.TempDir(), t.TempDir())
	rep := build(t, cfg, Options{Days: 7})
	if len(rep.Daily) != 7 {
		t.Fatalf("daily = %d", len(rep.Daily))
	}
	if rep.Summary.Sessions != 0 || rep.Summary.TotalTokens != 0 || rep.Priced {
		t.Errorf("empty summary = %+v priced=%v", rep.Summary, rep.Priced)
	}
	if got, want := rep.Since.Format(dateLayout), "2026-10-28"; got != want {
		t.Errorf("since = %s, want %s", got, want)
	}
}

func TestNormalizeDays(t *testing.T) {
	for in, want := range map[int]int{-1: DefaultDays, 0: DefaultDays, 7: 7, 1000: MaxDays} {
		if got := NormalizeDays(in); got != want {
			t.Errorf("NormalizeDays(%d) = %d, want %d", in, got, want)
		}
	}
	if got := NormalizeScope("WORKSPACE"); got != "workspace" {
		t.Errorf("NormalizeScope(WORKSPACE) = %q", got)
	}
	if got := NormalizeScope("nope"); got != "all" {
		t.Errorf("NormalizeScope(nope) = %q", got)
	}
}
