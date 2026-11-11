#!/usr/bin/env bash
# Live check against the real Ditto Router: the plugin loaded into a real
# CLIProxyAPI release, a fake subscription for claude-opus-5-5 that is out of
# quota, and a Ditto credential for a real endpoint. Spends a few cents.
#
#   1. /v1/models lists the mirrored id and the endpoint slug
#   2. Messages, exhausted subscription -> answered by Ditto
#   3. Messages streaming               -> SSE from Ditto intact
#   4. Responses with the endpoint slug  -> answered by Ditto
#   5. Chat Completions with the slug    -> answered by Ditto
#
# Usage: DITTO_KEY_FILE=/abs/key DITTO_ENDPOINT=<slug> test/e2e/live.sh [cli-proxy-api binary]
# Optional: DITTO_BASE_URL (defaults to the plugin's).
set -euo pipefail

: "${DITTO_KEY_FILE:?set DITTO_KEY_FILE to an endpoint key file}"
: "${DITTO_ENDPOINT:?set DITTO_ENDPOINT to the endpoint slug}"
CPA_VERSION="${CPA_VERSION:-8.0.53}"
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
WORK="$(mktemp -d)"
GOOS="$(go env GOOS)"; GOARCH="$(go env GOARCH)"
EXT=so; [ "$GOOS" = darwin ] && EXT=dylib

pids=()
cleanup() { for p in "${pids[@]:-}"; do [ -n "$p" ] && kill "$p" 2>/dev/null || true; done; }
trap cleanup EXIT

BIN="${1:-}"
if [ -z "$BIN" ]; then
  asset_arch="$GOARCH"; [ "$GOARCH" = arm64 ] && asset_arch=aarch64
  url="https://github.com/router-for-me/CLIProxyAPI/releases/download/v${CPA_VERSION}/CLIProxyAPI_${CPA_VERSION}_${GOOS}_${asset_arch}.tar.gz"
  curl -fsSL "$url" | tar -xz -C "$WORK"
  BIN="$WORK/cli-proxy-api"
fi

mkdir -p "$WORK/plugins/$GOOS/$GOARCH" "$WORK/auths"
(cd "$ROOT" && CGO_ENABLED=1 go build -buildmode=c-shared -o "$WORK/plugins/$GOOS/$GOARCH/ditto.$EXT" .)

port() { python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])'; }
SUB_PORT=$(port); UNUSED_PORT=$(port); CPA_PORT=$(port)
LOG="$WORK/requests.jsonl"

python3 -I - "$WORK/auths/ditto-$DITTO_ENDPOINT.json" "$DITTO_ENDPOINT" "$DITTO_KEY_FILE" "${DITTO_BASE_URL:-}" <<'PY'
import json, sys
path, endpoint, key_file, base = sys.argv[1:]
doc = {"type": "ditto", "endpoint": endpoint, "api_key_file": key_file}
if base:
    doc["base_url"] = base
json.dump(doc, open(path, "w"))
PY

cat > "$WORK/config.yaml" <<EOF
config-version: 8
server:
  host: 127.0.0.1
  port: $CPA_PORT
access:
  api-keys: ["live-access-key"]
oauth:
  auth-dir: $WORK/auths
api-keys:
  claude:
    - name: subscription
      base-url: http://127.0.0.1:$SUB_PORT
      models:
        - name: claude-opus-5-5
      keys:
        - api-key: sk-ant-fake-subscription
plugins:
  enabled: true
  dir: $WORK/plugins
  configs:
    ditto:
      enabled: true
EOF

SUB_MODE=exhausted python3 "$ROOT/test/e2e/fakes.py" "$SUB_PORT" "$UNUSED_PORT" "$LOG" & pids+=($!)
(cd "$WORK" && exec "$BIN" -config "$WORK/config.yaml" > "$WORK/cpa.log" 2>&1) & pids+=($!)

fail() { echo "FAIL: $*" >&2; tail -n 40 "$WORK/cpa.log" >&2; exit 1; }
api() { # $1 = path, $2 = body
  curl -sS --max-time 180 "http://127.0.0.1:$CPA_PORT$1" \
    -H 'Authorization: Bearer live-access-key' -H 'anthropic-version: 2023-06-01' -H 'content-type: application/json' \
    -d "$2"
}

echo "== 1. models"
models=""
for _ in $(seq 1 150); do
  models=$(curl -fsS -H 'Authorization: Bearer live-access-key' "http://127.0.0.1:$CPA_PORT/v1/models" 2>/dev/null || true)
  grep -q "\"$DITTO_ENDPOINT\"" <<<"$models" && break
  sleep 0.2
done
for id in claude-opus-5-5 "$DITTO_ENDPOINT"; do
  grep -q "\"$id\"" <<<"$models" || fail "model $id not listed"
done
echo "listed: claude-opus-5-5, $DITTO_ENDPOINT"

echo "== 2. Messages, exhausted subscription"
out=$(api /v1/messages '{"model":"claude-opus-5-5","max_tokens":32,"messages":[{"role":"user","content":"Reply with the single word: pong"}]}')
python3 -I -c 'import json,sys; d=json.loads(sys.argv[1]); assert d.get("type")=="message", d; t="".join(b.get("text","") for b in d["content"]); assert "from-subscription" not in t; print("ditto answered:", repr(t[:60]), "model:", d.get("model"), "usage:", d.get("usage"))' "$out" \
  || fail "Messages: $out"
tried=$(python3 -I -c 'import json,sys; print(sum(1 for l in open(sys.argv[1]) if json.loads(l)["who"]=="subscription"))' "$LOG")
[ "$tried" -ge 1 ] || fail "subscription was never tried first"
echo "subscription tried=$tried"

echo "== 3. Messages streaming"
out=$(api /v1/messages '{"model":"claude-opus-5-5","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"Reply with the single word: pong"}]}')
for ev in message_start content_block_delta message_stop; do
  grep -q "$ev" <<<"$out" || fail "stream missing $ev: $out"
done
grep -q '"type":"error"' <<<"$out" && fail "stream carried an error: $out"
echo "stream intact: $(grep -c '^event:' <<<"$out") events"

echo "== 4. Responses via the endpoint slug"
out=$(api /v1/responses "{\"model\":\"$DITTO_ENDPOINT\",\"max_output_tokens\":64,\"input\":\"Reply with the single word: pong\"}")
python3 -I -c 'import json,sys; d=json.loads(sys.argv[1]); assert d.get("object")=="response" and d.get("status")=="completed", d; print("status:", d["status"], "model:", d.get("model"))' "$out" \
  || fail "Responses: $out"

echo "== 5. Chat Completions via the endpoint slug"
out=$(api /v1/chat/completions "{\"model\":\"$DITTO_ENDPOINT\",\"max_tokens\":64,\"messages\":[{\"role\":\"user\",\"content\":\"Reply with the single word: pong\"}]}")
python3 -I -c 'import json,sys; d=json.loads(sys.argv[1]); c=d["choices"][0]["message"]; print("answered:", repr((c.get("content") or "")[:60]), "model:", d.get("model"))' "$out" \
  || fail "Chat Completions: $out"

grep -qi 'panic\|sigsegv' "$WORK/cpa.log" && fail "CLIProxy log shows a crash"
echo "PASS"
