package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"

	"codedeep/internal/analytics"
	"codedeep/internal/config"
	"codedeep/internal/util"
)

func cmdStats(args []string) int {
	fs := flag.NewFlagSet("codedeep stats", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var configPath string
	var days int
	var workspace bool
	var jsonOut bool
	fs.StringVar(&configPath, "config", "", "config file path")
	fs.IntVar(&days, "days", analytics.DefaultDays, "reporting window in days")
	fs.BoolVar(&workspace, "workspace", false, "only sessions from the current workspace")
	fs.BoolVar(&jsonOut, "json", false, "print the report as JSON")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "codedeep:", err)
		return 1
	}
	scope := "all"
	if workspace {
		scope = "workspace"
	}
	rep, err := analytics.Build(cfg, analytics.Options{Days: days, Scope: scope})
	if err != nil {
		fmt.Fprintln(os.Stderr, "codedeep:", err)
		return 1
	}
	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			fmt.Fprintln(os.Stderr, "codedeep:", err)
			return 1
		}
		return 0
	}
	printStats(os.Stdout, rep, styles{on: colorEnabled(os.Stdout)})
	return 0
}

// printStats renders the observability report as a terminal summary. It is
// shared by `codedeep stats` and the REPL's /stats command.
func printStats(w io.Writer, rep analytics.Report, p styles) {
	scope := "all workspaces"
	if rep.Scope == "workspace" {
		scope = "this workspace"
	}
	fmt.Fprintf(w, "%s %s\n", p.bold("Observability"), p.dim(fmt.Sprintf("last %d day(s) · %s", rep.Days, scope)))
	if rep.Summary.Sessions == 0 {
		fmt.Fprintf(w, "  %s\n", p.dim("no activity in this window"))
		return
	}

	s := rep.Summary
	line := func(label, value string) {
		fmt.Fprintf(w, "  %-10s %s\n", p.dim(label), value)
	}
	line("sessions", fmt.Sprintf("%d · %d turns · %d active day(s)", s.Sessions, s.Turns, s.ActiveDays))
	line("tokens", fmt.Sprintf("%s total (%s prompt · %s completion)",
		humanTokens(s.TotalTokens), humanTokens(s.PromptTokens), humanTokens(s.CompletionTokens)))
	cache := fmt.Sprintf("%.1f%% hit · %s read · %s written",
		s.CacheHitRate*100, humanTokens(s.CacheHitTokens), humanTokens(s.CacheWriteTokens))
	if s.PromptTokens == 0 {
		cache = "no prompt tokens recorded"
	}
	line("cache", cache)
	if rep.Priced {
		line("cost", fmt.Sprintf("~$%.4f (estimated)", s.CostUSD))
	} else {
		line("cost", "unavailable (no pricing configured)")
	}
	line("tools", fmt.Sprintf("%d calls across %d tool(s)", s.ToolCalls, len(rep.Tools)))

	if spark := tokenSparkline(rep.Daily); spark != "" {
		fmt.Fprintf(w, "\n  %s\n", p.bold("Tokens per day"))
		fmt.Fprintf(w, "  %s  %s %s\n", p.cyan(spark),
			p.dim(rep.Daily[0].Date), p.dim(rep.Daily[len(rep.Daily)-1].Date))
	}

	if len(rep.Tools) > 0 {
		fmt.Fprintf(w, "\n  %s\n", p.bold("Tools"))
		printed := 0
		for _, tool := range rep.Tools {
			if printed == 8 {
				break
			}
			printed++
			extra := humanTokens(tool.Tokens) + " tok"
			if tool.CostUSD > 0 {
				extra += " · ~$" + fmt.Sprintf("%.4f", tool.CostUSD)
			}
			fmt.Fprintf(w, "  %-22s %5d calls  %-4s %s\n",
				p.cyan(tool.Name), tool.Calls, pctBar(tool.Calls, s.ToolCalls), p.dim(extra))
		}
		if len(rep.Tools) > printed {
			fmt.Fprintf(w, "  %s\n", p.dim(fmt.Sprintf("… and %d more tool(s)", len(rep.Tools)-printed)))
		}
	}

	if len(rep.Models) > 0 {
		fmt.Fprintf(w, "\n  %s\n", p.bold("Models"))
		for _, m := range rep.Models {
			cost := "—"
			if rep.Priced {
				cost = fmt.Sprintf("~$%.4f", m.CostUSD)
			}
			fmt.Fprintf(w, "  %-24s %4d turns  %8s tok  %5.1f%% cached  %s\n",
				p.cyan(m.Model), m.Turns, humanTokens(m.TotalTokens), m.CacheHitRate*100, p.dim(cost))
		}
	}

	if len(rep.Sessions) > 0 {
		fmt.Fprintf(w, "\n  %s\n", p.bold("Sessions"))
		fmt.Fprintf(w, "  %-24s  %-16s  %8s  %6s  %s\n",
			p.dim("ID"), p.dim("UPDATED"), p.dim("TOKENS"), p.dim("CACHE"), p.dim("TITLE"))
		for _, sess := range rep.Sessions {
			fmt.Fprintf(w, "  %-24s  %-16s  %8s  %5.0f%%  %s\n",
				sess.ID,
				sess.Updated.Local().Format("2006-01-02 15:04"),
				humanTokens(sess.TotalTokens),
				sess.CacheHitRate*100,
				util.Truncate(sess.Title, 52))
		}
	}
}

func tokenSparkline(days []analytics.Day) string {
	const blocks = "▁▂▃▄▅▆▇█"
	if len(days) == 0 {
		return ""
	}
	bucket := 1
	if len(days) > 60 {
		bucket = int(math.Ceil(float64(len(days)) / 60))
	}
	values := make([]int, 0, len(days)/bucket+1)
	for i := 0; i < len(days); i += bucket {
		end := i + bucket
		if end > len(days) {
			end = len(days)
		}
		sum := 0
		for _, d := range days[i:end] {
			sum += d.TotalTokens
		}
		values = append(values, sum)
	}
	max := 0
	for _, v := range values {
		if v > max {
			max = v
		}
	}
	if max == 0 {
		return ""
	}
	var b strings.Builder
	for _, v := range values {
		if v == 0 {
			b.WriteByte(' ')
			continue
		}
		idx := int(math.Round(float64(v) / float64(max) * float64(len(blocks)-1)))
		b.WriteByte(blocks[idx])
	}
	return b.String()
}

func pctBar(part, total int) string {
	if total <= 0 || part <= 0 {
		return ""
	}
	pct := int(math.Round(float64(part) / float64(total) * 100))
	width := int(math.Round(float64(part) / float64(total) * 8))
	if width < 1 {
		width = 1
	}
	return strings.Repeat("█", width) + strings.Repeat("·", 8-width) + fmt.Sprintf(" %3d%%", pct)
}

// runStatsCommand handles the REPL /stats command: days and scope are optional.
func (r *repl) runStats(arg string) {
	days := analytics.DefaultDays
	scope := "all"
	for _, field := range strings.Fields(arg) {
		if n, err := strconv.Atoi(field); err == nil {
			days = n
			continue
		}
		switch strings.ToLower(field) {
		case "workspace", "here":
			scope = "workspace"
		case "all":
			scope = "all"
		default:
			r.render.Notice("usage: /stats [days] [workspace|all]")
			return
		}
	}
	rep, err := analytics.Build(r.cfg, analytics.Options{Days: days, Scope: scope})
	if err != nil {
		r.render.Notice("stats: %v", err)
		return
	}
	printStats(os.Stdout, rep, r.render.p)
}

// doctorStats summarizes recent activity for the doctor output.
func doctorStats(cfg *config.Config) string {
	rep, err := analytics.Build(cfg, analytics.Options{Days: analytics.DefaultDays})
	if err != nil {
		return "unavailable (" + err.Error() + ")"
	}
	if rep.Summary.Sessions == 0 {
		return "no activity in the last 30 days"
	}
	parts := []string{
		fmt.Sprintf("%d session(s)", rep.Summary.Sessions),
		fmt.Sprintf("%d turns", rep.Summary.Turns),
		humanTokens(rep.Summary.TotalTokens) + " tokens",
	}
	if rep.Summary.PromptTokens > 0 {
		parts = append(parts, fmt.Sprintf("%.0f%% cache hit", rep.Summary.CacheHitRate*100))
	}
	if rep.Priced {
		parts = append(parts, fmt.Sprintf("~$%.4f", rep.Summary.CostUSD))
	}
	return strings.Join(parts, " · ") + " in the last 30 days"
}
