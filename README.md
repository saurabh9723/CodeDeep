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
- **Approvals** — `ask` (default), `auto`, or `readonly`; read-only tools are
  never prompted, and "always allow" is remembered per tool for the session.
- **Context management** — automatic compaction at 80% of the context window
  with a boundary-safe cut, plus `/compact` on demand.
- **Subagents** — isolated history, shared approvals, nested up to two levels.
- **MCP** — starts stdio JSON-RPC servers and exposes their tools as
  `mcp__<server>__<tool>`; `readOnlyHint` tools skip approval.
- **Sessions** — saved after every step, resumable from the CLI or the web UI.
- **Web UI** — Qoder-style interface served from the same binary, no Node or
  Electron required.
- **Confinement** — tool paths stay inside the workspace, including symlink
  checks; binary files and build directories are skipped by search tools.

## Requirements

- Go 1.24 or newer to build.
- An API key for DeepSeek (or another OpenAI-compatible endpoint).

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
./bin/codedeep -r <id>    # resume a specific session
```

REPL commands: `/help`, `/new`, `/sessions`, `/resume <id|#>`, `/title`,
`/model`, `/models`, `/approval`, `/reasoning`, `/status`, `/tools`,
`/compact`, `/clear`, `/quit`.

### Web UI

```bash
./bin/codedeep serve
# opens http://127.0.0.1:7777
```

`serve` accepts `-addr` and `-no-open`, plus the common flags below.

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
- `-m`, `-model NAME` — model name.
- `-approval MODE` — `ask`, `auto`, or `readonly`.
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
  "allow_outside": false,
  "mcp_servers": {}
}
```

Prefer environment variables over storing keys in files:

```bash
DEEPSEEK_API_KEY=sk-...          # CODEAGENT_API_KEY and OPENAI_API_KEY also work
DEEPSEEK_BASE_URL=...            # CODEAGENT_BASE_URL and OPENAI_BASE_URL also work
CODEAGENT_MODEL=deepseek-chat
CODEAGENT_APPROVAL=ask
CODEAGENT_CONTEXT_WINDOW=128000
CODEAGENT_COMPACT_AT=0.8
CODEAGENT_MAX_STEPS=60
CODEAGENT_SESSIONS_DIR=$HOME/.codedeep/sessions
CODEAGENT_WEB_ADDR=127.0.0.1:7777
CODEAGENT_ALLOW_OUTSIDE=false
CODEAGENT_DEBUG=false
CODEAGENT_CONFIG=$HOME/.codedeep/config.json
CODEAGENT_MCP_CONFIG=$HOME/.codedeep/mcp.json
```

`work_dir` is always the CLI `-cwd` value or the current directory; it is not
read from the config file.

### Project instructions

On every run CodeDeep reads `AGENTS.md`, `CODEAGENT.md`, and
`.codedeep/AGENTS.md` from the workspace and each parent directory, plus
`~/.codedeep/AGENTS.md`. Closest files take precedence. Use these for build
commands, conventions, and constraints.

## Tools and approvals

Built-in tools:

- `read_file` — numbered lines with `offset`/`limit`.
- `write_file` — create or overwrite a file, creating parent directories.
- `edit_file` — exact-substring replacement with CRLF fallback; requires a
  unique match unless `replace_all` is set.
- `glob` — `*`, `?`, `[...]`, and `**` patterns.
- `grep` — RE2 search with an optional file filter and case-insensitive mode.
- `bash` — shell command with timeout and combined stdout/stderr.
- `agent` — delegate a self-contained task to a subagent.

`glob` and `grep` skip `.git`, `node_modules`, `vendor`, `dist`, `build`,
virtualenvs, caches, and other generated directories. Three tools are read-only
and never prompt: `read_file`, `glob`, `grep`. Everything else follows the
approval mode:

- `ask` — prompt before each mutating tool call; choose allow once, always
  allow, or deny.
- `auto` — run mutating tools without prompting.
- `readonly` — deny every mutating tool.

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

The tests cover glob matching and file tools, workspace confinement, session
persistence and titles, config precedence, the full agent loop against a fake
SSE provider, approval paths, and compaction boundaries.

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
internal/agent    loop, compaction, subagents, system prompt, events
internal/llm      OpenAI-compatible streaming client
internal/tools    registry plus file, search, and shell tools
internal/mcp      stdio JSON-RPC client and tool registration
internal/session  atomic JSON session store
internal/web      HTTP API, NDJSON streaming, embedded web UI
internal/approval approval interface and decisions
```

The agent emits typed events (`text`, `reasoning`, `tool_start`, `tool_end`,
`approval_request`, `compact`, `subagent_start`, `done`, …). The CLI renderer
and the web UI both consume the same events, so behavior stays consistent.

## Security notes

- Tool paths are confined to the workspace by default; set `allow_outside` only
  if you need it.
- Mutating tools require approval unless you opt into `auto`.
- The web UI binds to `127.0.0.1` by default and has no authentication. Do not
  expose it to untrusted networks; use a reverse proxy with auth if you must.
- Keep API keys in environment variables, not in committed config files.
- The agent is instructed not to run destructive commands without explicit
  intent, but approval mode is the real guardrail.

## Roadmap

- Diff preview before edits and `/undo`.
- Session search and export.
- Optional web authentication and remote bind support.
- More providers and image input.
- Token/cost breakdown per tool and per turn.
