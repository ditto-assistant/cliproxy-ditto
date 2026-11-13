#!/usr/bin/env bash
# End-to-end: the Auto Mode classifier route in a real CLIProxyAPI release.
#
# A fake subscription (priority 0) serves claude-opus-5-5 and, through a model
# alias, serves the classifier id as claude-sonnet-5: the fallback. A Ditto
# classifier credential (priority 10) serves only the classifier id.
#
#   1. a permission check goes to Ditto, addressed to the endpoint slug
#   2. main-loop traffic never reaches the classifier credential
#   3. a main-class or unlabelled request for the id goes to the subscription
#   4. a Router 400 falls back to the subscription without a cooldown
#   5. a Router 503 fails over to the subscription
#   6. a 200 with no text falls back to the subscription
#   7. a check over max_input_tokens never reaches Ditto
#
# Usage: test/e2e/classifier.sh [cli-proxy-api binary]  (downloads the release if omitted)
set -euo pipefail

CPA_VERSION="${CPA_VERSION:-8.0.53}"
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
WORK="$(mktemp -d)"
GOOS="$(go env GOOS)"; GOARCH="$(go env GOARCH)"
EXT=so; [ "$GOOS" = darwin ] && EXT=dylib
CLASSIFIER=ditto-auto-mode-classifier

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
SUB_PORT=$(port); DITTO_PORT=$(port); CPA_PORT=$(port)
LOG="$WORK/requests.jsonl"
printf 'ditto_inf_e2e\n' > "$WORK/ditto.key"

credential() { # $1 = max_input_tokens
  cat > "$WORK/auths/ditto-classifier.json" <<EOF
{"type":"ditto","endpoint":"auto-mode-classifier","api_key_file":"$WORK/ditto.key","base_url":"http://127.0.0.1:$DITTO_PORT","priority":10,"classifier_model":"$CLASSIFIER","max_input_tokens":$1}
EOF
}

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
        - name: claude-sonnet-5
          alias: $CLASSIFIER
      keys:
        - api-key: sk-ant-fake-subscription
plugins:
  enabled: true
  dir: $WORK/plugins
  configs:
    ditto:
      enabled: true
EOF

start() { # $1 = DITTO_MODE
  : > "$LOG"
  DITTO_MODE="$1" SUB_MODE=ok python3 "$ROOT/test/e2e/fakes.py" "$SUB_PORT" "$DITTO_PORT" "$LOG" & pids+=($!)
  (cd "$WORK" && exec "$BIN" -config "$WORK/config.yaml" > "$WORK/cpa-$1.log" 2>&1) & pids+=($!)
  for _ in $(seq 1 100); do
    if curl -fsS -H 'Authorization: Bearer e2e-access-key' "http://127.0.0.1:$CPA_PORT/v1/models" 2>/dev/null | grep -q "\"$CLASSIFIER\""; then
      return 0
    fi
    sleep 0.2
  done
  echo "CLIProxy never listed $CLASSIFIER; log:" >&2; tail -40 "$WORK/cpa-$1.log" >&2; exit 1
}
stop() { cleanup; pids=(); sleep 0.5; }

check_body() { # $1 = model
  printf '{"model":"%s","max_tokens":2112,"stop_sequences":["</block>"],"system":[{"type":"text","text":"You are a security monitor for autonomous AI coding agents.","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":[{"type":"text","text":"<transcript>\\nUser: probe\\n</transcript>\\n"},{"type":"text","text":"Bash: touch /tmp/marker"}]}]}' "$1"
}
ask() { # $1 = model, $2 = request class ("" for none)
  local class=()
  [ -n "$2" ] && class=(-H "x-claude-code-request-class: $2")
  curl -sS "http://127.0.0.1:$CPA_PORT/v1/messages" \
    -H 'x-api-key: e2e-access-key' -H 'anthropic-version: 2023-06-01' -H 'content-type: application/json' \
    -H 'user-agent: claude-cli/2.1.323 (external, sdk-cli)' "${class[@]}" -d "$(check_body "$1")"
}
count() { python3 -c "import json,sys; print(sum(1 for l in open(sys.argv[1]) if (e:=json.loads(l))['who']==sys.argv[2] and e['path'].startswith('/v1/messages')))" "$LOG" "$1"; }
models_seen() { python3 -c "import json,sys; print(sorted({json.loads(e['body']).get('model') for e in map(json.loads, open(sys.argv[1])) if e['who']==sys.argv[2] and e['path'].startswith('/v1/messages')}))" "$LOG" "$1"; }
fail() { echo "FAIL: $*" >&2; tail -n 40 "$WORK"/cpa-*.log >&2; exit 1; }
expect() { # $1 = output, $2 = from-ditto|from-subscription, $3 = label
  grep -q "$2" <<<"$1" || fail "$3: expected $2, got: $1"
}

echo "== models"
credential 0
start ok
models=$(curl -fsS -H 'Authorization: Bearer e2e-access-key' "http://127.0.0.1:$CPA_PORT/v1/models")
grep -q "\"$CLASSIFIER\"" <<<"$models" || fail "classifier id not listed"
if grep -q '"auto-mode-classifier"' <<<"$models"; then fail "the classifier credential must not register its endpoint slug"; fi

echo "== 1. a permission check goes to Ditto"
expect "$(ask "$CLASSIFIER" auxiliary)" from-ditto "check"
[ "$(count subscription)" = 0 ] || fail "the subscription answered a check Ditto should have taken"
[ "$(models_seen ditto)" = "['auto-mode-classifier']" ] || fail "Ditto saw models $(models_seen ditto), want the endpoint slug"
python3 - "$LOG" <<'PY' || fail "Ditto request carried the wrong credentials or lost the check"
import json, sys
for e in map(json.loads, open(sys.argv[1])):
    if e["who"] == "ditto" and e["path"].startswith("/v1/messages"):
        assert e["authorization"] == "Bearer ditto_inf_e2e", e
        assert "e2e-access-key" not in e["authorization"] + e["x_api_key"], e
        body = json.loads(e["body"])
        assert body["system"][0]["text"].startswith("You are a security monitor"), body
        assert "<transcript>" in body["messages"][0]["content"][0]["text"], body
PY
echo "ditto=$(count ditto) subscription=$(count subscription)"

echo "== 2. main-loop traffic never reaches the classifier credential"
: > "$LOG"
expect "$(ask claude-opus-5-5 main)" from-subscription "opus"
[ "$(count ditto)" = 0 ] || fail "an Opus request reached the classifier credential"

echo "== 3. a main-class or unlabelled request for the id stays on Claude"
for class in main ""; do
  : > "$LOG"
  expect "$(ask "$CLASSIFIER" "$class")" from-subscription "class '${class:-none}'"
  [ "$(count ditto)" = 0 ] || fail "class '${class:-none}' reached Ditto"
  [ "$(models_seen subscription)" = "['claude-sonnet-5']" ] || fail "fallback used $(models_seen subscription)"
done
stop

echo "== 4. a Router 400 falls back to Claude, with no cooldown"
start 400
expect "$(ask "$CLASSIFIER" auxiliary)" from-subscription "router 400"
expect "$(ask "$CLASSIFIER" auxiliary)" from-subscription "router 400 again"
[ "$(count ditto)" = 2 ] || fail "after a 400 Ditto must still be tried first (no cooldown): ditto=$(count ditto)"
echo "ditto=$(count ditto) subscription=$(count subscription)"
stop

echo "== 5. a Router 503 fails over to Claude"
start 503
expect "$(ask "$CLASSIFIER" auxiliary)" from-subscription "router 503"
[ "$(count ditto)" -ge 1 ] || fail "Ditto was not tried"
stop

echo "== 6. a 200 with no text falls back to Claude"
start empty
expect "$(ask "$CLASSIFIER" auxiliary)" from-subscription "empty reply"
[ "$(count ditto)" = 1 ] || fail "want one Ditto attempt, got $(count ditto)"
stop

echo "== 7. a check over max_input_tokens never reaches Ditto"
credential 50
start ok
expect "$(ask "$CLASSIFIER" auxiliary)" from-subscription "oversize"
[ "$(count ditto)" = 0 ] || fail "an oversize check reached Ditto"
stop

echo "PASS"
