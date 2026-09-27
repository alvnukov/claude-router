# Router UI implementation

This work lives in `codex/router-ui`, based on `origin/main` at `4365961`.
The visual reference is the accepted v2 prototype in
`/Users/zol/src/coordinator/notes/ui-prototype/v2/` and the UI redesign decision
of 2026-09-25, including the prototype review corrections.

## Plan

1. Add `internal/ui`: embedded production assets, a typed JSON read model,
   bounded request history/detail endpoints, refresh events and guarded actions.
   Bridge the existing private configuration and routing types in `ui_api.go`;
   reuse their validation, persistence and account isolation.
2. Implement the four Svelte/TypeScript screens: overview, requests, routes,
   connections. Use real server data, accessible confirmation dialogs,
   light/dark/system themes and responsive navigation.
3. Preserve existing configuration and raw download contracts. Do not show
   synthetic traffic, privacy counters or bypass detections that the backend
   cannot provide. Reject destructive account removal while references remain.
4. Pin the build toolchain and dependencies, check types, embed the generated
   assets, enforce the size budget and add a reproducibility check to CI.
5. Verify API security, isolated configuration changes and browser workflows
   on synthetic test homes, including mobile layout and both themes. Never
   use the live router home or modify the user's Claude settings for testing.

## Integration boundary

The current base has history, limits and platform packages, while routing,
configuration and the existing UI backend still use private root-package
symbols. The new `internal/ui` package owns the web application and HTTP
contract. An adapter in `ui_api.go`, wired from the existing `ui.go`, is necessary until the planned
configuration/routing extraction lands; moving the entire backend is a
separate refactor.

## Verification

Verified on macOS, Node 26.7.0:

- `go test ./... -count=1`: all seven packages pass.
- `go test -race . ./internal/ui -run '^(TestUIJSON.*|TestRouterServerStandbyAndActivation|TestUIReject.*|TestUIEvents.*)$' -count=1`: both packages pass.
- `go vet ./...` and `golangci-lint run ./...`: pass, zero lint issues.
- `npm ci --ignore-scripts && npm run build`: zero Svelte/TypeScript errors
  and warnings. The fresh install/build reproduces every asset byte for byte;
  the asset set is 134004 bytes, below the 307200-byte budget.
- `npm run test:browser` and `npm run test:browser -- --webkit`: both pass.
  Scenarios cover real local/cloud proxy requests, request search/detail,
  account creation, independent credential identity, display-name rename,
  rejection of referenced connection removal, route save, pool ordering/cloning,
  per-member effort inheritance and mapping, compact route summaries with
  explicit exceptions, draft survival across refresh, recorded effort details,
  profile activation, all four pages, both themes and 375px width.
  Browser runs assert no page errors, no runtime CSP violations, no API key
  in the read model, and rejection of a foreign-origin mutation.
- axe-core WCAG A/AA scan: zero violations on the four main screens in
  fresh light and dark Chromium contexts. This is not a manual screen-reader
  audit and does not claim coverage of every expanded dialog.
- UI event streams are cancelled during router shutdown; an open dashboard
  no longer prevents graceful stopping. The existing server lifecycle test
  now keeps an event stream open while stopping the router.

The browser harness only uses temporary router/Claude homes and a loopback
upstream. WebKit screenshots are captured in separate pages because
Playwright's screenshot helper injects a stylesheet blocked by CSP; the
functional page keeps the strict zero-CSP-violations assertion.

Existing test request hosts now explicitly use localhost, as production
rejects httptest's default example.com. Ordinary `/settings` page assertions
now expect the SPA; legacy fragment assertions and mutation invariants remain.

## Pool effort semantics

Pool members retain their existing `effort` behavior. The optional value
`request` preserves the incoming effort; a request without effort still sends
no effort. `effort_map` supplies per-input overrides, e.g.
`{"high":"xhigh","medium":"low"}`. Missing entries use the main rule; an
explicit empty value uses the model default. The same resolution is applied to
each failover candidate. Fixed mappings are checked against known model
capabilities. Clones and profile snapshots deep-copy these maps; catalog
inheritance retains them only when fixed values are supported by the new model.

Additional targeted/race tests cover all incoming efforts, failover, Codex
serialization, persisted mapping/clone independence, duplicate and stale-profile
rejection, recorded effort evidence and absent/unknown values. The two existing
profile assertions now use deep equality because pool targets include a map;
the expected values and invariants are unchanged. happ diagnostics are clean
on the touched implementation and test files; the initial cached diagnostics
cleared after the changed dependency files were loaded.

UX priorities and unimplemented follow-ups are in `ui-routing-ux.md`.

## Default pool and debug traffic

Each routing profile can opt into `default_pool`; empty retains rejection.
The fallback applies only when the incoming model has no nonempty version or
family rule set. Explicit disabled routes and missing levels of a configured
model/family remain disabled. Pool existence is validated, deleting the selected
pool is rejected, and empty pools are warned about in the UI. Profile creation,
cloning, switching and separate-file persistence retain this setting.

The request list can filter `unrecognized=1`: unknown paths passed upstream,
invalid message bodies, unreadable bodies, missing routes and default-pool
requests. It shows method/path, status, reason, incoming model and final target.
Details open on raw incoming content and show safe protocol headers. Paths
exclude query parameters; credential headers are never captured. Successful
fallback use is distinct from an HTTP error and includes the selected pool.

Unknown-path bodies are tee-captured as the proxy consumes them; uploads and
responses continue streaming. Debug request and response prefixes are limited
to 256 KiB. Rejected message bodies also have a 256 KiB history cap. Partial
capture is explicit and never claims a complete downloadable file. Successful
model traffic, including default-pool requests, retains normal history limits.
Added record fields are omitted when empty, preserving existing history fixtures.

Checks include unchanged upstream request/response bytes and query/auth forwarding,
request and response streaming, persistence/reload, capture bounds/read failures,
default-rule precedence and effort resolution, profile isolation and deletion
protection. Targeted race tests pass. Chromium and WebKit pass default-pool and
raw-debug workflows, including successful unrecognized traffic and error-only
filtering. Full Go suite passes; lint reports zero issues.

## Remaining integration work

- Legacy templates and static assets remain for existing fragment and form
  clients. Browser navigation uses the new application. Removing that adapter
  belongs with extraction of the remaining root UI backend.
- Session links describe the last recorded request, not an independent
  detector of Claude Code traffic bypassing the router. Bypass detection,
  a persistent interception change log and privacy P3 require backend work
  and are not presented as implemented.
- SSE emits a refresh signal every three seconds; it is not a persistent
  incident/event history.
- The Linux reproducibility job is configured in CI; a remote CI run and
  native Safari/Firefox testing have not been performed in this worktree.
- The test harness does not perform live deployment or account login.

## Review corrections

The Standards and Spec review against `4365961` found five functional defects
and one duplication concern, now corrected and independently rechecked:

- Route editors are keyed by model ID, so adding a sorted catalog entry cannot
  move a draft to another model.
- The read model carries explicit rule presence. Autofill preserves disabled
  rules; saving other changes preserves absent rules as inheritance.
- Compact matching checks effective inherited destinations, exposing exceptions.
- Model IDs containing colons retain their complete identity during effort fill.
- Request/session/attempt display errors and failed response previews use the
  credential sanitizer. Original history and raw download contracts are retained.
- Message and token-count diagnostics use the same annotation function.

`TestUIJSONHistoryErrorsHideCredentials` and all four browser regressions failed
before the fixes and pass afterwards. The browser scenarios live in
`scripts/ui-review-regressions.mjs` and run in both Chromium and WebKit as part
of the isolated harness. Full Go tests, focused race tests and lint pass.
