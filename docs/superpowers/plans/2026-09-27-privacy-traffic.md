# Privacy traffic integration — implementation plan

> Use superpowers:executing-plans. Implementation stays uncommitted, per implement skill and current user authorization.

**Goal:** Apply privacy profiles before real upstream traffic and restore responses safely.
**Architecture:** Immutable policy snapshot + request-owned exchange in internal/privacy, thin router adapter at the request/candidate seams. Atomic bounded response validation; no raw history while privacy is enabled.
**Tech Stack:** Go standard library, existing privacy module, Svelte UI.
**Spec:** docs/privacy-traffic.md

## Global Constraints

- Existing isolated worktree codex/privacy-ui; preserve other worktrees and main.
- No new dependencies, deployment, push or commit.
- Errors fail closed; no raw content in diagnostics.
- 32 MiB input, 16 MiB response, four admitted protected requests.

## Review Focus

- Unknown fields/opaque thinking and metadata cannot silently bypass masking (Task 1).
- Policy changes and file removal cannot race into raw failover (Tasks 1, 2).
- No partial tool call or truncated SSE accepted (Tasks 1, 2).
- Header/redirect/error/history paths cannot export input (Task 2).
- UI state distinguishes applied policy, bypass and failures (Task 3).

### Task 1: Runtime and strict transport exchange

**Files:** internal/privacy/runtime.go, transport.go, runtime_test.go.
**Interfaces:** NewRuntime(home) *Runtime; Snapshot() (*Policy,error); Policy.Enabled(); Policy.Prepare(Target,body) (*Exchange,[]byte,error); Exchange.Restore(body,stream), Close(); Runtime.State()/Reject().
- [x] Write tests for masking/restoration, stable sessions, changed profiles, invalid/deleted files, uncovered values, opaque blocks and atomic SSE errors.
- [x] Run go test ./internal/privacy -run 'TestRuntime|TestTransport' -count=1; expect missing implementation failure.
- [x] Implement bounded cached immutable snapshots, private marker/namespaces, strict envelope and restoration.
- [x] Same command passes; record evidence.

### Task 2: Router integration

**Files:** privacy_traffic.go, privacy_traffic_test.go, main.go, local.go, settings.go, ui_privacy.go.
**Interfaces:** configStore.privacyRuntime(); protected request context carries Policy; per-candidate preparation, buffered response completion. Existing disabled routing untouched.
- [x] Write real HTTP tests: Anthropic/OpenAI, SSE, count tokens, failure/retry, privacy scopes, no history, no redirects/header leaks, bounded input/cancellation.
- [x] Run go test . -run TestPrivacyTraffic -count=1; observe failure before implementation.
- [x] Wire runtime ahead of history; mask before each candidate translation; keep response atomic; sanitize upstream errors.
- [x] Same command passes; record evidence.

### Task 3: UI, documentation, verification

**Files:** internal/ui/privacy.go, internal/ui/web/src/Privacy.svelte, privacy.ts, PrivacyProfiles.svelte, docs/privacy-ui.md, docs/features.md, scripts/ui-privacy-check.mjs.
**Interfaces:** /api/ui/privacy includes runtime counts and status independently from lab.
- [x] Update HTTP/browser assertions before changing displayed behavior; check failure.
- [x] Show current runtime state and counts, clarify buffered streaming and explicit bypass.
- [x] Run targeted tests, Svelte check/build, browser checks; full Go tests/build/vet/lint and narrow race tests; independent final code review.
- [x] Update evidence and handoff, refresh isolated preview only.
