# CodeDeep

A coding agent in Go, powered by any OpenAI-compatible chat API (DeepSeek by
default). It reads and edits your workspace, runs shell commands, searches the
codebase, compacts long conversations, delegates work to subagents, connects to
MCP servers over stdio, and keeps every session on disk.

Use it as an interactive terminal REPL, a one-shot command, or a local web UI in
the browser.

## Features

- **Agent loop** — streams model text and reasoning, executes tool calls, asks
  for approval, feeds results back, and repeats until the task is done.
- **Coding tools** — `read_file`, `write_file`, `edit_file`, `glob`, `grep`,
  `bash`, and `agent` for subagents.
- **Diff previews and undo** — `write_file` and `edit_file` show a unified diff
  before they run, including in the approval prompt. Every turn snapshots the
  files it touches, so `/undo` (or the web undo button) restores the previous
  contents or removes files that were created.
- **Plan mode** — a read-only mode that blocks mutations and asks the model to
  produce a concrete plan. Switch to another mode to execute it.
- **Git awareness** — read-only `git_status` and `git_diff` tools, plus the
  current branch and uncommitted-change count in the system prompt.
- **Approvals and permission rules** — `ask` (default), `auto`, `readonly`, or
  `plan`; read-only tools are never prompted. Allow/deny rules can match tools,
  commands, and paths, and "always allow" is narrowed to the exact command or
  file rather than the whole tool.
- **Parallel read-only tools** — a model turn that calls several read-only tools
  (`read_file`, `glob`, `grep`, `git_*`) runs them concurrently; mutating tool
  calls stay sequential and ordered.
- **Context management** — automatic compaction at 80% of the context window
  with a boundary-safe cut, plus `/compact` on demand.
- **Subagents** — isolated history, shared approvals and checkpoints, nested up
  to two levels; their token usage counts toward the session.
- **Cost estimates** — optional per-model pricing in the config; the CLI and
  web UI show token totals and estimated USD per step, turn, and session.
- **MCP** — starts stdio JSON-RPC servers and exposes their tools as
  `mcp__<server>__<tool>`; `readOnlyHint` tools skip approval.
- **Sessions** — saved after every step, resumable by id, number, or title,
  searchable across titles and transcripts, and exportable as Markdown or JSON
  from the CLI or the web UI.
- **Providers** — DeepSeek and any OpenAI-compatible endpoint, plus native
  Anthropic Messages and Gemini generateContent adapters and an Ollama preset;
  switch with `-provider` or the config file.
- **Web UI** — Qoder-style interface served from the same binary, no Node or
  Electron required. Includes diff cards, plan mode, undo, a searchable session
  sidebar, a file picker with `@`-mentions, a slash-command palette, and
  image paste/drag-drop for vision models.
- **Images** — attach up to four PNG/JPEG/WebP/GIF images to a message (5 MB
  each) in the web UI; they are sent as multimodal content and kept in session
  exports.
- **Hooks** — pre- and post-tool shell hooks receive a JSON payload on stdin and
  `CODEDEEP_*` environment variables; a failing pre-hook can block the call.
- **Post-edit diagnostics** — pattern-matched build, vet, or lint commands run
  after a file change and their output is fed back to the model.
- **Skills** — markdown instruction packs in `.codedeep/skills` (project) and
  `~/.codedeep/skills` (global), loaded by the `use_skill` tool or `/skill`.
- **Background jobs** — `bash` can run a command in the background; `job_output`,
  `job_list`, and `job_kill` manage it from the agent, CLI, or web UI.
- **Web authentication** — an optional bearer/cookie token protects the UI, and
  the server refuses non-loopback binds without a token.
- **Editor integration** — `codedeep open`, REPL `/open`, and a web file-picker
  action jump to files in your configured editor.
- **Confinement** — tool paths stay inside the workspace, including symlink
  checks; binary files and build directories are skipped by search tools.

## Requirements

- Go 1.24 or newer to build.
- An API key for DeepSeek, Anthropic, Gemini, or another compatible endpoint.
  A local Ollama server needs no key.

## Quick start

```bash
git clone <your-repo-url> codedeep
cd codedeep

# Windows
go build -o bin/codedeep.exe ./cmd/codedeep
# macOS / Linux
go build -o bin/codedeep ./cmd/codedeep

export DEEPSEEK_API_KEY=sk-...        # PowerShell: $env:DEEPSEEK_API_KEY="sk-..."
```

### One-shot

```bash
./bin/codedeep "explain the architecture of this repo"
./bin/codedeep -p "fix the failing tests" -y
echo "summarize the last commit" | ./bin/codedeep -p -
./bin/codedeep -p "review internal/agent" -json > events.ndjson
```

### REPL

```bash
./bin/codedeep            # start a new session
./bin/codedeep -c         # continue the latest session in this workspace
./bin/codedeep -r <id>    # resume a session by id or title
```

REPL commands: `/help`, `/new`, `/sessions [query]`, `/resume <id|#|title>`,
`/export [md|json] [path]`, `/title`, `/model`, `/models`, `/approval`,
`/plan [on|off]`, `/undo`, `/reasoning`, `/status`, `/tools`, `/compact`,
`/skills`, `/skill <name> [task]`, `/jobs`, `/job <id>`, `/kill <id>`,
`/open [path[:line]]`, `/clear`, `/quit`.

Sessions can also be searched and exported without entering the REPL:

```bash
./bin/codedeep sessions -q "flaky test"
./bin/codedeep sessions -export "parser refactor" -format md -o notes.md
```

### Web UI

```bash
./bin/codedeep serve
# opens http://127.0.0.1:7777
```

`serve` accepts `-addr` and `-no-open`, plus the common flags below. In the
composer, type `@` (or click the folder button) to browse and mention files,
`/` for commands such as `/compact`, `/export`, `/skills`, `/skill`, `/open`, and
paste or drag images to attach them. The sidebar search matches session titles
and transcript text; the export button downloads Markdown or JSON. File-picker
rows have an `↗` action that opens the file in your editor.

Set `web_token` (or `CODEAGENT_WEB_TOKEN`) to require sign-in. The server also
refuses to listen on a non-loopback address without a token unless
`web_insecure` is explicitly true:

```bash
CODEAGENT_WEB_TOKEN=long-random-string ./bin/codedeep serve -addr 0.0.0.0:7777
```

### Diagnostics

```bash
./bin/codedeep doctor          # config, workspace, tools, MCP servers
./bin/codedeep doctor -ping    # verify the provider connection and list models
./bin/codedeep sessions        # list saved sessions
./bin/codedeep sessions -json -workspace
```

## Common flags

- `-config PATH` — config file (default `~/.codedeep/config.json`).
- `-cwd DIR` — workspace directory (default: current directory).
- `-provider NAME` — `openai` (default), `anthropic`, `gemini`, or `ollama`.
- `-m`, `-model NAME` — model name.
- `-approval MODE` — `ask`, `auto`, `readonly`, or `plan`.
- `-plan` — shorthand for `-approval plan` (read-only investigation).
- `-y`, `-yes` — shorthand for `-approval auto`.
- `-debug` — verbose logging to stderr.

Chat-only flags: `-p/-prompt`, `-c/-continue`, `-r/-resume`, `-json`,
`-reasoning`, `-plain`.

## Configuration

Configuration is loaded from `~/.codedeep/config.json` (override with
`-config` or `CODEAGENT_CONFIG`). Every field is optional; a missing default
config file is fine. See [`config.example.json`](config.example.json).

```json
{
  "provider": "openai",
  "base_url": "https://api.deepseek.com",
  "model": "deepseek-chat",
  "max_tokens": 8192,
  "temperature": 0.3,
  "context_window": 128000,
  "compact_at": 0.8,
  "approval": "ask",
  "max_steps": 60,
  "shell": "",
  "web_addr": "127.0.0.1:7777",
  "web_token": "",
  "web_insecure": false,
  "allow_outside": false,
  "mcp_servers": {},
  "hooks": {
    "pre_tool": [
      { "command": "gofmt -l .", "tools": ["write_file", "edit_file"], "timeout_sec": 10 }
    ],
    "post_tool": [
      { "command": "echo hook-finished", "tools": ["bash"], "timeout_sec": 10 }
    ]
  },
  "diagnostics": [
    { "pattern": "*.go", "command": "go vet ./..." }
  ],
  "diagnostic_timeout_sec": 30,
  "editor": { "command": "code", "args": ["-g", "{file}:{line}"] },
  "permissions": [
    { "tool": "bash", "action": "allow", "command": "go test*" },
    { "tool": "bash", "action": "deny", "command": "rm -rf *" },
    { "tool": "write_file", "action": "allow", "path": "docs/**" }
  ],
  "pricing": {
    "deepseek-chat": {
      "input_per_mtok": 0.27,
      "output_per_mtok": 1.1,
      "cache_hit_per_mtok": 0.07
    }
  }
}
```

`pricing` is optional. Its keys are model names; values are USD per million
tokens. When a model has a price entry, `/status` and the web header show an
estimated cost alongside token counts. Cached prompt tokens use
`cache_hit_per_mtok` when set, otherwise the normal input rate. Prices change,
so check your provider's current rates.

Prefer environment variables over storing keys in files:

```bash
CODEAGENT_PROVIDER=openai        # openai, anthropic, gemini, or ollama
DEEPSEEK_API_KEY=sk-...          # CODEAGENT_API_KEY and OPENAI_API_KEY also work
ANTHROPIC_API_KEY=sk-ant-...     # when CODEAGENT_PROVIDER=anthropic
GEMINI_API_KEY=...               # when CODEAGENT_PROVIDER=gemini (GOOGLE_API_KEY also works)
DEEPSEEK_BASE_URL=...            # CODEAGENT_BASE_URL and OPENAI_BASE_URL also work
CODEAGENT_MODEL=deepseek-chat
CODEAGENT_APPROVAL=ask
CODEAGENT_CONTEXT_WINDOW=128000
CODEAGENT_COMPACT_AT=0.8
CODEAGENT_MAX_STEPS=60
CODEAGENT_SESSIONS_DIR=$HOME/.codedeep/sessions
CODEAGENT_WEB_ADDR=127.0.0.1:7777
CODEAGENT_WEB_TOKEN=long-random-string
CODEAGENT_WEB_INSECURE=false
CODEAGENT_DIAGNOSTIC_TIMEOUT=30
CODEAGENT_ALLOW_OUTSIDE=false
CODEAGENT_DEBUG=false
CODEAGENT_CONFIG=$HOME/.codedeep/config.json
CODEAGENT_MCP_CONFIG=$HOME/.codedeep/mcp.json
```

`work_dir` is always the CLI `-cwd` value or the current directory; it is not
read from the config file.

### Providers

`provider` (`openai`, `anthropic`, `gemini`, or `ollama`) selects the wire
protocol. The provider presets fill in a base URL, model, and context window
only when those fields are still empty or set to the DeepSeek defaults, so an
explicit `base_url` or `model` always wins.

- **openai** — DeepSeek by default, or any OpenAI-compatible server through
  `base_url`. Also works for OpenRouter, vLLM, LM Studio, and similar.
- **anthropic** — the native Messages API, including streamed text, thinking,
  tool use, and images. Set `ANTHROPIC_API_KEY` (default model
  `claude-sonnet-4-5`).
- **gemini** — the native `generateContent` API with SSE streaming, function
  calls, thinking parts, and inline images. Set `GEMINI_API_KEY` or
  `GOOGLE_API_KEY` (default model `gemini-2.5-pro`).
- **ollama** — a local server using the OpenAI-compatible `/v1` endpoint at
  `http://localhost:11434/v1` (default model `qwen2.5-coder:7b`). No API key is
  required. Pull the model first with `ollama pull qwen2.5-coder:7b`.

```bash
codedeep -provider anthropic -m claude-sonnet-4-5 "review internal/agent"
codedeep -provider gemini -m gemini-2.5-pro "explain this repo"
codedeep -provider ollama -m qwen2.5-coder:7b "fix the tests"
```

### Project instructions

On every run CodeDeep reads `AGENTS.md`, `CODEAGENT.md`, and
`.codedeep/AGENTS.md` from the workspace and each parent directory, plus
`~/.codedeep/AGENTS.md`. Closest files take precedence. Use these for build
commands, conventions, and constraints.

### Skills

Skills are reusable instruction packs stored as markdown files. Put project
skills in `.codedeep/skills/*.md` and personal skills in
`~/.codedeep/skills/*.md`; a project skill overrides a global one with the same
name. Optional frontmatter sets the name and description:

```markdown
---
name: review
description: Review a change for correctness and regressions
---

Read the diff, then report bugs before style issues. Quote file and line
numbers. Do not modify files.
```

The names and descriptions are added to the system prompt, and the model can
load a skill with the read-only `use_skill` tool. From the CLI, `/skills` lists
them and `/skill review src/parser.go` sends the skill instructions plus a task.
In the web composer, `/skills` lists them and `/skill review <task>` starts a
run with the skill body.

### Hooks

Hooks run shell commands around tool calls. `tools` restricts a hook to named
tools (an empty list matches every tool). Each hook gets a JSON object on stdin:

```json
{
  "event": "pre_tool",
  "tool": "edit_file",
  "args": { "path": "main.go", "old_string": "...", "new_string": "..." },
  "session_id": "…",
  "cwd": "/path/to/workspace",
  "result": "…",
  "is_error": false
}
```

The hook process also sees `CODEDEEP_HOOK_EVENT`, `CODEDEEP_TOOL`,
`CODEDEEP_SESSION_ID`, and `CODEDEEP_WORKDIR`. Output is shown in the transcript
and prepended to the tool result. A failing pre-hook is reported but does not
stop the tool unless `block_on_failure` is true; post-hooks never block. The
default timeout is 10 seconds (maximum 300).

```json
{
  "hooks": {
    "pre_tool": [
      { "command": "gofmt -l .", "tools": ["write_file", "edit_file"], "block_on_failure": true }
    ],
    "post_tool": [
      { "command": "echo done >> .codedeep/hooks.log", "tools": ["bash"] }
    ]
  }
}
```

### Post-edit diagnostics

After a tool reports a file change, CodeDeep runs each matching diagnostic
command in the workspace and appends its output to the tool result. A pattern
without `/` matches the file name at any depth; otherwise it matches the
workspace-relative path. `{file}` expands to the relative path and `{abs}` to
the absolute path. Diagnostics are best effort — a non-zero exit is reported to
the model, not treated as a failed edit.

```json
{
  "diagnostics": [
    { "pattern": "*.go", "command": "go vet ./..." },
    { "pattern": "web/**/*.ts", "command": "npx tsc --noEmit" },
    { "pattern": "*.py", "command": "python -m py_compile \"{abs}\"" }
  ],
  "diagnostic_timeout_sec": 30
}
```

### Background jobs

Pass `background: true` to `bash` to start a long-running command without
blocking the turn. The agent gets a job id and can use `job_output` to read the
tail, `job_list` to see status, and `job_kill` to stop the process tree. The
REPL exposes the same operations as `/jobs`, `/job <id>`, and `/kill <id>`.

Jobs keep up to 256 KB of combined output, at most 32 jobs are retained, and
jobs are stopped when CodeDeep exits.

### Editor integration

`editor` selects the command used by `codedeep open`, the REPL `/open`, and the
web file-picker action. Args expand `{file}` and `{line}`; if `{file}` is absent
the path is appended. Without an editor config, `$VISUAL`, `$EDITOR`, and
finally `code -g` are tried.

```bash
codedeep open src/main.go:42
# REPL: /open src/main.go:42
```

```json
{
  "editor": { "command": "code", "args": ["-g", "{file}:{line}"] }
}
```

### Remote access

Set `web_token` or `CODEAGENT_WEB_TOKEN` to protect the web UI. The browser
exchanges the token for an HttpOnly `SameSite=Strict` cookie at `/login`;
scripts can send `Authorization: Bearer <token>` instead. A `?token=` link is
accepted once and immediately redirected to a clean URL. Without a token the
server only binds to loopback by default; a non-loopback `-addr` is refused
unless `web_insecure` is true.

## Tools and approvals

Built-in tools:

- `read_file` — numbered lines with `offset`/`limit`.
- `write_file` — create or overwrite a file, creating parent directories.
- `edit_file` — exact-substring replacement with CRLF fallback; requires a
  unique match unless `replace_all` is set.
- `glob` — `*`, `?`, `[...]`, and `**` patterns.
- `grep` — RE2 search with an optional file filter and case-insensitive mode.
- `bash` — shell command with timeout and combined stdout/stderr; set
  `background: true` to run it as a background job.
- `job_output`, `job_list`, `job_kill` — read, list, and stop background jobs.
- `git_status` — branch, upstream state, last commit, and changed files.
- `git_diff` — unified diff of uncommitted changes; supports `staged`, `stat`,
  `path`, and `context`.
- `agent` — delegate a self-contained task to a subagent.
- `use_skill` — load a workspace or global skill from `.codedeep/skills`.

`glob` and `grep` skip `.git`, `node_modules`, `vendor`, `dist`, `build`,
virtualenvs, caches, and other generated directories. The read-only tools
(`read_file`, `glob`, `grep`, `git_status`, `git_diff`, `job_output`,
`job_list`, `use_skill`) never prompt. Everything else follows the approval
mode:

- `ask` — prompt before each mutating tool call; choose allow once, always
  allow, or deny. When the call has a preview, the prompt includes the diff.
- `auto` — run mutating tools without prompting.
- `readonly` — deny every mutating tool.
- `plan` — investigate with read-only tools, allow no mutations, and produce a
  concrete plan for a later run.

Each turn records a checkpoint of the pre-change contents of every file that
`write_file` or `edit_file` touches (up to 1 MB per file). `/undo` restores the
most recent checkpoint and removes files that did not exist before the turn.
The last 20 checkpoints are kept with the session.

When a model requests several read-only tools in one turn, CodeDeep runs them
concurrently (up to four) and still reports results in call order. Any batch
that contains a mutating tool runs sequentially.

### Permission rules

`permissions` grant or deny tool calls without prompting. Each rule has a
`tool` glob and an `action` of `allow` or `deny`; `command` and `path` are
optional globs. When both are present, both must match.

```json
{
  "permissions": [
    { "tool": "bash", "action": "allow", "command": "go test*" },
    { "tool": "bash", "action": "deny", "command": "rm -rf *" },
    { "tool": "write_file", "action": "allow", "path": "docs/**" },
    { "tool": "mcp__github__*", "action": "allow" }
  ]
}
```

- A matching `deny` always wins, including over `auto` and over read-only
  detection.
- A matching `allow` skips the prompt in `ask` mode, but `readonly` and `plan`
  still block mutations.
- Path patterns are matched relative to the workspace; an absolute path inside
  the workspace is converted to a relative one. A pattern without `/` also
  matches the file name at any depth (`*.md`).
- Command patterns must match the whole command string.
- Choosing "always allow" in a prompt remembers only that exact command or
  file, not every use of the tool.

## MCP servers

MCP servers use stdio JSON-RPC. Add them to the main config:

```json
{
  "mcp_servers": {
    "filesystem": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-filesystem", "."],
      "env": { "LOG_LEVEL": "info" },
      "timeout_sec": 30
    }
  }
}
```

Claude-style `"mcpServers"` is also accepted. A separate file can be merged in
with `CODEAGENT_MCP_CONFIG`:

```json
{
  "mcpServers": {
    "github": { "command": "npx", "args": ["-y", "@modelcontextprotocol/server-github"] }
  }
}
```

Servers are started at launch; failures are logged and do not abort startup.
Tools appear as `mcp__<server>__<tool>` in the tool list and in `doctor`.

## Testing

```bash
go test ./...            # unit and integration tests
go vet ./...
```

The tests cover glob matching and file tools, workspace confinement and diff
previews, checkpoint/undo behavior, plan mode, git tools in and out of a
repository, pricing math, session persistence, search/export, and resume by
title or id prefix, config precedence and provider presets, permission rules,
parallel read-only tool batches, the full agent loop against a fake SSE
provider, approval paths, compaction boundaries, multimodal message encoding,
the Anthropic and Gemini streaming adapters, hooks and diagnostics (including
blocking and timeouts), skill discovery and `use_skill`, background jobs and
their tools, editor resolution, and web authentication (bearer, cookie, login,
and query-token flows).

End-to-end smoke test (builds the binaries, starts the deterministic mock model,
runs a real tool turn, and checks the written file):

```bash
bash scripts/e2e-smoke.sh
```

Two dev servers are included:

- `cmd/mockllm` — OpenAI-compatible SSE server with scenarios `basic`, `multi`,
  `subagent`, `read`, `write`, `text`, `xss`, `mcp`, `mcp-ro`.
- `cmd/mockmcp` — minimal stdio MCP server with `echo` and the read-only
  `workspace_info`.

```bash
go run ./cmd/mockllm        -addr 127.0.0.1:18080 -scenario write
go run ./cmd/mockmcp
```

## Continuous integration and releases

GitHub Actions runs formatting, `go vet`, `go test -race`, a full build, and the
end-to-end smoke test on every push to `main` and every pull request.

To publish a release, push a version tag:

```bash
git tag v0.1.0
git push origin v0.1.0
```

The release workflow builds `linux/amd64`, `linux/arm64`, `darwin/amd64`,
`darwin/arm64`, and `windows/amd64` binaries, adds SHA-256 checksums, and
attaches everything to the GitHub release.

## Architecture

```text
cmd/codedeep      CLI, REPL, renderer, web launcher
cmd/mockllm       deterministic OpenAI-compatible test provider
cmd/mockmcp       deterministic stdio MCP server
internal/app      wiring: config, client, registry, sessions, MCP
internal/agent    loop, compaction, subagents, system prompt, checkpoints, events
internal/llm      provider interface, OpenAI-compatible and Anthropic/Gemini clients
internal/diff     unified diff engine for previews
internal/tools    registry plus file, search, shell, and git tools
internal/mcp      stdio JSON-RPC client and tool registration
internal/hooks    pre/post tool shell hooks
internal/diagnostics post-edit build, vet, and lint commands
internal/skills   markdown skill discovery and the use_skill tool
internal/jobs     background process manager and job tools
internal/editor   external editor resolution and launch
internal/proc     process-tree setup and termination
internal/session  atomic JSON session store with checkpoints
internal/web      HTTP API, auth, NDJSON streaming, embedded web UI
internal/approval approval interface and decisions
```

The agent emits typed events (`text`, `reasoning`, `tool_start`, `tool_end`,
`diff`, `approval_request`, `usage`, `compact`, `subagent_start`, `hook`,
`diagnostics`, `done`, …). The CLI renderer and the web UI both consume the same
events, so behavior stays consistent.

## Security notes

- Tool paths are confined to the workspace by default; set `allow_outside` only
  if you need it.
- Mutating tools require approval unless you opt into `auto`.
- The web UI binds to `127.0.0.1` by default. Set `web_token` before binding to
  another interface; the server refuses an unauthenticated non-loopback bind
  unless `web_insecure` is explicitly enabled. Cookies are HttpOnly and
  `SameSite=Strict`, and bearer tokens are compared in constant time.
- Hooks and diagnostics run shell commands you configure. Treat the config file
  as trusted input, and remember that hooks run with the same permissions as
  CodeDeep.
- Keep API keys in environment variables, not in committed config files.
- The agent is instructed not to run destructive commands without explicit
  intent, but approval mode is the real guardrail.

## Roadmap

- Per-tool cost attribution inside a turn.
- Additional provider-specific reasoning controls.
- Richer diagnostics mapping (inline line annotations instead of appended text).
