# local-router

Routes Claude Code's `haiku` model alias to an OpenAI-compatible endpoint.
Every other alias reaches api.anthropic.com untouched, unless
`ROUTER_CLOUD_ONLY` narrows that down further.

    cp env.example env   # if missing; then set ROUTER_LOCAL_* to your endpoint
    ./claude-local       # launches Claude Code wired to the router

`ROUTER_LOCAL_*` in `env` only seed the first run. After that, providers and
models live in `providers.json` (see below) and the UI manages them.

Then `haiku` is the local slot: `--model haiku`, the model picker, or a subagent
spawned with `model: "haiku"`. `fable` is a plain cloud alias again.

## Running it

Sources live in this directory; the binary and everything it writes (`env`,
`providers.json`, `models.json`, `history.jsonl`, `router.log`) live in
`ROUTER_HOME`, `~/.claude/local-router` by default. `./router build` compiles
into that directory. The binary reads `env` beside itself at startup, so it
needs nothing sourced first. `router` and `claude-local` may be symlinked from
`ROUTER_HOME`; they find the sources through the link.

`claude-local` starts the router on demand if it is not already up, so normally
there is nothing to manage. For the rest:

    ./router status      # is it up?
    ./router start
    ./router stop
    ./router restart     # rebuilds first; use after editing env or Go sources
    ./router logs        # tail -f router.log

The router holds no state between requests, so a restart mid-session is safe:
the next request reconnects. Everything the UI can change is hot: the router
re-reads `env` and `providers.json` within two seconds of an edit, and the web
UI writes the same files. Only the listen addresses and the upstream URL need
`./router restart`. Claude Code sends the fixed alias `local-model`; the router
substitutes the preferred local model itself, so switching the model does not
touch a running session.

## How it routes

`ANTHROPIC_BASE_URL` points Claude Code at the router. `ANTHROPIC_DEFAULT_HAIKU_MODEL`
remaps the `haiku` alias to the fixed id `local-model`. The router dispatches on the
model id in the body:

  a configured local model, or local-*   ->  translated to /chat/completions
  anything else                          ->  byte-for-byte reverse proxy upstream

`ROUTER_CLOUD_ONLY` inverts the second line. Set to a comma-separated list of
substrings of model ids, it keeps only those on Anthropic and answers everything
else locally:

    ROUTER_CLOUD_ONLY=claude-opus-5,claude-fable-5   # those two cloud, rest local

Matching is by substring, so a short entry covers a family: `claude-fable-5`
catches both `claude-fable-5-1` (first-party) and `claude-fable-5` (gateway),
while the longer id would miss the shorter one.

That closes a hole the model picker does not cover: a subagent spawned with
`model: "sonnet"`, or a background chore Claude Code runs on haiku, chooses its
own model id, and without this it reaches Anthropic no matter what the session
was told to use. Two consequences to expect -- those calls are answered by
whatever the preferred local model is, and they are subject to
`ROUTER_LOCAL_MAX_INPUT_CHARS`, so a large subagent prompt is trimmed rather
than refused.

## Providers and local models

A provider is one OpenAI-compatible endpoint: a name, a base URL and an
optional API key. There can be several (LM Studio on this machine, Ollama on
another box, a vLLM server). Every local model belongs to a provider and is
addressed as `provider/model` in the UI, the history and the rating file.

`providers.json` next to the binary (mode 0600, it holds the keys) is the
source of truth:

    {
      "providers": [{"name": "lmstudio", "base_url": "http://127.0.0.1:1234/v1", "api_key": ""}],
      "models":    [{"provider": "lmstudio", "model": "coding-large"}],
      "preferred": "lmstudio/coding-large"
    }

The file is created by the first change in the UI. Until then the set is
seeded from `ROUTER_LOCAL_BASE_URL`, `ROUTER_LOCAL_API_KEY`,
`ROUTER_LOCAL_MODEL` and `ROUTER_LOCAL_MODELS` in `env`, as one provider named
after the host. Once the file exists those variables are ignored.
`ROUTER_PROVIDERS_FILE` moves the file. A hand edit is applied within two
seconds; an invalid one is logged and ignored.

In the UI, pick a provider from the list (or "+ новый провайдер") and the
router asks it for `GET /models` and lists the ones not yet configured with a
one-click add; a model the endpoint does not list can be typed in. Whatever
metadata the server returns with each model (context size, capabilities,
availability, display name, description: the fields differ per server) is
flattened and shown as tags, both in that list and in the configured models
table. Filters are derived from the data: a select per boolean or short enum
field, an "at least" select per numeric field, and a search over id, name and
description. Fields unique to every model (hashes, timestamps) are skipped. The
provider's URL and key can be edited or the provider deleted in the same pane;
deleting it drops its models.

With `ROUTER_LOCAL_FAILOVER=1` (the default) a request that a model fails
before answering is retried on the next candidate, across providers: no
response headers within `ROUTER_LOCAL_FIRST_BYTE_TIMEOUT` seconds, a 5xx, 404,
408 or 429, or a connection error. Once a response has started it belongs to
that model; a failure after that point only counts against its rating.

Every model has a rating: an exponential moving average of its success rate
and of its time to first byte, plus a cooldown after a failure that doubles
with each consecutive one (20s up to 5min). The next request goes to the
preferred model while its rating is at least 50% and it is not cooling down,
then to the best-rated healthy model, and to cooling models last. The rating
lives in `models.json` next to the binary; the UI shows it and can reset it.

The upstream path never parses a body and never rewrites a header, with one
exception while the UI is on: `Accept-Encoding` is dropped from `/v1/messages` so
the transport negotiates gzip itself and the capture sees a decoded body. The local
path never forwards Anthropic credentials: a provider sees its own key only.

## Web UI

    http://127.0.0.1:8788        # ROUTER_UI_LISTEN; empty disables

An htmx panel served by the router itself, loopback only, no auth:

- status strip: counts per route, in-flight, errors, trimmed prompts, local
  endpoint reachability (`GET /models`), current routing rule;
- request list, live-updating, filtered by route and model, with full-text search
  over the raw request body and the response text;
- per request: **Структура** (system blocks, tool definitions, message timeline
  with typed blocks, sizes, `cache_control` markers, a share-by-kind bar and a
  clickable minimap), **Отправлено локально** (the translated OpenAI payload and
  trim notes), **Ответ** (parsed blocks, usage, stop reason), **Raw**, and
  download links; search matches are highlighted per block;
- settings: the cloud-only list with one-click flips per model family; the
  providers (add, edit, delete, probe) and the models each one offers; the
  local models table with rating, ok/failure counts, first byte latency,
  cooldown and the preferred model; the failover switch, first byte timeout
  and input budget. Changes apply to the next request and are written back to
  `env` or `providers.json` (0600, via temp file and rename). A model id that
  was ever a configured local model stays local for the life of the process,
  so a Claude Code session started before the change cannot be routed to
  Anthropic by surprise.

History is a ring of `ROUTER_UI_HISTORY` requests (default 300), kept in memory
and appended to `history.jsonl` next to the binary (mode 0600, it holds prompts),
so it survives a restart. `ROUTER_UI_HISTORY_FILE` moves it; set it empty to
keep history in memory only. "Очистить" in the UI truncates the file too.

## Running it as a service

`./router install` writes a launchd agent (`com.claude-local-router`)
to `~/Library/LaunchAgents` and loads it. `RunAtLoad` starts the router at
login, `KeepAlive` restarts it if it dies. `./router uninstall` reverses it.

It is a user agent, not a system daemon: it starts at login rather than at boot,
which is what the terminal actually needs, and it avoids running a proxy that
holds an API key as root. The key stays in `env` (mode 600), read by the binary
itself; the plist is world-readable and never sees it.

Once the agent is installed, `start`, `stop` and `restart` delegate to
`launchctl` -- a plain kill would only be undone by `KeepAlive`. `stop` unloads
the agent, so it stays down until the next login or an explicit `start`.

## Limits worth knowing

- `ANTHROPIC_BASE_URL` is per process, not per subagent, so all traffic in the
  session traverses the router, cloud calls included.
- Claude Code has no per-model context window, so it cannot size a conversation
  for the local endpoint. `ROUTER_LOCAL_MAX_INPUT_CHARS` is a character budget
  the router enforces by trimming, not by refusing: refusing would wedge the
  session, because `/compact` is itself a request carrying the same
  conversation. Trimming elides oversized blocks first, drops the oldest turns
  only if that is not enough, never touches tool definitions, and never leaves a
  tool reply whose call it dropped. Each trim is logged with what it cost.
  `LOCAL_CONTEXT_TOKENS` clamps the whole session and should stay empty unless
  everything runs locally.
- `thinking` blocks are dropped in translation; there is no OpenAI equivalent.

## Confidentiality

Routing decides where a model runs, not what the orchestrator already read. A
prompt the cloud model composed by reading a sensitive file is already in the
cloud. Give the local subagent a path and let it read the file itself.
