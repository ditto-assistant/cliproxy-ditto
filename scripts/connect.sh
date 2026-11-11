#!/usr/bin/env bash
# Connect a Ditto endpoint to CLIProxy: mint an endpoint key into a private
# file and write the credential CLIProxy loads from its auth directory.
#
#   scripts/connect.sh <endpoint-slug> [priority]
#
# Run once per endpoint you want CLIProxy to fall back to. Priority defaults
# to the plugin's (-100); among several Ditto endpoints the higher one is used
# first. CPA_AUTH_DIR and DITTO_KEY_DIR override the EasyCLIProxyAPI defaults.
set -euo pipefail

endpoint="${1:?usage: connect.sh <endpoint-slug> [priority]}"
priority="${2:-}"
[[ "$endpoint" =~ ^[A-Za-z0-9._-]+$ ]] || { echo "unexpected endpoint slug: $endpoint" >&2; exit 1; }
[ -z "$priority" ] || [[ "$priority" =~ ^-?[0-9]+$ ]] || { echo "priority must be an integer" >&2; exit 1; }

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

[ -d "$auth_dir" ] || { echo "no CLIProxy auth directory at $auth_dir (set CPA_AUTH_DIR)" >&2; exit 1; }
mkdir -p "$key_dir" && chmod 700 "$key_dir"
key_file="$key_dir/$endpoint.key"
if [ -e "$key_file" ]; then
  echo "reusing the key already in $key_file"
else
  heyditto endpoints keys create "$endpoint" --store file --key-file "$key_file"
fi

cred="$auth_dir/ditto-$endpoint.json"
python3 -I - "$cred" "$endpoint" "$key_file" "$priority" <<'PY'
import json, os, sys
path, endpoint, key_file, priority = sys.argv[1:]
doc = {"type": "ditto", "endpoint": endpoint, "api_key_file": key_file}
if priority:
    doc["priority"] = int(priority)
fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
with os.fdopen(fd, "w") as f:
    json.dump(doc, f, indent=2)
    f.write("\n")
PY
echo "wrote $cred"
echo "CLIProxy picks the credential up on its own; check its quota page for Ditto credits"
