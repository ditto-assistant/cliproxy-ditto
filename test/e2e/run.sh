#!/usr/bin/env bash
# End-to-end: the plugin loaded into a real CLIProxyAPI release, with a fake
# subscription credential at default priority and the Ditto credential at its
# default -100, both serving claude-opus-5-5.
#
#   1. healthy subscription  -> Ditto receives no model request (control)
#   2. exhausted subscription -> the same request is answered by Ditto
#   3. streaming, exhausted  -> SSE from Ditto reaches the client intact
#
# Usage: test/e2e/run.sh [cli-proxy-api binary]  (downloads the release if omitted)
set -euo pipefail

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
  url="https://github.com/router-for-me/CLIProxyAPI/releases/download/v${CPA_VERSION}/CLIProxyAPI_${CPA_VERSION}_${GOOS}_${GOARCH}.tar.gz"
  curl -fsSL "$url" | tar -xz -C "$WORK"
  BIN="$WORK/cli-proxy-api"
fi

mkdir -p "$WORK/plugins/$GOOS/$GOARCH" "$WORK/auths"
(cd "$ROOT" && CGO_ENABLED=1 go build -buildmode=c-shared -o "$WORK/plugins/$GOOS/$GOARCH/ditto.$EXT" .)

port() { python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])'; }
SUB_PORT=$(port); DITTO_PORT=$(port); CPA_PORT=$(port)
LOG="$WORK/requests.jsonl"

printf 'ditto_inf_e2e\n' > "$WORK/ditto.key"
cat > "$WORK/auths/ditto-dev.json" <<EOF
{"type":"ditto","endpoint":"ditto-dev","api_key_file":"$WORK/ditto.key","base_url":"http://127.0.0.1:$DITTO_PORT"}
EOF

cat > "$WORK/config.yaml" <<EOF
config-version: 8
server:
  host: 127.0.0.1
  port: $CPA_PORT
access:
  api-keys: ["e2e-access-key"]
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

start() {
  : > "$LOG"
  SUB_MODE="$1" python3 "$ROOT/test/e2e/fakes.py" "$SUB_PORT" "$DITTO_PORT" "$LOG" & pids+=($!)
  (cd "$WORK" && exec "$BIN" -config "$WORK/config.yaml" > "$WORK/cpa-$1.log" 2>&1) & pids+=($!)
  for _ in $(seq 1 100); do
    if curl -fsS -H 'Authorization: Bearer e2e-access-key' "http://127.0.0.1:$CPA_PORT/v1/models" 2>/dev/null | grep -q '"ditto-dev"'; then
      return 0
    fi
    sleep 0.2
  done
  echo "CLIProxy never listed ditto-dev; log:" >&2; tail -40 "$WORK/cpa-$1.log" >&2; exit 1
}
stop() { cleanup; pids=(); sleep 0.5; }

ask() { # $1 = stream true|false
  curl -sS "http://127.0.0.1:$CPA_PORT/v1/messages" \
    -H 'x-api-key: e2e-access-key' -H 'anthropic-version: 2023-06-01' -H 'content-type: application/json' \
    -d "{\"model\":\"claude-opus-5-5\",\"max_tokens\":16,\"stream\":$1,\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}"
}
count() { python3 -c "import json,sys; print(sum(1 for l in open(sys.argv[1]) if (e:=json.loads(l))['who']==sys.argv[2] and e['path'].startswith('/v1/messages')))" "$LOG" "$1"; }
fail() { echo "FAIL: $*" >&2; tail -n 40 "$WORK"/cpa-*.log >&2; exit 1; }

echo "== models"
start ok
models=$(curl -fsS -H 'Authorization: Bearer e2e-access-key' "http://127.0.0.1:$CPA_PORT/v1/models")
for id in claude-opus-5-5 ditto-dev; do
  grep -q "\"$id\"" <<<"$models" || fail "model $id not listed"
done
echo "listed: claude-opus-5-5, ditto-dev"

echo "== 1. healthy subscription (control)"
out=$(ask false)
grep -q from-subscription <<<"$out" || fail "healthy subscription did not answer: $out"
[ "$(count ditto)" = 0 ] || fail "Ditto received a model request while the subscription was healthy"
echo "subscription answered; ditto model requests=$(count ditto)"
stop

echo "== 2. exhausted subscription"
start exhausted
out=$(ask false)
grep -q from-ditto <<<"$out" || fail "Ditto did not answer after the subscription ran out: $out"
[ "$(count subscription)" -ge 1 ] || fail "subscription was never tried first"
python3 - "$LOG" <<'PY' || fail "Ditto request carried the wrong credentials"
import json, sys
for e in map(json.loads, open(sys.argv[1])):
    if e["who"] == "ditto" and e["path"].startswith("/v1/messages"):
        assert e["authorization"] == "Bearer ditto_inf_e2e", e
        assert "e2e-access-key" not in (e["authorization"] + e["x_api_key"]), e
        assert e["via"].startswith("cliproxy-ditto/"), e
        assert json.loads(e["body"])["model"] == "claude-opus-5-5", e
PY
echo "ditto answered; subscription tried=$(count subscription) ditto=$(count ditto)"

echo "== 3. streaming, exhausted"
out=$(ask true)
for ev in message_start content_block_delta message_stop from-ditto; do
  grep -q "$ev" <<<"$out" || fail "stream missing $ev: $out"
done
echo "stream intact: $(grep -c '^event:' <<<"$out") events"
stop

echo "PASS"
