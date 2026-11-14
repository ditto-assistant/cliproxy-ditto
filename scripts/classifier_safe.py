#!/usr/bin/env python3
"""Exit 0 when an endpoint (`heyditto endpoints show <slug> --output json` on
stdin) is safe to answer Claude Code Auto Mode permission checks; otherwise
print each problem and exit 1.

Safe means the Router forwards the check to the endpoint's model and nothing
else: no memory recall or recording of the transcript, no compaction or
snapshot replies, no injected system prompt or tools, no rerouting to another
model. Only setting names and booleans are printed, never values that could
hold a prompt or a key.
"""
import json
import sys


def problems(e):
    out = []
    for key in ("recallEnabled", "recordEnabled", "recordTrace", "recordAttachments", "codeMode", "toolCompression"):
        if e.get(key):
            out.append(f"{key} must be false")
    for key in ("contextCompaction", "speculativeCompaction", "resultCompression"):
        if (e.get(key) or "off") != "off":
            out.append(f"{key} must be off")
    if e.get("precompactAtTokens"):
        out.append("precompactAtTokens must be 0")
    for key in ("tools", "kindRoutes", "subagentRoutes", "failoverPolicy", "aliases"):
        if e.get(key):
            out.append(f"{key} must be empty")
    if (e.get("systemPrompt") or "").strip():
        out.append("systemPrompt must be empty")
    options = e.get("providerOptions") or {}
    for key in ("claude_system_prompt", "claude_prompt_mode", "prompt_enhancement", "cliproxy_relay", "private_upstream_routing", "claude_speculative_compaction"):
        if options.get(key):
            out.append(f"providerOptions.{key} must be unset")
    if (options.get("ditto_failover") or {}).get("enabled"):
        out.append("providerOptions.ditto_failover must be disabled")
    if e.get("status") not in (None, "active"):
        out.append("status must be active")
    return out


def main():
    try:
        endpoint = json.load(sys.stdin)
    except json.JSONDecodeError:
        print("could not read the endpoint settings", file=sys.stderr)
        return 1
    if not isinstance(endpoint, dict):
        print("could not read the endpoint settings", file=sys.stderr)
        return 1
    found = problems(endpoint)
    for p in found:
        print(f"  {p}", file=sys.stderr)
    return 1 if found else 0


if __name__ == "__main__":
    sys.exit(main())
