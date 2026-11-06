package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"codedeep/internal/agent"
	"codedeep/internal/app"
	"codedeep/internal/approval"
	"codedeep/internal/config"
	"codedeep/internal/jobs"
	"codedeep/internal/llm"
	"codedeep/internal/memory"
	"codedeep/internal/session"
	"codedeep/internal/skills"
	"codedeep/internal/tools"
	"codedeep/internal/util"
)

const version = "0.1.0"

func main() {
	os.Exit(dispatch(os.Args[1:]))
}

func dispatch(args []string) int {
	if len(args) > 0 {
		switch args[0] {
		case "serve":
			return cmdServe(args[1:])
		case "doctor":
			return cmdDoctor(args[1:])
		case "sessions":
			return cmdSessions(args[1:])
		case "stats":
			return cmdStats(args[1:])
		case "open":
			return cmdOpen(args[1:])
		case "version", "--version", "-version", "-v":
			fmt.Println(versionString())
			return 0
		case "help", "--help":
			printMainUsage(os.Stdout)
			return 0
		}
	}
	return cmdChat(args)
}

func versionString() string {
	return fmt.Sprintf("codedeep %s (%s %s/%s)", version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}

func printMainUsage(w io.Writer) {
	fmt.Fprintf(w, `%s

An agentic coding assistant powered by any OpenAI-compatible API (default: DeepSeek).

Usage:
  codedeep                     start the interactive REPL
  codedeep "fix the tests"     run one prompt and exit
  codedeep -p "..."            same as above
  echo "..." | codedeep -p -   read the prompt from stdin
  codedeep serve               start the local web UI
  codedeep doctor              show configuration and environment
  codedeep sessions            list saved sessions
  codedeep sessions -q parser  search titles and transcripts
  codedeep sessions -export ID export a session as markdown or JSON
  codedeep stats               token, cost, cache, and tool activity
  codedeep stats -days 7 -workspace -json
  codedeep open src/main.go:42 open a file in your editor
  codedeep version             print version information

Common flags:
  -config PATH       config file (default ~/.codedeep/config.json)
  -cwd DIR           workspace directory (default: current directory)
  -provider NAME     openai (default), anthropic, gemini, or ollama
  -m, -model NAME    model name
  -approval MODE     ask, auto, readonly, or plan
  -y, -yes           shorthand for -approval auto
  -plan              read-only planning mode (blocks mutations)
  -debug             verbose logging to stderr

Chat flags:
  -p, -prompt TEXT   prompt for a one-shot run ("-" reads stdin)
  -c, -continue      continue the most recent session in this workspace
  -r, -resume ID     resume a session by id or title
  -json              emit NDJSON events instead of the terminal transcript
  -reasoning         show the model's reasoning stream
  -plain             disable colors

Environment:
  CODEAGENT_PROVIDER provider: openai, anthropic, gemini, or ollama
  DEEPSEEK_API_KEY   API key (CODEAGENT_API_KEY and OPENAI_API_KEY also work)
  ANTHROPIC_API_KEY  API key when -provider anthropic
  GEMINI_API_KEY     API key when -provider gemini (GOOGLE_API_KEY also works)
  DEEPSEEK_BASE_URL  API base URL (default https://api.deepseek.com)
  CODEAGENT_MODEL    model name
  CODEAGENT_CONFIG   config file path

Tips:
  In the REPL, type /help for commands. End a line with a backslash to continue it.
`, versionString())
}

type commonFlags struct {
	configPath string
	provider   string
	model      string
	approval   string
	cwd        string
	auto       bool
	plan       bool
	debug      bool
}

func (c *commonFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&c.configPath, "config", "", "config file path")
	fs.StringVar(&c.provider, "provider", "", "provider: openai, anthropic, gemini, or ollama")
	fs.StringVar(&c.model, "m", "", "model name")
	fs.StringVar(&c.model, "model", "", "model name")
	fs.StringVar(&c.approval, "approval", "", "approval mode: ask, auto, readonly, or plan")
	fs.StringVar(&c.cwd, "cwd", "", "workspace directory")
	fs.BoolVar(&c.auto, "y", false, "shorthand for -approval auto")
	fs.BoolVar(&c.auto, "yes", false, "shorthand for -approval auto")
	fs.BoolVar(&c.plan, "plan", false, "read-only planning mode")
	fs.BoolVar(&c.debug, "debug", false, "verbose logging to stderr")
}

func (c *commonFlags) apply(cfg *config.Config) error {
	if c.debug {
		cfg.Debug = true
	}
	if c.provider != "" {
		if err := cfg.SetProvider(c.provider); err != nil {
			return err
		}
	}
	if c.model != "" {
		cfg.Model = strings.TrimSpace(c.model)
	}
	if c.auto {
		cfg.Approval = "auto"
	}
	if c.approval != "" {
		mode := strings.ToLower(strings.TrimSpace(c.approval))
		switch mode {
		case "ask", "auto", "readonly", "plan":
			cfg.Approval = mode
		default:
			return fmt.Errorf("invalid approval mode %q (use ask, auto, readonly, or plan)", c.approval)
		}
	}
	if c.plan {
		cfg.Approval = "plan"
	}
	if c.cwd != "" {
		abs, err := filepath.Abs(c.cwd)
		if err != nil {
			return fmt.Errorf("resolve workspace %s: %w", c.cwd, err)
		}
		st, err := os.Stat(abs)
		if err != nil {
			return fmt.Errorf("workspace %s: %w", abs, err)
		}
		if !st.IsDir() {
			return fmt.Errorf("workspace %s is not a directory", abs)
		}
		if real, err := filepath.EvalSymlinks(abs); err == nil {
			abs = real
		}
		cfg.WorkDir = abs
	}
	return nil
}

type chatFlags struct {
	commonFlags
	prompt    string
	jsonOut   bool
	cont      bool
	resume    string
	plain     bool
	reasoning bool
}

func cmdChat(args []string) int {
	fs := flag.NewFlagSet("codedeep", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { printMainUsage(os.Stderr) }

	var cf chatFlags
	cf.commonFlags.register(fs)
	fs.StringVar(&cf.prompt, "p", "", "prompt for a one-shot run (\"-\" reads stdin)")
	fs.StringVar(&cf.prompt, "prompt", "", "prompt for a one-shot run (\"-\" reads stdin)")
	fs.BoolVar(&cf.cont, "c", false, "continue the most recent session")
	fs.BoolVar(&cf.cont, "continue", false, "continue the most recent session")
	fs.StringVar(&cf.resume, "r", "", "resume a session by id or title")
	fs.StringVar(&cf.resume, "resume", "", "resume a session by id or title")
	fs.BoolVar(&cf.jsonOut, "json", false, "emit NDJSON events (one-shot only)")
	fs.BoolVar(&cf.reasoning, "reasoning", false, "show the reasoning stream")
	fs.BoolVar(&cf.plain, "plain", false, "disable colors")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	prompt := strings.TrimSpace(cf.prompt)
	if prompt == "" && len(fs.Args()) > 0 {
		prompt = strings.TrimSpace(strings.Join(fs.Args(), " "))
	}
	piped := !stdinIsTerminal()
	if prompt == "-" {
		data, err := readStdin()
		if err != nil {
			fmt.Fprintln(os.Stderr, "codedeep:", err)
			return 2
		}
		prompt = strings.TrimSpace(data)
		piped = false
	}
	oneShot := prompt != "" || piped
	if oneShot && prompt == "" && piped {
		data, err := readStdin()
		if err != nil {
			fmt.Fprintln(os.Stderr, "codedeep:", err)
			return 2
		}
		prompt = strings.TrimSpace(data)
	}
	if oneShot && prompt == "" {
		fmt.Fprintln(os.Stderr, "codedeep: empty prompt")
		return 2
	}
	if cf.jsonOut && !oneShot {
		fmt.Fprintln(os.Stderr, "codedeep: -json requires a one-shot prompt")
		return 2
	}

	cfg, err := config.Load(cf.configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "codedeep:", err)
		return 2
	}
	if err := cf.apply(cfg); err != nil {
		fmt.Fprintln(os.Stderr, "codedeep:", err)
		return 2
	}

	if oneShot && !cfg.APIKeyPresent() {
		printMissingKey()
		return 2
	}

	a, err := app.New(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "codedeep:", err)
		return 1
	}
	defer a.Close()

	sess, err := selectSession(a, cf.resume, cf.cont)
	if err != nil {
		fmt.Fprintln(os.Stderr, "codedeep:", err)
		return 1
	}
	if oneShot {
		return runOneShot(a, cfg, &cf, sess, prompt)
	}
	return runInteractive(a, cfg, &cf, sess)
}

func runOneShot(a *app.App, cfg *config.Config, cf *chatFlags, sess *session.Session, prompt string) int {
	sess.Model = cfg.Model
	if err := a.Sessions.Save(sess); err != nil {
		a.Logf("save session: %v", err)
	}

	var emit agent.Emitter
	var rend *renderer
	if cf.jsonOut {
		emit = newJSONEmitter(os.Stdout)
	} else {
		rend = newRenderer(os.Stdout, cfg.Debug, cf.plain, cf.reasoning)
		emit = rend.Emit
	}

	var lines *lineReader
	if stdinIsTerminal() {
		lines = newLineReader(os.Stdin)
	}
	approver := buildApprover(cfg, lines, os.Stderr)

	ag := a.NewAgent(approver, emit)
	ctx, cancel := signalContext(context.Background())
	defer cancel()
	if rend != nil {
		rend.StartTurn()
	}
	err := ag.Run(ctx, sess, prompt)
	if rend != nil {
		rend.FinishTurn()
	}
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return 130
		}
		return 1
	}
	return 0
}

func runInteractive(a *app.App, cfg *config.Config, cf *chatFlags, sess *session.Session) int {
	sess.Model = cfg.Model
	if err := a.Sessions.Save(sess); err != nil {
		a.Logf("save session: %v", err)
	}
	rend := newRenderer(os.Stdout, cfg.Debug, cf.plain, cf.reasoning)
	lines := newLineReader(os.Stdin)
	approver := buildApprover(cfg, lines, os.Stderr)
	return runREPL(a, cfg, sess, rend, lines, approver)
}

func selectSession(a *app.App, resume string, cont bool) (*session.Session, error) {
	if resume != "" {
		if resume == "last" || resume == "latest" {
			sess, err := a.Sessions.Latest(a.Cfg.WorkDir)
			if err != nil {
				return nil, fmt.Errorf("no saved session for %s", a.Cfg.WorkDir)
			}
			return sess, nil
		}
		sess, err := a.Sessions.Resolve(resume)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("no session matches %q", resume)
			}
			return nil, fmt.Errorf("load session %s: %w", resume, err)
		}
		return sess, nil
	}
	if cont {
		sess, err := a.Sessions.Latest(a.Cfg.WorkDir)
		if err == nil {
			return sess, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	return a.Sessions.New(a.Cfg.Model, a.Cfg.WorkDir), nil
}

func buildApprover(cfg *config.Config, lines *lineReader, out io.Writer) approval.Approver {
	switch cfg.Approval {
	case "auto":
		return approval.Auto
	case "readonly":
		return approval.DenyAll
	}
	if lines == nil {
		return approval.Func(func(_ context.Context, req approval.Request) (approval.Decision, error) {
			fmt.Fprintf(out, "codedeep: denied %s — approval is required but stdin is not interactive (use -approval auto)\n", req.Tool)
			return approval.Deny, nil
		})
	}
	return &stdinApprover{lines: lines, out: out, p: styles{on: colorEnabled(out)}}
}

func signalContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		select {
		case <-ch:
			cancel()
		case <-ctx.Done():
		}
		signal.Stop(ch)
	}()
	return ctx, cancel
}

func stdinIsTerminal() bool {
	if st, err := os.Stdin.Stat(); err == nil && st.Mode()&os.ModeCharDevice != 0 {
		return true
	}
	// Git Bash / mintty presents a pty as a pipe to native programs.
	term := strings.ToLower(os.Getenv("TERM"))
	return os.Getenv("CI") == "" && term != "" && term != "dumb" && term != "unknown"
}

func readStdin() (string, error) {
	data, err := io.ReadAll(io.LimitReader(os.Stdin, 4<<20))
	if err != nil {
		return "", fmt.Errorf("read stdin: %w", err)
	}
	return string(data), nil
}

func printMissingKey() {
	fmt.Fprintln(os.Stderr, `codedeep: no API key configured.

Set one of:
  export DEEPSEEK_API_KEY=sk-...        (recommended)
  export CODEAGENT_API_KEY=sk-...
  export OPENAI_API_KEY=sk-...

Or add "api_key" to ~/.codedeep/config.json. Run "codedeep doctor" to check.`)
}

func reasoningSummary(cfg *config.Config) string {
	parts := make([]string, 0, 2)
	if cfg.ReasoningEffort != "" {
		parts = append(parts, "effort "+cfg.ReasoningEffort)
	}
	if cfg.ReasoningBudget > 0 {
		parts = append(parts, fmt.Sprintf("budget %d tokens", cfg.ReasoningBudget))
	}
	if len(parts) == 0 {
		return "off"
	}
	return strings.Join(parts, " · ")
}

func cacheSummary(cfg *config.Config) string {
	if !cfg.PromptCache {
		return "off"
	}
	switch cfg.Provider {
	case "anthropic":
		return "on (cache_control breakpoints)"
	default:
		return "on (automatic)"
	}
}

func maskKey(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return "missing"
	}
	if len(key) <= 12 {
		return "set"
	}
	return "set (" + key[:4] + "…" + key[len(key)-4:] + ")"
}

func cmdDoctor(args []string) int {
	fs := flag.NewFlagSet("codedeep doctor", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var configPath string
	var ping bool
	fs.StringVar(&configPath, "config", "", "config file path")
	fs.BoolVar(&ping, "ping", false, "check provider connectivity and list models")
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

	status := "defaults"
	if st, err := os.Stat(cfg.ConfigPath); err == nil && !st.IsDir() {
		status = "loaded"
	}
	shell := cfg.Shell
	if shell == "" {
		shell = "auto"
	}
	fmt.Println(versionString())
	fmt.Println()
	fmt.Printf("config file    %s (%s)\n", cfg.ConfigPath, status)
	fmt.Printf("provider       %s\n", cfg.Provider)
	fmt.Printf("base url       %s\n", cfg.BaseURL)
	fmt.Printf("model          %s\n", cfg.Model)
	fmt.Printf("api key        %s\n", maskKey(cfg.APIKey))
	fmt.Printf("approval       %s\n", cfg.Approval)
	fmt.Printf("workspace      %s\n", cfg.WorkDir)
	fmt.Printf("sessions dir   %s\n", cfg.SessionsDir)
	fmt.Printf("shell          %s\n", shell)
	fmt.Printf("context        %d tokens (compact at %.0f%%)\n", cfg.ContextWindow, cfg.CompactAt*100)
	fmt.Printf("max steps      %d\n", cfg.MaxSteps)
	fmt.Printf("reasoning      %s\n", reasoningSummary(cfg))
	fmt.Printf("prompt cache   %s\n", cacheSummary(cfg))
	verifyCommand := strings.TrimSpace(cfg.Verify.Command)
	if verifyCommand == "" {
		verifyCommand = "off"
	}
	fmt.Printf("verify         %s\n", verifyCommand)
	mem := memory.New(cfg.WorkDir, cfg.Home)
	fmt.Printf("memory         %d notes\n", len(mem.List()))
	fmt.Printf("activity       %s\n", doctorStats(cfg))

	fsys := tools.NewFS(cfg.WorkDir, cfg.AllowOutside)
	reg := tools.NewRegistry()
	manager := jobs.NewManager(cfg.Shell, cfg.WorkDir)
	tools.RegisterCoreWithJobs(reg, fsys, cfg.Shell, manager)
	jobs.RegisterTools(reg, manager)
	catalog := skills.Discover(cfg.WorkDir, cfg.Home)
	catalog.RegisterTools(reg)
	fmt.Printf("tools          %d built-in\n", len(reg.List()))

	skillNames := make([]string, 0, catalog.Len())
	for _, skill := range catalog.List() {
		skillNames = append(skillNames, skill.Name)
	}
	if len(skillNames) == 0 {
		fmt.Println("skills         none")
	} else {
		fmt.Printf("skills         %d: %s\n", len(skillNames), strings.Join(skillNames, ", "))
	}
	fmt.Printf("hooks          %d pre · %d post\n", len(cfg.Hooks.PreTool), len(cfg.Hooks.PostTool))
	fmt.Printf("diagnostics    %d rules\n", len(cfg.Diagnostics))
	editor := "auto ($VISUAL/$EDITOR/code)"
	if cfg.Editor != nil {
		editor = cfg.Editor.Command
	}
	fmt.Printf("editor         %s\n", editor)
	switch {
	case cfg.WebToken != "":
		fmt.Printf("web auth       token required\n")
	case cfg.WebInsecure:
		fmt.Printf("web auth       disabled (web_insecure)\n")
	default:
		fmt.Printf("web auth       loopback only\n")
	}

	if len(cfg.MCPServers) == 0 {
		fmt.Println("mcp servers    none")
	} else {
		names := make([]string, 0, len(cfg.MCPServers))
		for name, srv := range cfg.MCPServers {
			if !srv.Disabled {
				names = append(names, name)
			}
		}
		fmt.Printf("mcp servers    %d configured: %s\n", len(names), strings.Join(names, ", "))
	}

	if !ping {
		return 0
	}
	fmt.Println()
	fmt.Println("provider check")
	if !cfg.APIKeyPresent() {
		fmt.Println("  ✗ no api key")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	models, err := llm.NewProvider(cfg.Provider, cfg.BaseURL, cfg.APIKey).ListModels(ctx)
	if err != nil {
		fmt.Printf("  ✗ %v\n", err)
		return 1
	}
	fmt.Printf("  ✓ connected · %d models\n", len(models))
	for i, name := range models {
		if i == 12 {
			fmt.Printf("    … and %d more\n", len(models)-i)
			break
		}
		fmt.Printf("    %s\n", name)
	}
	return 0
}

func cmdSessions(args []string) int {
	fs := flag.NewFlagSet("codedeep sessions", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var configPath string
	var jsonOut bool
	var limit int
	var workspace bool
	var query string
	var exportRef string
	var format string
	var output string
	fs.StringVar(&configPath, "config", "", "config file path")
	fs.BoolVar(&jsonOut, "json", false, "print JSON")
	fs.IntVar(&limit, "n", 20, "maximum number of sessions")
	fs.BoolVar(&workspace, "workspace", false, "only sessions from the current workspace")
	fs.StringVar(&query, "q", "", "search titles and transcripts")
	fs.StringVar(&exportRef, "export", "", "export a session by id or title")
	fs.StringVar(&format, "format", "md", "export format: md or json")
	fs.StringVar(&output, "o", "", "export path (default: ./<generated name>, - for stdout)")
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
	store, err := session.NewStore(cfg.SessionsDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "codedeep:", err)
		return 1
	}

	if exportRef != "" {
		sess, err := store.Resolve(exportRef)
		if err != nil {
			fmt.Fprintln(os.Stderr, "codedeep:", err)
			return 1
		}
		data, _, name, err := session.Export(sess, format)
		if err != nil {
			fmt.Fprintln(os.Stderr, "codedeep:", err)
			return 1
		}
		dest := output
		if dest == "" {
			dest = name
		}
		if dest == "-" {
			_, _ = os.Stdout.Write(data)
			return 0
		}
		if err := os.WriteFile(dest, data, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "codedeep:", err)
			return 1
		}
		fmt.Printf("exported %s (%d bytes)\n", dest, len(data))
		return 0
	}

	var metas []session.Meta
	if query != "" {
		metas, err = store.Search(query)
	} else {
		metas, err = store.List()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "codedeep:", err)
		return 1
	}
	if workspace {
		filtered := metas[:0]
		for _, m := range metas {
			if sameDir(m.Cwd, cfg.WorkDir) {
				filtered = append(filtered, m)
			}
		}
		metas = filtered
	}
	if limit > 0 && len(metas) > limit {
		metas = metas[:limit]
	}
	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(metas)
		return 0
	}
	if len(metas) == 0 {
		if query != "" {
			fmt.Printf("no sessions matching %q\n", query)
		} else {
			fmt.Println("no saved sessions")
		}
		return 0
	}
	fmt.Printf("%-24s  %-16s  %-15s  %7s  %s\n", "ID", "UPDATED", "MODEL", "TOKENS", "TITLE")
	for _, m := range metas {
		fmt.Printf("%-24s  %-16s  %-15s  %7s  %s\n",
			m.ID,
			m.Updated.Local().Format("2006-01-02 15:04"),
			util.Truncate(m.Model, 15),
			humanTokens(m.TotalTokens),
			util.Truncate(m.Title, 60))
		if m.Snippet != "" {
			fmt.Printf("    %s\n", util.Truncate(m.Snippet, 110))
		}
	}
	return 0
}

func sameDir(a, b string) bool {
	aa, errA := filepath.Abs(a)
	bb, errB := filepath.Abs(b)
	if errA != nil || errB != nil {
		return a == b
	}
	return strings.EqualFold(filepath.Clean(aa), filepath.Clean(bb))
}
