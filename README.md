# claude-router

Routes Claude Code requests to Anthropic, OpenAI-compatible providers, or a
Codex subscription. Configure it at http://127.0.0.1:8788/settings.

Функциональный обзор (на русском): [docs/features.md](docs/features.md).

```sh
./router build
./claude-local
```

`claude-local` sets `ANTHROPIC_BASE_URL` and preserves the model selected in
Claude Code. It no longer remaps Haiku to a local alias.

To use the regular `claude` command, click **Подключить Claude к роутеру**
in the dashboard. The same button becomes **Восстановить настройки Claude**.
It changes only `env.ANTHROPIC_BASE_URL` in the user's Claude settings and
keeps the original value in a private `settings.json.router-proxy-backup` file.
Restore preserves unrelated edits made since connection. `CLAUDE_CONFIG_DIR`
is respected. Relaunch Claude Code after switching; project or managed
settings can override the user setting. See the official
[environment-variable reference](https://code.claude.com/docs/en/env-vars).

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

## Routing and pools

Routing uses a model family (Opus, Sonnet, Haiku, Fable) plus
`output_config.effort`. An explicit version override has priority. Each
combination selects one of:

- **Не настроено**: reject the request without contacting a provider.
- **Anthropic**: forward it to the configured Anthropic upstream.
- **Модель**: send to one configured `provider/model` with a fixed target effort.
  There is no failover to another model if it fails.
- **Пул**: choose among the members of a named pool, with optional failover.

An absent effort uses the separate `default` / **Не указан** row. There is no
fallback from an unconfigured effort to another effort. New versions inherit
their named family dynamically: `claude-opus-5-5` and future Opus versions
use the Opus rules. Unconfigured families and empty pools reject requests.
An explicit disabled version/effort overrides an enabled family. Backend
model IDs do not bypass the routing table.

A named pool contains multiple `provider/model` members. Each member has its
own fixed target effort. For example, Opus/high can select a pool containing
`codex/gpt-6-sol` with `xhigh` and another provider's model with `high`.
Opus/low can select a different pool. Sonnet/high can reuse either pool.
Leaving a member's effort empty lets that backend use its default. A direct
model assignment uses the same effort choices without creating a one-member
pool; choose a pool instead when failover is wanted. For example, a direct
family route can be stored as `"high": {"mode": "model", "model":
"codex/gpt-6-sol", "effort": "xhigh"}`.

`providers.json` beside the binary is authoritative (mode 0600, contains keys):

```json
{
  "providers": [{"name": "codex", "type": "codex", "base_url": "https://chatgpt.com/backend-api/codex"}],
  "models": [{"provider": "codex", "model": "gpt-6-sol"}]
}
```

Profiles group routes, pools, their member order and target efforts, and per-pool
settings. Providers and the model catalog remain global. On first startup the
current configuration becomes the active `default` profile and the old file is
backed up as `providers.json.before-profiles`. A profile is a live saved
snapshot: edits to routes or pools immediately persist to the active profile.
Clone the active profile to experiment, or create a clean profile with all
routes disabled. Switch in the dashboard or via the API port:

```sh
curl -X POST http://127.0.0.1:8787/api/profiles/my-profile/activate
```

The response reports `active_profile`; an unknown name returns HTTP 400.
The new profile applies to the next request; requests already in flight keep
their original configuration. Session bindings reset when profiles switch.
Connections and model-catalog edits are shared, including provider removal,
which can disable routes or empty pools in every profile. Active and final
profiles cannot be deleted. Automatic switching by remaining quota is not
implemented.

The on-disk schema keeps `providers`, `models` and `catalog` in
`providers.json`. Each profile has its own `providers.json.profiles/<name>.json`
with `family_routes`, `routes`, `model_pools` and `pool_settings`.
`providers.json.active-profile` contains the active profile name as a JSON
string. The previous combined format is accepted on upgrade. Activation writes
only the pointer file; a stale settings form must be refreshed before saving.

For example, `providers.json.profiles/default.json` can contain:

```json
{
  "family_routes": {
    "opus": {
      "high": {"mode": "pool", "pool": "Deep work"},
      "low": {"mode": "anthropic"}
    }
  },
  "routes": {},
  "model_pools": {
    "Deep work": [{"model": "codex/gpt-6-sol", "effort": "xhigh"}]
  }
}
```

This enables the two listed effort combinations for all Opus versions.
Use `routes` with exact model IDs for individual exceptions; absent entries
inherit the family. Add further
members in **Пулы моделей** and assignments in **Маршруты**. Removing a
model or provider disables its direct assignments; renaming a provider
updates their model keys.

The dashboard offers three main sections: **Маршруты**, **Пулы**, and
**Подключения**. Connections expose the provider model catalog, including
effort choices when available. Removing a model also removes it from pools;
an emptied pool remains disabled. A pool referenced by a route cannot be
deleted until the assignments are changed.

Settings apply to the next request. The router reloads `providers.json` and
`env` within two seconds; invalid edits are ignored. Addresses and upstream
URL require a restart. `ROUTER_PROVIDERS_FILE` overrides the config path.

### Migration

On first startup with an old providers file, the router writes a private
`providers.json.before-pools` backup and converts known legacy routes to
explicit destinations and named pools. Existing member order and effort
mappings are retained. An old explicit empty pool becomes **Anthropic**.
The old `local-model` alias is preserved as an explicit migrated route for
already running clients. The next migration saves
`providers.json.before-families` and seeds each family from its latest configured
version. Equal version rules become inherited rules; different overrides remain
explicit. Unconfigured families remain disabled.

`ROUTER_CLOUD_ONLY`, legacy `pools`, and global `preferred` are migration inputs
only. They no longer control routing. A fresh installation has no enabled
routes; `ROUTER_LOCAL_BASE_URL`, `ROUTER_LOCAL_API_KEY`, `ROUTER_LOCAL_MODEL`,
and `ROUTER_LOCAL_MODELS` only seed the backend catalog.

## Sessions, timeouts and streaming

For requests with a Claude Code `metadata.user_id` session ID, the router pins
the selected backend separately for each incoming model and effort. Subsequent
requests retain that model despite load or rating changes. Requests without a
session ID are selected independently.

Each pool has its own **Сессии и таймауты** form: pool type, failover,
response-start timeout, idle probes and context character limit.
Settings persist in `providers.json` under `pool_settings`. Existing pools
receive a copy of their previous global values on upgrade (backup:
`providers.json.before-pool-settings`). Legacy `ROUTER_LOCAL_*` behavior
variables only supply initial defaults for new pools; they do not override
saved pool settings.

A pool has a `type`. `failover` is the default (an absent `type` is
failover): a new session goes to the first healthy member in pool order.
`balance` is set explicitly on a pool, in the form or in the file, and sends
each new session to the connection with the fewest sessions of this pool.
The type switches without a restart and applies to new sessions. In both
types pool order, not rating, decides; unhealthy members go last. A session
stays on its connection and model while it is healthy, moves to the next
member on failure and does not return. A signed-out Codex connection is
skipped. A client cancel (Esc) is not a failure. The old numeric pool
`balance` field is ignored and dropped at the next save; the global
`ROUTER_LOCAL_BALANCE` is unchanged. Ratings and cooldowns remain in
`models.json`.

With **Переключать модель при сбое** enabled in the pool, a timeout, connection failure, HTTP 404/408/429
or 5xx before the response begins retries another member of the same pool.
The session then remains on that member. No implicit switch to Anthropic or
another pool occurs. The pool’s **Ожидание начала ответа** setting limits time to the first
usable streaming event, including text, thinking or a tool call. Headers,
heartbeats, and role-only announcements do not end this timeout. For a
non-streaming response the timeout ends at the first body byte. Zero disables
this timeout; it is not an inactivity timeout after streaming starts.

Only the first usable SSE event is read before committing to a backend; the
response is forwarded incrementally. Text, thinking, and tool argument deltas
are not held until completion. A failure after output has started ends that
response without splicing in output from a different model; a later request
can use the next member.

Session bindings are in memory, expire after 24 hours of inactivity, and are
bounded to 4096 entries. A restart, a different route/pool, removal of the
pinned member, or a change to its effort/connection clears the affected
selection. Appending newly discovered models preserves existing bindings. The conversation itself
is supplied by the client on every request.

Idle-model probes use each pool’s interval (0 disables). Models outside pools
are not probed. A model shared by several pools is checked once at the shortest
enabled interval, using that pool’s response-start timeout (capped at the probe
interval). Health ratings are shared. Codex models are excluded so probes do not
spend subscription quota.

## Codex subscription

Use **Войти через браузер** under **Подключения → Подписка Codex**. The router
starts an OAuth callback on `127.0.0.1:1455`; finish sign-in in the opened tab.
Add a provider of type **Codex**; the next catalog refresh imports the account
models. Include the desired models in pools. Its endpoint is fixed and it does not use an API key.
An existing CLI login can be imported explicitly from the provider pane; it
is never read automatically.

The router saves and refreshes its own credential in
`~/.config/claude-router/codex-auth.json` (0600). `ROUTER_CODEX_AUTH_FILE` changes
that path.

Each Codex provider is a separate subscription with its own sign-in, import
and limits; the **Подписка Codex** section shows one block per connection.
New connections get an `auth_id`, and their credential lives next to
`codex-auth.json` as `codex-auth-<hex name>-<auth_id>.json`. Renaming a
connection is not supported: remove it, add it again and sign in. A removed
connection leaves its file behind; delete it by hand if it is not needed. A
hand-added Codex provider without `auth_id` (other than the original `codex`)
is rejected until it is added through the dashboard. Calls use stateless Responses requests and subscription access,
not OpenAI API billing. Availability follows the signed-in account catalog.
Anthropic credentials are never forwarded to other providers.

## Codex subscription limits

**Лимиты Codex** opens the subscription section with the connected account’s
email, plan and account ID. **Обновить лимиты** is the only action that requests
subscription usage from OpenAI: opening or reloading the page reads the local
identity and in-memory cache, without quota polling. The last successful result
remains visible with its timestamp and an error if a subsequent refresh fails.
Changing accounts clears the displayed quotas from the previous account.

The read-only adapter follows Cozyphi’s `doc/codex-usage.md` contract and calls
only `GET https://chatgpt.com/backend-api/wham/usage`. It displays primary and
additional windows, remaining percentages, reset times and available reset
credits. Missing data is shown as unavailable, not zero. Reset credits are
shown as a count; no credit-consuming action is provided. These are account-wide
limits, not a token budget or usage limited to requests through this router.
Redirects are rejected, responses are bounded to 1 MiB, errors omit response
bodies and credentials, and cached results are bound to the connected account.

## Automatic catalog updates

The router checks catalogs at startup and once per hour. **Обновить модели**
in the dashboard runs the same update for Anthropic and every configured
provider. Anthropic IDs come from the copyable model IDs in the official
[model comparison](https://platform.claude.com/docs/en/models/overview), without
requiring an API key. Codex uses the connected account's model catalog. Last
successful catalogs and check results persist inside `providers.json`; a
failed fetch keeps previous data and displays the failure.

New Claude versions immediately inherit their family's routes, even before
catalog refresh. Version controls show **Наследовать** and the effective
family destination. Updating family rules updates all inheriting versions.

Newly discovered Codex versions are added to the model registry. In every pool,
a newer version of the same named variant inherits that pool's newest earlier
member's effort and is appended after existing members. Sol, Astra, Luna,
Terra and other distinct variants are kept separate. Older models remain.
A manually removed known member is not re-added on the next hourly refresh.

Codex effort choices are read from `supported_reasoning_levels` for the selected
model, including the persisted successful catalog while offline. The form
refreshes the choices when its model changes; unsupported submitted values
are rejected. Auto-inheritance only adds a member if its target effort is
confirmed by that new model's catalog (or the target uses the model default).
Unsupported inheritance is skipped and reported in the refresh result.

## Request history

The web UI is served on loopback, without authentication. History supports
filtering by model, route and session, full-text search, request/response
inspection, translated payloads and downloads. It retains up to
`ROUTER_UI_HISTORY` requests (default 300), persisted in `history.jsonl` (0600,
contains prompts). `ROUTER_UI_HISTORY_FILE` overrides its location; set it empty
for memory only. Capturing history does not delay forwarding the response.

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

## Context handling

`ROUTER_LOCAL_MAX_INPUT_CHARS` limits the prompt sent to pool members; zero
disables trimming. Oversized blocks are elided first, then the oldest turns.
Tool definitions are retained and tool replies are not left without their
calls. The history records trimming. Thinking blocks in previous assistant
messages are omitted when translating to an OpenAI-compatible provider.

## Verification

```sh
go test ./...
go test -race ./...
go vet ./...
```
