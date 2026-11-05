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
MOCK_PID=""
WEB_PID=""
AUTH_PID=""
cleanup() {
  if [ -n "$AUTH_PID" ]; then
    kill "$AUTH_PID" 2>/dev/null || true
    wait "$AUTH_PID" 2>/dev/null || true
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
grep -q '"name":"review"' "$TMP/boot.json"

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

curl -fsS -X POST -H 'Content-Type: application/json' \
  -d "{\"session_id\":\"$SID\"}" "http://127.0.0.1:$WEBPORT/api/undo" >"$TMP/undo.json"
grep -q '"reverted":\["mock-output.txt"\]' "$TMP/undo.json"
test ! -f "$TMP/work/mock-output.txt"

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
