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
MOCK_PID=""
cleanup() {
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
test -f "$TMP/work/mock-output.txt"
grep -q "written by the mock model" "$TMP/work/mock-output.txt"

if ! compgen -G "$TMP/sessions/*.json" >/dev/null; then
  echo "no session file was persisted" >&2
  exit 1
fi

echo "e2e smoke passed"
