# CodeDeep evaluation harness

The eval harness is a benchmark-style quality check. It runs CodeDeep against a
suite of realistic tasks in throwaway workspaces and scores the result from four
independent sources:

- the NDJSON event stream (`-json`): tool sequence, errors, retries, subagents,
  verification attempts, and the final answer
- the files left in the workspace
- an optional external verifier such as `go test ./...`
- efficiency metrics: model calls, tool calls, tokens, cost, and wall time

This is different from `scripts/e2e-smoke.sh`: smoke tests prove that features
work, while evals measure how well the agent solves tasks and catch quality or
efficiency regressions over time.

The default mode is fully offline. A deterministic OpenAI-compatible mock model
runs inside the harness process, so no API key, network access, or separate mock
server is required.

## Quick start

```bash
# Run every task with the mock model (offline, deterministic)
go run ./cmd/codedeep eval

# List tasks and tags without running anything
go run ./cmd/codedeep eval -list

# Run one capability and repeat it
go run ./cmd/codedeep eval -task write-file -repeat 3

# Run tasks in parallel and require a 90% pass rate across repeats
go run ./cmd/codedeep eval -jobs 4 -repeat 2 -min-pass-rate 0.9

# Machine-readable and Markdown reports
go run ./cmd/codedeep eval -json > evals/report.json
go run ./cmd/codedeep eval -md evals/report.md

# Compare against the checked-in baseline
go run ./cmd/codedeep eval -baseline evals/baseline.json -fail-on-regression
```

When the harness is run through `go run`, each task re-executes the temporary
`codedeep` binary that `go run` built. In CI, build once and point at the binary:

```bash
go build -o bin/codedeep ./cmd/codedeep
./bin/codedeep eval -binary ./bin/codedeep
```

`-binary` is optional; without it the harness reuses its own executable.

Exit codes:

- `0` all selected tasks passed (and no regression when `-fail-on-regression`)
- `1` at least one task failed, or a baseline regression was found
- `2` harness/setup error, invalid flags, or missing credentials

## What the suite covers

The checked-in suite lives in `evals/tasks`. Each directory is one task.

- `write-file` — create a file with exact content; strict budget of 2 model calls
- `read-and-summarize` — read before answering; proves tool output is used
- `edit-existing` — targeted `edit_file`; forbids a full rewrite
- `parallel-reads` — independent read-only calls in one turn
- `todo-plan` — record a plan and emit the `todos` event
- `memory-roundtrip` — `remember` then `recall` from an isolated HOME
- `subagent-delegation` — delegate to the `agent` tool, assert
  `subagent_start`/`subagent_end`, child usage, and the reported result. The
  harness process itself does not spawn subagents; the events come from
  CodeDeep's own `agent` tool, and the scripted child turns prove that child
  tool calls and usage are attributed correctly.
- `plan-mode-guard` — plan mode blocks mutation; no file is created
  (mock-only: it depends on a scripted write attempt)
- `retry-recovery` — two HTTP 429s with `Retry-After`; asserts retry events
  (mock-only: real providers cannot be asked to rate-limit on demand)
- `verify-repair` — the automatic verification gate fails an edit, feeds the
  failure back, and the repair passes (mock-only: scripted two-phase edit)
- `go-bug-fix` — read, patch, and prove a Go bug fix with `go test ./...`

Tags (`capability`, `file`, `coding`, `reliability`, `safety`, `mock-only`, …)
drive selection with `-tag` and `-skip-tag`. Tasks tagged `mock-only` are
skipped automatically in real mode so a benchmark run against a real model does
not fail for reasons unrelated to model quality.

## Task format

A task is `evals/tasks/<name>/task.json` plus an optional `files/` fixture
directory and an optional script file:

```json
{
  "name": "write-file",
  "description": "Create a file and report success.",
  "tags": ["capability", "file"],
  "prompt": "Create report.txt containing status: ok.",
  "timeout_sec": 90,
  "timeout_sec_real": 300,
  "fixture_dir": "files",
  "setup": [{ "path": "notes.txt", "content": "seeded\n", "mode": "0644" }],
  "config": {
    "max_steps": 24,
    "approval": "auto",
    "verify": { "command": "go test ./...", "timeout_sec": 60 }
  },
  "mock": { "script": "script.json" },
  "expect": {
    "exit_code": 0,
    "files": [{ "path": "report.txt", "contains": "status: ok" }],
    "text_contains": ["report.txt"],
    "text_not_contains": ["cannot"],
    "tools": {
      "required": ["write_file"],
      "forbidden": ["bash"],
      "counts": { "write_file": 1 },
      "min": { "read_file": 1 },
      "max": { "write_file": 2 },
      "no_errors": true,
      "output_contains": { "write_file": ["created"] }
    },
    "events": { "required": ["done"], "forbidden": ["error"] },
    "subagents": { "min": 0, "max": 0 },
    "retries": { "min": 0, "max": 2 },
    "verify": { "min": 1, "require_success": true },
    "verifier_exit": 0,
    "no_error_events": true
  },
  "budget": {
    "max_model_calls": 4,
    "max_tool_calls": 3,
    "max_total_tokens": 8000,
    "max_cost_usd": 0.01,
    "max_duration_ms": 30000,
    "strict": false,
    "real": {
      "max_model_calls": 8,
      "max_tool_calls": 6,
      "strict": true
    }
  },
  "verifier": "go test ./...",
  "verifier_timeout_sec": 120
}
```

Field notes:

- `prompt` is passed to `codedeep -p`; `skip`/`skip_reason` disable a task
  explicitly and report it as skipped.
- `fixture_dir` defaults to `files`; fixtures are copied into every run's
  workspace. Symlinks are rejected.
- `setup` files are written after fixtures, which is handy for a mutable seed
  file that also exists in the fixture tree.
- `config` is deep-merged over the harness base config. `provider`, `model`,
  `base_url`, `api_key`, `sessions_dir`, and `allow_outside` are reserved and
  rejected, because the harness owns connection and isolation settings.
- `expect.files` supports `exists`, `contains`, `not_contains`, `equals`, and
  `regex`.
- `expect.tools.output_contains` matches the combined `tool_end` output for that
  tool name.
- Budgets are warnings by default so noisy timing/token drift does not fail CI.
  Set `"strict": true` to make any exceeded budget a hard failure.
- `budget.real` overrides the thresholds for real-model runs. Scripted mock
  trajectories are efficient by construction; a real model usually reads a file
  before editing it or takes an extra turn, so the checked-in tasks give real
  runs more headroom while still catching runaway loops. When `budget.real` is
  absent, real runs fall back to the base thresholds.
- `timeout_sec` applies to mock runs (default 90s). `timeout_sec_real` applies
  to real runs and defaults to 300s, because a model call over a local or hosted
  API takes far longer than a scripted response.
- `expect.verifier_exit` defaults to `0` when a `verifier` is configured.

Strict JSON decoding is intentional: a misspelled field is an error, not a
silently ignored assertion.

## Mock scripts

Scripts describe the deterministic trajectory for mock mode. They are consumed
one step per chat completion request, including requests made by subagents. A
step can emit text and any number of tool calls:

```json
{
  "steps": [
    {
      "reasoning": "Read first.",
      "text": "Reading math.go.",
      "calls": [{ "name": "read_file", "args": { "path": "math.go" } }]
    },
    { "text": "Done." }
  ]
}
```

A bare array of steps is also accepted. Arguments must be JSON objects. The
legacy named scenarios (`basic`, `read`, `write`, `multi`, `subagent`, `todo`,
`memory`, `flaky`, `xss`, `mcp`, `mcp-ro`) still work without a script. When a
script is exhausted the mock replies with `Eval script complete.` so a runaway
loop is visible instead of hanging.

Scripts are a mock detail only. In real mode the mock is not started and the
script is ignored; the prompt and the model drive the trajectory.

## Metrics and scoring

Every repeated run is scored independently, then aggregated per task:

- task status is `passed` when the pass rate meets `-min-pass-rate` and at least
  one run passed; individual failed runs are listed as `flaky: …` warnings
- metrics are medians across repeats, which limits timing noise
- model calls are counted from `usage` events (one per provider call)
- token/cost totals come from the final `done` event, so subagent usage is
  counted exactly once
- cost uses mock prices in mock mode, or the `-price-*` flags in real mode
- wall-time drift over 50% is a warning because clocks are noisy

The report prints one line per task plus a run summary. `-v` also prints failed
stderr tails and kept workspace paths.

## Baselines

A baseline records pass state and median metrics per task:

```bash
# Regenerate after an intentional improvement
go run ./cmd/codedeep eval -update-baseline evals/baseline.json

# Enforce it in CI
go run ./cmd/codedeep eval -baseline evals/baseline.json -fail-on-regression
```

Regressions include a task that passed in the baseline but fails now, and metric
increases over `-regression-pct` (default 25%) for model calls, tool calls,
tokens, and cost. Timing is only a warning. Improvements are reported but do not
fail the run. Tasks present in the baseline but not selected for the current run
produce a warning so accidental filters are visible.

Use a separate baseline per real model, for example
`evals/baseline.deepseek-chat.json`. Cross-model comparisons are not meaningful
because different models legitimately need different numbers of calls.

## Switching to a real model

Mock mode tests the harness and the agent plumbing; real mode measures model
quality. The switch is only three flags plus a credential, because the harness
isolates the subprocess and passes connection settings through the generated
config:

```bash
export DEEPSEEK_API_KEY="..."   # or put it in your shell profile once

go run ./cmd/codedeep eval \
  -mode real \
  -provider openai \
  -model deepseek-chat
```

Prompts, fixtures, expectations, verifier, repeat count, and reporting stay the
same. Budgets and timeouts switch automatically: real runs use `budget.real`
and `timeout_sec_real` when a task defines them, otherwise they use the base
budget and a 300s default timeout. Tasks tagged `mock-only` are skipped and
listed with a reason.

### Provider commands and credentials

Provider names are normalized, so `deepseek`/`openai` are both OpenAI-compatible
and `claude`/`google` are aliases for `anthropic`/`gemini`.

- **DeepSeek (default OpenAI-compatible)**
  ```bash
  export DEEPSEEK_API_KEY="..."
  go run ./cmd/codedeep eval -mode real -provider openai -model deepseek-chat
  ```
- **OpenAI**
  ```bash
  export OPENAI_API_KEY="..."
  go run ./cmd/codedeep eval -mode real -provider openai -model gpt-4.1-mini \
    -base-url https://api.openai.com/v1
  ```
- **Anthropic**
  ```bash
  export ANTHROPIC_API_KEY="..."
  go run ./cmd/codedeep eval -mode real -provider anthropic -model claude-sonnet-4-5
  ```
- **Gemini**
  ```bash
  export GEMINI_API_KEY="..."
  go run ./cmd/codedeep eval -mode real -provider gemini -model gemini-2.5-pro
  ```
- **Ollama (local, no key)**
  ```bash
  ollama serve
  go run ./cmd/codedeep eval -mode real -provider ollama -model qwen2.5-coder:7b
  ```
  The default base URL is `http://localhost:11434/v1`; override it with
  `-base-url` if Ollama listens elsewhere.

`CODEAGENT_API_KEY` works as a fallback for every hosted provider. Credentials
are checked before the first task, so a missing key fails fast with a message
naming the variables to set instead of producing a suite of 401 failures.
Environment variables are never written to the generated config file, and mock
mode strips all provider keys before starting the agent.

Connection flags override any value a task might try to set:

- `-provider` picks the adapter (`openai`, `anthropic`, `gemini`, `ollama`)
- `-model` picks the model name and is also the pricing lookup key
- `-base-url` is optional; leave it unset to use the provider default

The harness deliberately ignores `CODEAGENT_PROVIDER`, `CODEAGENT_MODEL`, and
`CODEAGENT_BASE_URL` from your shell so a run is reproducible from the command
line. Pass these as flags instead; API keys are the only connection settings
taken from the environment.

### Cost accounting for real models

Mock prices are built in. For a real model, pass the current list prices so the
report can compare cost across runs:

```bash
go run ./cmd/codedeep eval -mode real -provider openai -model deepseek-chat \
  -price-in 0.27 -price-out 1.10
```

Flags are USD per million tokens: `-price-in`, `-price-out`,
`-price-cache-hit`, `-price-cache-write`. Price flags require `-model`. Without
them the run still executes; cost simply reports `$0`. Always take the numbers
from the provider's current pricing page rather than hard-coding stale values.

### Changing to a newer model later

No harness code changes are needed. Keep the same provider command and change
the model, then give the new model its own baseline:

```bash
export OPENAI_API_KEY="..."
go run ./cmd/codedeep eval -mode real -provider openai -model gpt-4.1-mini \
  -base-url https://api.openai.com/v1 \
  -update-baseline evals/baseline.gpt-4.1-mini.json
```

If the newer model is served by the same API, only `-model` changes. If it
lives behind another endpoint, add `-base-url`. If it belongs to another
provider family, change `-provider` and set that provider's environment
variable. Compare runs only against a baseline recorded for the same
provider/model; a newer model is expected to beat the old one on pass rate and
efficiency, but the numbers are not directly comparable across models.

### What to expect in real mode

- Output is not deterministic. Use `-repeat 3` and judge tasks by pass rate.
- Real runs use the relaxed `budget.real` thresholds and a 300s default timeout.
  A tiny local model can still exceed those on a slow CPU; raise
  `timeout_sec_real` or lower `-jobs` if a run is timing out rather than
  failing an assertion.
- Do not reuse the mock baseline. Create a baseline for the model you ship:
  ```bash
  go run ./cmd/codedeep eval -mode real -provider ollama -model qwen2.5-coder:7b \
    -repeat 3 -update-baseline evals/baseline.qwen2.5-coder-7b.json
  ```
- Some expectations are strict on purpose (exact file contents, forbidden
  rewrite). If a legitimate model strategy differs, adjust the task rather than
  loosening the assertion blindly.
- `-include-mock-only` forces the mock-only tasks to run too, but the scripted
  trajectory is ignored, so their exact tool-count assertions will usually fail.
  It is useful only when you have rewritten a task to be provider-agnostic.

To evaluate a local build rather than the harness binary:

```bash
go build -o bin/codedeep ./cmd/codedeep
./bin/codedeep eval -mode real -provider openai -model deepseek-chat \
  -binary ./bin/codedeep
```

## Isolation guarantees

Each run gets a fresh workspace, `HOME`/`USERPROFILE`, sessions directory,
config file, and Go build cache root. In addition:

- `allow_outside` is forced off and paths in task definitions are checked for
  traversal
- external MCP config and all `CODEAGENT_*` overrides are cleared, and
  `mcp_servers` is empty unless a task opts in through its own config
- approval defaults to `auto` so tasks never block on a prompt; a task can set
  `"approval": "plan"` or `"readonly"` to test a guard
- mock mode removes provider keys from the environment and points the agent at
  the in-process mock URL
- `-keep` preserves temp workspaces for debugging; files are otherwise removed

These guarantees are what make it safe to run the same suite in CI, on a laptop
with real keys exported, and against a local Ollama server.

## Adding a task

1. Create `evals/tasks/<name>/task.json`; add `files/` and `script.json` if
   needed.
2. Write a prompt that a real model could answer without seeing the script.
3. Assert outcomes, not phrasing: files, tool requirements, event counts,
   verifier exit code. Use `text_contains` only for stable facts.
4. Add tags; mark script-dependent tasks `mock-only`.
5. Run `go run ./cmd/codedeep eval -task <name> -v` and then the full suite.
6. Regenerate the baseline only after the new task is stable.

## CI

The `eval` job in `.github/workflows/ci.yml` builds `codedeep`, runs the suite in
mock mode with parallelism, and compares it against
`evals/baseline.json`. No secrets are required. Real-model runs are intentionally
manual because they cost money and are non-deterministic.
