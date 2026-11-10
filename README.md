# cliproxy-ditto

A [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) plugin that adds
[Ditto Router](https://heyditto.ai) as a **last-resort provider**. Your Claude,
ChatGPT/Codex, Gemini and Grok subscriptions keep serving every request they
can. When all of them are rate limited or out of quota, CLIProxy sends the
overflow to Ditto instead of failing, or instead of a provider's own pay-as-you-go
"extra usage".

Through Ditto you get the endpoint you choose: its model routing, BYOK keys,
spend limits and savings (prompt caching, compaction, recall), and any model
Ditto can serve.

## How it works

The plugin registers a `ditto` provider with three parts:

| Part | What it does |
|---|---|
| **Credential at priority -100** | Each connected Ditto endpoint is a CLIProxy credential. CLIProxy only selects credentials from the highest-priority tier that still has an available one, so Ditto is chosen only after every subscription for that model is cooling down, out of quota or failing. |
| **Mirrored model ids** | Ditto registers the same model ids your subscriptions do (`claude-opus-5-5`, `gpt-6.1-sol`, …, filtered by `fallback-models`). This is what puts Ditto in the fallback set for a model. It also registers your endpoint's slug and aliases (for example `ditto-dev`), so you can ask for an endpoint by name. |
| **Passthrough executor** | Ditto Router natively speaks Anthropic Messages, OpenAI Responses (including Codex) and Chat Completions. The plugin declares all of them, so CLIProxy hands over the client's request untranslated. Thinking blocks, cache breakpoints and tool calls arrive as the client sent them. Only an allowlisted set of headers is forwarded; your CLIProxy access key is never sent. |

The plugin also reports Ditto's credit balance and endpoint spend limits as
CLIProxy quota, so they appear next to your subscriptions in the management UI.

Why a plugin rather than an `openai-compatibility` entry: a compat entry
translates every request to Chat Completions and can't share model ids with
your subscriptions at a lower priority. Requests would lose Anthropic-native
features, and Ditto would be a separate model rather than a fallback.

## Install

Requirements: CLIProxyAPI 8.x with plugins enabled, Go 1.26, and the Ditto CLI
(`npm i -g @heyditto/cli && heyditto login`).

```bash
git clone https://github.com/ditto-assistant/cliproxy-ditto && cd cliproxy-ditto
scripts/install.sh                     # builds against your CLIProxy binary and installs ditto.dylib/.so next to it
scripts/connect.sh <endpoint-slug>     # mints an endpoint key into a 0600 file, writes the credential
```

`install.sh` defaults to the EasyCLIProxyAPI core on macOS. Pass another
`cli-proxy-api` path to use a different binary. It builds with the Go release
and architecture that binary was built with, which a Go c-shared plugin
requires.

> **Apple Silicon:** the Intel build of EasyCLIProxyAPI runs an x86_64 core
> under Rosetta, and no native plugin can load there (CLIProxy's own example
> plugins crash the same way). `install.sh` detects this and stops. Install the
> Apple Silicon build of EasyCLIProxyAPI first.

Then enable the plugin in CLIProxy's `config.yaml`:

```yaml
plugins:
  enabled: true
  configs:
    ditto:
      enabled: true
      # Optional; defaults shown.
      # base-url: https://inference.heyditto.ai
      # fallback-models: ["claude-*", "gpt-*", "o1*", "o3*", "o4*", "codex-*", "gemini-*", "grok-*"]
      # extra-models: []          # ids to serve through Ditto even with no subscription for them
      # expose-catalog: false     # also list Ditto's whole catalog (provider/model ids)
      # credential-priority: -100
```

### Choosing endpoints

Run `connect.sh` once per endpoint. Several endpoints can be connected; give one
a higher priority (still below your subscriptions' `0`) to try it first:

```bash
scripts/connect.sh ditto-dev -50
scripts/connect.sh cheap-overflow      # -100
```

Do **not** connect an endpoint with `cliproxy_relay` enabled (one that relays
Ditto inference back into your local CLIProxy). Overflow would loop back to
CLIProxy, and `connect.sh` refuses it.

The credential CLIProxy loads looks like this; the key stays in its own file:

```json
{"type": "ditto", "endpoint": "ditto-dev", "api_key_file": "/abs/path/ditto-dev.key", "priority": -50}
```

`api_key` (inline) and `base_url` are also accepted.

## Behaviour to know about

- **Turn off the providers' own extra usage.** CLIProxy fails over only when a
  subscription refuses a request. If "extra usage" (Claude) or credits
  (ChatGPT) are enabled on the account, the provider keeps answering and bills
  you, and Ditto is never reached. Disable them for overflow to reach Ditto.
- **Session affinity.** With `routing.session-affinity: true`, a session that
  failed over to Ditto stays bound to Ditto until the binding expires
  (`session-affinity-ttl`, 1h by default) or Ditto fails, even if your
  subscription has recovered. That keeps the prompt cache warm, but it can
  spend Ditto credit after a subscription resets. Shorten the TTL if you want
  sessions to return sooner.
- **Ditto out of credit.** A 402 from Ditto (balance or endpoint spend limit)
  is reported to CLIProxy as a quota error, so the Ditto credential cools down
  instead of being retried.
- **Not covered yet:** Codex's WebSocket transport (HTTP Responses is),
  Gemini-native clients (CLIProxy translates them to a format Ditto accepts),
  and a browser login (keys come from `heyditto`).

## Development

```bash
go vet ./... && go test ./...
bash test/e2e/run.sh            # downloads the CLIProxyAPI release for this OS/arch
```

The end-to-end test loads the plugin into a real CLIProxyAPI release next to a
fake subscription credential serving `claude-opus-5-5`, and checks three cases:

1. With the subscription healthy, Ditto receives no model request.
2. With the subscription returning 429, the same request is answered by Ditto,
   using the Ditto key and never the client's.
3. Streaming events arrive intact.

To check against the real Router, mint a short-lived capped key and run the
live variants. They spend a few cents on the endpoint's routing:

```bash
heyditto endpoints keys create <endpoint> --store file --key-file "$PWD/.tmp/live/<endpoint>.key" \
  --expires 1h --budget 5000000 --spend-period never --yes      # needs a private (0700) .tmp/live
export DITTO_KEY_FILE="$PWD/.tmp/live/<endpoint>.key" DITTO_ENDPOINT=<endpoint>
go test -tags live -run Live -v .   # /v1/models and /v1/credits as the plugin reads them
bash test/e2e/live.sh               # Messages (plain and streaming) after a 429, Responses and Chat Completions by slug
heyditto endpoints keys revoke <endpoint> <key-id> --yes
```

`ci/github-actions.yml` runs the offline test on Linux amd64 and macOS arm64. It is not wired
up yet: it moves to `.github/workflows/` with a push from a token that has the
`workflow` scope.

## License

MIT
