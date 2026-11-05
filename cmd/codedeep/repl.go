package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"codedeep/internal/app"
	"codedeep/internal/approval"
	"codedeep/internal/config"
	"codedeep/internal/editor"
	"codedeep/internal/session"
	"codedeep/internal/util"
)

// lineReader pumps stdin from a single goroutine so the REPL prompt and the
// approval prompt can share it without competing reads.
type lineReader struct {
	ch  chan string
	err chan error
}

func newLineReader(r io.Reader) *lineReader {
	l := &lineReader{ch: make(chan string), err: make(chan error, 1)}
	go func() {
		br := bufio.NewReaderSize(r, 64*1024)
		for {
			line, err := br.ReadString('\n')
			if line != "" {
				line = strings.TrimRight(line, "\r\n")
				l.ch <- line
			}
			if err != nil {
				if !errors.Is(err, io.EOF) {
					l.err <- err
				}
				close(l.ch)
				return
			}
		}
	}()
	return l
}

func (l *lineReader) ReadLine(ctx context.Context) (string, error) {
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case line, ok := <-l.ch:
		if !ok {
			select {
			case err := <-l.err:
				return "", err
			default:
				return "", io.EOF
			}
		}
		return line, nil
	}
}

type stdinApprover struct {
	lines *lineReader
	out   io.Writer
	p     styles
}

func (a *stdinApprover) Approve(ctx context.Context, req approval.Request) (approval.Decision, error) {
	fmt.Fprintln(a.out)
	fmt.Fprintf(a.out, "  %s %s\n", a.p.yellow("approve"), a.p.bold(req.Tool))
	if detail := describeToolArgs(req.Tool, req.Args, 300); detail != "" {
		fmt.Fprintf(a.out, "  %s\n", a.p.dim(detail))
	}
	fmt.Fprintf(a.out, "  %s ", a.p.dim("[a]llow  [f]orever (this command/file)  [d]eny ›"))
	line, err := a.lines.ReadLine(ctx)
	if err != nil {
		fmt.Fprintln(a.out)
		if errors.Is(err, io.EOF) {
			fmt.Fprintf(a.out, "  %s\n", a.p.dim("denied (no input)"))
			return approval.Deny, nil
		}
		return approval.Deny, err
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "a", "allow", "y", "yes":
		return approval.AllowOnce, nil
	case "f", "always":
		return approval.AllowAlways, nil
	}
	fmt.Fprintf(a.out, "  %s\n", a.p.dim("denied"))
	return approval.Deny, nil
}

type repl struct {
	app      *app.App
	cfg      *config.Config
	render   *renderer
	lines    *lineReader
	approver approval.Approver
	agent    agentRunner

	sess     *session.Session
	listings []session.Meta
	// prevApproval is restored when plan mode is turned off.
	prevApproval string

	mu     sync.Mutex
	cancel context.CancelFunc
	busy   bool
}

type agentRunner interface {
	Run(ctx context.Context, sess *session.Session, prompt string) error
	CompactNow(ctx context.Context, sess *session.Session) (int, error)
	Undo(sess *session.Session) ([]string, error)
	UndoAt(sess *session.Session, idx int) ([]string, error)
}

func runREPL(a *app.App, cfg *config.Config, sess *session.Session, rend *renderer, lines *lineReader, approver approval.Approver) int {
	r := &repl{
		app:      a,
		cfg:      cfg,
		render:   rend,
		lines:    lines,
		approver: approver,
		sess:     sess,
	}
	r.agent = a.NewAgent(approver, rend.Emit)
	r.handleSignals()
	r.banner()

	for {
		fmt.Fprint(os.Stdout, rend.p.green("›")+" ")
		line, err := lines.ReadLine(context.Background())
		if err != nil {
			fmt.Fprintln(os.Stdout)
			break
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "/") {
			if r.command(line) {
				break
			}
			continue
		}
		prompt := r.readContinuation(line)
		if strings.TrimSpace(prompt) == "" {
			continue
		}
		r.runTurn(prompt)
	}
	fmt.Fprintln(os.Stdout, rend.p.dim("bye"))
	return 0
}

func (r *repl) banner() {
	p := r.render.p
	fmt.Fprintln(os.Stdout)
	fmt.Fprintf(os.Stdout, "  %s %s\n", p.bold("CodeDeep"), p.dim(version))
	fmt.Fprintf(os.Stdout, "  %s\n", p.dim(r.cfg.Model+" · "+tildePath(r.cfg.WorkDir)))
	fmt.Fprintf(os.Stdout, "  %s\n", p.dim(fmt.Sprintf("approval %s · %d tools · session %s",
		r.cfg.Approval, len(r.app.Registry.List()), r.sess.ID)))
	if !r.cfg.APIKeyPresent() {
		fmt.Fprintf(os.Stdout, "  %s\n", p.yellow("no API key configured — set DEEPSEEK_API_KEY before chatting"))
	}
	fmt.Fprintf(os.Stdout, "  %s\n\n", p.dim("type /help for commands, /quit to exit"))
}

func (r *repl) prompt() string {
	return r.render.p.green("›") + " "
}

func (r *repl) readContinuation(first string) string {
	var b strings.Builder
	line := first
	for strings.HasSuffix(line, "\\") {
		b.WriteString(strings.TrimSuffix(line, "\\"))
		b.WriteByte('\n')
		fmt.Fprint(os.Stdout, r.render.p.dim("… "))
		next, err := r.lines.ReadLine(context.Background())
		if err != nil {
			break
		}
		line = next
	}
	b.WriteString(line)
	return strings.TrimSpace(b.String())
}

func (r *repl) begin() (context.Context, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	r.mu.Lock()
	r.cancel = cancel
	r.busy = true
	r.mu.Unlock()
	return ctx, func() {
		cancel()
		r.mu.Lock()
		r.busy = false
		r.cancel = nil
		r.mu.Unlock()
	}
}

func (r *repl) runTurn(prompt string) {
	ctx, done := r.begin()
	defer done()
	r.render.StartTurn()
	if err := r.agent.Run(ctx, r.sess, prompt); err != nil && !errors.Is(err, context.Canceled) {
		r.render.Notice("%v", err)
	}
	r.render.FinishTurn()
}

func (r *repl) handleSignals() {
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, os.Interrupt)
	go func() {
		for range ch {
			r.mu.Lock()
			cancel, busy := r.cancel, r.busy
			r.mu.Unlock()
			if busy && cancel != nil {
				fmt.Fprintln(os.Stderr, "\n"+r.render.p.dim("interrupt received, stopping…"))
				cancel()
			} else {
				fmt.Fprintln(os.Stderr, r.render.p.dim("(use /quit or Ctrl+D to exit)"))
			}
		}
	}()
}

// command handles a slash command and reports whether the REPL should exit.
func (r *repl) command(line string) bool {
	fields := strings.Fields(line)
	cmd := strings.ToLower(fields[0])
	arg := strings.TrimSpace(strings.TrimPrefix(line, fields[0]))

	switch cmd {
	case "/help", "/h", "/?":
		r.printHelp()

	case "/quit", "/exit", "/q":
		return true

	case "/new":
		r.sess = r.app.Sessions.New(r.cfg.Model, r.cfg.WorkDir)
		r.saveSession()
		r.render.Notice("new session %s", r.sess.ID)

	case "/sessions":
		var metas []session.Meta
		var err error
		if arg != "" {
			metas, err = r.app.Sessions.Search(arg)
		} else {
			metas, err = r.app.Sessions.List()
		}
		if err != nil {
			r.render.Notice("cannot list sessions: %v", err)
			return false
		}
		r.listings = metas
		if arg != "" {
			r.render.Notice("%d session(s) matching %q", len(metas), arg)
		}
		r.printSessions(metas)

	case "/export":
		r.exportSession(arg)

	case "/resume":
		r.resume(arg)

	case "/title":
		if arg == "" {
			r.render.Notice("title: %s", r.sess.Title)
			return false
		}
		r.sess.Title = arg
		r.saveSession()
		r.render.Notice("session renamed to %q", arg)

	case "/model":
		if arg == "" {
			r.render.Notice("model: %s", r.cfg.Model)
			return false
		}
		r.cfg.Model = arg
		r.sess.Model = arg
		r.saveSession()
		r.render.Notice("model set to %s for this session", arg)

	case "/models":
		r.listModels()

	case "/approval":
		if arg == "" {
			r.render.Notice("approval mode: %s", r.cfg.Approval)
			return false
		}
		switch strings.ToLower(arg) {
		case "ask", "auto", "readonly", "plan":
			if strings.ToLower(arg) == "plan" && r.cfg.Approval != "plan" {
				r.prevApproval = r.cfg.Approval
			}
			r.cfg.Approval = strings.ToLower(arg)
			r.render.Notice("approval mode set to %s (this run only)", r.cfg.Approval)
		default:
			r.render.Notice("usage: /approval [ask|auto|readonly|plan]")
		}

	case "/plan":
		r.togglePlan(arg)

	case "/undo":
		r.undoTurn(arg)

	case "/checkpoints", "/cps":
		r.printCheckpoints()

	case "/checkpoint", "/cp":
		r.showCheckpointDiff(arg)

	case "/reasoning":
		r.render.showReasoning = !r.render.showReasoning
		state := "off"
		if r.render.showReasoning {
			state = "on"
		}
		r.render.Notice("reasoning display %s", state)

	case "/status":
		r.printStatus()

	case "/tools":
		r.printTools()

	case "/skills":
		r.printSkills()

	case "/skill":
		r.runSkill(arg)

	case "/jobs":
		r.printJobs()

	case "/job":
		r.printJob(arg)

	case "/kill":
		r.killJob(arg)

	case "/open":
		r.openInEditor(arg)

	case "/compact":
		r.compact()

	case "/clear":
		if r.render.p.on {
			fmt.Fprint(os.Stdout, "\x1b[2J\x1b[H")
		} else {
			fmt.Fprintln(os.Stdout, strings.Repeat("\n", 40))
		}

	default:
		r.render.Notice("unknown command %s — type /help", cmd)
	}
	return false
}

func (r *repl) printHelp() {
	fmt.Fprintln(os.Stdout, r.render.p.bold("Commands"))
	for _, line := range [][2]string{
		{"/help", "show this help"},
		{"/new", "start a new session"},
		{"/sessions [query]", "list or search saved sessions"},
		{"/resume <id|#|title>", "resume a session (number refers to the last listing)"},
		{"/export [md|json] [path]", "export this session (- prints to stdout)"},
		{"/title <text>", "rename the current session"},
		{"/model [name]", "show or change the model"},
		{"/models", "list models from the provider"},
		{"/approval [mode]", "ask, auto, readonly, or plan"},
		{"/plan [on|off]", "toggle read-only planning mode"},
		{"/undo [n]", "revert the last turn, or roll back through checkpoint n"},
		{"/checkpoints", "list checkpoints with per-turn diff stats"},
		{"/checkpoint <n>", "show the diff captured by checkpoint n"},
		{"/reasoning", "toggle the reasoning stream"},
		{"/status", "session and runtime status"},
		{"/tools", "list available tools"},
		{"/skills", "list workspace skills"},
		{"/skill <name> [task]", "load a skill and run it on a task"},
		{"/jobs", "list background jobs"},
		{"/job <id>", "show output from a background job"},
		{"/kill <id>", "stop a background job"},
		{"/open <path>[:line]", "open a file in your editor"},
		{"/compact", "summarize older messages to free context"},
		{"/clear", "clear the screen"},
		{"/quit", "exit"},
	} {
		fmt.Fprintf(os.Stdout, "  %-22s %s\n", r.render.p.cyan(line[0]), r.render.p.dim(line[1]))
	}
	fmt.Fprintln(os.Stdout, r.render.p.dim("  Tip: end a line with \\ to continue it on the next line."))
}

func (r *repl) printSessions(metas []session.Meta) {
	if len(metas) == 0 {
		r.render.Notice("no saved sessions")
		return
	}
	fmt.Fprintf(os.Stdout, "  %s  %-16s  %-15s  %7s  %s\n",
		r.render.p.dim(" #"), r.render.p.dim("UPDATED"), r.render.p.dim("MODEL"),
		r.render.p.dim("TOKENS"), r.render.p.dim("TITLE"))
	limit := len(metas)
	if limit > 15 {
		limit = 15
	}
	for i := 0; i < limit; i++ {
		m := metas[i]
		marker := " "
		if m.ID == r.sess.ID {
			marker = r.render.p.green("•")
		}
		fmt.Fprintf(os.Stdout, "  %s%d  %-16s  %-15s  %7s  %s\n",
			marker, i+1,
			m.Updated.Local().Format("2006-01-02 15:04"),
			util.Truncate(m.Model, 15),
			humanTokens(m.TotalTokens),
			util.Truncate(m.Title, 56))
	}
	if len(metas) > limit {
		r.render.Notice("… and %d more (use /sessions in a fresh listing)", len(metas)-limit)
	}
	r.render.Notice("resume with /resume <number, id, or title>")
}

func (r *repl) resume(arg string) {
	if arg == "" {
		r.render.Notice("usage: /resume <id|#> (see /sessions)")
		return
	}
	id := arg
	if n, err := strconv.Atoi(arg); err == nil {
		if r.listings == nil {
			metas, err := r.app.Sessions.List()
			if err != nil {
				r.render.Notice("cannot list sessions: %v", err)
				return
			}
			r.listings = metas
		}
		if n < 1 || n > len(r.listings) {
			r.render.Notice("no session #%d (run /sessions first)", n)
			return
		}
		id = r.listings[n-1].ID
	}
	sess, err := r.app.Sessions.Resolve(id)
	if err != nil {
		r.render.Notice("cannot load %s: %v", id, err)
		return
	}
	r.sess = sess
	r.render.Notice("resumed %s · %d messages · %s", sess.ID, len(sess.Messages), sess.Title)
}

func (r *repl) exportSession(arg string) {
	format := "md"
	fields := strings.Fields(arg)
	if len(fields) > 0 {
		switch strings.ToLower(fields[0]) {
		case "md", "markdown":
			format = "md"
			fields = fields[1:]
		case "json":
			format = "json"
			fields = fields[1:]
		}
	}
	dest := strings.TrimSpace(strings.Join(fields, " "))
	data, _, name, err := session.Export(r.sess, format)
	if err != nil {
		r.render.Notice("export failed: %v", err)
		return
	}
	if dest == "" {
		dest = name
	}
	if dest == "-" {
		_, _ = os.Stdout.Write(data)
		return
	}
	if err := os.WriteFile(dest, data, 0o644); err != nil {
		r.render.Notice("export failed: %v", err)
		return
	}
	r.render.Notice("exported %d bytes to %s", len(data), dest)
}

func (r *repl) listModels() {
	if !r.cfg.APIKeyPresent() {
		r.render.Notice("no API key configured")
		return
	}
	ctx, done := r.begin()
	defer done()
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	models, err := r.app.Client.ListModels(ctx)
	if err != nil {
		r.render.Notice("cannot list models: %v", err)
		return
	}
	sort.Strings(models)
	fmt.Fprintln(os.Stdout, r.render.p.bold("Models"))
	for _, name := range models {
		marker := " "
		if name == r.cfg.Model {
			marker = r.render.p.green("•")
		}
		fmt.Fprintf(os.Stdout, "  %s %s\n", marker, name)
	}
}

func (r *repl) printStatus() {
	p := r.render.p
	u := r.sess.Usage
	fmt.Fprintln(os.Stdout, p.bold("Status"))
	fmt.Fprintf(os.Stdout, "  %-12s %s\n", p.dim("session"), r.sess.ID)
	fmt.Fprintf(os.Stdout, "  %-12s %s\n", p.dim("title"), r.sess.Title)
	fmt.Fprintf(os.Stdout, "  %-12s %s\n", p.dim("model"), r.cfg.Model)
	fmt.Fprintf(os.Stdout, "  %-12s %s\n", p.dim("workspace"), r.cfg.WorkDir)
	fmt.Fprintf(os.Stdout, "  %-12s %s\n", p.dim("approval"), r.cfg.Approval)
	fmt.Fprintf(os.Stdout, "  %-12s %d (%d turns)\n", p.dim("messages"), len(r.sess.Messages), r.sess.Turns)
	fmt.Fprintf(os.Stdout, "  %-12s %s total (%s prompt, %s completion)\n", p.dim("tokens"),
		humanTokens(u.TotalTokens), humanTokens(u.PromptTokens), humanTokens(u.CompletionTokens))
	if cost, ok := r.cfg.UsageCost(u); ok {
		fmt.Fprintf(os.Stdout, "  %-12s %s (estimated)\n", p.dim("cost"), humanCost(cost))
	}
	fmt.Fprintf(os.Stdout, "  %-12s %d turn checkpoint(s)\n", p.dim("undo"), len(r.sess.Checkpoints))
	fmt.Fprintf(os.Stdout, "  %-12s %d built-in, %d MCP server(s)\n", p.dim("tools"), len(r.app.Registry.List()), r.app.MCPCount())
}

func (r *repl) togglePlan(arg string) {
	switch strings.ToLower(strings.TrimSpace(arg)) {
	case "", "on":
		if r.cfg.Approval == "plan" {
			r.render.Notice("plan mode is already on")
			return
		}
		r.prevApproval = r.cfg.Approval
		r.cfg.Approval = "plan"
		r.render.Notice("plan mode on — read-only investigation; file changes are blocked")
	case "off":
		if r.cfg.Approval != "plan" {
			r.render.Notice("plan mode is already off")
			return
		}
		r.cfg.Approval = r.prevApproval
		if r.cfg.Approval == "" || r.cfg.Approval == "plan" {
			r.cfg.Approval = "ask"
		}
		r.render.Notice("plan mode off — approval mode %s", r.cfg.Approval)
	default:
		r.render.Notice("usage: /plan [on|off]")
	}
}

func (r *repl) undoTurn(arg string) {
	if arg == "" {
		files, err := r.agent.Undo(r.sess)
		if len(files) > 0 {
			r.render.Notice("reverted %d file(s): %s", len(files), strings.Join(files, ", "))
		}
		if err != nil {
			r.render.Notice("undo: %v", err)
		}
		return
	}
	n, err := strconv.Atoi(strings.TrimSpace(arg))
	if err != nil || n < 1 {
		r.render.Notice("usage: /undo [checkpoint-number] (see /checkpoints)")
		return
	}
	cp := r.checkpointAt(n)
	if cp == nil {
		return
	}
	files, err := r.agent.UndoAt(r.sess, n-1)
	if len(files) > 0 {
		r.render.Notice("rolled back %d file(s) through checkpoint %d (turn %d): %s", len(files), n, cp.Turn, strings.Join(files, ", "))
	}
	if err != nil {
		r.render.Notice("undo: %v", err)
	}
}

func (r *repl) checkpointAt(n int) *session.Checkpoint {
	if len(r.sess.Checkpoints) == 0 {
		r.render.Notice("no checkpoints yet — a checkpoint is kept for every turn that changes files")
		return nil
	}
	if n < 1 || n > len(r.sess.Checkpoints) {
		r.render.Notice("checkpoint %d out of range (1-%d)", n, len(r.sess.Checkpoints))
		return nil
	}
	return &r.sess.Checkpoints[n-1]
}

func (r *repl) printCheckpoints() {
	p := r.render.p
	cps := r.sess.Checkpoints
	if len(cps) == 0 {
		r.render.Notice("no checkpoints yet — a checkpoint is kept for every turn that changes files")
		return
	}
	fmt.Fprintln(os.Stdout, p.bold("Checkpoints (oldest first)"))
	for i, cp := range cps {
		paths := make([]string, 0, len(cp.Files))
		for _, f := range cp.Files {
			paths = append(paths, f.Path)
		}
		stats := p.dim("no changes")
		if cp.Diff != "" {
			stats = fmt.Sprintf("%s %s", p.green(fmt.Sprintf("+%d", cp.Added)), p.red(fmt.Sprintf("-%d", cp.Removed)))
		}
		fmt.Fprintf(os.Stdout, "  %s turn %-3d %s  %d file(s)  %s\n",
			p.cyan(fmt.Sprintf("#%d", i+1)), cp.Turn, cp.Time.Format("2006-01-02 15:04"), len(cp.Files), stats)
		if arg := strings.Join(paths, ", "); arg != "" {
			fmt.Fprintf(os.Stdout, "      %s\n", p.dim(arg))
		}
	}
	r.render.Notice("show a diff with /checkpoint <n>, roll back with /undo <n>")
}

func (r *repl) showCheckpointDiff(arg string) {
	n, err := strconv.Atoi(strings.TrimSpace(arg))
	if err != nil || n < 1 {
		r.render.Notice("usage: /checkpoint <n> (see /checkpoints)")
		return
	}
	cp := r.checkpointAt(n)
	if cp == nil {
		return
	}
	if cp.Diff == "" {
		r.render.Notice("checkpoint %d (turn %d) has no diff", n, cp.Turn)
		return
	}
	r.render.printDiff(fmt.Sprintf("checkpoint #%d (turn %d)\n%s", n, cp.Turn, cp.Diff), 0)
}

func (r *repl) printTools() {
	p := r.render.p
	fmt.Fprintln(os.Stdout, p.bold("Tools"))
	for _, spec := range r.app.Registry.List() {
		marker := p.dim("·")
		if spec.ReadOnly {
			marker = p.green("r")
		}
		fmt.Fprintf(os.Stdout, "  %s %-22s %s\n", marker, p.cyan(spec.Name), p.dim(util.FirstLine(spec.Description, 88)))
	}
	fmt.Fprintln(os.Stdout, p.dim("  r = read-only (never needs approval)"))
}

func (r *repl) printSkills() {
	list := r.app.Skills.List()
	fmt.Fprintln(os.Stdout, r.render.p.bold("Skills"))
	if len(list) == 0 {
		r.render.Notice("no skills found — add markdown files to .codedeep/skills/")
		return
	}
	for _, skill := range list {
		fmt.Fprintf(os.Stdout, "  %-20s %s\n", r.render.p.cyan(skill.Name), r.render.p.dim(skill.Description))
	}
	r.render.Notice("run one with /skill <name> <task>")
}

func (r *repl) runSkill(arg string) {
	fields := strings.Fields(arg)
	if len(fields) == 0 {
		r.render.Notice("usage: /skill <name> [task] (see /skills)")
		return
	}
	skill, ok := r.app.Skills.Get(fields[0])
	if !ok {
		r.render.Notice("unknown skill %q — run /skills", fields[0])
		return
	}
	task := strings.TrimSpace(strings.TrimPrefix(arg, fields[0]))
	if task == "" {
		fmt.Fprintln(os.Stdout, skill.Body)
		r.render.Notice("loaded %s — add a task with /skill %s <task>", skill.Name, skill.Name)
		return
	}
	r.runTurn(skill.Prompt(task))
}

func (r *repl) printJobs() {
	list := r.app.Jobs.List()
	if len(list) == 0 {
		r.render.Notice("no background jobs — use bash with background=true or ask the agent to start one")
		return
	}
	p := r.render.p
	fmt.Fprintln(os.Stdout, p.bold("Background jobs"))
	for _, j := range list {
		fmt.Fprintf(os.Stdout, "  %-8s %-8s exit=%-3d pid=%-7d %s\n",
			p.cyan(j.ID), jobStatus(p, j.Status), j.ExitCode, j.PID, p.dim(util.Truncate(j.Command, 80)))
	}
}

func jobStatus(p styles, status string) string {
	switch status {
	case "running":
		return p.yellow(status)
	case "killed":
		return p.red(status)
	default:
		return p.dim(status)
	}
}

func (r *repl) printJob(arg string) {
	if arg == "" {
		r.render.Notice("usage: /job <id> (see /jobs)")
		return
	}
	job, ok := r.app.Jobs.Get(arg)
	if !ok {
		r.render.Notice("unknown job %q — run /jobs", arg)
		return
	}
	snap := job.Snapshot()
	fmt.Fprintf(os.Stdout, "%s · %s · exit %d · pid %d\n", snap.ID, snap.Status, snap.ExitCode, snap.PID)
	if strings.TrimSpace(snap.Output) == "" {
		r.render.Notice("(no output yet)")
		return
	}
	fmt.Fprintln(os.Stdout, snap.Output)
	if snap.Truncated {
		r.render.Notice("older output was discarded")
	}
}

func (r *repl) killJob(arg string) {
	if arg == "" {
		r.render.Notice("usage: /kill <id> (see /jobs)")
		return
	}
	job, ok := r.app.Jobs.Get(arg)
	if !ok {
		r.render.Notice("unknown job %q — run /jobs", arg)
		return
	}
	if err := job.Kill(); err != nil {
		r.render.Notice("%v", err)
		return
	}
	job.Wait(2 * time.Second)
	snap := job.Snapshot()
	r.render.Notice("stopped %s (%s)", snap.ID, snap.Status)
}

func (r *repl) openInEditor(arg string) {
	target, err := editor.Resolve(r.cfg.WorkDir, arg)
	if err != nil {
		r.render.Notice("open: %v", err)
		return
	}
	if err := editor.Open(r.cfg, target); err != nil {
		r.render.Notice("open: %v", err)
		return
	}
	where := target.Path
	if target.Line > 0 {
		where = fmt.Sprintf("%s:%d", where, target.Line)
	}
	r.render.Notice("opened %s", where)
}

func (r *repl) compact() {
	if len(r.sess.Messages) <= 8 {
		r.render.Notice("the conversation is too short to compact")
		return
	}
	ctx, done := r.begin()
	defer done()
	r.render.StartTurn()
	removed, err := r.agent.CompactNow(ctx, r.sess)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			r.render.Notice("compact failed: %v", err)
		}
	} else {
		r.render.Notice("compacted %d messages", removed)
	}
	r.render.FinishTurn()
}

func (r *repl) saveSession() {
	if r.sess != nil {
		if err := r.app.Sessions.Save(r.sess); err != nil {
			r.render.Notice("session save failed: %v", err)
		}
	}
}

func tildePath(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if strings.EqualFold(path, home) {
		return "~"
	}
	prefix := home + string(os.PathSeparator)
	if len(path) > len(prefix) && strings.EqualFold(path[:len(prefix)], prefix) {
		return "~" + string(os.PathSeparator) + path[len(prefix):]
	}
	return path
}
