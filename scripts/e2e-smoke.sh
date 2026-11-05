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
cleanup() {
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

mkdir -p "$TMP/work" "$TMP/sessions"

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
"$TMP/codedeep$EXT" -cwd "$TMP/work" -p "write a demo file" -plain >"$TMP/out.txt"

grep -q "created mock-output.txt" "$TMP/out.txt"
grep -q "diff mock-output.txt" "$TMP/out.txt"
grep -q '+written by the mock model' "$TMP/out.txt"
test -f "$TMP/work/mock-output.txt"
grep -q "written by the mock model" "$TMP/work/mock-output.txt"

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
"$TMP/codedeep$EXT" serve -addr "127.0.0.1:$WEBPORT" -no-open -cwd "$TMP/work" >"$TMP/web.log" 2>&1 &
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

echo "e2e smoke passed"
