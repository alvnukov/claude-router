# Official ChatGPT plan access implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:executing-plans` for native execution, or `superpowers:subagent-driven-development` for execution through task workers. Complete tasks in order.

**Goal:** Discover the selected account's current models and use its ChatGPT subscription through the router's own verified OAuth registration.

**Architecture:** Preserve provider type `codex`, named credential slots and the existing caller-owned tool loop. Add an explicit `chatgpt-plan` credential mode. Its catalog and inference requests use pinned public API endpoints; legacy credentials keep their existing transport until a verified new sign-in replaces them.

**Tech Stack:** Go 1.26, existing HTTP/SSE provider engine, Svelte/TypeScript UI, `github.com/coreos/go-oidc/v3` v3.21.0 for ID-token signature verification.

**Spec:** `docs/superpowers/specs/2026-10-01-chatgpt-plan-access-design.md`

## Global constraints

- Work only in `/Users/avvnukov/.codex/worktrees/chatgpt-plan-access/claude-router`. Keep the original checkout and installed service unchanged during implementation.
- Before editing an existing function, inspect its callers with happ `code`, `op=calls`. Run happ diagnostics on touched source files before handoff.
- Add regressions before implementation and observe their expected failure. Do not weaken existing tests. Explain intentional changes to old login expectations before editing them.
- Tests use temporary credential directories and deterministic local HTTP/JWKS/SSE fixtures. Never read production tokens or reach production endpoints in tests.
- Before authorization, requests retain their pinned public HTTPS URL. Fixture transports may rewrite it afterwards; redirects remain forbidden. Synthetic builds must never fall back to the internet.
- New grants require no API key. Do not silently migrate CLI credentials, parse opaque access tokens, borrow the Codex OAuth client ID, or manufacture a CLI version.
- Use bounded responses, private atomic files and existing locks. Do not log OAuth URLs, tokens, ID-token hints or registration identifiers.
- Focused checks precede adjacent suites. Run the repository suite once at the end because auth, routing, privacy and catalog contracts all change. Delegate long installs/builds/browser operations and monitoring to a worker under the user's infrastructure rule.

## Review focus

1. Callback client-ID substitution, invalid nonce/signature and a changed returning subject must not overwrite active credentials: Task 1 signed-JWKS fixtures; Task 2 adoption tests.
2. Concurrent refresh, rotated disk credentials and a failed atomic write must not expose unpersisted tokens: Task 2 multi-store and failure tests.
3. Identity-only sign-in and removed subscription scope must not become routing candidates: Tasks 1/2 grant tests and Task 4 UI assertions.
4. Namespaced function calls, forced choice and replay across account/privacy boundaries must preserve the caller's tool contract: Task 3 complete tool round trips.
5. A plan token must never reach private quota/backend endpoints, and a failure after partial output must never restart generation: Tasks 3/4 target and streaming tests.

## File responsibilities

- `internal/chatgptplan/{identity,oauth,host}.go` and matching tests own OAuth registration, verified identity, renewal metadata and stable host identity.
- `codex_auth.go`, `codex_oauth.go`, `codex_stores.go`, `ui.go`, new `codex_plan_test.go` and existing auth tests integrate the new mode with credential lifetime and browser login.
- `codex_transport.go`, new `codex_plan_payload.go` and tests, `internal/catalogstartup/{fetch,source_synthetic}.go` and tests, `internal/providers/codex/protocol.go` and tests own transport selection, request adaptation and public catalog fixtures.
- `codex_usage.go`, `ui_api.go`, `internal/ui/model.go`, UI components/types and new `internal/buildinfo/version.go` expose only safe connection and build information.
- `README.md`, generated `internal/ui/web/dist` assets and a focused browser scenario document and exercise the user-facing migration.

## Task 1: Verified registration and host identity

**Files:** Add `internal/chatgptplan/identity.go`, `oauth.go`, `host.go` and corresponding `_test.go` files; update `go.mod`, `go.sum`.

**Produces:**

- Constants `AuthMode = "chatgpt-plan"`, `Issuer = "https://auth.openai.com"`, `Resource = "https://api.openai.com/v1"`, `ModelsURL = Resource + "/models"`, `ResponsesURL = Resource + "/responses"`.
- `Registration` with `Issuer`, `Subject`, `ClientID`, `HostID`, `Email`, `Name` strings; `Scopes []string`; `ExpiresAt`, `EarliestRefreshAt time.Time`. All fields have explicit JSON names.
- `Tokens` with `AccessToken`, `RefreshToken`, `IDToken string`.
- `Registration.IdentityKey() string`: deterministic collision-resistant hash of issuer, subject and issued client ID; `Registration.SharingEnabled() bool`: granted scope contains `chatgpt.tokens.use.direct`.
- `HostID(ctx context.Context, path string) (string, error)`: locked private atomic persistence of a UUID URN, rejecting invalid stored identity.
- `NewAttempt(issuer, redirectURI, hostID string, previous *Registration, idTokenHint string) (*Attempt, error)`; `Attempt.URL string`; `Attempt.Callback(values url.Values) (code, clientID string, err error)`; `Attempt.Exchange(ctx context.Context, client *http.Client, code, clientID string) (Registration, Tokens, error)`.
- `Renew(ctx context.Context, client *http.Client, registration Registration, tokens Tokens) (Registration, Tokens, error)`.

- [ ] Add signed RSA/JWKS fixture tests for initial registration and returning authorization. Assert exact issuer paths, resource, scopes, name hint, stable host ID, PKCE S256, state and nonce. Assert callback state is checked even on errors, duplicate/empty values are rejected, initial issued client ID is required and returning client ID cannot change.
- [ ] Run `go test ./internal/chatgptplan -count=1`; observe missing implementation. Add the pinned OIDC dependency and minimal types/flow, retaining the same redirect URI for authorize and exchange.
- [ ] Add negative verification cases: wrong signature, issuer, audience, expiration, nonce, missing subject/issued-at and changed returning subject. Add valid identity-only scope and opaque-access-token cases; expiry comes exclusively from the token response. Use published JWKS discovery with the bounded no-redirect client and maintained verifier; additionally enforce nonce and required identity claims.
- [ ] Verify scope and rotating-token responses through `Renew`: use issued client ID, new token endpoint and resource; retain omitted refresh token/ID hint and granted scope on refresh, but honor an explicitly returned scope. Validate any new ID token against the retained issuer/subject/client ID, without requiring the original login nonce on refresh. Respect earliest refresh guidance without indefinite retries.
- [ ] Add host-ID restart/concurrent-creation tests, owner-only permissions and malformed/oversized token/JWKS responses. Implement only behavior required by those assertions.
- [ ] Run the package suite once green and commit `feat: add verified ChatGPT plan registration`.

## Task 2: Credential lifetime and browser adoption

**Files:** Modify `codex_auth.go`, `codex_oauth.go`, `codex_stores.go`, `ui.go`; add `codex_plan_test.go`; extend auth tests where relevant.

**Consumes:** Task 1 `Registration`, `Tokens`, `HostID`, `NewAttempt`, `Renew`.

**Produces:** `codexCredential.Plan *chatgptplan.Registration`; `codexCredential.accountKey() string`; `codexAuthStore.startPlanBrowserFlow(ctx context.Context, addr string) (*codexBrowserFlow, error)`. Existing public/root provider interfaces retain their signatures.

- [ ] Add failing tests that read both legacy `chatgpt` and new `chatgpt-plan` records. Require consistent registration, tokens, expiry and stable identity for the new mode; do not require legacy `chatgpt_account_id` or JWT-shaped access tokens. A verified identity-only record is connected but cannot generate or enter a pool.
- [ ] Run `go test . -run 'TestChatGPTPlanCredential' -count=1`, then implement mode-aware validation, expiry and `accountKey()`. Legacy keys remain their stored account ID; new keys use verified registration identity.
- [ ] Add browser integration tests for first/returning login, ephemeral numeric-loopback callback, failed consent, identity mismatch and provider deletion/replacement during pending login. Preserve old credentials until verification and a successful atomic write. Switch the UI login entry point to `startPlanBrowserFlow`; preserve legacy import behavior and explicit legacy-flow tests.
- [ ] Extend `codexBrowserFlow` with a plan attempt and callback client ID. Use the existing result channel/one-shot listener lifecycle; process plan callbacks through `Attempt.Callback` and exchange through `Attempt.Exchange`. Persist host identity alongside the credential directory and reuse registration/client ID/ID hint for returning plan sign-ins. Request consent again only when enabling a missing sharing grant.
- [ ] Add refresh tests for two stores sharing one file, rejected-token single-flight, revoked grants, scope removal, quiesced lifecycle and persistence failure. Run focused tests red, then branch renewal by mode, reload rotated disk credentials under the existing lock and save before adopting in memory. Reject retries if registration or mode changed.
- [ ] Preserve `authorize`/`doWithReauth` signatures using an internal request-context snapshot of mode and account key installed during authorization. Reauthorization validates the captured snapshot; new-mode authorization permits only GET models and POST responses at the exact public origin/path, without userinfo, alternate ports, query injection or redirects. Legacy authorization keeps its existing restrictions and headers.
- [ ] Run focused new tests, then `go test . -run 'Test(Codex|ChatGPTPlan)' -count=1`; commit `feat: use ChatGPT plan credentials in router login`.

## Task 3: Public catalog and Responses requests

**Files:** Modify `ui.go`, `codex_transport.go`, `internal/catalogstartup/fetch.go`, `source_synthetic.go` and targeted tests; add `codex_plan_payload.go`, `codex_plan_payload_test.go`, `codex_plan_transport_test.go`; extend `internal/providers/codex/protocol.go` and tests only for public subscription errors.

**Consumes:** Credential mode, `accountKey()`, authorization snapshot and Task 1 pinned endpoint constants.

**Produces:** `catalogstartup.ProbeInput.ChatGPTPlan bool`; `toChatGPTPlanPayload(body []byte) ([]byte, error)`; mode-aware request/replay selection. No second agent or tool executor.

- [ ] Add a failing catalog fixture using the actual provider probe/refresh path. Return list-visible `gpt-6.1-sol` and reasoning levels, then assert order, persisted catalog, inherited routing and UI model visibility. A failed later refresh preserves the successful catalog. Assert the selected plan token targets public models with no `client_version`; legacy tokens retain their target.
- [ ] Run `go test . ./internal/catalogstartup -run 'TestChatGPTPlanCatalog' -count=1`, then implement mode-aware endpoint selection in `ProbeInput` and `FetchModels`. Derive mode from the captured credential and bind authorization to the same identity. Existing legacy synthetic fixtures keep their old version query.
- [ ] Extend the synthetic manifest with optional `chatgpt_plan_models_url`, validated as numeric loopback `/v1/models` with no query; accept only explicitly marked plan fixture credentials in that transport. Authorize the original pinned public URL before rewriting to its fixture. Add denial tests for absent targets, production credentials and redirects; no permissive network fallback.
- [ ] Add payload tests for instructions/full history, `store=false`, `stream=true`, namespace grouping, all tool-choice forms already accepted by the router, tool call/output IDs and forbidden fields. Group unique local function names in namespace `functions`; preserve original function names. Forced choice stays the documented `{type:"function", name:<original>}` shape; preserve `namespace:"functions"` on function-call input items. Do not invent a namespace field on forced-choice objects.
- [ ] Run `go test . -run 'TestChatGPTPlanPayload' -count=1`, then implement the adapter around existing `toCodex` output. Omit unsupported subscription fields, HTTP previous-response continuation and private session/turn-state headers. Do not add hosted tool search. Preserve already restored opaque output items and encrypted reasoning rather than remapping away their IDs/provenance.
- [ ] Add actual streaming tool-round-trip fixtures, including forced calls, assistant continuation, process restart/account switch and privacy-protected turns. Capture the mode/account snapshot before selecting target and replay scope; ensure no new token is sent to an old/private target after a concurrent login. Use the unchanged caller-owned execution loop.
- [ ] Add partial-output failure and `subscription_sharing_usage_limit_exceeded` / `subscription_sharing_usage_unavailable` tests at the existing provider error boundary. Require `response.completed`; incomplete/failed/interrupted streams remain failures and cannot retry once output starts.
- [ ] Run new transport tests and adjacent `go test ./internal/providers/codex ./internal/catalogstartup -count=1`; verify relevant synthetic-tag tests; commit `feat: route ChatGPT plan catalog and inference through public API`.

## Task 4: Safe connection status, own version and release verification

**Files:** Modify `codex_usage.go`, `ui_api.go`, `ui.go`, `internal/ui/model.go`, `internal/ui/web/src/types.ts`, `CodexLogin.svelte`, `Connections.svelte`, `App.svelte`, `README.md`; add `internal/buildinfo/version.go` and tests plus `scripts/ui-chatgpt-plan-check.mjs`; regenerate UI dist assets.

**Consumes:** Plan registration's safe identity/scope metadata and existing provider catalog `UpdatedAt`.

**Produces:** `buildinfo.Version() string`, `buildinfo.UserAgent() string`; `State.Version string` with JSON `version`; `Connection.AuthMode string`, `SubscriptionEnabled bool`, `CatalogUpdated time.Time`, `UsageURL string` with camel-case JSON names. No private registration identifiers in these DTOs.

- [ ] Add failing state/usage tests: plan identity-only and enabled states differ, account display uses verified metadata, catalog time is independent of quota observation, and plan usage polling performs zero private HTTP requests. Legacy quota behavior remains covered by its existing fixtures.
- [ ] Run `go test . -run 'TestChatGPTPlan(State|Usage)' -count=1`, then implement mode-aware usage/account projection. New-mode quota limits remain unknown and link to ChatGPT Usage settings; do not mark unknown quotas blocked or invent percentages. Refresh catalog after successful enabled login through the existing lifecycle-controlled updater, preserving good data on failure.
- [ ] Add version tests using injected build-info fixtures: prefer actual VCS revision and dirty marker, otherwise `dev`. Publish it in UI state and use `claude-router/<version>` for the new OAuth/catalog/inference requests. Reuse this helper rather than adding a second version source.
- [ ] Update connection UI with `Continue with ChatGPT`, explicit subscription permission and an enabling action when absent; retain migration/import distinction. Display router version and last successful catalog update without showing a Codex CLI version. Add a browser scenario that exercises these states and verifies the consent popup interaction without live OAuth.
- [ ] Delegate a worker to install locked UI dependencies, run `npm run build` and the focused browser scenario in isolated fixture state. Preserve generated assets. Document subscription scope/limits, new browser consent, legacy migration and actual version display in README.
- [ ] Run `go test ./... -count=1` once for the cross-cutting regression risk, plus focused race tests for new host/refresh/adoption paths and affected build-tag suites. Fix genuine failures without weakening assertions. Obtain one fresh independent whole-branch review and happ diagnostics; address actionable findings with focused rechecks.
- [ ] Commit `feat: expose ChatGPT plan status and router build version`. Report exact checks and any outstanding live verification separately.

## Live completion boundary

After reviewed automated checks, build/deploy the verified worktree revision using the router's existing local deployment mechanism. Start the selected connection's new login and have the user complete browser consent; this is an interactive authentication step, not approval of a patch. Verify successful catalog refresh and one completed minimal inference through the new mode before claiming the missing-model problem fixed. If consent has not completed, leave tested changes and a working login ready, and state that live subscription access remains unverified. Never substitute a version bump or reuse the legacy token to manufacture that result.

## Plan self-review

Spec sections map to Tasks 1–4 and the live boundary. OAuth signatures, credential fields and endpoint constants have one owner and are consumed consistently. Review-focus failures have named fixture coverage in their owning tasks. The four tasks share auth/identity interfaces and must execute in order; native execution with one independent final reviewer is recommended for lower overhead.
