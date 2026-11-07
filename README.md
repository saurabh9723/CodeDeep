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
- **Coding tools** — `read_file`, `write_file`, `edit_file`, `apply_patch`
  (atomic multi-file edits), `glob`, `grep`, `bash`, and `agent` for subagents.
- **Codebase index** — an incremental symbol index for Go, Python, JS/TS, Rust,
  Java, C#, Kotlin, Ruby, PHP, C/C++, Swift, Scala, Shell, Markdown, Lua, and
  Elixir. `repo_map` and `find_symbol` give the model a token-budgeted map of
  the workspace and exact declarations by name; a ranked map is also appended
  to the system prompt (configurable budget, on by default), and the web file
  palette searches symbols alongside files.
- **Diff previews and undo** — `write_file`, `edit_file`, and `apply_patch`
  show a unified diff before they run, including in the approval prompt. Every
  turn snapshots the files it touches and records the turn's unified diff, so
  `/undo` (or the web
  undo button) restores the previous contents or removes files that were
  created, while `/checkpoints` and the web checkpoint panel review each turn's
  diff and roll back through any checkpoint.
- **Plan mode** — a read-only mode that blocks mutations and asks the model to
  produce a concrete plan. Switch to another mode to execute it.
- **Session plan** — the root agent can keep a persistent checklist with
  `todo_write` (`pending`, `in_progress`, `completed`). The plan is saved with
  the session, injected into the system prompt on every step and after
  compaction, shown in the web plan panel, and listed by `/todos`.
- **Project memory** — `remember`, `recall`, and `forget` store durable notes in
  `<workspace>/.codedeep/memory` (project) and `~/.codedeep/memory` (global).
  The system prompt carries a bounded index of note names and descriptions, and
  `/memory` searches it.
- **Automatic verification** — optionally run a build, test, or lint command
  when a turn finishes with unverified changes. Failures are fed back to the
  model for up to two correction attempts before the turn fails.
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
- **Parallel subagents** — a model turn that spawns several `agent` subagents
  runs them concurrently. Each subagent keeps its own isolated history while
  sharing one serialized lock set for events, approvals, checkpoints, and
  session saves, so parallel children cannot corrupt shared state; their token
  usage counts toward the session. Subagents nest up to two levels.
- **Subagents** — isolated history, shared approvals and checkpoints, nested up
  to two levels; their token usage counts toward the session.
- **Cost estimates** — optional per-model pricing in the config; the CLI and
  web UI show token totals and estimated USD per step, turn, and session.
- **Observability** — `codedeep stats` and the web dashboard aggregate saved
  sessions into token/cost trends, prompt-cache hit rates, per-model breakdowns,
  tool call counts, and a 24-hour tool heatmap. Sessions record per-turn usage,
  so the report is bucketed by when the work actually happened.
- **Evaluation harness** — `codedeep eval` runs a benchmark suite of realistic
  tasks in isolated workspaces, scores files, tools, events, verification, and
  cost, and compares against a baseline. Mock mode is deterministic and offline;
  `-mode real` measures an actual provider with the same tasks.
- **Run traces** — eval runs and web chat turns save their timestamped event
  streams. The web viewer filters and searches runs, replays the timeline, and
  compares two runs side by side: metric deltas, fixed and new failures, an
  aligned tool-call sequence, event counts, and prompt/answer diffs.
- **MCP** — starts stdio JSON-RPC servers and exposes their tools as
  `mcp__<server>__<tool>`; `readOnlyHint` tools skip approval.
- **Sessions** — saved after every step, resumable by id, number, or title,
  searchable across titles and transcripts, and exportable as Markdown or JSON
  from the CLI or the web UI.
- **Providers** — DeepSeek and any OpenAI-compatible endpoint, plus native
  Anthropic Messages and Gemini generateContent adapters and an Ollama preset;
  switch with `-provider` or the config file.
- **Web UI** — Qoder-style interface served from the same binary, no Node or
  Electron required. Includes diff cards, plan mode, a collapsible live plan
  panel, verification results, a checkpoint review panel with per-turn diffs and
  rollback, undo, a searchable session sidebar, a file picker with `@`-mentions,
  a slash-command palette, and image paste/drag-drop for vision models.
- **Resilient providers** — transient failures (408/409/429/5xx and connection
  errors) are retried with exponential backoff and jitter. `Retry-After` is
  honored, and the CLI and web stream show a retry notice with the delay.
- **Resumable web runs** — a chat turn belongs to the server, not the tab.
  Refreshing the page or losing the connection reattaches to the buffered
  event stream and resumes from the last event it saw; the web UI can also stop
  a running turn.
- **Images** — attach up to four PNG/JPEG/WebP/GIF images to a message (5 MB
  each) in the web UI; they are sent as multimodal content and kept in session
  exports.
- **Hooks** — pre- and post-tool shell hooks receive a JSON payload on stdin and
  `CODEDEEP_*` environment variables; a failing pre-hook can block the call.
- **Post-edit diagnostics** — pattern-matched build, vet, or lint commands run
  after a file change; `file:line:column` output is parsed into annotations that
  are shown inline (and linked to your editor in the web UI) as well as fed
  back to the model.
- **Reasoning controls** — `reasoning_effort` maps to OpenAI-compatible
  `reasoning_effort`; `reasoning_budget_tokens` enables Anthropic extended
  thinking and Gemini `thinkingConfig`.
- **Cost attribution** — token and cost totals are per turn, per session, and
  per tool call, including a by-tool breakdown in `/status` and the web UI.
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

# Option A: export the key in your shell
export DEEPSEEK_API_KEY=sk-...        # PowerShell: $env:DEEPSEEK_API_KEY="sk-..."

# Option B: keep it in a local .env (git-ignored)
cp .env.example .env                  # then fill in DEEPSEEK_API_KEY
```

CodeDeep reads `.env` from the current working directory before loading the
config file. Real shell variables take precedence over `.env`, and `.env`
takes precedence over `config.json`. Set `CODEAGENT_ENV_FILE` to load a
different env file.

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
`/plan [on|off]`, `/checkpoints`, `/checkpoint <n>`, `/undo [n]`, `/todos`,
`/memory [query]`, `/reasoning`, `/status`, `/stats [days] [workspace|all]`,
`/tools`, `/compact`, `/skills`,
`/skill <name> [task]`, `/jobs`, `/job <id>`, `/kill <id>`,
`/open [path[:line]]`, `/clear`, `/quit`.

Sessions can also be searched and exported without entering the REPL:

```bash
./bin/codedeep sessions -q "flaky test"
./bin/codedeep sessions -export "parser refactor" -format md -o notes.md
./bin/codedeep stats -days 7 -workspace
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
rows have an `↗` action that opens the file in your editor. The activity button
in the top bar opens the observability dashboard: session/turn/token/cost
summary cards, a daily token chart, cache hit breakdown, per-model and per-tool
usage with an hourly heatmap, and recent sessions that open on click. The range
(7/30/90/365 days) and scope (all workspaces or this one) are saved in the
browser, and the dashboard refreshes after each completed turn.

Set `web_token` (or `CODEAGENT_WEB_TOKEN`) to require sign-in. The server also
refuses to listen on a non-loopback address without a token unless
`web_insecure` is explicitly true:

```bash
CODEAGENT_WEB_TOKEN=long-random-string ./bin/codedeep serve -addr 0.0.0.0:7777
```

### Streaming, retries, and reconnect

Each chat turn runs on the server as an independent run with a seq-numbered
event buffer; the last 2000 events (or 4 MB) are kept for two minutes after the
turn finishes. When the browser connection drops, the page reattaches with
`GET /api/sessions/{id}/stream?after=<last seq it saw>`. A freshly loaded page
resumes from the last persisted point, so transcript history loaded from disk is
not rendered twice; the terminal event is still replayed so usage and status
finalize. If the buffer no longer reaches back that far, the server sends a
`reset` marker and the page reloads the transcript before continuing.
`POST /api/sessions/{id}/stop` cancels the run (the web stop button uses it),
`GET /api/sessions/{id}` reports `running`, and `GET /api/bootstrap` lists
`running_sessions` so a reload can reattach immediately.

Provider calls retry transient failures before any content is streamed: 408,
409, 429, 500, 502, 503, 504, and connection errors, up to four attempts with
exponential backoff (0.8s, 1.6s, 3.2s, capped at 8s) and ±10% jitter.
`Retry-After` (seconds or an HTTP date) takes precedence and is capped at 60s.
Each retry emits a `retry` event and a CLI notice with the attempt, status, and
delay. Client errors such as 401 and 400 are never retried.

### Diagnostics

```bash
./bin/codedeep doctor          # config, workspace, tools, MCP servers
./bin/codedeep doctor -ping    # verify the provider connection and list models
./bin/codedeep sessions        # list saved sessions
./bin/codedeep sessions -json -workspace
./bin/codedeep stats           # tokens, cost, cache hits, and tools (30 days)
./bin/codedeep stats -days 7 -workspace -json
```

### Observability

`stats` reads every saved session and aggregates the requested window. Sessions
written by the current agent store per-turn usage (`turn_stats`), which lets the
report bucket tokens, cost, and tool calls by day and hour; older sessions fall
back to their session totals attributed to the last-updated day. Prompt-cache
hit rate is `cache_hit_tokens / prompt_tokens`, and cost uses each session's own
model pricing, so mixed-model history is priced correctly. The same report is
available at `GET /api/analytics?days=30&scope=all|workspace` and powers the web
dashboard. When no model pricing is configured the report still shows tokens and
cache metrics and marks cost as unavailable.

### Trace viewer

Every run can be recorded as a trace: a file-backed JSON document holding the
run metadata, prompt, verifier result, and the full timestamped event stream.
Eval runs and web chat runs both write traces, so benchmarks and live sessions
can be inspected the same way.

Traces live in `~/.codedeep/traces` by default. Set `traces_dir` (or
`CODEAGENT_TRACES_DIR`) to move them, or to an empty path to disable capture.
Each trace stores a small metadata header used for listing plus the full event
log; the store keeps the newest 500 and prunes older files.

The web UI has a **Traces** button that opens the viewer: filter by kind (eval
or session), search by title/task/model/status, and select a run to see its
metadata, prompt, verifier output, and a merged timeline of messages, tool
calls, diffs, approvals, retries, and usage. Traces can be deleted from the
detail pane.

**Compare** diffs any two runs: click `Compare` on a trace, then pick the
second one. The comparison shows both headers, metric deltas (duration, model
and tool calls, tokens, cost, events), failures fixed in B versus newly
introduced, warnings, an aligned tool-call sequence (added, removed, and shared
calls via a longest-common-subsequence match), per-type event counts, and line
diffs of the prompt and final answer when they differ. `Swap A/B` reverses the
direction. Comparison is computed server-side, so it works the same for eval
runs and live sessions.

```text
GET    /api/traces?kind=eval|session&q=TEXT
GET    /api/traces/{id}
GET    /api/traces/compare?a=ID&b=ID
DELETE /api/traces/{id}
```

The eval harness writes a trace per run and accepts `-trace-dir DIR` to choose
the destination and `-no-traces` to skip capture entirely.

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
  "reasoning_effort": "",
  "reasoning_budget_tokens": 0,
  "prompt_cache": true,
  "context_window": 128000,
  "compact_at": 0.8,
  "approval": "ask",
  "traces_dir": "",
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
  "repo_map": { "prompt_tokens": 1200, "max_files": 5000, "refresh_sec": 2 },
  "verify": { "command": "go test ./...", "timeout_sec": 300 },
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
`cache_hit_per_mtok` when set, otherwise the normal input rate; Anthropic cache
writes use `cache_write_per_mtok` the same way. Each model turn
is split evenly across the tool calls it requested, so `/status`, the web
header, and the finished-turn summary can show usage and cost by tool as well as
per turn and per session. Prices change, so check your provider's current rates.

Environment variables override the config file and are parsed through one
typed schema in [`internal/env`](internal/env/env.go). A malformed numeric or
boolean value fails fast with the variable name instead of being ignored.
CodeDeep loads `.env` from the current working directory (or the path in
`CODEAGENT_ENV_FILE`) before reading the config; exported shell variables win
over `.env` values. Copy [`.env.example`](.env.example) to `.env` to get
started.

```dotenv
CODEAGENT_PROVIDER=openai        # openai, anthropic, gemini, or ollama
DEEPSEEK_API_KEY=sk-...          # CODEAGENT_API_KEY and OPENAI_API_KEY also work
ANTHROPIC_API_KEY=sk-ant-...     # when CODEAGENT_PROVIDER=anthropic
GEMINI_API_KEY=...               # when CODEAGENT_PROVIDER=gemini (GOOGLE_API_KEY also works)
DEEPSEEK_BASE_URL=...            # CODEAGENT_BASE_URL and OPENAI_BASE_URL also work
CODEAGENT_MODEL=deepseek-chat
CODEAGENT_REASONING_EFFORT=medium   # minimal, low, medium, or high
CODEAGENT_REASONING_BUDGET=8000     # extended thinking tokens (Anthropic, Gemini)
CODEAGENT_PROMPT_CACHE=true          # Anthropic cache_control breakpoints
CODEAGENT_APPROVAL=ask
CODEAGENT_CONTEXT_WINDOW=128000
CODEAGENT_COMPACT_AT=0.8
CODEAGENT_MAX_STEPS=60
CODEAGENT_MAX_TOKENS=8192
CODEAGENT_TEMPERATURE=0.3
CODEAGENT_SESSIONS_DIR=$HOME/.codedeep/sessions
CODEAGENT_TRACES_DIR=$HOME/.codedeep/traces
CODEAGENT_WEB_ADDR=127.0.0.1:7777
CODEAGENT_WEB_TOKEN=long-random-string
CODEAGENT_WEB_INSECURE=false
CODEAGENT_DIAGNOSTIC_TIMEOUT=30
CODEAGENT_ALLOW_OUTSIDE=false
CODEAGENT_DEBUG=false
CODEAGENT_REPO_MAP_TOKENS=1200       # 0 disables the system-prompt map
CODEAGENT_REPO_MAP_MAX_FILES=5000
CODEAGENT_REPO_MAP_REFRESH_SEC=2
CODEAGENT_CONFIG=$HOME/.codedeep/config.json
CODEAGENT_MCP_CONFIG=$HOME/.codedeep/mcp.json
```

`CODEAGENT_ENV_FILE` must be exported in the shell; it selects the env file
before that file is read.

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

### Reasoning controls

Reasoning is opt-in and provider-specific:

- **OpenAI-compatible** — `reasoning_effort` (`minimal`, `low`, `medium`, or
  `high`) is sent as the `reasoning_effort` request field. Leave it empty for
  models that do not support it.
- **Anthropic** — `reasoning_budget_tokens` enables extended thinking with that
  budget. `max_tokens` is raised to `budget + 4096` when needed, temperature is
  omitted (the API requires the default), and thinking blocks are echoed back
  with their signatures so multi-step tool use remains valid.
- **Gemini** — `reasoning_budget_tokens` becomes
  `generationConfig.thinkingConfig` with `includeThoughts: true`, so reasoning
  streams to the CLI and web UI like the other providers.

Budgets are clamped to 1024–64000 tokens and `0` disables extended thinking.
`codedeep doctor` prints the active reasoning settings.

### Prompt caching

Prompt caching reuses the model's read of your system prompt, tool
declarations, and conversation prefix, cutting both cost and latency.

- **Anthropic** — explicit `cache_control` breakpoints are placed on the system
  prompt, the last tool, and the tail of the conversation. Set
  `prompt_cache: false` (or `CODEAGENT_PROMPT_CACHE=false`) to send plain
  requests. Cache reads are reported as cache-hit tokens and priced with
  `cache_hit_per_mtok`; cache writes are reported and priced with
  `cache_write_per_mtok`.
- **OpenAI-compatible and DeepSeek** — caching is automatic and always on; no
  request changes are needed. Cache hits show up in token usage.
- **Gemini** — Gemini 2.5 implicit caching is automatic; cached tokens are
  reported in usage.

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
command in the workspace and appends its output to the tool result. Common
`path:line:column: message` and `path(line,column): message` formats are also
parsed into structured annotations: the CLI prints them as file/line/column
rows, and the web UI renders clickable rows that open the location in your
editor. A pattern
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

### Session plan, memory, and verification

Three capabilities keep long tasks coherent across turns and sessions:

- **Plan** — `todo_write` replaces the session's checklist (up to 50 items).
  Exactly one item should be `in_progress`. The plan survives turns and
  compaction, is re-injected into the system prompt on every step, and appears
  in the web plan panel and `/todos`. Subagents see the plan but cannot edit it.
- **Memory** — `remember` saves a note with a description and markdown body;
  saving the same name replaces it. `recall` lists the index or reads matching
  notes in full, and `forget` removes one. Project notes shadow global notes
  with the same name. The prompt index is capped so memory cannot crowd out the
  conversation. Memory lives in plain files, so it can be reviewed and
  committed if you want to share it with your team.
- **Verification** — set `verify.command` to run once a turn changed files and
  the model stops calling tools. A failure is appended to the conversation as
  `[automatic verification failed]` with the command output, and the model gets
  up to two attempts to fix it; after that the turn fails instead of reporting
  success. Commands run in the workspace with the configured shell and are
  capped by `timeout_sec` (default 300, maximum 900). `doctor` shows the
  configured command, and the CLI/web stream report `verify_start` and `verify`
  events.

```json
{
  "verify": { "command": "go test ./...", "timeout_sec": 300 }
}
```

Plans and memory are updated by internal tools that do not require approval and
remain available in plan and read-only modes. They are still subject to explicit
deny rules.

### Repo map and symbol index

CodeDeep keeps an in-memory index of the workspace so the model can orient
itself without reading every file. It records files, languages, line counts, and
declarations (functions, methods, types, classes, constants, and markdown
headings) and refreshes incrementally when file size or mtime changes.

The index is model-agnostic: nothing depends on the model's size or context
window. Two tools expose it, and both stay available whatever the prompt budget:

- `repo_map` — ranked files with their key declarations and line numbers,
  budgeted by tokens. Pass `query` to rank for a task, `path` to limit a
  subtree, and `max_tokens` (default 1200, maximum 8000).
- `find_symbol` — exact, prefix, substring, and fuzzy name matching across
  kinds; returns `path:line` with the declaration signature.

A map section is appended to the end of the system prompt, ranked by the latest
user request, so the stable prompt prefix stays cache-friendly. `prompt_tokens`
sets its budget (default 1200; 0 disables injection). The index fails open: if
the workspace cannot be read, the prompt is unchanged.

```json
{
  "repo_map": { "prompt_tokens": 1200, "max_files": 5000, "refresh_sec": 2 }
}
```

```bash
codedeep index                    # print the token-budgeted map
codedeep index -q Resolve         # search declarations by name
codedeep index -q handler -kind func -path internal/web -n 20 -json
```

Environment overrides: `CODEAGENT_REPO_MAP_TOKENS`,
`CODEAGENT_REPO_MAP_MAX_FILES`, and `CODEAGENT_REPO_MAP_REFRESH_SEC`.

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
- `apply_patch` — an ordered list of `edit_file`-style replacements across one
  or more files, applied atomically: every edit is validated first, and if any
  edit fails no file is written. Repeated hunks for the same file apply in
  order, the approval prompt shows the combined diff, and one checkpoint
  captures every touched file for undo.
- `glob` — `*`, `?`, `[...]`, and `**` patterns.
- `grep` — RE2 search with an optional file filter and case-insensitive mode.
- `bash` — shell command with timeout and combined stdout/stderr; set
  `background: true` to run it as a background job.
- `job_output`, `job_list`, `job_kill` — read, list, and stop background jobs.
- `git_status` — branch, upstream state, last commit, and changed files.
- `git_diff` — unified diff of uncommitted changes; supports `staged`, `stat`,
  `path`, and `context`.
- `agent` — delegate a self-contained task to a subagent.
- `todo_write` — replace the session's persistent working plan.
- `remember`, `recall`, `forget` — save, read, and delete durable project or
  global memory notes.
- `repo_map` — token-budgeted map of files and declarations, ranked by query.
- `find_symbol` — find functions, methods, types, and classes by name.
- `use_skill` — load a workspace or global skill from `.codedeep/skills`.

`glob` and `grep` skip `.git`, `node_modules`, `vendor`, `dist`, `build`,
virtualenvs, caches, and other generated directories. The read-only tools
(`read_file`, `glob`, `grep`, `git_status`, `git_diff`, `job_output`,
`job_list`, `recall`, `repo_map`, `find_symbol`, `use_skill`) never prompt.
`todo_write`, `remember`, and
`forget` are internal state tools: they skip approval and stay available in
read-only and plan modes, but explicit deny rules still apply. Everything else
follows the approval mode:

- `ask` — prompt before each mutating tool call; choose allow once, always
  allow, or deny. When the call has a preview, the prompt includes the diff.
- `auto` — run mutating tools without prompting.
- `readonly` — deny every mutating tool.
- `plan` — investigate with read-only tools, allow no mutations, and produce a
  concrete plan for a later run.

Each turn records a checkpoint of the pre-change contents of every file that
`write_file`, `edit_file`, or `apply_patch` touches (up to 1 MB per file), plus the unified diff
of the turn's changes. `/checkpoints` lists them with per-turn `+added/-removed`
stats, `/checkpoint <n>` shows a checkpoint's diff, and `/undo` restores the
most recent checkpoint (removing files that did not exist before the turn).
`/undo <n>` rolls the workspace back through checkpoint `n` — it reverts every
file change from that turn onward and drops the rolled-back checkpoints. The
last 20 checkpoints are kept with the session.

When a model requests several read-only tools in one turn, CodeDeep runs them
concurrently (up to four) and still reports results in call order. A batch of
`agent` subagent calls runs concurrently too, sharing one serialized lock set
for events, approvals, checkpoints, and session saves. Any batch that contains
a mutating tool runs sequentially.

### Permission rules

`permissions` grant or deny tool calls without prompting. Each rule has a
`tool` glob and an `action` of `allow` or `deny`; `command` and `path` are
optional globs. When both are present, both must match. For `apply_patch`, a
path rule is checked against every edit: a deny rule blocks the call if any
path matches, and an allow rule covers the call only if every path matches.
"Always allow" for `apply_patch` is narrowed to the exact set of files in that
patch.

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
the Anthropic and Gemini streaming adapters (including extended-thinking and
reasoning request bodies), hooks and diagnostics (including inline annotation
parsing, blocking, and timeouts), per-tool usage attribution, skill discovery
and `use_skill`, background jobs and
their tools, editor resolution, provider retry/`Retry-After` handling,
server-owned run buffering and replay (disconnect/reattach, slow-viewer drops,
reset on buffer overflow), concurrent-run rejection and stop, web
authentication (bearer, cookie, login, and query-token flows), the session plan
tool and prompt injection, durable memory scoping/search/limits and its tools,
and the verification gate (passing, failure feedback, the two-attempt cap, and
skipping turns that changed nothing). Analytics have dedicated fixtures for
daily bucketing, range and workspace filters, cache-hit math, per-model and
per-tool aggregation, the hourly heatmap, legacy sessions without per-turn
stats, and the `/api/analytics` endpoint. Trace coverage includes store
round-trip, pruning and unsafe-ID rejection, eval trace persistence, live
session capture, the trace list/get/delete API, and the compare API and engine
(metric deltas, failure sets, tool-sequence alignment, event counts, text
diffs). Environment coverage
includes typed parsing, provider alias precedence, `.env` loading, and
malformed-value errors. Index coverage includes Go AST symbols, regex parsers
for the other supported languages, ignored directories, noise and binary files,
incremental refresh, fuzzy search, map ranking and budgets, prompt caching and
invalidation, the `/api/index` endpoint, and system-prompt injection.

The evaluation harness has its own tests for task discovery and strict schema
validation, filter selection, config and environment isolation, scoring and
budget enforcement, baseline regression detection, and an end-to-end runner test
that builds `codedeep`, executes a scripted task through the in-process mock,
and checks the resulting files and metrics.

End-to-end smoke test (builds the binaries, starts the deterministic mock model,
runs tool turns, and checks files and sessions on disk). It also proves provider
retries against a rate-limited mock, that a run survives a dropped connection
and can be reattached and stopped, the auth/plan/undo/checkpoint flows, the
verification gate (including the retry cap), plan persistence and `/todos`,
durable memory across runs, `/memory`, the `stats` report and analytics API,
and the new `doctor` lines:

```bash
bash scripts/e2e-smoke.sh
```

Benchmark-style evaluation suite. It runs realistic tasks in isolated
workspaces, scores the event stream, files, and an optional external verifier,
and tracks calls, tokens, cost, and wall time. Mock mode is deterministic and
fully offline:

```bash
go run ./cmd/codedeep eval
go run ./cmd/codedeep eval -repeat 3 -jobs 4
go run ./cmd/codedeep eval -baseline evals/baseline.json -fail-on-regression
```

To measure a real model, switch mode and pass the provider/model. Credentials
come from the shell or the `.env` file:

```bash
export DEEPSEEK_API_KEY="..."   # or DEEPSEEK_API_KEY=... in .env
go run ./cmd/codedeep eval -mode real -provider openai -model deepseek-chat \
  -price-in 0.27 -price-out 1.10
```

See [`evals/README.md`](evals/README.md) for the task schema, mock scripts,
scoring rules, baselines, provider-specific commands, and isolation guarantees.

Two dev servers are included:

- `cmd/mockllm` — OpenAI-compatible SSE server with scenarios `basic`, `multi`,
  `subagent`, `read`, `write`, `text`, `todo`, `memory`, `flaky` (rate limits
  the first two attempts), `xss`, `mcp`, `mcp-ro`. It is a thin wrapper around
  the reusable `internal/mockllm` server.
- `cmd/mockmcp` — minimal stdio MCP server with `echo` and the read-only
  `workspace_info`.

```bash
go run ./cmd/mockllm        -addr 127.0.0.1:18080 -scenario write
go run ./cmd/mockmcp
```

## Continuous integration and releases

GitHub Actions runs formatting, `go vet`, `go test -race`, a full build, the
end-to-end smoke test, and the mock evaluation suite (with baseline comparison)
on every push to `main` and every pull request.

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
internal/env      typed environment schema and .env loading for the whole app
internal/agent    loop, compaction, subagents, system prompt, checkpoints, events
internal/llm      provider interface, OpenAI-compatible and Anthropic/Gemini clients
internal/mockllm  reusable scripted mock provider for tests and evals
internal/eval     eval tasks, isolated runner, scoring, reports, and baselines
internal/diff     unified diff engine for previews
internal/tools    registry plus file, search, shell, and git tools
internal/mcp      stdio JSON-RPC client and tool registration
internal/hooks    pre/post tool shell hooks
internal/index    incremental symbol index, repo map ranking, and index tools
internal/diagnostics post-edit build, vet, and lint commands
internal/memory   durable project/global notes and the remember/recall/forget tools
internal/analytics session aggregation for token/cost/cache/tool observability
internal/trace     file-backed run traces for eval and live session replay
internal/verify   end-of-turn verification command runner
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
`diagnostics`, `todos`, `verify_start`, `verify`, `done`, …). The CLI renderer
and the web UI both consume the same events, so behavior stays consistent.

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
- Keep API keys in environment variables or a git-ignored `.env`, not in
  committed config files. `.gitignore` already excludes `.env` and `.env.*`.
- The agent is instructed not to run destructive commands without explicit
  intent, but approval mode is the real guardrail.

## Roadmap

Nothing queued — the previously planned per-turn diff review across multiple
checkpoints is now implemented.

## Done from the roadmap

- Multi-checkpoint diff review (`/checkpoints`, `/checkpoint <n>`, `/undo <n>`,
  and the web checkpoint panel).
- Session plans with `todo_write`, `/todos`, and the web plan panel.
- Durable project/global memory with `remember`, `recall`, `forget`, and
  `/memory`.
- The automatic verification gate with bounded retries and the `verify` config.
- Parallel subagents (and parallel read-only tool batches).
- Provider-native prompt caching controls.
