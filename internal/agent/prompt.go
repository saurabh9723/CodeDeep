package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"codedeep/internal/config"
	"codedeep/internal/llm"
	"codedeep/internal/session"
	"codedeep/internal/skills"
	"codedeep/internal/tools"
)

// BuildSystemPrompt assembles the agent identity, environment, rules, skills,
// and project instructions.
func BuildSystemPrompt(cfg *config.Config, depth int, catalog *skills.Catalog) string {
	var b strings.Builder
	if depth == 0 {
		b.WriteString("You are CodeDeep, a command-line coding agent working directly in the user's workspace.\n")
		b.WriteString("You can read and modify files, search the codebase, run shell commands, and delegate focused work to subagents.\n")
	} else {
		b.WriteString("You are a subagent inside CodeDeep, working independently on a task assigned by another agent.\n")
		b.WriteString("You cannot talk to the user. Do not ask questions; make reasonable decisions and report back.\n")
	}

	fmt.Fprintf(&b, "\nWorkspace: %s\n", cfg.WorkDir)
	fmt.Fprintf(&b, "Platform: %s/%s", runtime.GOOS, runtime.GOARCH)
	if cfg.Shell != "" {
		fmt.Fprintf(&b, " (shell: %s)", cfg.Shell)
	}
	fmt.Fprintf(&b, "\nDate: %s\n", time.Now().Format("2006-01-02"))
	approval := cfg.ApprovalMode()
	fmt.Fprintf(&b, "Approval mode: %s\n", approval)
	if depth == 0 {
		if brief := tools.GitBrief(cfg.WorkDir); brief != "" {
			fmt.Fprintf(&b, "Git: %s. Use git_status and git_diff for details.\n", brief)
		}
	}

	b.WriteString(`
# How to work
- Use the tools instead of guessing. Read relevant files before you modify them.
- Explore with glob, grep, and read_file. Use bash for builds, tests, git, and commands that need a real shell.
- Use git_status and git_diff to understand uncommitted changes before reviewing or modifying code.
- Keep changes focused and consistent with the existing code. Do not refactor unrelated code.
- When several files must change together, use apply_patch so the edits land atomically or not at all.
- After changing code, verify it when practical (build, type-check, tests) and report the actual result.
- Save durable project facts (build commands, conventions, decisions, gotchas) with remember, and check recall before re-investigating them.
- Never fabricate file contents, command output, or test results.
- If a tool call fails, diagnose the cause instead of repeating it blindly.
- Never commit, push, reset, or delete data unless the user explicitly asks.
- Do not print secrets (API keys, tokens, passwords) in your responses.
- Prefer non-interactive commands. Avoid commands that wait for input.
- Do not run destructive commands (rm -rf, force push, git reset --hard) without explicit user intent.
- The user can see tool calls and their output, so do not paste large outputs back into your reply.
- End with a short summary: what changed, where, and how it was verified.
`)
	if depth == 0 {
		b.WriteString("- For multi-step work, write a short plan with todo_write and keep it current as items finish.\n")
	}
	if section := catalog.Section(); section != "" {
		b.WriteString(section)
	}
	if approval == "plan" {
		b.WriteString(`
# Plan mode
- You are in read-only planning mode. Investigate with read-only tools only.
- Do not create, edit, delete, or move files, and do not run commands that change the workspace.
- Produce a concrete numbered plan: files to change, the approach, risks, and how to verify the result.
- Do not claim that any change was made. The user will leave plan mode and approve execution separately.
`)
	}
	if depth > 0 {
		b.WriteString(`
# Subagent output
- Return a concise report: findings, file paths, exact commands run, and anything left unresolved.
- Do not include raw dumps of files or logs. Quote only what is necessary.
`)
	}

	if instructions := loadProjectInstructions(cfg); instructions != "" {
		b.WriteString("\n# Project instructions\n")
		b.WriteString("These instructions come from the repository and should be followed unless they conflict with the user's request.\n\n")
		b.WriteString(instructions)
		b.WriteString("\n")
	}
	return b.String()
}

func loadProjectInstructions(cfg *config.Config) string {
	type item struct {
		path string
		body string
	}
	seen := map[string]bool{}
	var items []item

	readFile := func(path string) {
		if seen[path] {
			return
		}
		seen[path] = true
		data, err := os.ReadFile(path)
		if err != nil || len(data) == 0 {
			return
		}
		if len(data) > 64*1024 {
			data = data[:64*1024]
		}
		body := strings.TrimSpace(string(data))
		if body == "" {
			return
		}
		items = append(items, item{path: path, body: body})
	}

	dir := cfg.WorkDir
	for depth := 0; depth < 8 && dir != ""; depth++ {
		for _, name := range cfg.InstructionFiles {
			readFile(filepath.Join(dir, name))
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	if cfg.Home != "" {
		readFile(filepath.Join(cfg.Home, ".codedeep", "AGENTS.md"))
	}

	// Parents were collected before the nearest directory; reverse so the
	// closest instructions come last and take precedence.
	for i, j := 0, len(items)-1; i < j; i, j = i+1, j-1 {
		items[i], items[j] = items[j], items[i]
	}

	var b strings.Builder
	for _, it := range items {
		if b.Len() > 48*1024 {
			b.WriteString("\n[additional instruction files omitted]\n")
			break
		}
		fmt.Fprintf(&b, "### %s\n%s\n\n", it.path, it.body)
	}
	return strings.TrimSpace(b.String())
}

// latestUserQuery returns the most recent user text so the codebase map can
// rank relevant files. Subagents keep their task in the live message list.
func latestUserQuery(msgs []llm.Message, sess *session.Session) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			if text := strings.TrimSpace(msgs[i].Content); text != "" {
				return text
			}
		}
	}
	if sess != nil {
		for i := len(sess.Messages) - 1; i >= 0; i-- {
			if sess.Messages[i].Role == "user" {
				if text := strings.TrimSpace(sess.Messages[i].Content); text != "" {
					return text
				}
			}
		}
	}
	return ""
}

// PlanSection renders the session's todo list for the system prompt.
func PlanSection(todos []session.Todo) string {
	if len(todos) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n# Current plan\n")
	b.WriteString("Your working plan for this session. Keep it current with todo_write; " +
		"at most one item should be in_progress.\n")
	for _, todo := range todos {
		marker := "[ ]"
		switch todo.Status {
		case "completed":
			marker = "[x]"
		case "in_progress":
			marker = "[~]"
		}
		fmt.Fprintf(&b, "- %s %s\n", marker, todo.Text)
	}
	return b.String()
}
