package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"codedeep/internal/agent"
	"codedeep/internal/llm"
	"codedeep/internal/util"
)

// styles renders ANSI escapes when the output stream supports them.
type styles struct{ on bool }

func (s styles) wrap(code, text string) string {
	if !s.on || text == "" {
		return text
	}
	return "\x1b[" + code + "m" + text + "\x1b[0m"
}

func (s styles) bold(t string) string    { return s.wrap("1", t) }
func (s styles) dim(t string) string     { return s.wrap("2", t) }
func (s styles) red(t string) string     { return s.wrap("31", t) }
func (s styles) green(t string) string   { return s.wrap("32", t) }
func (s styles) yellow(t string) string  { return s.wrap("33", t) }
func (s styles) blue(t string) string    { return s.wrap("34", t) }
func (s styles) magenta(t string) string { return s.wrap("35", t) }
func (s styles) cyan(t string) string    { return s.wrap("36", t) }

// colorEnabled reports whether ANSI output makes sense for w. Git Bash and
// other MSYS terminals expose a pty as a pipe, so TERM is used as a hint.
func colorEnabled(w io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	term := strings.ToLower(os.Getenv("TERM"))
	if term == "dumb" {
		return false
	}
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	if err != nil {
		return false
	}
	if st.Mode().IsRegular() {
		return false
	}
	if st.Mode()&os.ModeCharDevice != 0 {
		return true
	}
	return term != ""
}

// renderer turns agent events into a terminal transcript. Agent events arrive
// from a single goroutine, but the mutex keeps the REPL's notices safe.
type renderer struct {
	out   *bufio.Writer
	p     styles
	width int

	showReasoning bool
	debug         bool

	mu          sync.Mutex
	turnStart   time.Time
	turnUsage   llm.Usage
	inReasoning bool
	atLineStart bool
}

func newRenderer(w *os.File, debug, plain, showReasoning bool) *renderer {
	return &renderer{
		out:           bufio.NewWriterSize(w, 32*1024),
		p:             styles{on: !plain && colorEnabled(w)},
		width:         termWidth(),
		showReasoning: showReasoning,
		debug:         debug,
		atLineStart:   true,
	}
}

func termWidth() int {
	if v := os.Getenv("COLUMNS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 40 && n <= 400 {
			return n
		}
	}
	return 100
}

func (r *renderer) StartTurn() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.turnStart = time.Now()
	r.turnUsage = llm.Usage{}
	r.inReasoning = false
	r.atLineStart = true
}

func (r *renderer) FinishTurn() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ensureNL()
	r.out.Flush()
}

// Notice prints an informational line outside of a model turn.
func (r *renderer) Notice(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ensureNL()
	fmt.Fprintf(r.out, "  %s %s\n", r.p.dim("·"), r.p.dim(fmt.Sprintf(format, args...)))
	r.out.Flush()
}

func (r *renderer) ensureNL() {
	if !r.atLineStart {
		r.out.WriteByte('\n')
		r.atLineStart = true
	}
}

func (r *renderer) depthPrefix(depth int) string {
	if depth <= 0 {
		return ""
	}
	return "  " + strings.Repeat("  ", depth-1) + "│ "
}

func (r *renderer) stream(text, prefix string, style func(string) string) {
	if text == "" {
		return
	}
	for {
		if r.atLineStart && prefix != "" {
			r.out.WriteString(prefix)
		}
		idx := strings.IndexByte(text, '\n')
		if idx < 0 {
			r.out.WriteString(style(text))
			r.atLineStart = false
			return
		}
		r.out.WriteString(style(text[:idx]))
		r.out.WriteByte('\n')
		r.atLineStart = true
		text = text[idx+1:]
		if text == "" {
			return
		}
	}
}

func (r *renderer) Emit(ev agent.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()

	switch ev.Type {
	case agent.EventReasoning:
		if !r.showReasoning || ev.Text == "" {
			return
		}
		if !r.inReasoning {
			r.ensureNL()
			fmt.Fprintf(r.out, "%s%s %s ", r.depthPrefix(ev.Depth), r.p.dim("thinking"), r.p.dim("›"))
			r.inReasoning = true
		}
		r.stream(ev.Text, r.depthPrefix(ev.Depth), r.p.dim)

	case agent.EventText:
		if ev.Text == "" {
			return
		}
		if r.inReasoning {
			r.out.WriteByte('\n')
			r.atLineStart = true
			r.inReasoning = false
		}
		r.stream(ev.Text, r.depthPrefix(ev.Depth), func(s string) string { return s })

	case agent.EventToolStart:
		r.ensureNL()
		r.inReasoning = false
		fmt.Fprintf(r.out, "%s%s %s %s\n",
			r.depthPrefix(ev.Depth), r.p.cyan("➜"), r.p.bold(ev.Name), r.p.dim(describeToolArgs(ev.Name, ev.Args, 120)))
		r.atLineStart = true

	case agent.EventToolEnd:
		r.ensureNL()
		r.inReasoning = false
		if ev.IsError {
			fmt.Fprintf(r.out, "%s%s %s %s\n",
				r.depthPrefix(ev.Depth), r.p.red("✗"), r.p.bold(ev.Name), r.p.red(outputSummary(ev)))
		} else {
			fmt.Fprintf(r.out, "%s%s %s %s\n",
				r.depthPrefix(ev.Depth), r.p.green("✓"), r.p.bold(ev.Name), r.p.dim(outputSummary(ev)))
		}
		if r.debug && strings.TrimSpace(ev.Output) != "" {
			r.stream(util.TruncateMiddle(ev.Output, 4000), r.depthPrefix(ev.Depth)+"  ", r.p.dim)
			r.ensureNL()
		}

	case agent.EventDiff:
		r.ensureNL()
		r.inReasoning = false
		label := r.p.cyan("diff")
		if ev.Path != "" {
			label += " " + r.p.bold(ev.Path)
		}
		fmt.Fprintf(r.out, "%s%s\n", r.depthPrefix(ev.Depth), label)
		r.printDiff(ev.Diff, ev.Depth)
		r.atLineStart = true

	case agent.EventCompact:
		r.ensureNL()
		fmt.Fprintf(r.out, "%s %s\n", r.p.yellow("compacted"), r.p.dim(ev.Message))

	case agent.EventSubagentStart:
		r.ensureNL()
		fmt.Fprintf(r.out, "%s %s %s\n", r.p.magenta("⌁ subagent"), r.p.bold(ev.Name), r.p.dim("started"))
		r.atLineStart = true

	case agent.EventSubagentEnd:
		r.ensureNL()
		label := r.p.magenta("⌁ subagent") + " " + r.p.bold(ev.Name)
		if ev.IsError {
			fmt.Fprintf(r.out, "%s %s\n", label, r.p.red("failed"))
		} else {
			fmt.Fprintf(r.out, "%s %s\n", label, r.p.dim("finished"))
		}
		r.atLineStart = true

	case agent.EventApprovalResult:
		if ev.IsError {
			r.ensureNL()
			fmt.Fprintf(r.out, "  %s %s\n", r.p.red("approval error"), r.p.dim(ev.Message))
		}

	case agent.EventUsage:
		if ev.Usage != nil {
			r.turnUsage = r.turnUsage.Add(*ev.Usage)
		}

	case agent.EventError:
		r.ensureNL()
		r.inReasoning = false
		fmt.Fprintf(r.out, "%s %s\n", r.p.red("error"), ev.Message)

	case agent.EventDone:
		r.ensureNL()
		r.inReasoning = false
		elapsed := time.Since(r.turnStart)
		if r.turnStart.IsZero() {
			elapsed = 0
		}
		parts := make([]string, 0, 3)
		if elapsed > 0 {
			parts = append(parts, util.HumanDuration(elapsed))
		}
		if r.turnUsage.TotalTokens > 0 {
			parts = append(parts, humanTokens(r.turnUsage.TotalTokens)+" tokens")
		}
		if cost := humanCost(ev.CostUSD); cost != "" {
			parts = append(parts, cost)
		}
		if ev.Message != "" {
			parts = append(parts, ev.Message)
		}
		if ev.SessionID != "" {
			parts = append(parts, "session "+ev.SessionID)
		}
		if len(parts) > 0 {
			fmt.Fprintf(r.out, "%s %s\n", r.p.dim("──"), r.p.dim(strings.Join(parts, " · ")))
		}
	}

	r.out.Flush()
}

func (r *renderer) printDiff(text string, depth int) {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	const maxLines = 160
	prefix := r.depthPrefix(depth) + "  "
	for i, line := range lines {
		if i >= maxLines {
			fmt.Fprintf(r.out, "%s%s\n", prefix, r.p.dim(fmt.Sprintf("… %d more lines", len(lines)-maxLines)))
			break
		}
		switch {
		case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"):
			line = r.p.dim(line)
		case strings.HasPrefix(line, "@@"):
			line = r.p.cyan(line)
		case strings.HasPrefix(line, "+"):
			line = r.p.green(line)
		case strings.HasPrefix(line, "-"):
			line = r.p.red(line)
		case strings.HasPrefix(line, "["):
			line = r.p.dim(line)
		}
		fmt.Fprintf(r.out, "%s%s\n", prefix, line)
	}
}

func humanCost(usd float64) string {
	if usd <= 0 {
		return ""
	}
	switch {
	case usd < 0.01:
		return fmt.Sprintf("~$%.4f", usd)
	case usd < 1:
		return fmt.Sprintf("~$%.3f", usd)
	default:
		return fmt.Sprintf("~$%.2f", usd)
	}
}

func outputSummary(ev agent.Event) string {
	out := strings.TrimRight(ev.Output, "\r\n")
	if out == "" {
		return "done"
	}
	lines := strings.Count(out, "\n") + 1
	if lines > 1 {
		return fmt.Sprintf("%d lines", lines)
	}
	return util.Truncate(strings.TrimSpace(out), 120)
}

// describeToolArgs picks the most informative argument for a one-line preview.
func describeToolArgs(name string, raw json.RawMessage, max int) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "{}" {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return util.Truncate(strings.Join(strings.Fields(trimmed), " "), max)
	}
	order := []string{
		"command", "description", "path", "file_path", "pattern", "query",
		"url", "name", "prompt", "old_string", "timeout_sec",
	}
	for _, key := range order {
		v, ok := m[key]
		if !ok {
			continue
		}
		text := strings.Join(strings.Fields(fmt.Sprint(v)), " ")
		if text == "" {
			continue
		}
		return util.Truncate(text, max)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, 3)
	for _, k := range keys {
		text := strings.Join(strings.Fields(fmt.Sprint(m[k])), " ")
		parts = append(parts, k+"="+util.Truncate(text, 40))
		if len(parts) == 2 {
			break
		}
	}
	return util.Truncate(strings.Join(parts, " "), max)
}

func humanTokens(n int) string {
	switch {
	case n < 1000:
		return strconv.Itoa(n)
	case n < 1_000_000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	default:
		return fmt.Sprintf("%.2fm", float64(n)/1_000_000)
	}
}

func newJSONEmitter(w io.Writer) agent.Emitter {
	enc := json.NewEncoder(w)
	return func(ev agent.Event) {
		_ = enc.Encode(ev)
	}
}
