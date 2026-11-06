#!/usr/bin/env bash
# Build the binaries, run a real tool turn against the deterministic mock
# provider, and verify the resulting file and session on disk.
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
TMP=$(mktemp -d)

case "$(uname -s)" in
  MINGW*|MSYS*|CYGWIN*)
    TMP=$(cd "$TMP" && pwd -W)
    EXT=".exe"
    ;;
  *)
    EXT=""
    ;;
esac

PORT=${PORT:-18080}
WEBPORT=${WEBPORT:-18081}
FLAKYPORT=${FLAKYPORT:-18083}
FLAKYWEBPORT=${FLAKYWEBPORT:-18084}
SLOWPORT=${SLOWPORT:-18085}
SLOWWEBPORT=${SLOWWEBPORT:-18086}
TODOPORT=${TODOPORT:-18087}
MEMPORT=${MEMPORT:-18088}
MOCK_PID=""
WEB_PID=""
MOCK2_PID=""
WEB2_PID=""
MOCK3_PID=""
WEB3_PID=""
MOCK4_PID=""
MOCK5_PID=""
AUTH_PID=""
cleanup() {
  if [ -n "$AUTH_PID" ]; then
    kill "$AUTH_PID" 2>/dev/null || true
    wait "$AUTH_PID" 2>/dev/null || true
  fi
  if [ -n "$MOCK5_PID" ]; then
    kill "$MOCK5_PID" 2>/dev/null || true
    wait "$MOCK5_PID" 2>/dev/null || true
  fi
  if [ -n "$MOCK4_PID" ]; then
    kill "$MOCK4_PID" 2>/dev/null || true
    wait "$MOCK4_PID" 2>/dev/null || true
  fi
  if [ -n "$WEB3_PID" ]; then
    kill "$WEB3_PID" 2>/dev/null || true
    wait "$WEB3_PID" 2>/dev/null || true
  fi
  if [ -n "$MOCK3_PID" ]; then
    kill "$MOCK3_PID" 2>/dev/null || true
    wait "$MOCK3_PID" 2>/dev/null || true
  fi
  if [ -n "$WEB2_PID" ]; then
    kill "$WEB2_PID" 2>/dev/null || true
    wait "$WEB2_PID" 2>/dev/null || true
  fi
  if [ -n "$MOCK2_PID" ]; then
    kill "$MOCK2_PID" 2>/dev/null || true
    wait "$MOCK2_PID" 2>/dev/null || true
  fi
  if [ -n "$WEB_PID" ]; then
    kill "$WEB_PID" 2>/dev/null || true
    wait "$WEB_PID" 2>/dev/null || true
  fi
  if [ -n "$MOCK_PID" ]; then
    kill "$MOCK_PID" 2>/dev/null || true
    wait "$MOCK_PID" 2>/dev/null || true
  fi
  rm -rf "$TMP"
}
trap cleanup EXIT

wait_http() {
  url=$1
  logfile=$2
  for _ in $(seq 1 50); do
    if curl -fsS "$url" >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.2
  done
  echo "server did not start: $url" >&2
  cat "$logfile" >&2 || true
  return 1
}

mkdir -p "$TMP/work" "$TMP/sessions" "$TMP/work/.codedeep/skills"
cat > "$TMP/work/.codedeep/skills/review.md" <<'EOF'
---
name: review
description: Review a change for regressions
---
Read the diff and report bugs before style issues.
EOF
cat > "$TMP/editor.sh" <<EOF
#!/usr/bin/env bash
echo "\$@" >> "$TMP/opened.txt"
EOF
chmod +x "$TMP/editor.sh"
cat > "$TMP/tier3.json" <<EOF
{
  "hooks": {
    "pre_tool": [
      { "command": "echo pre >> $TMP/hooks.log", "timeout_sec": 5 }
    ],
    "post_tool": [
      { "command": "echo post >> $TMP/hooks.log", "timeout_sec": 5 }
    ]
  },
  "diagnostics": [
    { "pattern": "mock-output.txt", "command": "echo diag >> $TMP/hooks.log && echo mock-output.txt:1:1: error: missing header" }
  ],
  "editor": { "command": "bash", "args": ["$TMP/editor.sh", "{file}:{line}"] }
}
EOF
echo '{}' > "$TMP/noauth.json"

cd "$ROOT"
go build -o "$TMP/mockllm$EXT" ./cmd/mockllm
go build -o "$TMP/codedeep$EXT" ./cmd/codedeep

"$TMP/mockllm$EXT" -addr "127.0.0.1:$PORT" -scenario write -delay 1ms >"$TMP/mock.log" 2>&1 &
MOCK_PID=$!

ready=0
for _ in $(seq 1 50); do
  if curl -fsS "http://127.0.0.1:$PORT/health" >/dev/null 2>&1; then
    ready=1
    break
  fi
  sleep 0.2
done
if [ "$ready" -ne 1 ]; then
  echo "mock provider did not start:" >&2
  cat "$TMP/mock.log" >&2
  exit 1
fi

DEEPSEEK_API_KEY=test \
CODEAGENT_BASE_URL="http://127.0.0.1:$PORT" \
CODEAGENT_SESSIONS_DIR="$TMP/sessions" \
CODEAGENT_APPROVAL=auto \
"$TMP/codedeep$EXT" -config "$TMP/tier3.json" -cwd "$TMP/work" -p "write a demo file" -plain >"$TMP/out.txt"

grep -q "created mock-output.txt" "$TMP/out.txt"
grep -q "diff mock-output.txt" "$TMP/out.txt"
grep -q '+written by the mock model' "$TMP/out.txt"
test -f "$TMP/work/mock-output.txt"
grep -q "written by the mock model" "$TMP/work/mock-output.txt"

# Pre/post hooks and post-edit diagnostics ran against the configured files.
for marker in pre post diag; do
  if ! grep -q "^$marker$" "$TMP/hooks.log"; then
    echo "expected hook/diagnostic marker '$marker' in hooks.log" >&2
    cat "$TMP/hooks.log" >&2 || true
    exit 1
  fi
done
grep -q "mock-output.txt:1:1: error: missing header" "$TMP/out.txt"
# Structured diagnostics are rendered as file/line/column annotations
# (distinct from the raw "path:line:col: severity: message" command output).
grep -q "mock-output.txt:1:1 error missing header" "$TMP/out.txt"

# Doctor reports the provider reasoning controls.
"$TMP/codedeep$EXT" doctor -config "$TMP/tier3.json" >"$TMP/doctor.txt"
grep -q "reasoning      off" "$TMP/doctor.txt"
grep -q "prompt cache   on (automatic)" "$TMP/doctor.txt"

# The editor launcher resolves workspace paths and passes the line number.
"$TMP/codedeep$EXT" open -config "$TMP/tier3.json" -cwd "$TMP/work" mock-output.txt:7 >"$TMP/open.txt"
grep -q "opened" "$TMP/open.txt"
grep -q "mock-output.txt:7" "$TMP/opened.txt"

if ! compgen -G "$TMP/sessions/*.json" >/dev/null; then
  echo "no session file was persisted" >&2
  exit 1
fi
SESS_FILE=$(ls "$TMP"/sessions/*.json | head -n 1)
SID=$(basename "$SESS_FILE" .json)
grep -q '"checkpoints"' "$SESS_FILE"
grep -q 'mock-output.txt' "$SESS_FILE"
grep -q '"turn_stats"' "$SESS_FILE"
grep -q '"prompt_cache_hit_tokens":' "$SESS_FILE"

# Observability: the CLI aggregates saved sessions into a token/cost/cache
# report, and the JSON form is machine-readable.
CODEAGENT_SESSIONS_DIR="$TMP/sessions" \
  "$TMP/codedeep$EXT" stats -config "$TMP/tier3.json" -days 7 >"$TMP/stats.txt"
grep -q "Observability" "$TMP/stats.txt"
grep -q "cache" "$TMP/stats.txt"
grep -q "Tools" "$TMP/stats.txt"
CODEAGENT_SESSIONS_DIR="$TMP/sessions" \
  "$TMP/codedeep$EXT" stats -config "$TMP/tier3.json" -days 7 -json >"$TMP/stats.json"
grep -q '"summary"' "$TMP/stats.json"
grep -q '"cache_hit_rate":' "$TMP/stats.json"
grep -q '"tools": \[' "$TMP/stats.json"

# Deeper agent capabilities: the plan tool, durable memory, and the automatic
# verification gate.
mkdir -p "$TMP/deeper-work" "$TMP/deeper-sessions" "$TMP/memory-work" "$TMP/memory-sessions" \
  "$TMP/todo-work" "$TMP/todo-sessions" "$TMP/fail-work" "$TMP/fail-sessions" "$TMP/doctor-work"
cat > "$TMP/verify.json" <<EOF
{ "verify": { "command": "test -f mock-output.txt && echo verified-ok", "timeout_sec": 30 } }
EOF
DEEPSEEK_API_KEY=test \
CODEAGENT_BASE_URL="http://127.0.0.1:$PORT" \
CODEAGENT_SESSIONS_DIR="$TMP/deeper-sessions" \
CODEAGENT_APPROVAL=auto \
"$TMP/codedeep$EXT" -config "$TMP/verify.json" -cwd "$TMP/deeper-work" -p "write a demo file" -plain >"$TMP/verify-out.txt"
grep -q "verify ok" "$TMP/verify-out.txt"
grep -q "verified-ok" "$TMP/verify-out.txt"

# A persistent failure is retried twice and then fails the turn instead of
# reporting success.
cat > "$TMP/verify-fail.json" <<EOF
{ "verify": { "command": "echo gate-broken && exit 1" } }
EOF
if DEEPSEEK_API_KEY=test \
  CODEAGENT_BASE_URL="http://127.0.0.1:$PORT" \
  CODEAGENT_SESSIONS_DIR="$TMP/fail-sessions" \
  CODEAGENT_APPROVAL=auto \
  "$TMP/codedeep$EXT" -config "$TMP/verify-fail.json" -cwd "$TMP/fail-work" -p "write a demo file" -plain >"$TMP/verify-fail-out.txt" 2>&1; then
  echo "a failing verification should fail the turn" >&2
  cat "$TMP/verify-fail-out.txt" >&2
  exit 1
fi
grep -q "verify failed" "$TMP/verify-fail-out.txt"
grep -q "automatic verification failed 2 times" "$TMP/verify-fail-out.txt"

# The plan is persisted with the session and rendered by the plan event.
"$TMP/mockllm$EXT" -addr "127.0.0.1:$TODOPORT" -scenario todo -delay 1ms >"$TMP/mock-todo.log" 2>&1 &
MOCK4_PID=$!
wait_http "http://127.0.0.1:$TODOPORT/health" "$TMP/mock-todo.log"
DEEPSEEK_API_KEY=test \
CODEAGENT_BASE_URL="http://127.0.0.1:$TODOPORT" \
CODEAGENT_SESSIONS_DIR="$TMP/todo-sessions" \
CODEAGENT_APPROVAL=auto \
"$TMP/codedeep$EXT" -cwd "$TMP/todo-work" -p "plan the task" -plain >"$TMP/todo-out.txt"
grep -q "plan 1/3 complete" "$TMP/todo-out.txt"
grep -q "inspect the workspace" "$TMP/todo-out.txt"
TODO_FILE=$(ls "$TMP"/todo-sessions/*.json | head -n 1)
grep -q '"todos": \[' "$TODO_FILE"
grep -q '"text": "inspect the workspace"' "$TODO_FILE"
grep -q '"status": "completed"' "$TODO_FILE"
grep -q '"status": "in_progress"' "$TODO_FILE"
printf '/todos\n/stats 7\n/quit\n' | env DEEPSEEK_API_KEY=test \
  CODEAGENT_BASE_URL="http://127.0.0.1:$TODOPORT" \
  CODEAGENT_SESSIONS_DIR="$TMP/todo-sessions" \
  "$TMP/codedeep$EXT" -cwd "$TMP/todo-work" -resume last -plain >"$TMP/todo-repl.txt"
grep -q "Plan" "$TMP/todo-repl.txt"
grep -q "\[~\] write the mock build command" "$TMP/todo-repl.txt"
grep -q "Observability" "$TMP/todo-repl.txt"
grep -q "cache" "$TMP/todo-repl.txt"

# Durable memory survives the session: a note is written under the workspace,
# read back through recall, and listed by the REPL and doctor.
"$TMP/mockllm$EXT" -addr "127.0.0.1:$MEMPORT" -scenario memory -delay 1ms >"$TMP/mock-memory.log" 2>&1 &
MOCK5_PID=$!
wait_http "http://127.0.0.1:$MEMPORT/health" "$TMP/mock-memory.log"
DEEPSEEK_API_KEY=test \
CODEAGENT_BASE_URL="http://127.0.0.1:$MEMPORT" \
CODEAGENT_SESSIONS_DIR="$TMP/memory-sessions" \
CODEAGENT_APPROVAL=auto \
"$TMP/codedeep$EXT" -cwd "$TMP/memory-work" -p "remember the build command" -plain >"$TMP/memory-out.txt"
grep -q "Run go build ./..." "$TMP/memory-out.txt"
test -f "$TMP/memory-work/.codedeep/memory/mock-build-command.md"
grep -q "description: how the mock project builds" "$TMP/memory-work/.codedeep/memory/mock-build-command.md"
printf '/memory mock\n/quit\n' | env DEEPSEEK_API_KEY=test \
  CODEAGENT_BASE_URL="http://127.0.0.1:$MEMPORT" \
  CODEAGENT_SESSIONS_DIR="$TMP/memory-sessions" \
  "$TMP/codedeep$EXT" -cwd "$TMP/memory-work" -plain >"$TMP/memory-repl.txt"
grep -q "mock-build-command" "$TMP/memory-repl.txt"
grep -q "Memory matching" "$TMP/memory-repl.txt"

# Doctor reports the verification command and the memory note count.
(cd "$TMP/doctor-work" && "$TMP/codedeep$EXT" doctor -config "$TMP/verify.json") >"$TMP/doctor-deeper.txt"
grep -q "verify         test -f mock-output.txt && echo verified-ok" "$TMP/doctor-deeper.txt"
(cd "$TMP/memory-work" && "$TMP/codedeep$EXT" doctor -config "$TMP/noauth.json") >"$TMP/doctor-memory.txt"
grep -q "verify         off" "$TMP/doctor-memory.txt"
grep -q "memory         1 notes" "$TMP/doctor-memory.txt"

# Plan mode must refuse the write and leave the workspace untouched.
mkdir -p "$TMP/plan-work" "$TMP/plan-sessions"
DEEPSEEK_API_KEY=test \
CODEAGENT_BASE_URL="http://127.0.0.1:$PORT" \
CODEAGENT_SESSIONS_DIR="$TMP/plan-sessions" \
"$TMP/codedeep$EXT" -cwd "$TMP/plan-work" -plan -p "write a demo file" -plain >"$TMP/plan.txt"
grep -q "blocked by plan mode" "$TMP/plan.txt"
test ! -f "$TMP/plan-work/mock-output.txt"

# The web API exposes bootstrap state, the plan toggle, and undo.
DEEPSEEK_API_KEY=test \
CODEAGENT_BASE_URL="http://127.0.0.1:$PORT" \
CODEAGENT_SESSIONS_DIR="$TMP/sessions" \
CODEAGENT_APPROVAL=auto \
"$TMP/codedeep$EXT" serve -config "$TMP/tier3.json" -addr "127.0.0.1:$WEBPORT" -no-open -cwd "$TMP/work" >"$TMP/web.log" 2>&1 &
WEB_PID=$!

web_ready=0
for _ in $(seq 1 50); do
  if curl -fsS "http://127.0.0.1:$WEBPORT/api/health" >/dev/null 2>&1; then
    web_ready=1
    break
  fi
  sleep 0.2
done
if [ "$web_ready" -ne 1 ]; then
  echo "web server did not start:" >&2
  cat "$TMP/web.log" >&2
  exit 1
fi

curl -fsS "http://127.0.0.1:$WEBPORT/api/bootstrap" >"$TMP/boot.json"
grep -q '"approval":"auto"' "$TMP/boot.json"
grep -q '"plan":false' "$TMP/boot.json"
grep -q '"provider":"openai"' "$TMP/boot.json"
grep -q '"reasoning_effort":""' "$TMP/boot.json"
grep -q '"reasoning_budget":0' "$TMP/boot.json"
grep -q '"prompt_cache":true' "$TMP/boot.json"
grep -q '"name":"review"' "$TMP/boot.json"

# Observability dashboard data: aggregated daily activity, cache metrics, and
# tool breakdowns. Bad ranges are rejected.
curl -fsS "http://127.0.0.1:$WEBPORT/api/analytics?days=7&scope=workspace" >"$TMP/analytics.json"
grep -q '"summary"' "$TMP/analytics.json"
grep -q '"cache_hit_rate":' "$TMP/analytics.json"
grep -q '"tools":\[' "$TMP/analytics.json"
grep -q '"daily":\[' "$TMP/analytics.json"
grep -q "\"id\":\"$SID\"" "$TMP/analytics.json"
ANALYTICS_BAD=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$WEBPORT/api/analytics?days=soon")
if [ "$ANALYTICS_BAD" != "400" ]; then
  echo "invalid analytics days should return 400, got $ANALYTICS_BAD" >&2
  exit 1
fi

# Skills are discoverable through the API and include their body on demand.
curl -fsS "http://127.0.0.1:$WEBPORT/api/skills" >"$TMP/skills.json"
grep -q '"name":"review"' "$TMP/skills.json"
curl -fsS "http://127.0.0.1:$WEBPORT/api/skills/review" >"$TMP/skill.json"
grep -q 'report bugs before style issues' "$TMP/skill.json"
SKILL_STATUS=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$WEBPORT/api/skills/nope")
if [ "$SKILL_STATUS" != "404" ]; then
  echo "unknown skill should return 404, got $SKILL_STATUS" >&2
  exit 1
fi

# Session search, export, and workspace file browsing.
curl -fsS "http://127.0.0.1:$WEBPORT/api/sessions?q=mock-output" >"$TMP/search.json"
grep -q "\"id\":\"$SID\"" "$TMP/search.json"
curl -fsS "http://127.0.0.1:$WEBPORT/api/sessions/$SID/export?format=md" >"$TMP/export.md"
grep -q '^## User' "$TMP/export.md"
grep -q 'mock-output.txt' "$TMP/export.md"
curl -fsS "http://127.0.0.1:$WEBPORT/api/sessions/$SID/export?format=json" >"$TMP/export.json"
grep -q '"messages"' "$TMP/export.json"
curl -fsS "http://127.0.0.1:$WEBPORT/api/files" >"$TMP/files.json"
grep -q 'mock-output.txt' "$TMP/files.json"
curl -fsS "http://127.0.0.1:$WEBPORT/api/files/search?q=mock-output" >"$TMP/files-search.json"
grep -q '"path":"mock-output.txt"' "$TMP/files-search.json"

# The web editor action resolves the path and launches the configured editor.
rm -f "$TMP/opened.txt"
curl -fsS -X POST -H 'Content-Type: application/json' \
  -d '{"path":"mock-output.txt","line":3}' "http://127.0.0.1:$WEBPORT/api/open" >"$TMP/web-open.json"
grep -q '"line":3' "$TMP/web-open.json"
grep -q "mock-output.txt:3" "$TMP/opened.txt"
OUTSIDE_STATUS=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' \
  -d '{"path":"../escape.txt"}' "http://127.0.0.1:$WEBPORT/api/open")
if [ "$OUTSIDE_STATUS" != "400" ]; then
  echo "opening outside the workspace should return 400, got $OUTSIDE_STATUS" >&2
  exit 1
fi

# Image attachments are validated and stored in the session.
BAD_STATUS=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' \
  -d '{"prompt":"x","images":[{"mime":"image/svg+xml","data":"aGk="}]}' \
  "http://127.0.0.1:$WEBPORT/api/chat")
if [ "$BAD_STATUS" != "400" ]; then
  echo "unsupported image type should be rejected with 400, got $BAD_STATUS" >&2
  exit 1
fi
PNG="iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNkYAAAAAYAAjCB0C8AAAAASUVORK5CYII="
curl -fsS -X POST -H 'Content-Type: application/json' \
  -d "{\"prompt\":\"describe this image\",\"images\":[{\"mime\":\"image/png\",\"data\":\"$PNG\"}]}" \
  "http://127.0.0.1:$WEBPORT/api/chat" >"$TMP/image-chat.ndjson"
grep -q '"type":"user"' "$TMP/image-chat.ndjson"
grep -q '"images":\[' "$TMP/image-chat.ndjson"
IMG_SID=$(grep -o '"session_id":"[^"]*"' "$TMP/image-chat.ndjson" | head -n 1 | sed 's/.*:"//; s/"$//')
if [ -z "$IMG_SID" ] || [ ! -f "$TMP/sessions/$IMG_SID.json" ]; then
  echo "image chat session was not persisted" >&2
  exit 1
fi
grep -q '"mime": "image/png"' "$TMP/sessions/$IMG_SID.json"

curl -fsS -X POST -H 'Content-Type: application/json' \
  -d '{"enabled":true}' "http://127.0.0.1:$WEBPORT/api/plan" >"$TMP/plan-on.json"
grep -q '"plan":true' "$TMP/plan-on.json"

curl -fsS -X POST -H 'Content-Type: application/json' \
  -d '{"enabled":false}' "http://127.0.0.1:$WEBPORT/api/plan" >"$TMP/plan-off.json"
grep -q '"plan":false' "$TMP/plan-off.json"

# Multi-checkpoint review: list a session's checkpoints, inspect a diff, and
# reject an out-of-range number.
curl -fsS "http://127.0.0.1:$WEBPORT/api/sessions/$SID/checkpoints" >"$TMP/cps.json"
grep -q '"number":1' "$TMP/cps.json"
grep -q '"turn":1' "$TMP/cps.json"
grep -q '"added":1' "$TMP/cps.json"
grep -q '"files":1' "$TMP/cps.json"
curl -fsS "http://127.0.0.1:$WEBPORT/api/sessions/$SID/checkpoints/1" >"$TMP/cp1.json"
grep -q '+written by the mock model' "$TMP/cp1.json"
grep -q '"removed":0' "$TMP/cp1.json"
CP_STATUS=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$WEBPORT/api/sessions/$SID/checkpoints/9")
if [ "$CP_STATUS" != "404" ]; then
  echo "unknown checkpoint should return 404, got $CP_STATUS" >&2
  exit 1
fi

curl -fsS -X POST -H 'Content-Type: application/json' \
  -d "{\"session_id\":\"$SID\"}" "http://127.0.0.1:$WEBPORT/api/undo" >"$TMP/undo.json"
grep -q '"reverted":\["mock-output.txt"\]' "$TMP/undo.json"
test ! -f "$TMP/work/mock-output.txt"

# Two mutating turns on one session create two checkpoints; rolling back
# through the first restores the pre-turn state (file removed).
curl -fsS -X POST -H 'Content-Type: application/json' \
  -d '{"prompt":"write a demo file"}' "http://127.0.0.1:$WEBPORT/api/chat" >"$TMP/cp-chat1.ndjson"
CP_SID=$(grep -o '"session_id":"[^"]*"' "$TMP/cp-chat1.ndjson" | head -n 1 | sed 's/.*:"//; s/"$//')
if [ -z "$CP_SID" ] || [ ! -f "$TMP/sessions/$CP_SID.json" ]; then
  echo "checkpoint chat session was not persisted" >&2
  exit 1
fi
test -f "$TMP/work/mock-output.txt"
curl -fsS -X POST -H 'Content-Type: application/json' \
  -d "{\"session_id\":\"$CP_SID\",\"prompt\":\"write it again\"}" \
  "http://127.0.0.1:$WEBPORT/api/chat" >"$TMP/cp-chat2.ndjson"
curl -fsS "http://127.0.0.1:$WEBPORT/api/sessions/$CP_SID/checkpoints" >"$TMP/cps2.json"
grep -q '"number":2' "$TMP/cps2.json"
curl -fsS -X POST -H 'Content-Type: application/json' \
  -d "{\"session_id\":\"$CP_SID\",\"checkpoint\":1}" "http://127.0.0.1:$WEBPORT/api/undo" >"$TMP/undo-cp.json"
grep -q '"reverted":\["mock-output.txt"\]' "$TMP/undo-cp.json"
test ! -f "$TMP/work/mock-output.txt"
curl -fsS "http://127.0.0.1:$WEBPORT/api/sessions/$CP_SID/checkpoints" >"$TMP/cps3.json"
if grep -q '"number":' "$TMP/cps3.json"; then
  echo "rolled-back checkpoints should be dropped" >&2
  exit 1
fi

# Provider retries: the flaky mock rate limits twice before answering, and the
# web stream surfaces retry events with the Retry-After delay.
mkdir -p "$TMP/flaky-sessions"
"$TMP/mockllm$EXT" -addr "127.0.0.1:$FLAKYPORT" -scenario flaky -delay 1ms >"$TMP/mock-flaky.log" 2>&1 &
MOCK2_PID=$!
wait_http "http://127.0.0.1:$FLAKYPORT/health" "$TMP/mock-flaky.log"

DEEPSEEK_API_KEY=test \
CODEAGENT_BASE_URL="http://127.0.0.1:$FLAKYPORT" \
CODEAGENT_SESSIONS_DIR="$TMP/flaky-sessions" \
CODEAGENT_APPROVAL=auto \
"$TMP/codedeep$EXT" serve -config "$TMP/noauth.json" -addr "127.0.0.1:$FLAKYWEBPORT" -no-open -cwd "$TMP/work" >"$TMP/web-flaky.log" 2>&1 &
WEB2_PID=$!
wait_http "http://127.0.0.1:$FLAKYWEBPORT/api/health" "$TMP/web-flaky.log"

curl -fsS -X POST -H 'Content-Type: application/json' \
  -d '{"prompt":"retry please"}' "http://127.0.0.1:$FLAKYWEBPORT/api/chat" >"$TMP/flaky-chat.ndjson"
grep -q '"type":"retry"' "$TMP/flaky-chat.ndjson"
grep -q '"status":429' "$TMP/flaky-chat.ndjson"
grep -q '"attempt":1' "$TMP/flaky-chat.ndjson"
grep -q '"type":"done"' "$TMP/flaky-chat.ndjson"
FLAKY_SID=$(grep -o '"session_id":"[^"]*"' "$TMP/flaky-chat.ndjson" | head -n 1 | sed 's/.*:"//; s/"$//')
if [ -z "$FLAKY_SID" ] || [ ! -f "$TMP/flaky-sessions/$FLAKY_SID.json" ]; then
  echo "retry chat session was not persisted" >&2
  exit 1
fi
grep -q 'Recovered after the rate limit' "$TMP/flaky-sessions/$FLAKY_SID.json"
if [ "$(grep -c 'flaky: 429' "$TMP/mock-flaky.log")" != "2" ]; then
  echo "expected exactly two rate-limited attempts:" >&2
  cat "$TMP/mock-flaky.log" >&2
  exit 1
fi

# Resumable streams: abandon the HTTP response mid-turn, reattach from the
# beginning, and verify the server-owned run finishes and persists.
mkdir -p "$TMP/slow-sessions"
"$TMP/mockllm$EXT" -addr "127.0.0.1:$SLOWPORT" -scenario text -delay 300ms >"$TMP/mock-slow.log" 2>&1 &
MOCK3_PID=$!
wait_http "http://127.0.0.1:$SLOWPORT/health" "$TMP/mock-slow.log"

DEEPSEEK_API_KEY=test \
CODEAGENT_BASE_URL="http://127.0.0.1:$SLOWPORT" \
CODEAGENT_SESSIONS_DIR="$TMP/slow-sessions" \
CODEAGENT_APPROVAL=auto \
"$TMP/codedeep$EXT" serve -config "$TMP/noauth.json" -addr "127.0.0.1:$SLOWWEBPORT" -no-open -cwd "$TMP/work" >"$TMP/web-slow.log" 2>&1 &
WEB3_PID=$!
wait_http "http://127.0.0.1:$SLOWWEBPORT/api/health" "$TMP/web-slow.log"

curl -s --max-time 1 -D "$TMP/resume-headers.txt" -o "$TMP/resume-partial.ndjson" \
  -X POST -H 'Content-Type: application/json' -d '{"prompt":"keep going"}' \
  "http://127.0.0.1:$SLOWWEBPORT/api/chat" || true
RESUME_SID=$(grep -i '^X-Session-Id:' "$TMP/resume-headers.txt" | tr -d '\r' | awk '{print $2}')
if [ -z "$RESUME_SID" ]; then
  echo "chat response is missing X-Session-Id:" >&2
  cat "$TMP/resume-headers.txt" >&2 || true
  exit 1
fi

resume_running=0
for _ in $(seq 1 50); do
  if curl -fsS "http://127.0.0.1:$SLOWWEBPORT/api/sessions/$RESUME_SID" | grep -q '"running":true'; then
    resume_running=1
    break
  fi
  sleep 0.1
done
if [ "$resume_running" -ne 1 ]; then
  echo "run did not survive the dropped connection" >&2
  exit 1
fi
curl -fsS "http://127.0.0.1:$SLOWWEBPORT/api/bootstrap" >"$TMP/resume-boot.json"
grep -q "\"$RESUME_SID\"" "$TMP/resume-boot.json"

curl -fsS -N "http://127.0.0.1:$SLOWWEBPORT/api/sessions/$RESUME_SID/stream?after=0" >"$TMP/resume-tail.ndjson"
grep -q '"type":"done"' "$TMP/resume-tail.ndjson"
grep -q '"type":"text"' "$TMP/resume-tail.ndjson"
grep -q 'The mock server streamed this text token by token' "$TMP/slow-sessions/$RESUME_SID.json"
curl -fsS "http://127.0.0.1:$SLOWWEBPORT/api/sessions/$RESUME_SID" >"$TMP/resume-session.json"
grep -q '"running":false' "$TMP/resume-session.json"

# Stop: the server cancels a running turn and reports it through the stream.
curl -fsS -X POST "http://127.0.0.1:$SLOWWEBPORT/api/sessions" >"$TMP/stop-session.json"
STOP_SID=$(grep -o '"id":"[^"]*"' "$TMP/stop-session.json" | head -n 1 | sed 's/.*:"//; s/"$//')
if [ -z "$STOP_SID" ]; then
  echo "could not create a session for the stop test" >&2
  exit 1
fi
curl -s -N -X POST -H 'Content-Type: application/json' \
  -d "{\"session_id\":\"$STOP_SID\",\"prompt\":\"stop me\"}" \
  "http://127.0.0.1:$SLOWWEBPORT/api/chat" >"$TMP/stop-chat.ndjson" &
STOP_CHAT_PID=$!
stop_running=0
for _ in $(seq 1 50); do
  if curl -fsS "http://127.0.0.1:$SLOWWEBPORT/api/sessions/$STOP_SID" | grep -q '"running":true'; then
    stop_running=1
    break
  fi
  sleep 0.1
done
if [ "$stop_running" -ne 1 ]; then
  echo "stop test run did not start" >&2
  exit 1
fi
curl -fsS -X POST "http://127.0.0.1:$SLOWWEBPORT/api/sessions/$STOP_SID/stop" >"$TMP/stop.json"
grep -q '"stopped":true' "$TMP/stop.json"
wait "$STOP_CHAT_PID"
grep -q '"message":"stopped"' "$TMP/stop-chat.ndjson"
curl -fsS "http://127.0.0.1:$SLOWWEBPORT/api/sessions/$STOP_SID" >"$TMP/stop-session-after.json"
grep -q '"running":false' "$TMP/stop-session-after.json"
STOP_UNKNOWN=$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  "http://127.0.0.1:$SLOWWEBPORT/api/sessions/$STOP_SID/stop")
if [ "$STOP_UNKNOWN" != "409" ]; then
  echo "stopping an idle session should return 409, got $STOP_UNKNOWN" >&2
  exit 1
fi

AUTHPORT=${AUTHPORT:-18082}
CODEAGENT_WEB_TOKEN=secret \
DEEPSEEK_API_KEY=test \
CODEAGENT_BASE_URL="http://127.0.0.1:$PORT" \
CODEAGENT_SESSIONS_DIR="$TMP/sessions" \
"$TMP/codedeep$EXT" serve -config "$TMP/noauth.json" -addr "127.0.0.1:$AUTHPORT" -no-open -cwd "$TMP/work" >"$TMP/auth-web.log" 2>&1 &
AUTH_PID=$!

auth_ready=0
for _ in $(seq 1 50); do
  if curl -s -o /dev/null -w '%{http_code}' -H 'Authorization: Bearer secret' \
    "http://127.0.0.1:$AUTHPORT/api/health" | grep -q '^200$'; then
    auth_ready=1
    break
  fi
  sleep 0.2
done
if [ "$auth_ready" -ne 1 ]; then
  echo "authenticated web server did not start:" >&2
  cat "$TMP/auth-web.log" >&2
  exit 1
fi

NOAUTH_STATUS=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$AUTHPORT/api/health")
if [ "$NOAUTH_STATUS" != "401" ]; then
  echo "api without a token should return 401, got $NOAUTH_STATUS" >&2
  exit 1
fi
curl -fsS -H 'Authorization: Bearer secret' "http://127.0.0.1:$AUTHPORT/api/bootstrap" >"$TMP/auth-boot.json"
grep -q '"workspace"' "$TMP/auth-boot.json"
curl -fsS "http://127.0.0.1:$AUTHPORT/login" >"$TMP/login.html"
grep -q 'name="token"' "$TMP/login.html"
LOGIN_STATUS=$(curl -s -o /dev/null -w '%{http_code}' -c "$TMP/cookies.txt" \
  -H 'Content-Type: application/x-www-form-urlencoded' -d 'token=secret' \
  "http://127.0.0.1:$AUTHPORT/login")
if [ "$LOGIN_STATUS" != "303" ]; then
  echo "login should redirect with 303, got $LOGIN_STATUS" >&2
  exit 1
fi
curl -fsS -b "$TMP/cookies.txt" "http://127.0.0.1:$AUTHPORT/api/sessions" >"$TMP/auth-sessions.json"
QUERY_STATUS=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$AUTHPORT/?token=secret")
if [ "$QUERY_STATUS" != "303" ]; then
  echo "query-token page load should redirect with 303, got $QUERY_STATUS" >&2
  exit 1
fi

# A non-loopback bind without a token must be refused before listening.
if CODEAGENT_WEB_TOKEN= "$TMP/codedeep$EXT" serve -config "$TMP/noauth.json" \
  -addr "0.0.0.0:$((AUTHPORT + 1))" -no-open -cwd "$TMP/work" >"$TMP/refuse.log" 2>&1; then
  echo "serve should refuse an unauthenticated non-loopback bind" >&2
  exit 1
fi
grep -q "refusing to listen" "$TMP/refuse.log"

echo "e2e smoke passed"
