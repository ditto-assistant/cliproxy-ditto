#!/usr/bin/env bash
# Connect a Ditto endpoint to CLIProxy: mint an endpoint key into a private
# file and write the credential CLIProxy loads from its auth directory.
#
#   scripts/connect.sh <endpoint-slug> [priority]
#
# Run once per endpoint you want CLIProxy to fall back to. Priority defaults
# to the plugin's (-100); among several Ditto endpoints the higher one is used
# first. CPA_AUTH_DIR and DITTO_KEY_DIR override the EasyCLIProxyAPI defaults.
#
# With DITTO_CLASSIFIER_MODEL=<id> the credential instead serves only that id,
# for Claude Code's Auto Mode permission checks (see the README). Its priority
# defaults to 10, ahead of the subscriptions, and the endpoint must be
# classifier-safe: nothing that recalls, records, compacts, injects a prompt or
# tools, or reroutes. DITTO_CLASSIFIER_MAX_INPUT_TOKENS optionally bounds what
# the route accepts; larger checks stay with Claude.
set -euo pipefail

endpoint="${1:?usage: connect.sh <endpoint-slug> [priority]}"
priority="${2:-}"
classifier="${DITTO_CLASSIFIER_MODEL:-}"
max_input="${DITTO_CLASSIFIER_MAX_INPUT_TOKENS:-}"
[[ "$endpoint" =~ ^[A-Za-z0-9._-]+$ ]] || { echo "unexpected endpoint slug: $endpoint" >&2; exit 1; }
[ -z "$priority" ] || [[ "$priority" =~ ^-?[0-9]+$ ]] || { echo "priority must be an integer" >&2; exit 1; }
if [ -n "$classifier" ]; then
  [[ "$classifier" =~ ^[a-z0-9][a-z0-9._-]{0,63}$ && "$classifier" != claude-* ]] || { echo "DITTO_CLASSIFIER_MODEL must be a dedicated lowercase id, not a claude-* id" >&2; exit 1; }
  [ -z "$max_input" ] || [[ "$max_input" =~ ^[0-9]+$ ]] || { echo "DITTO_CLASSIFIER_MAX_INPUT_TOKENS must be a whole number" >&2; exit 1; }
  priority="${priority:-10}"
elif [ -n "$max_input" ]; then
  echo "DITTO_CLASSIFIER_MAX_INPUT_TOKENS needs DITTO_CLASSIFIER_MODEL" >&2; exit 1
fi

app="$HOME/Library/Application Support/com.cpa.gui"
auth_dir="${CPA_AUTH_DIR:-$app/oauth}"
key_dir="${DITTO_KEY_DIR:-$app/ditto-keys}"

command -v heyditto >/dev/null || { echo "install the Ditto CLI first: npm i -g @heyditto/cli && heyditto login" >&2; exit 1; }

# A relay endpoint sends inference back into your local CLIProxy, which would
# then fall back to Ditto again: a loop. Fall back to a Ditto-routed endpoint.
show="$(heyditto endpoints show "$endpoint" --output json)"
if python3 -I -c 'import json,sys; sys.exit(0 if (json.load(sys.stdin).get("providerOptions") or {}).get("cliproxy_relay") else 1)' <<<"$show"; then
  echo "$endpoint relays inference to your local CLIProxy (cliproxy_relay); pick an endpoint Ditto routes itself" >&2
  exit 1
fi

# A permission check must reach the model whole and leave no trace: refuse an
# endpoint whose settings would remember, shorten, add to or reroute it.
if [ -n "$classifier" ]; then
  python3 -I "$(dirname "$0")/classifier_safe.py" <<<"$show" || { echo "$endpoint is not classifier-safe; nothing was written" >&2; exit 1; }
fi

[ -d "$auth_dir" ] || { echo "no CLIProxy auth directory at $auth_dir (set CPA_AUTH_DIR)" >&2; exit 1; }
mkdir -p "$key_dir" && chmod 700 "$key_dir"
key_file="$key_dir/$endpoint.key"
if [ -e "$key_file" ]; then
  echo "reusing the key already in $key_file"
else
  heyditto endpoints keys create "$endpoint" --store file --key-file "$key_file"
fi

cred="$auth_dir/ditto-$endpoint.json"
python3 -I - "$cred" "$endpoint" "$key_file" "$priority" "$classifier" "$max_input" <<'PY'
import json, os, sys
path, endpoint, key_file, priority, classifier, max_input = sys.argv[1:]
doc = {"type": "ditto", "endpoint": endpoint, "api_key_file": key_file}
if priority:
    doc["priority"] = int(priority)
if classifier:
    doc["classifier_model"] = classifier
    if max_input:
        doc["max_input_tokens"] = int(max_input)
fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
with os.fdopen(fd, "w") as f:
    json.dump(doc, f, indent=2)
    f.write("\n")
PY
echo "wrote $cred"
if [ -n "$classifier" ]; then
  echo "CLIProxy serves $classifier from $endpoint once it reloads; add the Claude fallback alias from the README before launching with heyditto claude --cliproxy --auto-mode-classifier $classifier"
else
  echo "CLIProxy picks the credential up on its own; check its quota page for Ditto credits"
fi
