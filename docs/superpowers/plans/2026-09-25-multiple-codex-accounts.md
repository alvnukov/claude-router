# Multiple Codex Connections Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let two or more ChatGPT/Codex subscriptions run as independent global providers (own OAuth, tokens, catalog, usage), route new sessions through failover pools (first healthy connection in pool order), and fail a 429 over to the next member with a repin.

**Architecture:** A Codex `provider` gains a persistent `auth_id`; `codexStoreFor(p)` maps each provider to its own `codexAuthStore` (legacy `codex` without an ID keeps the global store and file). Every Codex network path (requests, probe, usage, OAuth, import) asks for the store of the named provider. Pools get a `type` (only `failover`; absent = failover): a failover pool orders members by pool position instead of rating, and the existing session affinity keeps a session on its member (Task 5). A separate block (Task 7) adds an in-memory reload-error banner with its own acceptance.

**Tech Stack:** Go (module `localrouter`), `net/http`, `html/template` + htmx, `syscall.Flock`.

**Spec:** `docs/superpowers/specs/2026-09-25-multiple-codex-accounts-design.md` (509a256); the design review and the decision record (Clarifications 1–6) are kept outside the repository.

## Global Constraints

- Keep the provider name `codex` and all its references; the pre-existing `codex` with no `auth_id` always uses `ROUTER_CODEX_AUTH_FILE` (default `~/.config/claude-router/codex-auth.json`).
- `auth_id` is `json:"auth_id,omitempty"`: a legacy file round-trips byte-identical and `candidateSelection` does not change for legacy bindings.
- No auth tokens in `providers.json` or the UI.
- `withFileLock` copied verbatim from `091645c` (`filelock.go`); no lifecycle/quiesce guard (`s.life`) in this branch; `091645c` is not cherry-picked.
- Codex rename rejected; removing a provider leaves its auth file; an ID is never reused.
- All-429 behavior is unchanged. The rating + `balance` order is unchanged for the global config (checker, dashboard ranking); pool routes use pool order (Task 5).
- Pool type: field `type` in `pool_settings`; only `failover` exists; absent = failover, no migration, no file rewrite; any other value is rejected in `validate()` and shown by the Task 7 banner (Clarification 5). No balancer, no per-connection session counters, no round-robin in this branch.
- A bound session never changes connection or model while its member is healthy: switching loses the upstream session cache (owner). A failure moves it to the next healthy member and it stays there; a recovered first member gets only new sessions (Clarifications 4 and 5.2; Task 6 `TestCodexSessionStaysOnConnection`).
- No network usage query on page load; usage refresh is a per-connection button.
- Never deploy, restart or touch the live router; work only in this worktree; one branch, one PR.
- Verification: `go test -count=1 ./...`, `go test -race -count=1 ./...`, `go vet ./...`, `git diff --check`.
- Commit each verified task on the branch; push/merge only on request.

## Review Points (review, mandatory) → where they close

| # | Review point | Closed by | Test (file: name) |
|---|---|---|---|
| 1 | Pool type (Clarification 5, replaces the balancing point): only `failover`; absent = failover; unknown value → `validate()` error shown in the UI | Task 5: `poolSettings.Type`, `validate()`, `forModel` sets `poolType`, `pick` keeps pool order; `savePoolSettings` keeps the type | `pool_type_test.go`: `TestPoolWithoutTypeIsFailover`, `TestPoolTypeUnknownRejected`, `TestPoolSaveKeepsType` |
| 2 | Failover semantics (Clarification 5, replaces the session-count point): new session → first healthy member in pool order; on failure → next healthy, stays there; recovered first gets only new sessions | Task 5 (`pick` order, cooling last by `CoolUntil`); Task 6 through `handleLocal` with two Codex connections | `pool_type_test.go`: `TestFailoverPoolCoolingLast`; `local_codex_test.go`: `TestCodexFailoverNewSessionSkipsLimitedFirst`, `TestCodexSessionStaysOnConnection` |
| 3 | All-429 kept: cooldown 20s·2ⁿ, cooling ordered by `CoolUntil`, HTTP 429 to client, session stays on last member | Task 6: no routing change; per-provider auth only; pinned by regression tests through `handleLocal` | `local_codex_test.go`: `TestCodexFailoverNewSessionSkipsLimitedFirst`, `TestCodexAll429KeepsBehavior` |
| 4 | Codex provider without `auth_id`, name ≠ `codex` → rejected in `validate()`, error in log and UI; file never rewritten; only the one-time startup migration assigns an ID, after a backup | Task 1 (validate + startup migration with `.before-codex-ids` backup); Task 7 (log + settings banner, file bytes unchanged) | `codex_connections_test.go`: `TestCodexValidateRequiresAuthID`, `TestCodexStartupAssignsIDsOnceWithBackup`, `TestCodexLegacyFileUntouched`, `TestCodexManualReloadNeverAssignsID`; `ui_test.go`: `TestReloadErrorBanner` |

Plan review edits (review, ba194ec): edit 5 (balancing inert until `balance >= 2`) is withdrawn by Clarification 5. **6** — no switching inside a session: pinned while healthy, moved on failure and never returned → Task 6 `local_codex_test.go`: `TestCodexSessionStaysOnConnection`. Clarification 5 replaces review points 1–2 and edit 5: Task 5 is now the pool type, Task 6 the failover acceptance; `bindCandidatesIn`, per-connection session counters, the RR cursor and their tests are dropped (no distribution code was written).

Non-blocking notes, all covered: `refreshRejected` goes through `refreshLocked` (Task 2, `TestCodexConcurrentRejectedRefreshesOnce`); `/settings/codex/*` take `provider` and verify it (Tasks 3–4); tests named by file (table above, each task); failover tests set `balance: 3` to show it has no effect in a failover pool (Tasks 5–6); `auth_id` uses `omitempty` (Task 1, `TestCodexLegacyFileUntouched`, `TestCandidateSelectionStableForLegacy`).

Consequence to accept: a pre-existing non-`codex` Codex provider (if any exists at startup) gets a fresh ID and has no credential until an explicit login/import. The live config has only `codex`, so nothing changes there.

## Review Focus

1. A hand-edited `providers.json` with a second Codex provider but no `auth_id` while the router runs: old snapshot keeps serving, explicit error in log and settings, file left as the user wrote it (Task 7, `TestReloadErrorBanner`).
2. Deleting `work` and re-adding `work` while a browser login for the old `work` is pending: the finished login must not write tokens for the new `work` (Task 3, `TestCodexLoginForReplacedProviderIsDiscarded`).
3. Two router processes (old + new during a deploy) refreshing one connection's expired token at once: exactly one refresh request (Task 2, `TestCodexConcurrentExpiryRefreshesOnce`).
4. A failover pool whose first member is healthy but busy (requests in flight) with `balance` 3: new and bound sessions still go to the first member (Task 6, `TestCodexSessionStaysOnConnection`, sessions `s1` and `busy`).
5. A login/import/usage/status request naming a provider that is absent or not Codex: 400 with a message, no network, no file (Task 3, `TestCodexActionsRejectUnknownProvider`).

---

## Plan v3 addendum (read before the tasks; it overrides the v2 lines listed here)

v3 adds four things to v2:
- the plan-v2 review: edits 1–4 and two notes;
- Clarification 6: a `balance` pool type now, the type switchable at runtime, one replaceable criterion;
- the client-cancel fix;
- the public-repository rule.

Tasks 1–9 stay as written except where the table below overrides them. Tasks 10–12 are new. Apart from the overrides, the only edits to v2 text remove personal names and local paths (public-repository rule).

### Execution order

1. Task 0
2. Tasks 1, 2, 3 and 4
3. Task 5 steps 1–2 (red)
4. Task 6 step 1, then run it; record the red result
5. Task 5 steps 3–5
6. Task 6 steps 2–3
7. Task 10
8. Task 11
9. Task 12
10. Task 7
11. Task 8
12. Task 9

Every task is test-first: record the failing run before the code step.

### Overrides of v2 text

| v2 place | v3 rule |
|---|---|
| Header, Architecture: "only `failover`" | Two types: `failover` (the default; an absent type means failover) and `balance` (set explicitly). See Task 12. |
| Global Constraints: the "Pool type" line, including "No balancer…" | Replaced by "Global Constraints (v3)" below. |
| Review Points row 1: "only `failover`" | Types `failover` and `balance`; any other value is rejected. |
| Task 1 step 3: `loadLocalSetupChecked` keeps its signature and saves the migration itself | See "Task 1 alignment" below. The signature becomes `(localSetup, bool, error)` with no write inside; `loadConfigChecked` saves. |
| Task 5 rule 1, and `TestPoolTypeUnknownRejected` with `Type: "balance"` | The test uses `Type: "roundrobin"` and checks that the error names `roundrobin` and `pair`, because `balance` becomes valid in Task 12. The Task 5 message stays `(есть только failover)`; Task 12 widens it. |
| Task 5 `TestFailoverPoolCoolingLast`, the recovery step with `Score: 1` on both members | That test is green before the code exists (review note). Change the recovery step to set `hl.m["p/a"] = &modelStat{Score: 0.2, TTFBMs: 900}` and `hl.m["q/b"] = &modelStat{Score: 1, TTFBMs: 10}`, and keep expecting `p/a,q/b`. A rating order would put `q/b` first, so the test is red before Task 5 step 3 and pins pool order, not rating. |
| Task 5 step 3: `pick` condition `c.poolType == poolFailover` | Use `c.poolType != ""`. Both pool types keep pool order in `pick`. `balance` only reorders the first choice of a new session inside `bindCandidates` (Task 12), so Task 12 does not touch `pick`. |
| Task 5 rule 2 and step 3: `settings.Type = l.PoolSettings[name].Type` | `savePoolSettings` keeps the stored type only when `settings.Type == ""`. The form path merges through `updatePoolSettings`; see "Task 5 edit 3". |
| Task 5: the `balance` paragraph ("the field stays in the file and form") and the `ui/settings.html` hint for the balance field | Review edit 3, option (б), as tightened by the v3 verdict (edit 4). The pool's numeric `balance` leaves `poolSettings`, the pool form, `parsePoolSettings` and every file write. An old file that still has it is read, the value is ignored and one log line says so. The rating window outside pools comes only from the global `ROUTER_LOCAL_BALANCE` (`c.balance`: `checker.go`, the settings page), which is unchanged. The pool type is the `type` field (`Type`, `poolFailover`/`poolBalance`); no Go name is shared by the type and the old number. See "Task 5 edit 3". |
| Task 5 `TestPoolSaveKeepsType` | Replaced by the form-path version in "Task 5 edit 3". It also checks that the saved file has no `balance` key and the page has no `name="balance"`; `TestPoolSettingsLegacyBalanceIgnored` covers an old file. |
| Task 6: the recovery step in `TestCodexSessionStaysOnConnection` sets `hl.m["codex/gpt"].Score = 1` | Set `Score = 0.2` and `TTFBMs = 900`. The fresh session must still go to `codex/gpt`, and only pool order sends it there. |
| Task 6 step 2: "PASS once Tasks 2–5 are in", with the red shown by a temporary revert | Review edit 4. Write `local_codex_test.go` after Task 4 and before Task 5 step 3; the tests use no Task 5 identifier. Run them and record the red result in the receipt: `TestCodexSessionStaysOnConnection` fails with `busy: busy but healthy member skipped: work/gpt`, because rating plus `balance` 3 sends the busy new session to the idle second member. Task 5 step 3 then makes them green. |
| Task 6 `codexStatusByAccount` | Make its first line `if r.URL.Host != "chatgpt.com" { return usageResponse(400, `{"error":"invalid_grant"}`), nil }`. Any request to another host (the token refresh to `issuer.invalid`) then gets a 400, so a rejected token's refresh fails without network (Task 11). |
| Task 6 `runLocalSession` (described, not shown) | Use the code in Task 10 (`runLocalRequest`, `runLocalSession`). |
| Task 9 steps 4–6 | Replaced by the v3 Task 9 steps, which are updated in place. |

### Global Constraints (v3)

**Pool type**
- `pool_settings.<pool>.type` is `failover`, `balance`, or absent (= failover).
- No migration and no file rewrite.
- Any other value is rejected in `validate()` and shown by the Task 7 banner.
- `balance` is set explicitly per pool, in the form or the file.

**Balance pools**
- A balance pool chooses only the first connection of a new session.
- It chooses among connections (providers), never among models of one connection. Each connection is represented by its first member that is not cooling.
- The criterion is one function, `balanceCriterion`. Today it is the number of live bindings this pool has on that connection. The count comes from `h.sessions` under `h.mu` in `bindCandidates`, in the same critical section that records the binding.
- Ties keep pool order.
- Bound sessions never move while healthy. On failure a session moves and stays there (Clarifications 4 and 6.4).

**Runtime type switch**
- The type is read for every request from the current config snapshot.
- A form save or a settings reload switches it with no restart and without clearing bindings. Only new sessions follow the new type.
- Every binding records its pool and connection in both types, so a switch from failover to balance counts the sessions already running.

**Numeric `balance`**
- The pool's numeric `balance` (rating window) is gone from `poolSettings`, the pool form, `parsePoolSettings` and file writes.
- An old file with it still loads; the value is ignored with one log line and disappears at the next write. The global `ROUTER_LOCAL_BALANCE` is unchanged.

**Client cancel**
- A client cancel (Esc, closed connection) is not a model failure: record no fail and do not move the session.

**Signed-out Codex member**
- A Codex member is signed out when it has no credential file, or when the issuer rejected its token.
- It is skipped for new and bound sessions until the next login or import.
- A sign-in failure moves the request to the next member.
- If every member is signed out, they are still tried, so the client sees the sign-in error.

**Public repository**
- No personal names, session labels, pointers to private coordination notes, home-directory paths or e-mail addresses in commits, PR text, code comments or docs. Task 9 step 7 checks this.

### Review Points (v3 rows; they continue the table above)

| # | Review point | Closed by | Test (file: name) |
|---|---|---|---|
| 5 | `balance` type (Clarification 6.1–6.5): only new sessions; across connections, not models of one connection; counted per pool from bindings under `h.mu`; the criterion is one function | Task 12 | `pool_balance_test.go`: `TestBalancePoolSpreadsNewSessions`, `TestCodexSingleConnectionKeepsOrder`, `TestBalanceCountsPerPool`, `TestBalanceSkipsCoolingConnection`, `TestBalanceCriterionIsOneFunction`, `TestBalanceBoundSessionStays` |
| 6 | The type changes at runtime (6.6), from the form and from a reload; new sessions follow it; bound sessions stay; a switch from failover to balance sees the real load | Task 12 | `pool_balance_test.go`: `TestPoolSettingsFormSetsType`; `local_codex_test.go`: `TestCodexPoolTypeSwitchAtRuntime` |
| 7 | A signed-out Codex member fails over and is skipped until the next login (review edit 2) | Task 11 | `local_codex_test.go`: `TestCodexSignedOutMemberFailsOver` |
| 8 | A client cancel keeps the binding and records no failure | Task 10 | `local_codex_test.go`: `TestClientCancelKeepsBinding` |

The Clarification 5 failover acceptance stays in rows 2–3: `TestCodexFailoverNewSessionSkipsLimitedFirst`, `TestCodexSessionStaysOnConnection` and `TestFailoverPoolCoolingLast`.

### Task 0: Rebase onto main and clean the branch history (before any code)

The branch is not pushed yet. Its docs commits contain names and local paths that the public-repository rule forbids, so rewrite the history once, before the first push. The old history stays on a local branch.

- [ ] **Step 1:** In the worktree, run `git fetch origin`. The base is `origin/main`, which was 5273818 when this plan was written. It already contains f11cb64 (limits) and 5273818 (`docs/features.md`).
- [ ] **Step 2:** Run `git branch multi-codex-plan-history` to keep the old commits locally. Never push it; delete it only when asked.
- [ ] **Step 3:** Run `git rebase origin/main`. No conflicts are expected: the branch only adds `docs/superpowers/**` and `codex_connections_test.go`.
- [ ] **Step 4:** Run these in order:
  - `git reset --soft origin/main`
  - `git restore --staged codex_connections_test.go`
  - `git commit -m "docs: design and plan for multiple Codex connections"`
- [ ] **Step 5:** Run `git add codex_connections_test.go`, then `git commit -m "test: Codex connection identity and startup migration (red until Task 1)"`.
- [ ] **Step 6:** Run the Task 9 step 7 check over `git log -p origin/main..HEAD`. Expected: no output. Put the two new SHAs in the receipt.

### Task 1 alignment (overrides Task 1 step 3 where it differs)

The committed tests (`codex_connections_test.go:51,67,88`) fix the interface:

```go
// loadLocalSetupChecked reads providers.json strictly. assigned reports that a
// Codex provider without auth_id got a fresh ID in memory; the caller persists
// it once (startup only). The seed path for a missing file returns false.
func loadLocalSetupChecked(path string) (localSetup, bool, error)
```

`loadLocalSetupChecked` reads with `readProvidersWith(path, assignCodexAuthIDs)` and never writes. In `loadConfigChecked` (main.go), directly after the call:

```go
	local, assigned, err := loadLocalSetupChecked(providersPath())
	if err != nil {
		return c, err
	}
	if assigned {
		// One-time migration: persist the new auth_id values before the other
		// startup migrations read or rewrite the file.
		if err := saveConfigurationMigration(providersPath(), local, ".before-codex-ids"); err != nil {
			return c, err
		}
	}
```

Keep the error-return style that `loadConfigChecked` already uses.

The migration is idempotent:
- On a second start the IDs are already in the file, so `assigned` is false and nothing is written.
- `saveConfigurationMigration` never overwrites an existing backup (`O_EXCL`).
- `TestCodexStartupAssignsIDsOnceWithBackup` ("second start rewrote the file") and `TestCodexLegacyFileUntouched` pin this.

Also:
- Update `profiles_storage_test.go:376` to `if _, _, err := loadLocalSetupChecked(path); err == nil {`.
- `TestCandidateSelectionStableForLegacy` (affinity_test.go) stays in Task 1, as in v2.

### Task 5 edit 3: the numeric `balance` leaves the pool form (applied in Task 5 steps 1 and 3)

**Step 1.** Replace `TestPoolSaveKeepsType` with a test that goes through the form handler. It needs the imports `net/http/httptest` and `net/url`.

```go
func TestPoolSaveKeepsType(t *testing.T) {
	path := filepath.Join(t.TempDir(), "providers.json")
	cfg := twoMemberPool(map[string]poolSettings{"pair": {Type: poolFailover, FirstByteSec: 5}})
	cfg.upstream, _ = url.Parse("https://api.anthropic.com")
	if err := writeProviders(path, cfg.local); err != nil {
		t.Fatal(err)
	}
	cs := newConfigStore(cfg, path)
	u := newUIServer(newStore(10, ""), cs, newHealth(""))
	// The form has neither a type (until Task 12) nor the numeric balance.
	values := url.Values{"name": {"pair"}, "failover": {"1"}, "first_byte": {"7"}, "probe_every": {"0"}, "max_input_chars": {"0"}}
	r := httptest.NewRequest("POST", "/settings/pool-settings", strings.NewReader(values.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	u.settingsPoolSave(w, r)
	html := w.Body.String()
	if !strings.Contains(html, "Настройки пула сохранены") {
		t.Fatalf("save failed: %s", html)
	}
	if strings.Contains(html, `name="balance"`) {
		t.Fatal("pool form still offers the numeric balance")
	}
	l, err := readProviders(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := l.PoolSettings["pair"]; got.Type != poolFailover || got.FirstByteSec != 7 {
		t.Fatalf("saved %+v", got)
	}
}

// An old file with the pool's numeric balance still loads; the value is
// ignored, logged once per read, and gone after the next write.
func TestPoolSettingsLegacyBalanceIgnored(t *testing.T) {
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	var s poolSettings
	if err := json.Unmarshal([]byte(`{"failover":true,"first_byte_seconds":5,"balance":3}`), &s); err != nil {
		t.Fatal(err)
	}
	if s != (poolSettings{Failover: true, FirstByteSec: 5}) {
		t.Fatalf("read %+v", s)
	}
	if !strings.Contains(logs.String(), "balance") {
		t.Fatalf("no log line: %q", logs.String())
	}
	out, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "balance") {
		t.Fatalf("written back: %s", out)
	}
}
```

The fixture, the field names and the success text are the same as in `pool_settings_test.go` `TestPoolSettingsDashboardPersistsOnlySelectedPool`. Red before step 3:
- first, a build error on `Type` / `poolFailover`;
- then `pool form still offers the numeric balance`;
- `TestPoolSettingsLegacyBalanceIgnored`: `read {... Balance:3 ...}` until the field is removed.

**Step 3 additions.**

`pool_settings.go`:
- Remove `Balance` from `poolSettings`, from `validate`, from `apply` (the pool no longer sets `c.balance`; the global value stays) and from the `config.poolSettings` fallback literal. In `parsePoolSettings`, drop the `{"Распределение сессий", in.Balance, &s.Balance}` row.
- Add the legacy reader (imports `encoding/json`, `log`):

```go
// UnmarshalJSON accepts files written before the pool type existed: their
// numeric "balance" no longer does anything and is dropped at the next write.
func (s *poolSettings) UnmarshalJSON(data []byte) error {
	type plain poolSettings
	var in struct {
		plain
		Balance *json.RawMessage `json:"balance"`
	}
	if err := json.Unmarshal(data, &in); err != nil {
		return err
	}
	*s = poolSettings(in.plain)
	if in.Balance != nil {
		log.Printf("pool_settings: поле balance устарело и не используется; тип пула задаёт поле type")
	}
	return nil
}
```
- Split `savePoolSettings`, so that the merge happens under the store lock:

```go
// savePoolSettings stores settings as given; an empty Type keeps the stored one.
func (s *configStore) savePoolSettings(name string, settings poolSettings, expectedProfile ...string) error {
	return s.updatePoolSettings(name, func(old poolSettings) poolSettings {
		if settings.Type == "" {
			settings.Type = old.Type
		}
		return settings
	}, expectedProfile...)
}

// updatePoolSettings merges into the latest snapshot under the store lock, so
// a form never overwrites a field it does not show with a stale value.
func (s *configStore) updatePoolSettings(name string, merge func(old poolSettings) poolSettings, expectedProfile ...string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// ...the existing profile, pool-exists and migratePoolSettings checks, unchanged...
	l, _ := migratePoolSettings(s.c)
	settings := merge(l.PoolSettings[name])
	if err := settings.validate(); err != nil {
		return err
	}
	l.PoolSettings[name] = settings
	// ...the existing provPath, syncActiveProfile, writeProviders, mtime and s.c.local lines, unchanged...
}
```

`ui.go` `settingsPoolSave`:
- Drop `Balance: r.FormValue("balance")`.
- Replace the `savePoolSettings` call with:

```go
		err = u.cs.updatePoolSettings(r.FormValue("name"), func(old poolSettings) poolSettings {
			if settings.Type == "" {
				settings.Type = old.Type
			}
			return settings
		}, r.FormValue("profile"))
```

`ui/settings.html`:
- Remove the whole `<label>Распределять новые сессии между лучшими моделями…name="balance"…</label>` line from the pool form.
- Keep the v2 type hint line. Task 12 replaces it with the selector.

Existing tests:
- `pool_settings_test.go` `TestPoolSettingsDashboardPersistsOnlySelectedPool`: stop posting `"balance"`; `want` drops `Balance`.
- `profiles_test.go:411,428` (`savePoolSettings` with `Balance: 4`): use `ProbeSec: 4` instead.
- `profiles_storage_test.go:216-237`: use `FirstByteSec: 2` instead of `Balance: 2`; the stale profile is still rejected.

### Verdict edits on v3 (applied in place)

1. Task 8: the provider pane's import button carries its provider (`hx-vals`); `TestCodexProviderPaneImportNamesProvider`.
2. Task 8 README: `failover` is the default, `balance` is set explicitly on a pool.
3. Task 9 step 6 also updates §7 «Здоровье».
4. The pool's numeric `balance` is removed from the form and from file writes; an old file loads with one log line (`TestPoolSettingsLegacyBalanceIgnored`). The global `ROUTER_LOCAL_BALANCE` is unchanged.
5. Task 2 keeps `withFileLock` from `091645c` verbatim (Clarification 2). `withLimitsLock` gives up after 2 s and cannot be cancelled, while a token refresh must wait on the request context, so the two do not share semantics; merging them is left for later.

---

### Task 1: Connection identity, `validate()` rule, one-time startup migration

**Files:**
- Modify: `providers.go:19-24` (`provider`), `providers.go:120-150` (`validate`), `providers.go:271-323` (`readProviders`), `providers.go:424-440` (`loadLocalSetupChecked`)
- Modify: `ui.go:797-860` (`settingsProviders`: `add` assigns ID, `update` rejects Codex rename)
- Create: `codex_ids.go`
- Test: `codex_connections_test.go` (new), `affinity_test.go`

**Interfaces:**
- Produces: `provider.AuthID string` (`json:"auth_id,omitempty"`); `newCodexAuthID() string` (32 lowercase hex); `authIDOK(string) bool`; `assignCodexAuthIDs(*localSetup) bool`; `readProvidersWith(path string, prepare func(*localSetup) bool) (localSetup, bool, error)`. `readProviders(path)` keeps its signature and stays strict (nil prepare) — it is what the watcher uses. `loadLocalSetupChecked(path)` keeps its signature (main.go:73, profiles_storage_test.go:376 untouched) and performs the migration.

- [ ] **Step 1: Write the failing tests** (`codex_connections_test.go`)

```go
package main

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testAuthA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const testAuthB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func TestCodexValidateRequiresAuthID(t *testing.T) {
	missing := localSetup{Providers: []provider{{Name: "work", Type: "codex"}}}
	if err := missing.validate(); err == nil || !strings.Contains(err.Error(), "auth_id") {
		t.Fatalf("non-codex name without auth_id: %v", err)
	}
	legacy := localSetup{Providers: []provider{{Name: "codex", Type: "codex"}}}
	if err := legacy.validate(); err != nil {
		t.Fatalf("legacy codex rejected: %v", err)
	}
	for name, bad := range map[string]localSetup{
		"malformed": {Providers: []provider{{Name: "work", Type: "codex", AuthID: "XYZ"}}},
		"non-codex": {Providers: []provider{{Name: "p", BaseURL: "http://x.test/v1", AuthID: testAuthA}}},
		"duplicate": {Providers: []provider{{Name: "a", Type: "codex", AuthID: testAuthA}, {Name: "b", Type: "codex", AuthID: testAuthA}}},
		"long name": {Providers: []provider{{Name: strings.Repeat("n", 65), Type: "codex", AuthID: testAuthA}}},
	} {
		if bad.validate() == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func writeRaw(t *testing.T, path, raw string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCodexStartupAssignsIDsOnceWithBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "providers.json")
	raw := `{"providers":[{"name":"codex","type":"codex","base_url":"` + codexBaseURL + `"},{"name":"work","type":"codex","base_url":"` + codexBaseURL + `"}]}`
	writeRaw(t, path, raw)
	l, err := loadLocalSetupChecked(path)
	if err != nil {
		t.Fatal(err)
	}
	if l.Providers[0].AuthID != "" || !authIDOK(l.Providers[1].AuthID) {
		t.Fatalf("ids: %q %q", l.Providers[0].AuthID, l.Providers[1].AuthID)
	}
	if backup, _ := os.ReadFile(path + ".before-codex-ids"); string(backup) != raw {
		t.Fatal("backup missing or wrong")
	}
	first, _ := os.ReadFile(path)
	again, err := loadLocalSetupChecked(path)
	if err != nil || again.Providers[1].AuthID != l.Providers[1].AuthID {
		t.Fatalf("second start changed id: %v", err)
	}
	if second, _ := os.ReadFile(path); string(second) != string(first) {
		t.Fatal("second start rewrote the file")
	}
	if _, err := readProviders(path); err != nil {
		t.Fatalf("migrated file is not strictly valid: %v", err)
	}
}

func TestCodexLegacyFileUntouched(t *testing.T) {
	path := filepath.Join(t.TempDir(), "providers.json")
	if err := writeProviders(path, localSetup{Providers: []provider{{Name: "codex", Type: "codex", BaseURL: codexBaseURL}}}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	if _, err := loadLocalSetupChecked(path); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) || strings.Contains(string(after), "auth_id") {
		t.Fatal("legacy file rewritten")
	}
	if _, err := os.Stat(path + ".before-codex-ids"); !os.IsNotExist(err) {
		t.Fatal("legacy start made a backup")
	}
}

func TestCodexManualReloadNeverAssignsID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "providers.json")
	raw := `{"providers":[{"name":"work","type":"codex","base_url":"` + codexBaseURL + `"}]}`
	writeRaw(t, path, raw)
	if _, err := readProviders(path); err == nil || !strings.Contains(err.Error(), "auth_id") {
		t.Fatalf("strict read accepted: %v", err)
	}
	if data, _ := os.ReadFile(path); string(data) != raw {
		t.Fatal("strict read rewrote the file")
	}
}

func TestCodexProviderAddAssignsFreshIDAndRejectsRename(t *testing.T) {
	u, h := testUI(t)
	get(t, h, "POST", "/settings/providers", url.Values{"op": {"add"}, "name": {"work"}, "type": {"codex"}})
	p, _ := u.cs.get().local.provider("work")
	if !authIDOK(p.AuthID) {
		t.Fatalf("add gave id %q", p.AuthID)
	}
	w := get(t, h, "POST", "/settings/providers", url.Values{"op": {"update"}, "orig": {"work"}, "name": {"work2"}})
	if !strings.Contains(w.Body.String(), "переименовать") {
		t.Fatal("codex rename not rejected")
	}
	get(t, h, "POST", "/settings/providers", url.Values{"op": {"remove"}, "name": {"work"}})
	get(t, h, "POST", "/settings/providers", url.Values{"op": {"add"}, "name": {"work"}, "type": {"codex"}})
	again, _ := u.cs.get().local.provider("work")
	if again.AuthID == p.AuthID {
		t.Fatal("recreated provider reused the old id")
	}
}
```

When writing this test, check the `remove` case of `settingsProviders` and use its exact field name if it is not `name`.

Append to `affinity_test.go`:

```go
func TestCandidateSelectionStableForLegacy(t *testing.T) {
	data, _ := json.Marshal(provider{Name: "codex", Type: "codex", BaseURL: codexBaseURL})
	if strings.Contains(string(data), "auth_id") {
		t.Fatal("empty auth_id serialized; legacy bindings would change")
	}
}
```

- [ ] **Step 2: Run to see them fail**

Run: `go test -count=1 -run 'TestCodex(Validate|Startup|Legacy|Manual|ProviderAdd)|TestCandidateSelectionStable' .`
Expected: compile error `unknown field AuthID` / `undefined: authIDOK`.

- [ ] **Step 3: Implement**

`providers.go` `provider` gains `AuthID string \`json:"auth_id,omitempty"\`` after `APIKey`.

`codex_ids.go`:

```go
package main

import (
	"crypto/rand"
	"encoding/hex"
)

// newCodexAuthID names one connection's credential slot for its whole life.
func newCodexAuthID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func authIDOK(id string) bool {
	if len(id) != 32 {
		return false
	}
	for _, r := range id {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

// assignCodexAuthIDs is the one-time startup migration: only a pre-existing
// non-legacy Codex provider without an id gets one. The legacy "codex" keeps
// the old credential file and needs no id.
func assignCodexAuthIDs(l *localSetup) bool {
	changed := false
	for i := range l.Providers {
		p := &l.Providers[i]
		if p.Type == "codex" && p.AuthID == "" && strings.TrimSpace(p.Name) != "codex" {
			p.AuthID = newCodexAuthID()
			changed = true
		}
	}
	return changed
}
```

(import `strings` too.)

`validate()`: declare `authIDs := map[string]bool{}` beside `seen`; inside the provider loop, after the Codex BaseURL/APIKey check and before `providerNameOK`:

```go
		if p.Type == "codex" {
			if p.AuthID == "" && p.Name != "codex" {
				return fmt.Errorf("provider %q: у подключения Codex нет auth_id; удалите его и добавьте заново через дашборд", p.Name)
			}
			if p.AuthID != "" && !authIDOK(p.AuthID) {
				return fmt.Errorf("provider %q: неверный auth_id", p.Name)
			}
			if len(p.Name) > 64 {
				return fmt.Errorf("provider %q: имя подключения Codex длиннее 64 байт", p.Name)
			}
			if p.AuthID != "" && authIDs[p.AuthID] {
				return fmt.Errorf("provider %q: auth_id повторяется", p.Name)
			}
			authIDs[p.AuthID] = true
		} else if p.AuthID != "" {
			return fmt.Errorf("provider %q: auth_id бывает только у Codex", p.Name)
		}
```

`readProviders` body moves into `readProvidersWith(path, prepare)`; immediately before `l.validateProfiles()`:

```go
	changed := false
	if prepare != nil {
		changed = prepare(&l)
	}
```

returning `(l, changed, err)` on every path. `readProviders` becomes:

```go
func readProviders(path string) (localSetup, error) {
	l, _, err := readProvidersWith(path, nil)
	return l, err
}
```

`loadLocalSetupChecked`: replace `l, err := readProviders(path); if err == nil { return l, nil }` with

```go
		l, assigned, err := readProvidersWith(path, assignCodexAuthIDs)
		if err == nil {
			if assigned {
				if err := saveConfigurationMigration(path, l, ".before-codex-ids"); err != nil {
					return localSetup{}, fmt.Errorf("codex id migration: %w", err)
				}
			}
			return l, nil
		}
```

`saveConfigurationMigration` (migration.go:105) already backs up with `O_EXCL` and writes through `writeProviders` (atomic, skips unchanged bytes).

`ui.go` `settingsProviders`:
- `add`: extend the Codex branch to `p.BaseURL, p.APIKey, p.AuthID = codexBaseURL, "", newCodexAuthID()`.
- `update`: right after `p := &l.Providers[idx]`, before `p.Name = name`:
  ```go
  		if p.Type == "codex" && name != orig {
  			err = fmt.Errorf("подключение Codex нельзя переименовать: удалите и добавьте заново со входом")
  			break
  		}
  ```
- `remove`: unchanged (auth file stays; documented in Task 8).

- [ ] **Step 4: Run tests**

Run: `go test -count=1 ./...`
Expected: PASS. Fix any existing fixture that builds a non-`codex` Codex provider without an ID and passes through `validate` by adding `AuthID: testAuthA`.

- [ ] **Step 5: Commit**

```bash
git add providers.go codex_ids.go ui.go codex_connections_test.go affinity_test.go
git commit -m "codex: persistent auth_id, strict validate, one-time startup migration"
```

### Task 2: Per-provider auth store and cross-process lock

**Files:**
- Create: `filelock.go` (verbatim from `091645c`), `codex_stores.go`
- Modify: `codex_auth.go:87-125` (`credentialFor`), `codex_auth.go:221-234` (`importFromCLI`), `codex_auth.go:296-315` (`refreshRejected`), `codex_oauth.go:125-` (`finishBrowserFlow` final save under the lock)
- Test: `codex_test.go`, `codex_connections_test.go`

**Interfaces:**
- Consumes: `provider.AuthID`, `authIDOK` (Task 1).
- Produces: `codexStoreFor(p provider) (*codexAuthStore, error)`; `withFileLock(ctx context.Context, path string, fn func() error) error`; `(s *codexAuthStore) refreshLocked(ctx context.Context) error`; test helper `useTestCodexHome(t, issuer string, client *http.Client)`.

- [ ] **Step 1: Write the failing tests**

`codex_connections_test.go` (add `net/http` import):

```go
func useTestCodexHome(t *testing.T, issuer string, client *http.Client) {
	t.Helper()
	old := codexAuth
	t.Cleanup(func() { codexAuth = old })
	codexAuth = &codexAuthStore{path: filepath.Join(t.TempDir(), "codex-auth.json"), cliPath: filepath.Join(t.TempDir(), "cli.json"), issuer: issuer, client: client}
}

func TestCodexStoreForSeparatesConnections(t *testing.T) {
	useTestCodexHome(t, "http://issuer.invalid", http.DefaultClient)
	legacy, err := codexStoreFor(provider{Name: "codex", Type: "codex"})
	if err != nil || legacy != codexAuth {
		t.Fatalf("legacy store: %v", err)
	}
	a, _ := codexStoreFor(provider{Name: "codex", Type: "codex", AuthID: testAuthA})
	b, _ := codexStoreFor(provider{Name: "work", Type: "codex", AuthID: testAuthB})
	if a == codexAuth || a == b || a.path == b.path || filepath.Dir(a.path) != filepath.Dir(codexAuth.path) {
		t.Fatalf("paths %q %q", a.path, b.path)
	}
	if again, _ := codexStoreFor(provider{Name: "work", Type: "codex", AuthID: testAuthB}); again != b {
		t.Fatal("store not cached")
	}
	if _, err := codexStoreFor(provider{Name: "work", Type: "codex"}); err == nil {
		t.Fatal("non-legacy without id got a store")
	}
	if err := codexAuth.save(usageCredential("legacy")); err != nil {
		t.Fatal(err)
	}
	if b.connected() {
		t.Fatal("new connection adopted the legacy token")
	}
	if err := b.save(usageCredential("old-work")); err != nil {
		t.Fatal(err)
	}
	recreated, _ := codexStoreFor(provider{Name: "work", Type: "codex", AuthID: strings.Repeat("c", 32)})
	if recreated.connected() {
		t.Fatal("recreated name adopted the old id's token")
	}
}
```

`codex_test.go` (add `context`, `sync`, `sync/atomic` imports as needed):

```go
func TestCodexConcurrentExpiryRefreshesOnce(t *testing.T) {
	var refreshes atomic.Int32
	fresh := testJWT(time.Now().Add(2 * time.Hour))
	oauth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshes.Add(1)
		time.Sleep(50 * time.Millisecond)
		fmt.Fprintf(w, `{"access_token":%q,"refresh_token":"rotated"}`, fresh)
	}))
	defer oauth.Close()
	path := filepath.Join(t.TempDir(), "auth.json")
	expired := usageCredential("acct")
	expired.Tokens.AccessToken = testJWT(time.Now().Add(-time.Minute))
	if err := (&codexAuthStore{path: path}).save(expired); err != nil {
		t.Fatal(err)
	}
	// Two stores on one path stand in for two router processes.
	one := &codexAuthStore{path: path, issuer: oauth.URL, client: oauth.Client()}
	two := &codexAuthStore{path: path, issuer: oauth.URL, client: oauth.Client()}
	var wg sync.WaitGroup
	for _, s := range []*codexAuthStore{one, two} {
		wg.Add(1)
		go func(s *codexAuthStore) {
			defer wg.Done()
			if c, err := s.credentialFor(context.Background()); err != nil || c.Tokens.AccessToken != fresh {
				t.Errorf("credential: %v", err)
			}
		}(s)
	}
	wg.Wait()
	if refreshes.Load() != 1 {
		t.Fatalf("refreshes=%d", refreshes.Load())
	}
}

func TestCodexConcurrentRejectedRefreshesOnce(t *testing.T) {
	var refreshes atomic.Int32
	fresh := testJWT(time.Now().Add(2 * time.Hour))
	oauth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshes.Add(1)
		time.Sleep(50 * time.Millisecond)
		fmt.Fprintf(w, `{"access_token":%q,"refresh_token":"rotated"}`, fresh)
	}))
	defer oauth.Close()
	path := filepath.Join(t.TempDir(), "auth.json")
	cred := usageCredential("acct")
	if err := (&codexAuthStore{path: path}).save(cred); err != nil {
		t.Fatal(err)
	}
	one := &codexAuthStore{path: path, issuer: oauth.URL, client: oauth.Client(), loaded: true, credential: cred}
	two := &codexAuthStore{path: path, issuer: oauth.URL, client: oauth.Client(), loaded: true, credential: cred}
	var wg sync.WaitGroup
	for _, s := range []*codexAuthStore{one, two} {
		wg.Add(1)
		go func(s *codexAuthStore) {
			defer wg.Done()
			if c, err := s.refreshRejected(context.Background(), cred.Tokens.AccessToken, "acct"); err != nil || c.Tokens.AccessToken != fresh {
				t.Errorf("rejected refresh: %v", err)
			}
		}(s)
	}
	wg.Wait()
	if refreshes.Load() != 1 {
		t.Fatalf("refreshes=%d", refreshes.Load())
	}
}
```

The second store takes the lock after the first wrote, sees disk tokens differ from its memory, adopts them, and returns early because they are fresh; `refreshRejected` then returns the adopted credential.

- [ ] **Step 2: Run to see them fail**

Run: `go test -count=1 -run 'TestCodexStoreFor|TestCodexConcurrent(Expiry|Rejected)' .`
Expected: `undefined: codexStoreFor`; once it exists, the concurrency tests fail with `refreshes=2`.

- [ ] **Step 3: Implement**

`filelock.go`: `git show 091645c:filelock.go > filelock.go`, then confirm it contains only `withFileLock` (imports context, errors, os, path/filepath, syscall, time).

`codex_auth.go`, add `refreshLocked` exactly as in `091645c`:

```go
func (s *codexAuthStore) refreshLocked(ctx context.Context) error {
	return withFileLock(ctx, s.path+".lock", func() error {
		if disk, err := readCodexCredential(s.path); err == nil {
			if disk.Tokens.RefreshToken != s.credential.Tokens.RefreshToken || disk.Tokens.AccessToken != s.credential.Tokens.AccessToken {
				s.credential = disk
				if jwtExpiry(disk.Tokens.AccessToken).After(time.Now().Add(30 * time.Second)) {
					return nil
				}
			}
		} else if !os.IsNotExist(err) {
			return err
		}
		return s.refresh(ctx)
	})
}
```

- `credentialFor` and `refreshRejected`: replace `s.refresh(ctx)` with `s.refreshLocked(ctx)` (no `s.life` branch).
- `importFromCLI`: wrap `save` plus the state update in `withFileLock(context.Background(), s.path+".lock", func() error { ... })`.
- `finishBrowserFlow`: wrap its final save plus state update the same way (Task 3 adds the `stillCurrent` check inside this section).

`codex_stores.go`:

```go
package main

import (
	"encoding/hex"
	"fmt"
	"path/filepath"
	"sync"
)

var codexStores = struct {
	sync.Mutex
	m map[string]*codexAuthStore
}{m: map[string]*codexAuthStore{}}

// codexStoreFor gives each Codex connection its own credential slot. The
// pre-existing "codex" without an id keeps the legacy file; any other slot
// lives beside it and is named by both provider name and id, so a recreated
// name never reads an old token.
func codexStoreFor(p provider) (*codexAuthStore, error) {
	if p.Type != "codex" {
		return nil, fmt.Errorf("provider %q: не Codex", p.Name)
	}
	if p.AuthID == "" {
		if p.Name == "codex" || p.Name == "" {
			return codexAuth, nil
		}
		return nil, fmt.Errorf("provider %q: у подключения Codex нет auth_id", p.Name)
	}
	if !authIDOK(p.AuthID) {
		return nil, fmt.Errorf("provider %q: неверный auth_id", p.Name)
	}
	path := filepath.Join(filepath.Dir(codexAuth.path), "codex-auth-"+hex.EncodeToString([]byte(p.Name))+"-"+p.AuthID+".json")
	codexStores.Lock()
	defer codexStores.Unlock()
	if s, ok := codexStores.m[path]; ok {
		return s, nil
	}
	s := &codexAuthStore{path: path, cliPath: codexAuth.cliPath, issuer: codexAuth.issuer, client: codexAuth.client}
	codexStores.m[path] = s
	return s, nil
}
```

`p.Name == ""` keeps the nameless test fixtures on the legacy store until Task 4 names them. Tests use a fresh `t.TempDir()` per `codexAuth`, so cached paths never collide across tests.

- [ ] **Step 4: Run tests**

Run: `go test -count=1 ./...` then `go test -race -count=1 -run TestCodex .`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add filelock.go codex_stores.go codex_auth.go codex_oauth.go codex_test.go codex_connections_test.go
git commit -m "codex: per-connection auth store with verbatim file lock"
```

### Task 3: OAuth and CLI import target one named provider

**Files:**
- Modify: `ui.go:128-132` (routes unchanged, handlers read `provider`), `ui.go:493-502` (`codexLoginView`), `ui.go:925-985` (`settingsCodexImport`, `settingsCodexLogin`), `ui.go` `settingsCodexStatus`, `uiServer` fields (`oauthTarget provider`)
- Modify: `codex_oauth.go:125` (`finishBrowserFlow` signature)
- Test: `codex_test.go` (`TestCodexBrowserLogin` updated), `codex_connections_test.go`

**Interfaces:**
- Consumes: `codexStoreFor` (Task 2).
- Produces: `(u *uiServer) codexProvider(r *http.Request) (provider, *codexAuthStore, error)` — reads `r.FormValue("provider")`, finds it in `u.cs.get().local.Providers`, requires `Type == "codex"`, returns its store; `codexLoginView{Provider string; Connected, Pending bool; Error string}`; `(u *uiServer) codexLoginViewFor(p provider) codexLoginView`; `finishBrowserFlow(ctx, flow, stillCurrent func() bool) error`.

- [ ] **Step 1: Write the failing tests** (`codex_connections_test.go`)

```go
func codexUI(t *testing.T) (*uiServer, http.Handler) {
	t.Helper()
	u, h := testUI(t)
	l := u.cs.get().local.clone()
	l.Providers = append(l.Providers,
		provider{Name: "codex", Type: "codex", BaseURL: codexBaseURL},
		provider{Name: "work", Type: "codex", BaseURL: codexBaseURL, AuthID: testAuthB})
	c := u.cs.get()
	c.local = l
	u.cs.c = c
	return u, h
}

func TestCodexActionsRejectUnknownProvider(t *testing.T) {
	useTestCodexHome(t, "http://issuer.invalid", http.DefaultClient)
	_, h := codexUI(t)
	for _, target := range []string{"/settings/codex/import", "/settings/codex/login", "/settings/codex/usage"} {
		for _, name := range []string{"", "missing", "p"} {
			req := httptest.NewRequest("POST", target, strings.NewReader(url.Values{"provider": {name}}.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != http.StatusBadRequest {
				t.Errorf("%s provider=%q: %d", target, name, w.Code)
			}
		}
	}
	if entries, _ := os.ReadDir(filepath.Dir(codexAuth.path)); len(entries) != 0 {
		t.Fatalf("files written: %v", entries)
	}
}

func TestCodexImportTargetsNamedProvider(t *testing.T) {
	useTestCodexHome(t, "http://issuer.invalid", http.DefaultClient)
	u, h := codexUI(t)
	data, _ := json.Marshal(usageCredential("work-account"))
	writeRaw(t, codexAuth.cliPath, string(data))
	get(t, h, "POST", "/settings/codex/import", url.Values{"provider": {"work"}})
	work, _ := codexStoreFor(provider{Name: "work", Type: "codex", AuthID: testAuthB})
	if !work.connected() || codexAuth.connected() {
		t.Fatal("import went to the wrong connection")
	}
	if v := u.codexLoginViewFor(provider{Name: "codex", Type: "codex"}); v.Connected {
		t.Fatal("legacy shows work's login")
	}
}
```

`TestCodexBrowserLoginTargetsPending` and `TestCodexLoginForReplacedProviderIsDiscarded`: build on `TestCodexBrowserLogin` in `codex_test.go` (it already drives `startCodexBrowserFlow` against an `httptest` issuer and hits the callback). Keep its harness and change the steps to:

```go
	// 1) POST /settings/codex/login provider=work → 303 to the issuer URL.
	// 2) POST /settings/codex/login provider=codex while pending → 409, body names "work".
	// 3) POST /settings/codex/login provider=work while pending → 303 to the same URL.
	// 4) complete the callback → work's store connected, legacy not.
	// Replaced variant: after step 1, remove "work" and add "work" again (new AuthID)
	// through /settings/providers, then complete the callback:
	//   the old-id file and the new-id file are both absent,
	//   codexLoginViewFor(new work).Error == "" and Connected == false,
	//   the flow's stored error mentions "подключение изменилось".
```

Each numbered line becomes an assertion in the test body using the existing helpers in `TestCodexBrowserLogin` (read it before writing; do not change `startCodexBrowserFlow`).

- [ ] **Step 2: Run to see them fail**

Run: `go test -count=1 -run 'TestCodex(ActionsReject|ImportTargets|BrowserLogin)' .`
Expected: FAIL (`codexLoginViewFor` undefined; import writes the legacy file).

- [ ] **Step 3: Implement**

`ui.go`:

```go
// codexProvider resolves the connection a /settings/codex/* action names,
// checked against the config in force now.
func (u *uiServer) codexProvider(r *http.Request) (provider, *codexAuthStore, error) {
	name := strings.TrimSpace(r.FormValue("provider"))
	p, ok := u.cs.get().local.provider(name)
	if !ok || p.Type != "codex" {
		return provider{}, nil, fmt.Errorf("подключение Codex %q не найдено", name)
	}
	s, err := codexStoreFor(p)
	return p, s, err
}
```

Each of `settingsCodexImport`, `settingsCodexLogin`, `settingsCodexStatus`, `settingsCodexUsage` begins (after `sameOriginPost` where present) with:

```go
	p, store, err := u.codexProvider(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
```

Import uses `store.importFromCLI()`. Login:
- add field `oauthTarget provider` to `uiServer` beside `oauthFlow`.
- pending branch: if `u.oauthTarget.Name == p.Name && u.oauthTarget.AuthID == p.AuthID` redirect to the pending URL as today; otherwise `http.Error(w, fmt.Sprintf("идёт вход для %q; дождитесь его окончания", u.oauthTarget.Name), http.StatusConflict)`.
- on start set `u.oauthTarget = p`; goroutine calls

```go
		err := store.finishBrowserFlow(context.Background(), flow, func() bool {
			now, ok := u.cs.get().local.provider(p.Name)
			return ok && now.Type == "codex" && now.AuthID == p.AuthID
		})
```

`codex_oauth.go` `finishBrowserFlow(ctx, flow, stillCurrent func() bool)`: inside the locked save section added in Task 2, first line:

```go
		if stillCurrent != nil && !stillCurrent() {
			return errors.New("подключение изменилось во время входа; войдите заново")
		}
```

`codexLoginViewFor(p)`: `store, err := codexStoreFor(p)`; `Pending`/`Error` come from `u.oauthStatus`/`u.oauthError` only when `u.oauthTarget` has the same Name and AuthID; otherwise `Error = store.authStatus()`; `Connected = store.connected()`; `Provider = p.Name`. `err` from `codexStoreFor` becomes `Error`. Remove the old `codexLoginView()`; `settingsView` builds a `Logins []codexLoginView` (one per Codex provider, config order) in place of `Login`. `settingsCodexStatus` renders `codex-login` with `u.codexLoginViewFor(p)`.

- [ ] **Step 4: Run tests**

Run: `go test -count=1 ./...`
Expected: PASS (templates are changed in Task 8; until then the `codex-login` template receives the same fields plus `Provider`, and `settings.html` ranges over `.Logins` — make that one-line template change here so the suite compiles and renders).

- [ ] **Step 5: Commit**

```bash
git add ui.go codex_oauth.go ui/settings.html codex_test.go codex_connections_test.go
git commit -m "codex: OAuth and import target one named connection"
```

### Task 4: Requests, probe and usage per connection

**Files:**
- Modify: `local.go:210-245` (`tryModel` Codex branch), `ui.go:1210-1240` (`probeProvider`), `codex_usage.go:54-70,330-345` (`codexUsageView.Provider`, per-connection caches, handler)
- Test: `codex_reauth_test.go:36,63` (name fixtures), `codex_usage_test.go`, `codex_connections_test.go`

**Interfaces:**
- Consumes: `codexStoreFor` (Task 2), `u.codexProvider` (Task 3).
- Produces: `uiServer.codexUsages map[string]*codexUsageCache` (key `name+"\x00"+authID`, guarded by `u.probeMu`-independent `u.usageMu sync.Mutex`); `u.codexUsage` stays as the template whose `client` new caches copy (keeps `TestCodexUsagePanelAndManualButton` compiling); `codexUsageView.Provider string`.

- [ ] **Step 1: Write the failing tests** (`codex_connections_test.go`)

```go
func seedConnection(t *testing.T, p provider, account string) codexCredential {
	t.Helper()
	s, err := codexStoreFor(p)
	if err != nil {
		t.Fatal(err)
	}
	c := usageCredential(account)
	c.Tokens.AccessToken = testJWT(time.Now().Add(time.Hour)) + "." + account
	if err := s.save(c); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCodexRequestsUseTheirOwnAccount(t *testing.T) {
	useTestCodexHome(t, "http://issuer.invalid", http.DefaultClient)
	old := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = old })
	a := provider{Name: "codex", Type: "codex", BaseURL: codexBaseURL}
	b := provider{Name: "work", Type: "codex", BaseURL: codexBaseURL, AuthID: testAuthB}
	ca, cb := seedConnection(t, a, "acct-a"), seedConnection(t, b, "acct-b")
	seen := map[string]string{}
	var mu sync.Mutex
	http.DefaultTransport = usageTransport(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		seen[r.Header.Get("ChatGPT-Account-Id")] = r.Header.Get("Authorization")
		mu.Unlock()
		return usageResponse(200, `{"ok":true}`), nil
	})
	for _, p := range []provider{a, b} {
		res := tryModel(httptest.NewRequest("POST", "/", nil), config{firstByte: time.Second}, candidate{Key: p.Name + "/m", Provider: p}, []byte(`{}`), false)
		if res.err != nil {
			t.Fatal(res.err)
		}
		res.resp.Body.Close()
		res.cancel()
	}
	if seen["acct-a"] != "Bearer "+ca.Tokens.AccessToken || seen["acct-b"] != "Bearer "+cb.Tokens.AccessToken {
		t.Fatalf("accounts mixed: %v", seen)
	}
}
```

If `testJWT` output must stay a valid 3-part JWT for `jwtExpiry`, replace the `+ "." + account` suffix with a distinct `exp` per account (`time.Hour` vs `2*time.Hour`) — the test only needs two different tokens.

Probe: `TestCodexProbeUsesOwnAccount` — same transport, call `u.probeProvider(a, true)` and `u.probeProvider(b, true)` from `codexUI`, assert the `/models` requests carried `acct-a` then `acct-b`; then `u.probeProvider(b, false)` after re-adding `work` with a new AuthID must re-probe (cache checks AuthID).

Usage (`codex_usage_test.go`, extend `TestCodexUsagePanelAndManualButton`): GET `/settings/codex/usage?provider=work` performs no network (transport counter stays 0) and renders `class="codex-usage"` with `"provider":"work"`; POST `provider=work` queries once with `acct-b`; POST `provider=codex` queries with `acct-a`; the two views show different account IDs.

In `codex_reauth_test.go:36` and `:63` change `provider{Type: "codex"}` to `provider{Name: "codex", Type: "codex"}`.

- [ ] **Step 2: Run to see them fail**

Run: `go test -count=1 -run 'TestCodex(RequestsUse|ProbeUses|Usage)' .`
Expected: FAIL — both requests carry `acct-a` (global store).

- [ ] **Step 3: Implement**

`local.go` Codex branch of `tryModel`:

```go
		store, err := codexStoreFor(cand.Provider)
		if err != nil {
			cancel()
			return attemptResult{err: err}
		}
		if err := store.authorize(ctx, up); err != nil {
```

and `store.doWithReauth(client, up)` in place of `codexAuth.doWithReauth`. (Match the exact `attemptResult` the branch returns today on an `authorize` error; the line above is its minimal form.)

`probeProvider`: cache hit also requires `old.AuthID == p.AuthID` (add `AuthID string` to `probeResult`, set it in `res`); Codex branch calls `codexStoreFor(p)` and `store.authorize(req.Context(), req)`, setting `res.Msg` on error.

`codex_usage.go` `settingsCodexUsage`:

```go
	p, store, err := u.codexProvider(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	view := u.usageCache(p).get(ctx, store, r.Method == http.MethodPost)
	view.Provider = p.Name
```

```go
func (u *uiServer) usageCache(p provider) *codexUsageCache {
	u.usageMu.Lock()
	defer u.usageMu.Unlock()
	if u.codexUsages == nil {
		u.codexUsages = map[string]*codexUsageCache{}
	}
	key := p.Name + "\x00" + p.AuthID
	c, ok := u.codexUsages[key]
	if !ok {
		c = &codexUsageCache{client: u.codexUsage.client}
		u.codexUsages[key] = c
	}
	return c
}
```

- [ ] **Step 4: Run tests**

Run: `go test -count=1 ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add local.go ui.go codex_usage.go codex_reauth_test.go codex_usage_test.go codex_connections_test.go
git commit -m "codex: requests, probe and usage use the named connection"
```

### Task 5: Pool type `failover` and pool-order routing (Clarification 5)

**Files:**
- Modify: `pool_settings.go` (`poolSettings.Type`, `poolFailover`, `validate`, `savePoolSettings` keeps `Type`), `main.go:23-33` (`config.poolType`), `main.go:114-148` (`forModel` pool branch), `health.go:209-270` (`pick`), `ui/settings.html:36-44` (type line, balance hint)
- Create: `pool_type_test.go`

**Interfaces:**
- Produces: `const poolFailover = "failover"`; `poolSettings.Type string` with tag `json:"type,omitempty"`; `config.poolType string`; `forModel` sets `next.poolType = poolFailover` for every `pool` route. Task 6 relies on `forModel` producing a failover-ordered config.

Rule (exactly what the tests pin):
1. `poolSettings.validate()`: `Type` is `""` or `"failover"`; anything else → `тип пула "<v>" не поддерживается (есть только failover)`. `localSetup.validate()` already wraps it as `пул <name>: …` (providers.go:187), so a hand-edited file with `"type":"balance"` is rejected and shown by the Task 7 banner; the file is not rewritten.
2. No migration: an absent `type` means failover and is never written back. `savePoolSettings` copies the existing `Type` into the saved settings (the form has no type field), so an explicit `"failover"` in the file survives a UI save.
3. `forModel`, `pool` branch: `next.poolType = poolFailover` whether or not a `PoolSettings` entry exists. `model` routes and the global config keep `poolType == ""`.
4. `pick` with `c.poolType == poolFailover` (after the existing `!c.failover` early return): members that are not cooling, in pool order (`l.ordered()`: `Preferred` = first pool target, then the rest in target order, main.go:134-145); then cooling members ordered by `CoolUntil`. Score, TTFB, `Preferred` rank and `balance` are not consulted. `poolType == ""` keeps today's rating + `balance` code untouched (checker, dashboard ranking, and the existing `TestPickOrder`, `checker_test.go`, `affinity_test.go` balance tests).
5. Affinity is not changed: `bindCandidates` already pins a bound session to its member (even while cooling), and `moveSession` repins it when a retry is served by another member. So a session stays on its connection while healthy, moves on failure, and never returns; a recovered first member is at the head again only for new sessions (Clarification 4).
6. The pool's «Переключать модель при сбое» checkbox (`Failover`) keeps its meaning: whether a failed request retries on the next member. The type decides the order; the checkbox decides whether the request walks it. Acceptance tests run with the checkbox on (the migrated default from `ROUTER_LOCAL_FAILOVER`, default on).

`balance` and failover (Clarification 5.3): the field stays in the file and form, is still validated and saved, and has no effect in a failover pool. Reasons: (a) the live config has four pools with one member each and `balance` 0 (checked at plan time, ba194ec), so `pick` returns the same single member as today and nothing observable changes on live; (b) applying `balance` (default 3) inside a failover pool would move new sessions to the second connection whenever the first has a request in flight, which contradicts Clarification 5.2; (c) ignoring Score is required because Codex members get no background probes: a member whose EWMA fell below 0.5 would otherwise never lead new sessions again, while in failover it leads again once its cooldown ends (a still-broken first member then costs one retry on a new session's first request). The only behavior change is for multi-member pools, which move from rating order to pool order; that is the point of the type. A future balancer pool type is where `balance` gets meaning again. The UI says so next to the field.

- [ ] **Step 1: Write the failing tests** (`pool_type_test.go`)

```go
package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func twoMemberPool(settings map[string]poolSettings) config {
	l := localSetup{
		Providers:    []provider{{Name: "p", BaseURL: "http://p.test/v1"}, {Name: "q", BaseURL: "http://q.test/v1"}},
		Models:       []localModel{{Provider: "p", Model: "a"}, {Provider: "q", Model: "b"}},
		Routes:       map[string]map[string]modelRoute{"local-model": {"default": {Mode: "pool", Pool: "pair"}}},
		ModelPools:   map[string][]poolTarget{"pair": {{Model: "p/a"}, {Model: "q/b"}}},
		PoolSettings: settings,
	}
	return config{local: l, failover: true, balance: 3, firstByte: 5 * time.Second}
}

func keys(cands []candidate) string {
	var out []string
	for _, c := range cands {
		out = append(out, c.Key)
	}
	return strings.Join(out, ",")
}

func TestPoolWithoutTypeIsFailover(t *testing.T) {
	for name, settings := range map[string]map[string]poolSettings{
		"no settings entry":  nil,
		"entry without type": {"pair": {Failover: true, FirstByteSec: 5}},
		"explicit failover":  {"pair": {Type: "failover", Failover: true, FirstByteSec: 5}},
	} {
		cfg := twoMemberPool(settings).forModel("local-model", "")
		hl := newHealth("")
		// The second member looks better on every rating signal and is idle;
		// the first is busy. Failover still starts with the first.
		hl.m["q/b"] = &modelStat{Score: 1, TTFBMs: 10}
		hl.m["p/a"] = &modelStat{Score: 0.3, TTFBMs: 900}
		hl.inflight["p/a"] = 5
		if got := keys(hl.pick(cfg)); got != "p/a,q/b" {
			t.Fatalf("%s: order %s, want pool order", name, got)
		}
	}
}

func TestFailoverPoolCoolingLast(t *testing.T) {
	cfg := twoMemberPool(nil).forModel("local-model", "")
	hl := newHealth("")
	hl.m["p/a"] = &modelStat{Score: 1, CoolUntil: time.Now().Add(time.Minute)}
	hl.m["q/b"] = &modelStat{Score: 1}
	if got := keys(hl.pick(cfg)); got != "q/b,p/a" {
		t.Fatalf("cooling first member: %s", got)
	}
	hl.m["q/b"].CoolUntil = time.Now().Add(30 * time.Second)
	if got := keys(hl.pick(cfg)); got != "q/b,p/a" {
		t.Fatalf("all cooling must order by CoolUntil: %s", got)
	}
	hl.m["p/a"].CoolUntil = time.Time{}
	hl.m["q/b"].CoolUntil = time.Time{}
	if got := keys(hl.pick(cfg)); got != "p/a,q/b" {
		t.Fatalf("recovered first member must lead new sessions: %s", got)
	}
}

func TestPoolTypeUnknownRejected(t *testing.T) {
	l := twoMemberPool(map[string]poolSettings{"pair": {Type: "balance"}}).local
	err := l.validate()
	if err == nil || !strings.Contains(err.Error(), "balance") || !strings.Contains(err.Error(), "pair") {
		t.Fatalf("unknown pool type: %v", err)
	}
}

func TestPoolSaveKeepsType(t *testing.T) {
	path := filepath.Join(t.TempDir(), "providers.json")
	cfg := twoMemberPool(map[string]poolSettings{"pair": {Type: "failover"}})
	if err := writeProviders(path, cfg.local); err != nil {
		t.Fatal(err)
	}
	cs := &configStore{c: cfg, provPath: path}
	if err := cs.savePoolSettings("pair", poolSettings{Failover: true, FirstByteSec: 7}); err != nil {
		t.Fatal(err)
	}
	l, err := readProviders(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := l.PoolSettings["pair"]; got.Type != "failover" || got.FirstByteSec != 7 {
		t.Fatalf("saved %+v", got)
	}
}
```

Before running `TestPoolSaveKeepsType`, read `settings.go` for how `configStore` is constructed; if `savePoolSettings` needs more fields (profile sync, reload hook), build the store the way `ui_test.go`'s `testUI` does and post the form to `/settings/pool-settings` instead. The assertion stays the same.

- [ ] **Step 2: Run to see them fail**

Run: `go test -count=1 -run 'TestPool(WithoutType|TypeUnknown|SaveKeepsType)|TestFailoverPoolCoolingLast' .`
Expected: build error `unknown field Type in struct literal of type poolSettings`; after adding only the field: `order q/b,p/a, want pool order`, `unknown pool type: <nil>`, `saved {Type: ...}` without the type.

- [ ] **Step 3: Implement**

`pool_settings.go`:

```go
// poolFailover is the only pool type so far: a new session goes to the first
// healthy member in pool order. An absent type means failover.
const poolFailover = "failover"

type poolSettings struct {
	Type          string `json:"type,omitempty"`
	Failover      bool   `json:"failover"`
	FirstByteSec  int    `json:"first_byte_seconds"`
	ProbeSec      int    `json:"probe_seconds"`
	MaxInputChars int    `json:"max_input_chars"`
}
```

`poolSettings(name)`'s fallback literal becomes keyed (`poolSettings{Failover: c.failover, FirstByteSec: ..., ...}`) since a positional literal no longer compiles. In `validate()`, first:

```go
	if s.Type != "" && s.Type != poolFailover {
		return fmt.Errorf("тип пула %q не поддерживается (есть только failover)", s.Type)
	}
```

In `savePoolSettings`, before `l.PoolSettings[name] = settings`: `settings.Type = l.PoolSettings[name].Type`.

`main.go` `config`: `poolType string // poolFailover for a pool route: pool order, no rating; "" keeps the rating order`. In `forModel`, `case "pool":` add `next.poolType = poolFailover` before the `PoolSettings` lookup.

`health.go` `pick`, directly after `if !c.failover && len(out) > 0 { return out[:1] }`:

```go
	if c.poolType == poolFailover {
		// Pool order, not rating: members that are not cooling keep the order
		// the pool lists them in; cooling ones go last, soonest back first.
		sort.SliceStable(out, func(i, j int) bool {
			ci, cj := out[i].Stat.Cooling(), out[j].Stat.Cooling()
			if ci != cj {
				return cj
			}
			return ci && out[i].Stat.CoolUntil.Before(out[j].Stat.CoolUntil)
		})
		return out
	}
```

Add one sentence to the `pick` doc comment: "A failover pool (poolType) ignores rating and balance and keeps pool order."

`ui/settings.html`, pool behavior form: above the failover checkbox add `<p class="hint">Тип пула: failover — новая сессия идёт на первую здоровую модель по порядку пула и остаётся на ней</p>`; the `balance` input is removed (Task 5 edit 3).

- [ ] **Step 4: Run tests**

Run: `go test -count=1 ./...`
Expected: PASS, including unchanged `TestPickOrder`, `checker_test.go` and `affinity_test.go` balance cases (global config, `poolType == ""`).

- [ ] **Step 5: Commit**

```bash
git add pool_settings.go main.go health.go ui/settings.html pool_type_test.go
git commit -m "pools: failover pool type keeps pool order for new sessions"
```

### Task 6: Failover acceptance across two Codex connections through `handleLocal` (Clarification 5, review point 3)

**Files:**
- Create: `local_codex_test.go`
- Modify: none expected; if a test fails, fix the per-provider plumbing from Tasks 2–5, not the failover policy.

**Interfaces:**
- Consumes: `seedConnection`, `useTestCodexHome` (Task 2), `forModel` + `poolFailover` (Task 5), `runLocal` pattern (`failover_test.go:57`).

- [ ] **Step 1: Write the tests** (`local_codex_test.go`)

```go
package main

import (
	"net/http"
	"sync"
	"testing"
	"time"
)

// twoCodexPool routes local-model to a failover pool [codex/gpt, work/gpt].
// The config goes through forModel exactly as main.go:271 does.
func twoCodexPool() config {
	l := localSetup{
		Providers: []provider{
			{Name: "codex", Type: "codex", BaseURL: codexBaseURL},
			{Name: "work", Type: "codex", BaseURL: codexBaseURL, AuthID: testAuthB},
		},
		Models:     []localModel{{Provider: "codex", Model: "gpt"}, {Provider: "work", Model: "gpt"}},
		Routes:     map[string]map[string]modelRoute{"local-model": {"default": {Mode: "pool", Pool: "gpt"}}},
		ModelPools: map[string][]poolTarget{"gpt": {{Model: "codex/gpt"}, {Model: "work/gpt"}}},
	}
	c := config{local: l, failover: true, balance: 3, firstByte: 5 * time.Second}
	return c.forModel("local-model", "")
}

const codexStreamOK = `data: {"type":"response.output_text.delta","delta":"ok"}

` +
	`data: {"type":"response.completed","response":{"status":"completed"}}

`

func codexStatusByAccount(t *testing.T, status map[string]int) *sync.Map {
	t.Helper()
	calls := &sync.Map{}
	old := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = old })
	http.DefaultTransport = usageTransport(func(r *http.Request) (*http.Response, error) {
		acct := r.Header.Get("ChatGPT-Account-Id")
		n, _ := calls.LoadOrStore(acct, new(int))
		*n.(*int)++
		if code := status[acct]; code != 0 && code != 200 {
			return usageResponse(code, `{"error":{"message":"limit"}}`), nil
		}
		return usageResponse(200, codexStreamOK), nil
	})
	return calls
}

func seedTwoConnections(t *testing.T) {
	useTestCodexHome(t, "http://issuer.invalid", http.DefaultClient)
	seedConnection(t, provider{Name: "codex", Type: "codex"}, "acct-a")
	seedConnection(t, provider{Name: "work", Type: "codex", AuthID: testAuthB}, "acct-b")
}
```

`codexStreamOK` uses the event shape of `codex_test.go:84-86`. `runLocal` sends no session; add `runLocalSession(t, cfg, hl, session string) (*httptest.ResponseRecorder, *localTrace)` in this file, a copy of `runLocal` whose body carries `"metadata":{"user_id":"{\"session_id\":\"<session>\"}"}` so `affinityKey` binds it. The status map is written only between requests, so the transport reads it without a race. `attempt.Model` is the candidate key (local.go:131).

```go
func TestCodexFailoverNewSessionSkipsLimitedFirst(t *testing.T) {
	seedTwoConnections(t)
	cfg, hl := twoCodexPool(), newHealth("")
	status := map[string]int{"acct-a": 429}
	codexStatusByAccount(t, status)
	w, tr := runLocalSession(t, cfg, hl, "s1")
	if w.Code != 200 || tr.Served != "work/gpt" {
		t.Fatalf("429 on first: %d served %s", w.Code, tr.Served)
	}
	// The first connection is cooling now: the next new session goes straight
	// to the second without spending a request on the first.
	_, tr = runLocalSession(t, cfg, hl, "s2")
	if tr.Served != "work/gpt" || len(tr.Attempts) != 1 {
		t.Fatalf("new session during cooldown: %s after %d attempts", tr.Served, len(tr.Attempts))
	}
}

func TestCodexSessionStaysOnConnection(t *testing.T) {
	seedTwoConnections(t)
	cfg, hl := twoCodexPool(), newHealth("")
	status := map[string]int{}
	codexStatusByAccount(t, status)
	for i := 0; i < 3; i++ {
		if _, tr := runLocalSession(t, cfg, hl, "s1"); tr.Served != "codex/gpt" {
			t.Fatalf("healthy first member not used: %s", tr.Served)
		}
	}
	// Load on the first member moves neither a bound session nor a new one.
	hl.mu.Lock()
	hl.inflight["codex/gpt"] = 9
	hl.mu.Unlock()
	for _, s := range []string{"s1", "busy"} {
		if _, tr := runLocalSession(t, cfg, hl, s); tr.Served != "codex/gpt" {
			t.Fatalf("%s: busy but healthy member skipped: %s", s, tr.Served)
		}
	}
	hl.mu.Lock()
	hl.inflight["codex/gpt"] = 0
	hl.mu.Unlock()
	status["acct-a"] = 429
	if _, tr := runLocalSession(t, cfg, hl, "s1"); tr.Served != "work/gpt" {
		t.Fatalf("failure did not move the session: %s", tr.Served)
	}
	// The first connection recovers completely; s1 stays where it moved.
	delete(status, "acct-a")
	hl.mu.Lock()
	hl.m["codex/gpt"].CoolUntil = time.Time{}
	hl.m["codex/gpt"].Score = 1
	hl.mu.Unlock()
	for i := 0; i < 3; i++ {
		if _, tr := runLocalSession(t, cfg, hl, "s1"); tr.Served != "work/gpt" {
			t.Fatalf("session returned to the recovered member: %s", tr.Served)
		}
	}
	// Only new sessions go back to the first connection.
	if _, tr := runLocalSession(t, cfg, hl, "fresh"); tr.Served != "codex/gpt" {
		t.Fatalf("new session after recovery went to %s", tr.Served)
	}
}

func TestCodexAll429KeepsBehavior(t *testing.T) {
	seedTwoConnections(t)
	cfg, hl := twoCodexPool(), newHealth("")
	codexStatusByAccount(t, map[string]int{"acct-a": 429, "acct-b": 429})
	w, tr := runLocalSession(t, cfg, hl, "s1")
	if w.Code != 429 {
		t.Fatalf("client got %d, want 429", w.Code)
	}
	a, b := hl.snapshot("codex/gpt"), hl.snapshot("work/gpt")
	if !a.Cooling() || !b.Cooling() {
		t.Fatal("both members must cool down")
	}
	if d := time.Until(a.CoolUntil); d <= 0 || d > cooldownBase {
		t.Fatalf("first failure cooldown %s, want <= %s", d, cooldownBase)
	}
	order := hl.pick(cfg)
	if order[1].Stat.CoolUntil.Before(order[0].Stat.CoolUntil) {
		t.Fatal("cooling members not ordered by CoolUntil")
	}
	hl.mu.Lock()
	var pinned string
	for _, b := range hl.sessions {
		pinned = b.Model
	}
	hl.mu.Unlock()
	if last := tr.Attempts[len(tr.Attempts)-1].Model; pinned != last {
		t.Fatalf("session pinned to %s, last tried %s", pinned, last)
	}
}
```

Read `failover_test.go` for how a cooldown is asserted today and mirror it.

- [ ] **Step 2: Run**

Run: `go test -count=1 -run 'TestCodex(FailoverNewSession|SessionStays|All429)' .`
Expected: PASS once Tasks 2–5 are in (these pin Task 5's order through the real request path with per-connection auth). Prove they can fail: temporarily drop `next.poolType = poolFailover` from `forModel` and see `TestCodexSessionStaysOnConnection` fail on the busy new session (rating + `balance` 3 picks the idle `work/gpt`); temporarily seed both providers with `acct-a` and see `TestCodexFailoverNewSessionSkipsLimitedFirst` fail; restore both.

- [ ] **Step 3: Full suite and commit**

Run: `go test -count=1 ./...` then `go test -race -count=1 ./...`

```bash
git add local_codex_test.go
git commit -m "codex: failover acceptance across two connections"
```

### Task 10: A client cancel is not a failure

**Files:**
- Modify: `local.go` (`handleLocal`: the non-stream Codex read error and the `werr` branch)
- Test: `local_codex_test.go`

**Interfaces:**
- Consumes: `seedTwoConnections`, `twoCodexPool`, `codexStreamOK` (Task 6); `usageTransport`, `usageResponse` (`codex_usage_test.go`).
- Produces: test helpers `runLocalRequest(t, ctx, cfg, hl, session string, stream bool)` and `runLocalSession(t, cfg, hl, session string)`, both returning `(*httptest.ResponseRecorder, *localTrace)`. Task 6 step 1 already needs `runLocalSession`, so the two helpers are written there (override table) and this task only adds the test and the fix.

Rule: when the client goes away (Esc in Claude Code closes the request), the member that was answering gets no failure, no cooldown and no session move. Today two paths treat it as a model failure:
- the non-stream Codex path turns the `readCodexEvents` error into a retryable failure (local.go:117-122), then records it and moves the session;
- the `werr` branch after an answer has started records a failure and moves the session to the next member (local.go:158-167).

- [ ] **Step 1: Write the failing test** (`local_codex_test.go`; add imports `context`, `fmt`, `net/http/httptest`, `strings`)

The helpers, as written in Task 6 step 1:

```go
func runLocalRequest(t *testing.T, ctx context.Context, cfg config, hl *health, session string, stream bool) (*httptest.ResponseRecorder, *localTrace) {
	t.Helper()
	body := fmt.Sprintf(`{"model":"local-model","max_tokens":10,"stream":%t,"metadata":{"user_id":%q},"messages":[{"role":"user","content":"hi"}]}`,
		stream, `{"session_id":"`+session+`"}`)
	r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body)).WithContext(ctx)
	w := httptest.NewRecorder()
	tr := &localTrace{}
	handleLocal(w, r, cfg, []byte(body), tr, hl)
	return w, tr
}

func runLocalSession(t *testing.T, cfg config, hl *health, session string) (*httptest.ResponseRecorder, *localTrace) {
	t.Helper()
	return runLocalRequest(t, context.Background(), cfg, hl, session, false)
}
```

The test:

```go
// cancelAfterFirstEvent sends one delta, then acts as the client pressing Esc:
// it cancels the client's context and fails the read once the cancel lands.
type cancelAfterFirstEvent struct {
	ctx    context.Context
	cancel context.CancelFunc
	sent   bool
}

func (b *cancelAfterFirstEvent) Read(p []byte) (int, error) {
	if !b.sent {
		b.sent = true
		return copy(p, `data: {"type":"response.output_text.delta","delta":"ok"}`+"\n\n"), nil
	}
	b.cancel()
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (b *cancelAfterFirstEvent) Close() error { return nil }

func TestClientCancelKeepsBinding(t *testing.T) {
	for _, stream := range []bool{true, false} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			seedTwoConnections(t)
			cfg, hl := twoCodexPool(), newHealth("")
			calls := &sync.Map{}
			var cancelNext context.CancelFunc
			old := http.DefaultTransport
			t.Cleanup(func() { http.DefaultTransport = old })
			http.DefaultTransport = usageTransport(func(r *http.Request) (*http.Response, error) {
				n, _ := calls.LoadOrStore(r.Header.Get("ChatGPT-Account-Id"), new(int))
				*n.(*int)++
				if cancelNext != nil {
					resp := usageResponse(200, "")
					resp.Body = &cancelAfterFirstEvent{ctx: r.Context(), cancel: cancelNext}
					cancelNext = nil
					return resp, nil
				}
				return usageResponse(200, codexStreamOK), nil
			})
			if _, tr := runLocalRequest(t, context.Background(), cfg, hl, "s1", stream); tr.Served != "codex/gpt" {
				t.Fatalf("first request served by %q", tr.Served)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cancelNext = cancel
			runLocalRequest(t, ctx, cfg, hl, "s1", stream)

			if st := hl.snapshot("codex/gpt"); st.Fail != 0 || st.Cooling() {
				t.Fatalf("client cancel counted as a failure: %+v", st)
			}
			if n := hl.load("codex/gpt"); n != 0 {
				t.Fatalf("in-flight count leaked: %d", n)
			}
			if _, ok := calls.Load("acct-b"); ok {
				t.Fatal("cancelled request was retried on the other connection")
			}
			_, tr := runLocalSession(t, cfg, hl, "s1")
			if tr.Served != "codex/gpt" || len(tr.Attempts) != 1 {
				t.Fatalf("cancelled session moved: %s after %d attempts", tr.Served, len(tr.Attempts))
			}
		})
	}
}
```

The transport sets `cancelNext` only between requests and reads it inside a request, on the handler's goroutine, so there is no race. The upstream request's context is a child of the client's context, so the blocked read fails exactly when the client is gone.

- [ ] **Step 2: Run to see it fail**

Run: `go test -count=1 -run TestClientCancelKeepsBinding .`
Expected: FAIL in both subtests with `client cancel counted as a failure: {... Fail:1 ...}`. Record this red output in the receipt.

- [ ] **Step 3: Implement** (`local.go`)

In the non-stream Codex branch:

```go
			if err != nil {
				res.err, res.retryable = err, true
				// A read cut short by the client (Esc) is not the model's fault.
				res.clientGone = r.Context().Err() != nil
			}
```

The existing `if res.clientGone { return }` below then releases the member and returns without recording.

In the `werr` branch, keep the trace line first and return before recording when the client is gone:

```go
		if werr != nil {
			if tr != nil {
				tr.Attempts[len(tr.Attempts)-1].Err = werr.Error()
			}
			// The client went away mid-answer: no failure, and the session stays.
			if r.Context().Err() != nil {
				return
			}
			hl.record(cand.Key, false, 0, werr.Error())
			if cfg.failover && i+1 < len(cands) {
				hl.moveSession(scope, cand.Key, cands[i+1])
			}
			return
		}
```

- [ ] **Step 4: Run the tests**

Run: `go test -count=1 -run 'TestClientCancel|TestCodex|TestFailover' . && go test -count=1 ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add local.go local_codex_test.go
git commit -m "local: a client cancel is not a member failure and keeps the session"
```

### Task 11: A signed-out Codex member fails over

**Files:**
- Modify: `codex_auth.go` (`signedIn`), `local.go` (`withoutSignedOut`, `handleLocal`, `tryModel`)
- Test: `local_codex_test.go`

**Interfaces:**
- Consumes: `codexStoreFor`, `seedConnection`, `useTestCodexHome` (Task 2); `codexStatusByAccount` with the v3 host check (Task 6, override table); `runLocalSession` (Task 6 step 1).
- Produces: `(s *codexAuthStore) signedIn() bool`; `withoutSignedOut(cands []candidate) []candidate`; test helpers `codexPoolSetup(members ...string) localSetup` and `codexPoolOf(members ...string) config`. Task 12 uses `codexPoolSetup`.

Rules:
- A Codex member that cannot authorize (no credential, or a token that was rejected and whose refresh failed) is dropped from the candidate list before the session is bound. So a new connection that has not signed in yet never leads a new session, and a session bound to it moves to the next member.
- If every member is signed out, the list is kept, so the client still gets the sign-in error as today.
- A sign-in failure during the request is retryable: the `authorize` error, `errCodexSignIn` from `doWithReauth` (status stays 401), and a Codex HTTP 401. The member is recorded as failed and the session moves, as for a 429.
- Recovery: `importFromCLI`, OAuth completion and `credentialFor` clear `authProblem`. `save` does not, so after a rejected token the member comes back through import or OAuth. After a missing credential it comes back as soon as the file exists.

- [ ] **Step 1: Write the failing test** (`local_codex_test.go`)

```go
// codexPoolSetup routes local-model to a failover pool of the given Codex
// members, over the two connections of seedTwoConnections.
func codexPoolSetup(members ...string) localSetup {
	l := localSetup{
		Providers: []provider{
			{Name: "codex", Type: "codex", BaseURL: codexBaseURL},
			{Name: "work", Type: "codex", BaseURL: codexBaseURL, AuthID: testAuthB},
		},
		Routes:     map[string]map[string]modelRoute{"local-model": {"default": {Mode: "pool", Pool: "gpt"}}},
		ModelPools: map[string][]poolTarget{"gpt": nil},
	}
	seen := map[string]bool{}
	for _, key := range members {
		if !seen[key] {
			seen[key] = true
			p, m, _ := strings.Cut(key, "/")
			l.Models = append(l.Models, localModel{Provider: p, Model: m})
		}
		l.ModelPools["gpt"] = append(l.ModelPools["gpt"], poolTarget{Model: key})
	}
	return l
}

func codexPoolOf(members ...string) config {
	c := config{local: codexPoolSetup(members...), failover: true, balance: 3, firstByte: 5 * time.Second}
	return c.forModel("local-model", "")
}

func TestCodexSignedOutMemberFailsOver(t *testing.T) {
	served := func(t *testing.T, cfg config, hl *health, session, want string) {
		t.Helper()
		w, tr := runLocalSession(t, cfg, hl, session)
		if w.Code != 200 || tr.Served != want || len(tr.Attempts) != 1 {
			t.Fatalf("%s: %d served %q after %d attempts, want %s in one", session, w.Code, tr.Served, len(tr.Attempts), want)
		}
	}
	t.Run("no credential", func(t *testing.T) {
		useTestCodexHome(t, "http://issuer.invalid", http.DefaultClient)
		seedConnection(t, provider{Name: "codex", Type: "codex"}, "acct-a")
		codexStatusByAccount(t, map[string]int{})
		cfg, hl := codexPoolOf("work/gpt", "codex/gpt"), newHealth("")
		served(t, cfg, hl, "s1", "codex/gpt")
		served(t, cfg, hl, "s1", "codex/gpt")
		// Signing in brings the connection back for new sessions only.
		seedConnection(t, provider{Name: "work", Type: "codex", AuthID: testAuthB}, "acct-b")
		served(t, cfg, hl, "n1", "work/gpt")
		served(t, cfg, hl, "s1", "codex/gpt")
	})
	t.Run("token rejected", func(t *testing.T) {
		seedTwoConnections(t)
		codexStatusByAccount(t, map[string]int{"acct-b": 401})
		cfg, hl := codexPoolOf("work/gpt", "codex/gpt"), newHealth("")
		w, tr := runLocalSession(t, cfg, hl, "s1")
		if w.Code != 200 || tr.Served != "codex/gpt" || len(tr.Attempts) != 2 {
			t.Fatalf("rejected first member: %d served %q after %d attempts", w.Code, tr.Served, len(tr.Attempts))
		}
		served(t, cfg, hl, "s1", "codex/gpt")
		served(t, cfg, hl, "s2", "codex/gpt")
	})
	t.Run("everyone signed out", func(t *testing.T) {
		useTestCodexHome(t, "http://issuer.invalid", http.DefaultClient)
		codexStatusByAccount(t, map[string]int{})
		cfg, hl := codexPoolOf("work/gpt", "codex/gpt"), newHealth("")
		w, _ := runLocalSession(t, cfg, hl, "s1")
		if w.Code == 200 || !strings.Contains(w.Body.String(), "войдите") {
			t.Fatalf("all signed out: %d %s", w.Code, w.Body.String())
		}
	})
}
```

In "token rejected", the refresh after the 401 goes to `http://issuer.invalid/oauth/token` through the swapped transport. Its host check answers 400 `invalid_grant`, so the store sets `authProblem`.

- [ ] **Step 2: Run to see it fail**

Run: `go test -count=1 -run TestCodexSignedOutMemberFailsOver .`
Expected: FAIL.
- "no credential": `s1: 502 served "" after 1 attempts`.
- "token rejected": `rejected first member: 401 served "" after 1 attempts`.
- "everyone signed out" already passes; it guards the kept list.

Record the red output in the receipt.

- [ ] **Step 3: Implement**

`codex_auth.go`:

```go
// signedIn reports whether the store can authorize a request without a new
// sign-in: a credential is loaded or on disk, and it was not rejected.
func (s *codexAuthStore) signedIn() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.authProblem != "" {
		return false
	}
	if s.loaded {
		return true
	}
	_, err := readCodexCredential(s.path)
	return err == nil
}
```

`local.go`, next to `handleLocal`:

```go
// withoutSignedOut drops Codex members that cannot authorize until someone
// signs in again. If every member is signed out the list is kept, so the
// client still gets the sign-in error.
func withoutSignedOut(cands []candidate) []candidate {
	var out []candidate
	for _, c := range cands {
		if c.Provider.Type == "codex" {
			if s, err := codexStoreFor(c.Provider); err != nil || !s.signedIn() {
				continue
			}
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		return cands
	}
	return out
}
```

In `handleLocal`: `cands := withoutSignedOut(hl.pick(pickCfg))`, before `bindCandidates`.

In `tryModel`:
- the Codex `authorize` error returns `attemptResult{err: err, retryable: true}`;
- the `errCodexSignIn` branch adds `retryable: true` and keeps `status: http.StatusUnauthorized, detail: err.Error()`;
- the status switch gets `case 401: res.retryable = cand.Provider.Type == "codex"`. An OpenAI-compatible provider's 401 means a bad key in the config, and it stays non-retryable.

- [ ] **Step 4: Run the tests**

Run: `go test -count=1 -run 'TestCodex|TestClientCancel|TestFailover' . && go test -count=1 ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add codex_auth.go local.go local_codex_test.go
git commit -m "local: skip signed-out Codex members and fail over on sign-in errors"
```

### Task 12: The `balance` pool type

**Files:**
- Modify:
  - `pool_settings.go` (`poolBalance`, `validate`, `parsePoolSettings`)
  - `settings.go:147-153` (`settingsInput.Type`)
  - `main.go` (`config.poolName`, `forModel`)
  - `affinity.go` (`sessionBinding`, `poolRoute`, `bindCandidates`, `moveSession`, `balanceCriterion`, `sessionsOnConnection`, `balanceFirst`)
  - `health.go` (`health.balanceBy`)
  - `local.go` (the `bindCandidates` call)
  - `ui.go` (`settingsPoolSave`)
  - `ui/settings.html` (a type select in the pool form)
- Modify tests: `affinity_test.go:96`, `profiles_storage_test.go:178,403`, `profiles_test.go:93,272` (pass `poolRoute{}`)
- Test: `pool_balance_test.go` (new), `local_codex_test.go`

**Interfaces:**
- Consumes: `poolFailover`, `poolSettings.Type`, `config.poolType`, `updatePoolSettings`, `twoMemberPool` (Task 5 with the v3 edits); `codexPoolSetup` (Task 11); `runLocalSession`, `seedTwoConnections`, `codexStatusByAccount` (Task 6).
- Produces:
  - `const poolBalance = "balance"`
  - `config.poolName string`
  - `type poolRoute struct{ Name, Type string }`
  - `(h *health) bindCandidates(scope string, pool poolRoute, candidates []candidate) []candidate`
  - `type balanceCriterion func(h *health, pool string, p provider) int`
  - `sessionsOnConnection`
  - `health.balanceBy balanceCriterion` (nil means `sessionsOnConnection`)

Rules (Clarification 6):
1. `balance` is set on a pool explicitly. A pool with no type stays failover.
2. A balance pool changes only which member a **new** session starts on. It compares connections (providers), never models of one connection. Each connection is represented by its first non-cooling member in pool order, and the models of one connection keep pool order.
3. The score is the number of live sessions of **this pool** bound to the connection. It is counted from `h.sessions` under `h.mu` inside `bindCandidates`, in the same critical section that records the new binding, so concurrent new sessions see each other.
4. A bound session keeps its member while it is healthy. On failure it moves (the failover code of Tasks 5-6) and stays there. Balance never moves a bound session.
5. The criterion is one function value, `balanceCriterion`. The lowest score wins; a tie keeps pool order. A criterion based on the account's remaining limit would replace only this function.
6. The type switches on the fly, through the pool form (`updatePoolSettings`) and through a reload of the settings file. The new type applies to new sessions; bound sessions stay. Bindings are not cleared: only a profile switch clears them, as today.

Also:
- `pick` does not change. With `poolType != ""`, both types keep pool order (override table, Task 5 step 3).
- A request without a session has no binding and is not balanced: it goes to the first member in pool order.
- A cooling member never leads a new session in a balance pool.

- [ ] **Step 1: Write the failing tests**

`pool_balance_test.go`:

```go
package main

import (
	"fmt"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var twoProviders = []provider{{Name: "p", BaseURL: "http://p.test/v1"}, {Name: "q", BaseURL: "http://q.test/v1"}}

// poolsOver builds one pool per entry of types, routed from a model of the
// same name, each with the given members in that order.
func poolsOver(providers []provider, members []string, types map[string]string) localSetup {
	l := localSetup{
		Providers:    providers,
		Routes:       map[string]map[string]modelRoute{},
		ModelPools:   map[string][]poolTarget{},
		PoolSettings: map[string]poolSettings{},
	}
	var targets []poolTarget
	for _, key := range members {
		p, m, _ := strings.Cut(key, "/")
		l.Models = append(l.Models, localModel{Provider: p, Model: m})
		targets = append(targets, poolTarget{Model: key})
	}
	for name, typ := range types {
		l.Routes[name] = map[string]modelRoute{"default": {Mode: "pool", Pool: name}}
		l.ModelPools[name] = targets
		l.PoolSettings[name] = poolSettings{Type: typ, Failover: true, FirstByteSec: 5}
	}
	return l
}

// firstFor is the member a session's request starts on, chosen the way
// handleLocal chooses it.
func firstFor(hl *health, l localSetup, pool, session string) string {
	cfg := config{local: l}.forModel(pool, "")
	return hl.bindCandidates(pool+":"+session, poolRoute{cfg.poolName, cfg.poolType}, hl.pick(cfg))[0].Key
}

func TestBalancePoolSpreadsNewSessions(t *testing.T) {
	l := poolsOver(twoProviders, []string{"p/x", "q/y"}, map[string]string{"bal": poolBalance, "fo": poolFailover})
	hl := newHealth("")
	count := map[string]int{}
	for i := 0; i < 10; i++ {
		count[firstFor(hl, l, "bal", fmt.Sprint("s", i))]++
		if got := firstFor(hl, l, "fo", fmt.Sprint("s", i)); got != "p/x" {
			t.Fatalf("failover pool sent a new session to %s", got)
		}
	}
	if count["p/x"] != 5 || count["q/y"] != 5 {
		t.Fatalf("sequential split %v, want 5/5", count)
	}
	hl, count = newHealth(""), map[string]int{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := firstFor(hl, l, "bal", fmt.Sprint("c", i))
			mu.Lock()
			count[key]++
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	if count["p/x"] != 5 || count["q/y"] != 5 {
		t.Fatalf("concurrent split %v, want 5/5", count)
	}
}

func TestCodexSingleConnectionKeepsOrder(t *testing.T) {
	codex := []provider{
		{Name: "codex", Type: "codex", BaseURL: codexBaseURL},
		{Name: "work", Type: "codex", BaseURL: codexBaseURL, AuthID: testAuthB},
	}
	hl := newHealth("")
	one := poolsOver(codex, []string{"codex/gpt", "codex/mini"}, map[string]string{"solo": poolBalance})
	for i := 0; i < 4; i++ {
		if got := firstFor(hl, one, "solo", fmt.Sprint("s", i)); got != "codex/gpt" {
			t.Fatalf("one connection: session %d started on %s", i, got)
		}
	}
	two := poolsOver(codex, []string{"codex/gpt", "codex/mini", "work/gpt"}, map[string]string{"bal": poolBalance})
	var got []string
	for i := 0; i < 4; i++ {
		got = append(got, firstFor(hl, two, "bal", fmt.Sprint("s", i)))
	}
	if strings.Join(got, ",") != "codex/gpt,work/gpt,codex/gpt,work/gpt" {
		t.Fatalf("new sessions went to %v; models of one connection must keep pool order", got)
	}
}

func TestBalanceCountsPerPool(t *testing.T) {
	l := poolsOver(twoProviders, []string{"p/x", "q/y"}, map[string]string{"bal": poolBalance, "fo": poolFailover})
	hl := newHealth("")
	for i := 0; i < 3; i++ {
		firstFor(hl, l, "fo", fmt.Sprint("s", i)) // three sessions on p, in another pool
	}
	if a, b := firstFor(hl, l, "bal", "n1"), firstFor(hl, l, "bal", "n2"); a != "p/x" || b != "q/y" {
		t.Fatalf("balance counted other pools: %s,%s", a, b)
	}
}

func TestBalanceSkipsCoolingConnection(t *testing.T) {
	l := poolsOver(twoProviders, []string{"p/x", "q/y"}, map[string]string{"bal": poolBalance})
	hl := newHealth("")
	hl.m["p/x"] = &modelStat{CoolUntil: time.Now().Add(time.Minute)}
	for i := 0; i < 4; i++ {
		if got := firstFor(hl, l, "bal", fmt.Sprint("s", i)); got != "q/y" {
			t.Fatalf("session %d started on cooling %s", i, got)
		}
	}
}

func TestBalanceCriterionIsOneFunction(t *testing.T) {
	l := poolsOver(twoProviders, []string{"p/x", "q/y"}, map[string]string{"bal": poolBalance})
	hl := newHealth("")
	hl.balanceBy = func(h *health, pool string, p provider) int {
		if p.Name == "q" {
			return 0
		}
		return 1
	}
	for i := 0; i < 4; i++ {
		if got := firstFor(hl, l, "bal", fmt.Sprint("s", i)); got != "q/y" {
			t.Fatalf("replaced criterion ignored: %s", got)
		}
	}
}

func TestBalanceBoundSessionStays(t *testing.T) {
	l := poolsOver(twoProviders, []string{"p/x", "q/y"}, map[string]string{"bal": poolBalance})
	hl := newHealth("")
	if a, b := firstFor(hl, l, "bal", "s1"), firstFor(hl, l, "bal", "s2"); a != "p/x" || b != "q/y" {
		t.Fatalf("setup: %s,%s", a, b)
	}
	firstFor(hl, l, "bal", "s3") // p/x: p now has two sessions, q one
	for i := 0; i < 3; i++ {
		if got := firstFor(hl, l, "bal", "s1"); got != "p/x" {
			t.Fatalf("bound session rebalanced to %s", got)
		}
	}
	// s1's connection fails: the session moves to q and stays there,
	// although p now has fewer sessions.
	cfg := config{local: l}.forModel("bal", "")
	for _, c := range hl.pick(cfg) {
		if c.Key == "q/y" {
			hl.moveSession("bal:s1", "p/x", c)
		}
	}
	for i := 0; i < 3; i++ {
		if got := firstFor(hl, l, "bal", "s1"); got != "q/y" {
			t.Fatalf("moved session went back to %s", got)
		}
	}
}

func TestPoolSettingsFormSetsType(t *testing.T) {
	path := filepath.Join(t.TempDir(), "providers.json")
	cfg := twoMemberPool(map[string]poolSettings{"pair": {Type: poolFailover, FirstByteSec: 5}})
	cfg.upstream, _ = url.Parse("https://api.anthropic.com")
	if err := writeProviders(path, cfg.local); err != nil {
		t.Fatal(err)
	}
	u := newUIServer(newStore(10, ""), newConfigStore(cfg, path), newHealth(""))
	post := func(typ string) string {
		values := url.Values{"name": {"pair"}, "failover": {"1"}, "first_byte": {"5"}, "probe_every": {"0"}, "max_input_chars": {"0"}}
		if typ != "" {
			values.Set("type", typ)
		}
		r := httptest.NewRequest("POST", "/settings/pool-settings", strings.NewReader(values.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		u.settingsPoolSave(w, r)
		return w.Body.String()
	}
	stored := func() poolSettings {
		l, err := readProviders(path)
		if err != nil {
			t.Fatal(err)
		}
		return l.PoolSettings["pair"]
	}
	html := post(poolBalance)
	if !strings.Contains(html, "Настройки пула сохранены") || stored().Type != poolBalance {
		t.Fatalf("balance not saved: %+v\n%s", stored(), html)
	}
	if !strings.Contains(html, `name="type"`) || strings.Contains(html, `name="balance"`) {
		t.Fatal("pool form must offer the type and not the numeric balance")
	}
	if post(""); stored().Type != poolBalance {
		t.Fatalf("a form without a type reset it: %+v", stored())
	}
	if html := post("zzz"); !strings.Contains(html, `тип пула &#34;zzz&#34; не поддерживается`) || stored().Type != poolBalance {
		t.Fatalf("unknown type: %+v\n%s", stored(), html)
	}
	if raw, _ := os.ReadFile(path); strings.Contains(string(raw), `"balance"`) {
		t.Fatalf("numeric balance written: %s", raw)
	}
}
```

The page escapes the quotes in the error; if the template renders it with a different escape, match the escaped form `html/template` actually produces. `settingsPoolSave` renders the whole settings page (`renderSettingsResult`), so the `name="type"` check sees the pool form.

`local_codex_test.go`, the runtime switch through the real request path (add imports `net/url`, `path/filepath`):

```go
func TestCodexPoolTypeSwitchAtRuntime(t *testing.T) {
	seedTwoConnections(t)
	codexStatusByAccount(t, map[string]int{})
	path := filepath.Join(t.TempDir(), "providers.json")
	start := config{local: codexPoolSetup("codex/gpt", "work/gpt"), failover: true, firstByte: 5 * time.Second}
	start.local.PoolSettings = map[string]poolSettings{"gpt": {Failover: true, FirstByteSec: 5}}
	start.upstream, _ = url.Parse("https://api.anthropic.com")
	if err := writeProviders(path, start.local); err != nil {
		t.Fatal(err)
	}
	cs, hl := newConfigStore(start, path), newHealth("")
	serve := func(session, want string) {
		t.Helper()
		if _, tr := runLocalSession(t, cs.get().forModel("local-model", ""), hl, session); tr.Served != want {
			t.Fatalf("%s served by %q, want %s", session, tr.Served, want)
		}
	}
	serve("s1", "codex/gpt")
	serve("s2", "codex/gpt")

	// Switched from the UI path: new sessions balance, bound ones stay.
	if err := cs.savePoolSettings("gpt", poolSettings{Type: poolBalance, Failover: true, FirstByteSec: 5}); err != nil {
		t.Fatal(err)
	}
	serve("n1", "work/gpt")
	serve("n2", "work/gpt")
	serve("n3", "codex/gpt")
	serve("s1", "codex/gpt")
	serve("s2", "codex/gpt")

	// Switched back by editing the file and reloading it.
	l, err := readProviders(path)
	if err != nil {
		t.Fatal(err)
	}
	ps := l.PoolSettings["gpt"]
	ps.Type = poolFailover
	l.PoolSettings["gpt"] = ps
	if err := writeProviders(path, l); err != nil {
		t.Fatal(err)
	}
	if err := cs.reloadProfiles(hl); err != nil {
		t.Fatal(err)
	}
	serve("n4", "codex/gpt")
	serve("n1", "work/gpt")
}
```

- [ ] **Step 2: Run to see them fail**

Run: `go test -count=1 -run 'TestBalance|TestCodexSingleConnection|TestPoolSettingsFormSetsType|TestCodexPoolTypeSwitch' .`
Expected: first a build error (`undefined: poolBalance`, `too many arguments in call to hl.bindCandidates`). Then add only the constant, the `poolRoute` type and the new signature, keeping the old body. Now the failures are `sequential split map[p/x:10], want 5/5` and `n1 served by "codex/gpt", want work/gpt`. Record both in the receipt.

- [ ] **Step 3: Implement**

`pool_settings.go`:

```go
// poolBalance spreads new sessions over the pool's connections; a bound
// session keeps its member, as in a failover pool.
const poolBalance = "balance"
```

In `validate()`, the type check becomes:

```go
	if s.Type != "" && s.Type != poolFailover && s.Type != poolBalance {
		return fmt.Errorf("тип пула %q не поддерживается (есть failover и balance)", s.Type)
	}
```

The other form wiring:
- `parsePoolSettings` sets `Type: strings.TrimSpace(in.Type)` in its `poolSettings` literal.
- `settings.go` `settingsInput` gains `Type string // pool type; "" keeps the stored one`.
- `ui.go` `settingsPoolSave` passes `Type: r.FormValue("type")`. Its `updatePoolSettings` merge already keeps the stored type when the new one is empty (Task 5 edit 3).

`main.go` `config`: `poolName string // the pool a pool route resolved to; "" otherwise`. In `forModel`, `case "pool":`

```go
		targets = c.local.ModelPools[route.Pool]
		next.poolName, next.poolType = route.Pool, poolFailover
		if settings, ok := c.local.PoolSettings[route.Pool]; ok {
			next = settings.apply(next)
			if settings.Type == poolBalance {
				next.poolType = poolBalance
			}
		}
```

`health.go`, `health` struct: `balanceBy balanceCriterion // nil means sessionsOnConnection`.

`affinity.go`:

```go
type sessionBinding struct {
	Model     string
	Selection string
	Pool      string // pool name, for the per-pool session count
	Provider  string // connection of Model
	Used      time.Time
}

// poolRoute names the pool a request was routed to and its type. The zero
// value (a model route, or a caller that does not route) never balances.
type poolRoute struct{ Name, Type string }

// balanceCriterion scores a connection for a new session of a balance pool.
// The lowest score wins; a tie keeps pool order. It runs under h.mu.
type balanceCriterion func(h *health, pool string, p provider) int

// sessionsOnConnection counts the live sessions of this pool bound to p.
func sessionsOnConnection(h *health, pool string, p provider) int {
	n := 0
	for _, b := range h.sessions {
		if b.Pool == pool && b.Provider == p.Name {
			n++
		}
	}
	return n
}

// balanceFirst moves the first member of the best-scored connection to the
// front. Connections are compared, not models: each is represented by its
// first member that is not cooling, and a cooling member never leads.
func (h *health) balanceFirst(pool string, candidates []candidate) []candidate {
	score := h.balanceBy
	if score == nil {
		score = sessionsOnConnection
	}
	best, bestScore := -1, 0
	seen := map[string]bool{}
	for i, c := range candidates {
		if c.Stat.Cooling() || seen[c.Provider.Name] {
			continue
		}
		seen[c.Provider.Name] = true
		if s := score(h, pool, c.Provider); best < 0 || s < bestScore {
			best, bestScore = i, s
		}
	}
	if best <= 0 {
		return candidates
	}
	lead := candidates[best]
	copy(candidates[1:best+1], candidates[:best])
	candidates[0] = lead
	return candidates
}
```

`bindCandidates(scope string, pool poolRoute, candidates []candidate)` changes in four places:
- The refresh of an existing binding writes `sessionBinding{Model: cand.Key, Selection: candidateSelection(cand), Pool: pool.Name, Provider: cand.Provider.Name, Used: now}`.
- After the loop over candidates, when the binding exists but its member is gone: `delete(h.sessions, scope)`, so the stale binding is not counted for its old connection.
- Before the eviction block: `if pool.Type == poolBalance { candidates = h.balanceFirst(pool.Name, candidates) }`.
- The new binding records `Pool: pool.Name, Provider: candidates[0].Provider.Name`.

Its doc comment becomes: "bindCandidates atomically chooses the first member for a session. A bound session keeps its member; in a balance pool (poolRoute) a new session goes to the least-loaded connection, chosen under the same lock that records it."

`moveSession` keeps `Pool: binding.Pool` and sets `Provider: to.Provider.Name`.

`local.go`: `cands = hl.bindCandidates(scope, poolRoute{cfg.poolName, cfg.poolType}, cands)`.

Test callers pass `poolRoute{}`: `affinity_test.go:96`, `profiles_storage_test.go:178,403`, `profiles_test.go:93,272`.

`ui/settings.html`, in the pool behavior form, replace the Task 5 type hint line with:

```html
<label>Тип пула <select name="type"><option value="failover" {{if ne .Settings.Type "balance"}}selected{{end}}>failover — новая сессия идёт на первую здоровую модель по порядку</option><option value="balance" {{if eq .Settings.Type "balance"}}selected{{end}}>balance — новые сессии распределяются между подключениями</option></select><small>Смена действует на новые сессии; закреплённые остаются на своей модели.</small></label>
```

- [ ] **Step 4: Run the tests**

Run: `go test -count=1 -run 'TestBalance|TestCodex|TestPool|TestFailover|TestAffinity|TestClientCancel' . && go test -count=1 ./... && go test -race -count=1 -run 'TestBalance|TestCodexPoolTypeSwitch' .`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add pool_settings.go settings.go main.go affinity.go health.go local.go ui.go ui/settings.html pool_balance_test.go local_codex_test.go affinity_test.go profiles_storage_test.go profiles_test.go
git commit -m "pools: balance type spreads new sessions across connections"
```

### Task 7 (separate block): Reload-error banner

Own acceptance, independent of Tasks 1–6 except that its test uses the Task 1 rule as the rejected input.

**Acceptance (all must hold):**
- Kept only in memory, per settings file (`providers.json` and the env file each have at most one entry): error text and the time it was first seen with that text; no history, nothing on disk.
- Settings page shows `файл <basename> не применён: <ошибка>; действует снимок от <время>` where `<время>` is when the currently active snapshot was applied (start or last successful reload).
- The previous snapshot stays active; the rejected file is never rewritten.
- The next successful reload of that file clears its banner.
- Log keeps one line per new error text (not one every tick).
- Point 4 behavior unchanged: the watcher uses strict `readProviders`, never the migration.

**Files:**
- Modify: `settings.go:20-29` (`configStore` fields), `settings.go:61-68` (`newConfigStore`), `settings.go:87-111` (`watch` → `pollOnce`)
- Modify: `ui.go` `settingsView` struct and builder (`ReloadErrors []reloadFailure`), `ui/settings.html:4-5`
- Test: `ui_test.go`

**Interfaces:**
- Produces: `type reloadFailure struct{ File, Err string; At, Snapshot time.Time }`; `(s *configStore) pollOnce()`; `(s *configStore) noteReload(file string, err error)`; `(s *configStore) reloadFailures() []reloadFailure` (sorted by `File`).

- [ ] **Step 1: Write the failing test** (`ui_test.go`)

```go
func TestReloadErrorBanner(t *testing.T) {
	cs, _, path := profileFixture(t)
	u := &uiServer{cs: cs}
	good, _ := os.ReadFile(path)
	bad := strings.Replace(string(good), `"providers":[`, `"providers":[{"name":"work","type":"codex","base_url":"`+codexBaseURL+`"},`, 1)
	writeRaw(t, path, bad)
	future := time.Now().Add(2 * time.Second)
	os.Chtimes(path, future, future)
	cs.pollOnce()
	v := u.settingsView()
	if len(v.ReloadErrors) != 1 || !strings.Contains(v.ReloadErrors[0].Err, "auth_id") || v.ReloadErrors[0].Snapshot.IsZero() {
		t.Fatalf("banner: %+v", v.ReloadErrors)
	}
	if _, ok := cs.get().local.provider("work"); ok {
		t.Fatal("rejected file was applied")
	}
	if data, _ := os.ReadFile(path); string(data) != bad {
		t.Fatal("rejected file was rewritten")
	}
	cs.pollOnce()
	if again := u.settingsView().ReloadErrors; len(again) != 1 || !again[0].At.Equal(v.ReloadErrors[0].At) {
		t.Fatal("same error re-stamped or duplicated")
	}
	writeRaw(t, path, string(good))
	later := future.Add(2 * time.Second)
	os.Chtimes(path, later, later)
	cs.pollOnce()
	if left := u.settingsView().ReloadErrors; len(left) != 0 {
		t.Fatalf("banner not cleared: %+v", left)
	}
}

func TestReloadErrorBannerRenders(t *testing.T) {
	u, h := testUI(t)
	u.cs.noteReload("/x/providers.json", errors.New("boom"))
	body := get(t, h, "GET", "/settings", nil).Body.String()
	if !strings.Contains(body, "файл providers.json не применён: boom; действует снимок от") {
		t.Fatal("banner not rendered")
	}
}
```

If `uiServer{cs: cs}` needs more fields for `settingsView`, construct it the way `testUI` does and swap in `cs`. If `profileFixture`'s file has no `"providers":[` prefix as written by `writeProviders`, locate the providers array with `json` instead: unmarshal to `map[string]any`, append the bad provider, marshal.

- [ ] **Step 2: Run to see it fail**

Run: `go test -count=1 -run 'TestReloadErrorBanner' .`
Expected: `cs.pollOnce undefined`.

- [ ] **Step 3: Implement**

`settings.go`: add to `configStore`

```go
	reloadMu   sync.Mutex
	reloadErrs map[string]reloadFailure
	appliedAt  time.Time
```

`newConfigStore` sets `s.appliedAt = time.Now()`.

```go
// reloadFailure is the latest rejected hand edit of one settings file. The
// previous snapshot keeps serving until the file is fixed.
type reloadFailure struct {
	File     string
	Err      string
	At       time.Time
	Snapshot time.Time
}

func (s *configStore) noteReload(file string, err error) {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	if err == nil {
		delete(s.reloadErrs, file)
		s.appliedAt = time.Now()
		return
	}
	if s.reloadErrs == nil {
		s.reloadErrs = map[string]reloadFailure{}
	}
	if old, ok := s.reloadErrs[file]; ok && old.Err == err.Error() {
		return
	}
	log.Printf("%s reload: %v", filepath.Base(file), err)
	s.reloadErrs[file] = reloadFailure{File: file, Err: err.Error(), At: time.Now(), Snapshot: s.appliedAt}
}

func (s *configStore) reloadFailures() []reloadFailure {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	out := make([]reloadFailure, 0, len(s.reloadErrs))
	for _, f := range s.reloadErrs {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].File < out[j].File })
	return out
}
```

`watch` becomes `go func() { for range time.Tick(every) { s.pollOnce() } }()`; `pollOnce` holds today's loop body with these changes:
- env: `changed, err := s.reloadEnv()`; `s.noteReload(s.envPath, err)` replaces the `log.Printf("env reload: %v", err)`; the success log stays.
- providers: on error, if `!os.IsNotExist(err)` call `s.noteReload(s.provPath, err)` (replaces the per-tick log); mtimes stay unchanged, so the file is retried each tick as today but logged once per new text; on success call `s.noteReload(s.provPath, nil)` beside the existing mtime update and log.

`ui.go`: `settingsView` gains `ReloadErrors []reloadFailure`, filled with `u.cs.reloadFailures()` in `settingsView()`. Add template func `base` (`filepath.Base`) to the existing FuncMap.

`ui/settings.html` after the Err banner (line 5):

```html
{{range .ReloadErrors}}<div class="err">файл {{base .File}} не применён: {{.Err}}; действует снимок от {{.Snapshot.Format "2006-01-02 15:04:05"}}</div>{{end}}
```

(use the class the existing Err banner uses.)

- [ ] **Step 4: Run tests**

Run: `go test -count=1 ./... && go test -race -count=1 -run 'TestReload|TestProfile' .`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add settings.go ui.go ui/settings.html ui_test.go
git commit -m "settings: show rejected hand edits until the next good reload"
```

Known, unchanged behavior to report (not fixed here): a UI save while the banner is shown writes the in-memory snapshot over the rejected manual file, exactly as today.

### Task 8: Dashboard per connection, README

**Files:**
- Modify: `ui/settings.html` (`#codex-subscription`), the `codex-login` template, `ui/codex_usage.html:5`, `README.md`
- Test: `ui_test.go`

- [ ] **Step 1: Write the failing test** (`ui_test.go`)

```go
func TestCodexSectionPerConnection(t *testing.T) {
	useTestCodexHome(t, "http://issuer.invalid", http.DefaultClient)
	var calls atomic.Int32
	old := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = old })
	http.DefaultTransport = usageTransport(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return usageResponse(500, ""), nil
	})
	_, h := codexUI(t)
	body := get(t, h, "GET", "/settings", nil).Body.String()
	for _, want := range []string{`"provider":"codex"`, `"provider":"work"`, `class="codex-usage"`, `hx-target="closest .codex-usage"`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s", want)
		}
	}
	if strings.Contains(body, "access_token") || strings.Contains(body, "private-refresh") {
		t.Fatal("token leaked into the page")
	}
	get(t, h, "GET", "/settings/codex/usage?provider=work", nil)
	if calls.Load() != 0 {
		t.Fatalf("page load hit the network %d times", calls.Load())
	}
}

// The provider pane's "import from Codex CLI" button names its provider;
// without it the import handler answers 400 after Task 3.
func TestCodexProviderPaneImportNamesProvider(t *testing.T) {
	useTestCodexHome(t, "http://issuer.invalid", http.DefaultClient)
	_, h := codexUI(t)
	pane := get(t, h, "GET", "/settings/provider?name=work", nil).Body.String()
	if !strings.Contains(pane, `hx-post="/settings/codex/import"`) || !strings.Contains(pane, `hx-vals='{"provider":"work"}'`) {
		t.Fatalf("import button without provider:\n%s", pane)
	}
	data, _ := json.Marshal(usageCredential("work-account"))
	writeRaw(t, codexAuth.cliPath, string(data))
	if code := get(t, h, "POST", "/settings/codex/import", url.Values{"provider": {"work"}}).Code; code != 200 {
		t.Fatalf("import: %d", code)
	}
	work, _ := codexStoreFor(provider{Name: "work", Type: "codex", AuthID: testAuthB})
	if !work.connected() || codexAuth.connected() {
		t.Fatal("import did not reach work's file")
	}
}
```

- [ ] **Step 2: Run to see it fail**

Run: `go test -count=1 -run TestCodexSectionPerConnection .`
Expected: FAIL — missing `"provider":"work"`. `go test -count=1 -run TestCodexProviderPaneImportNamesProvider .` fails with `import button without provider`.

- [ ] **Step 3: Implement**

`#codex-subscription`: `{{range .Logins}}<div class="codex-connection"><h4>{{.Provider}}</h4>{{template "codex-login" .}}<div class="codex-usage" hx-get="/settings/codex/usage?provider={{.Provider}}" hx-trigger="load" hx-swap="outerHTML"></div></div>{{end}}`.
`codex-login`: status poll `hx-get="/settings/codex/status?provider={{.Provider}}"`; login and import forms carry `<input type="hidden" name="provider" value="{{.Provider}}">` (the login form opens a new tab, so a hidden input rather than `hx-vals`).
`ui/settings.html` provider pane (the `{{if eq .P.Type "codex"}}` import button): add `hx-vals='{"provider":"{{.P.Name}}"}'`.
`codex_usage.html`: root element `class="codex-usage"`; button `hx-post="/settings/codex/usage" hx-vals='{"provider":"{{.Provider}}"}' hx-target="closest .codex-usage" hx-swap="outerHTML"`.
`README.md` (Codex section): a pool has a `type`. `failover` is the default (`type` absent = failover): a new session goes to the first healthy connection in pool order. `balance` is set explicitly on a pool (form or file) and sends each new session to the connection with the fewest sessions of this pool; the type switches without a restart. In both types a session stays on its connection and model while healthy, moves to the next on failure and does not return; a signed-out connection is skipped; Esc is not a failure. The old numeric pool `balance` is ignored (the global `ROUTER_LOCAL_BALANCE` is unchanged); each Codex provider is a separate subscription with its own login; new ones get `auth_id`; rename is not supported (remove + add + login); removed connections leave `codex-auth-<hex name>-<auth_id>.json` next to `codex-auth.json` — delete by hand if unneeded; a hand-added Codex provider without `auth_id` is rejected until added through the dashboard.

- [ ] **Step 4: Run tests**

Run: `go test -count=1 ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add ui/settings.html ui/*.html README.md ui_test.go
git commit -m "ui: login, import and usage per Codex connection"
```

### Task 9: Full verification and PR

- [ ] **Step 1:** `go test -count=1 ./...` — expect `ok  localrouter`.
- [ ] **Step 2:** `go test -race -count=1 ./...` — expect `ok  localrouter`.
- [ ] **Step 3:** `go vet ./...` — no output.
- [ ] **Step 4:** `git diff --check origin/main...HEAD` — no output.
- [ ] **Step 5:** Re-read the Review Points table; for each row run its named tests with `-v` and paste the PASS lines into the receipt.
- [ ] **Step 6:** Update `docs/features.md` in this PR, keeping each section's fields (what it does, where it is configured, code, limitations) and pointing at the new code lines:
  - §3: several Codex connections, each with its own sign-in, token file and catalog. Replace the limitation "one Codex account per router; a Codex 401 does not fail over": a signed-out member is skipped and a sign-in failure moves the request to the next member.
  - §5: the pool types `failover` (default) and `balance`, switched on the fly from the form or the file (field `type`). The pool's numeric `balance` is obsolete: ignored on read, not written; only the global `ROUTER_LOCAL_BALANCE` remains.
  - §7 «Здоровье»: inside a pool the rating does not set the order (pool order does); a client cancel (Esc) is not a failure; a Codex sign-in failure (401) is a failure and moves the request.
  - §8: a bound session stays on its connection and model while healthy, moves on failure and stays there; `balance` spreads only new sessions; a client cancel keeps the binding.
  - §10: limits are shown per connection.
  - Replace the "В работе" line about several Codex accounts and failover/balance pools with a pointer to these sections.

  Commit: `git add docs/features.md && git commit -m "docs: features for multiple Codex connections and pool types"`.
- [ ] **Step 7:** Public-repository check. Both commands must print nothing:
  - `git diff origin/main...HEAD | grep -v -F 'PUBLIC-CHECK' | grep -n -i -E 'theo|sasha|тео|саш|[^a-z0-9]1f[^a-z0-9]|[^a-z0-9]e6[^a-z0-9]|/Users/|receipts|decisions/|status\.md|@gmail|~/src|coordinator'` (PUBLIC-CHECK)
  - `git log -p origin/main..HEAD | grep -v -F 'PUBLIC-CHECK' | grep -n -i -E '<the same pattern>'` (PUBLIC-CHECK)

  The `grep -v` drops only these two lines of this plan, which carry the pattern itself and are marked `PUBLIC-CHECK`; nothing else in the branch may use that marker. A hit is fixed at its commit (`git commit --fixup`, then `git rebase --autosquash origin/main`) before the push.
- [ ] **Step 8:** Push the branch and open one PR. The PR text lists what changed and the verbatim test results; it names no people and links no private notes. The receipt, written outside the repository, covers:
  - the red results recorded in Tasks 5, 6, 10, 11 and 12;
  - that orphaned auth files need manual cleanup;
  - the unchanged behavior of a UI save over a rejected file;
  - that the manual two-account sign-in and usage check is left to the owner;
  - that the live router is untouched.

  Merge only after acceptance.
