# claude-router architecture

This document describes the target layout of the code. The code moves there
one step at a time: today `internal/platform` exists, and most of the code
still lives in `package main` at the repository root.

## Purpose

claude-router is a local proxy between Claude Code and model providers. It
listens on two loopback ports. The API port (`ROUTER_LISTEN`, 8787) takes
Claude Code's requests in the Anthropic format. It either passes them through
to the cloud or translates them for an OpenAI-compatible provider or Codex.
The UI port (`ROUTER_UI_LISTEN`, 8788) shows history and limits and edits the
configuration. State lives in `ROUTER_HOME` (default `~/.claude/local-router`).

## Packages

```
cmd/router/                     wiring: serve|install|uninstall|start|stop|status|env|deploy commands, building dependencies, startup
internal/platform/              paths, file lock, atomic write, private permissions, autostart, stop signal; *_unix.go / *_darwin.go / *_windows.go
internal/config/                types (Provider, Model, Pool, Route, Profile, Catalog), validation, profiles, migrations, env file, Store (read, write, watch, mutate)
internal/history/               request records: record/recorder, history.jsonl, compaction
internal/limits/                Anthropic limits from response headers, limits.json, view
internal/routing/               health (EWMA, cooldown), session affinity, candidate choice, pool checker, state snapshot
internal/providers/             Upstream interface, shared Request/Response, translation (Anthropic to OpenAI), trimming
internal/providers/openai/      adapter for OpenAI-compatible providers (chat/completions)
internal/providers/codex/       Codex adapter: Responses API, OAuth (auth store, login listener on 1455), usage, context
internal/providers/anthropic/   cloud reverse proxy to api.anthropic.com, count_tokens, limits observation hook
internal/server/                API port HTTP: /v1/messages (orchestration), /v1/messages/count_tokens, /healthz, /api/profiles/{name}/activate
internal/ui/                    UI port HTTP: templates (embed), view models, handlers; changes configuration only through config.Store methods
internal/claudecode/            Claude Code integration: settings.json (ANTHROPIC_BASE_URL) with a backup, environment for launching claude
internal/deploy/                lifecycle (standby/active/quiesced/draining), blue/green controller (deployOps), cutover, deploy.json
```

Four packages have their own reasons to exist:
- `server`: the API port is neither UI nor a provider.
- `deploy`: the zero-downtime deploy (PR #7) is a responsibility of its own.
- `claudecode`: the Claude Code integration is neither platform nor UI.
- `providers/openai`: the second adapter, next to `codex`.

`cmd/router` is the only `package main`.

## Package layout

The tree above is where code lives from now on, not only where it ends
up:

- New code goes into `internal/<package>`, into the package the tree
  assigns it. No new file joins the root `package main`.
- The root `package main` keeps only `main.go`, and `main.go` only wires:
  it reads the environment, builds the parts in the order listed under
  Process, and starts them. Handlers, routing, providers, file formats and
  the UI do not live there. Until `cmd/router` exists, the root `main.go`
  plays its role.
- Code that still lives in the root package moves out one package at a
  time. `providers`, `routing` and `server` move early, right after
  `history` and `limits`, so that most new work has a package to land in.
- A change to code that is still in the root package first moves that
  code into its package, then changes it.

What each package does not know:

| Package | Responsible for | Does not know |
|---|---|---|
| platform | the OS: paths, locks, permissions, service, signals | configuration, HTTP, models |
| config | configuration invariants and its files | who changes it and why; HTTP; network |
| history | the request log: the history.jsonl format, including parsing the response in the front-end format (Anthropic Messages, the Resp field) | the providers/* packages, UI |
| limits | Anthropic limit windows | Codex, UI |
| routing | which candidate is next | how to run a request; formats |
| providers/* | how to run a request against a provider | who chose the candidate; history |
| server | putting one request together | UI, deploy |
| ui | pages and forms | configuration files directly |
| claudecode | Claude Code's settings.json | everything else |
| deploy | process modes and slot switching | configuration, routing |

## Dependency direction

Layers from the outside in:

```
cmd/router
  → server, ui, deploy, claudecode
    → providers/*, routing, history, limits
      → config
        → platform
```

The rules:
- A package imports only layers deeper than itself.
- `platform` imports nothing from `internal`.
- `config` imports only `platform`.
- `routing` takes only types from `config`, not the Store.
- `limits` does not know `providers/codex`; `ui` assembles `/api/limits`.
- `server` and `ui` do not import each other.
- `syscall`, `os/exec` and `os/signal` appear only in `platform` and `deploy`.

Depguard rules in `.golangci.yml` pin these import rules and CI runs them, so
this section and that file change together.

Clocks, random number generators and HTTP clients are constructor
dependencies, not globals.

## Platform layer

One interface with two implementations (darwin and windows), plus a fake for
the tests of `cmd/router` and `deploy`.

```go
package platform

type Paths struct {
    Home      string // ROUTER_HOME: env, providers.json(+.profiles/, .active-profile, .before-*), models.json, history.jsonl, limits.json(+.lock), router.log, router.pid, deploy.json, state.<slot>.json
    ConfigDir string // Codex tokens: ~/.config/claude-router on darwin, %USERPROFILE%\.config\claude-router on windows
    ClaudeDir string // ~/.claude (Claude Code's settings.json): $HOME/.claude on both systems, overridden by CLAUDE_CONFIG_DIR
}
func DefaultPaths(getenv func(string) string) (Paths, error)

// Inter-process lock: flock on unix, LockFileEx on windows (golang.org/x/sys/windows).
// Busy means EWOULDBLOCK or ERROR_LOCK_VIOLATION. The only lock in the repository.
func WithLock(ctx context.Context, path string, fn func() error) error

// A temporary file, then a replace of the target: os.Rename on unix; on windows
// MoveFileEx(REPLACE_EXISTING), retried while a sharing violation or another
// handle on the target refuses it. Callers close their own handles on the
// target first. The only file replacement in the repository. perm applies on
// unix; on windows only its owner-write bit does, and secrets are protected
// by the %USERPROFILE% ACL.
func WriteFileAtomic(path string, data []byte, perm fs.FileMode) error
func ReplaceFile(src, dst string) error
func MkdirPrivate(path string) error

type ServiceSpec struct{ Label, Exe string; Args []string; Env map[string]string; LogPath string; KeepAlive bool }
type Service interface {
    Install(ServiceSpec) error
    Uninstall(label string) error
    Start(label string) error
    Stop(label string) error
    Status(label string) (Status, error)
}
func NewService() Service // chosen by build tag

// The context is done on SIGTERM/SIGINT (unix), or on Ctrl+C and the named event Local\claude-router-<label> (windows).
func ShutdownContext(parent context.Context, label string) (context.Context, context.CancelFunc)
```

| Aspect | darwin | windows |
|---|---|---|
| autostart | LaunchAgent (plist in ~/Library/LaunchAgents, RunAtLoad, KeepAlive); labels come from deploy.json | a Task Scheduler task from embedded XML: ONLOGON trigger, current user, RestartOnFailure; `schtasks /Create /XML` |
| stop | `launchctl kill SIGTERM`, then drain in deploy.lifecycle | SetEvent on the named event, then the same drain, then `schtasks /End` |
| lock | flock, LOCK_NB, polled every 10 ms | LockFileEx, LOCKFILE_EXCLUSIVE_LOCK\|FAIL_IMMEDIATELY, polled every 10 ms |
| permissions | 0600/0700 | do not apply; permission tests live only in `_unix_test.go` |
| paths | `~/.claude/local-router`, `~/.config/claude-router` | `%USERPROFILE%\.claude\local-router`, `%USERPROFILE%\.config\claude-router`: one rule, "home directory plus a relative path", as `codex_auth.go` already does and as Claude Code does (`%USERPROFILE%\.claude`). `%APPDATA%` was rejected: a second branch with no benefit, plus a token migration |
| zero-downtime deploy | Caddy and two launchd slots (PR #7) | none: `router deploy` answers "not supported", and a restart goes through Service.Stop/Start; a front end inside the router is a separate decision |
| Claude Code | claude-local / `router env` | `router env` writes `env.ANTHROPIC_BASE_URL` to `%USERPROFILE%\.claude\settings.json` (claudecode code, the same as in `claude_proxy.go`) and prints the value; no client is needed on the Windows machine, because an integration test checks this |
| scripts | bash `router` and `claude-local`, thin wrappers around the binary | `router.ps1`, a wrapper; `bash` on PATH on Windows may be WSL, so no shell scripts |
| console and logs | UTF-8 | log to a file; command output is ASCII or uses SetConsoleOutputCP(65001) (the console uses the OEM code page, such as cp866) |

On Windows the router runs as a Task Scheduler task, not as a service. It
has to live in the user's profile (Claude Code's settings.json, ROUTER_HOME,
the browser for the Codex login), and it has to install without
administrator rights. RestartOnFailure in the task's XML restarts it after a
crash, and a named event stops it with a drain.

The Windows autostart options considered:
- **Task Scheduler task: chosen.**
  - It needs no administrator rights.
  - It runs in the user's profile: paths, settings.json, the browser for the Codex login.
  - `<RestartOnFailure>` in the task's XML restarts it after a crash.
  - A named event stops it with a drain.
  - That covers both drawbacks raised against it: no restart, and no zero-downtime deploy. Windows does not need the latter.
- **A service (`x/sys/windows/svc`): rejected for the first version.**
  - Session 0 under LocalSystem does not see `%USERPROFILE%`. The installer would need administrator rights and would have to bake ROUTER_HOME and CLAUDE_CONFIG_DIR into the service's environment.
  - A Codex login through a browser from session 0 is doubtful.
  - It remains a possible second adapter behind the same Service interface, if the router ever has to start before the user logs in.
  - The choice changes if a check on Windows shows that the task does not restart the process after a crash.
- **NSSM/WinSW: rejected.** They are a third-party exe.
- **A front end inside the router instead of Caddy.** This is not autostart but a different deploy scheme, and a separate decision.

## Where state and configuration live

| File | Owning package | Written by | Protection |
|---|---|---|---|
| ROUTER_HOME/env | config | Store (UI) | atomic write; one writer, the active slot |
| providers.json, providers.json.profiles/, .active-profile, .before-* | config | Store | atomic; `writesSharedState` |
| models.json | routing | health.save | `writesSharedState` |
| history.jsonl | history | store, O_APPEND | fmu; compaction only in the active slot |
| limits.json (+.lock) | limits | saver | platform.WithLock plus a merge with the disk copy |
| deploy.json, state.<slot>.json | deploy | controller | atomic |
| router.log, router.pid | cmd/router | the process | none |
| ConfigDir/codex-auth-<hex>-<id>.json (+.lock) | providers/codex | auth store | platform.WithLock |
| ClaudeDir/settings.json (+.router-proxy-backup) | claudecode | a UI action | byte-for-byte conflict check |

File formats do not change. Every step must stay compatible with the files
already on disk.

## Test boundaries

- **platform:** per OS, with build tags; a fake Service for `cmd/router` and `deploy`.
- **config:** through the Store and t.TempDir; atomic-write failure tests in `_unix_test.go`.
- **routing:** pure tests with injected clocks, only through Pick/Record/Bind; no sleeps.
- **providers:** one table of contract tests for both adapters (openai, codex) against httptest; anthropic through an httptest upstream.
- **server:** end to end with httptest (the router plus a fake upstream): failover, affinity, catch-all, count_tokens.
- **ui:** handlers through `ui.New(...)`, no struct literals; configuration through a Store in a TempDir.
- **deploy:** fake deployOps/cutoverOps, as in PR #7; `deploy/check.sh` is the darwin integration test.
- **end to end on Windows:**
  - `go test ./...`;
  - `router.exe install`;
  - /healthz after logging in again;
  - one request from Claude Code.

## Process

`cmd/router serve` builds its parts in this order:
1. platform.Paths
2. config.Store (migrations, profiles)
3. history, limits, routing
4. providers
5. server, ui
6. deploy.lifecycle

Shutdown goes through `platform.ShutdownContext`, then drain, then exit.
`main()` stays under 200 lines, with no handler logic.
