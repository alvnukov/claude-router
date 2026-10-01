# ChatGPT plan access for claude-router

## Intent and success criteria

The user approved migrating the router to official Sign in with ChatGPT so
subscription access uses the router's own identity and current account model
catalog. The router must discover `gpt-6.1-sol` when the selected account can
access it, without impersonating a Codex CLI version. Claude Code's existing
requests, routing profiles, pools, session isolation and privacy controls keep
their contracts.

Completion requires a verified OAuth registration, a successful authenticated
catalog refresh and a completed inference request through the public Responses
API. Automated fixtures prove protocol and failure handling; live access also
requires the user to complete browser consent.

## Evidence

With the same router credential, the private Codex catalog excludes
`gpt-6.1-sol` with `client_version=0.156.0` and includes it with `0.159.2`.
Omitting the version returns HTTP 400. That existing credential receives HTTP
403 from `https://api.openai.com/v1/models`; it cannot be silently reused as a
new ChatGPT plan grant.

## Approach

Keep provider type `codex` and its existing named credential slots. New browser
sign-ins use the official ChatGPT plan OAuth flow. Store an explicit new auth
mode and registration metadata, so transport policy can distinguish new grants
from existing Codex credentials. Existing credentials continue to work during
the transition and are replaced only after the new identity is verified.

Direct Responses requests preserve the router's current tool execution model.
Using Codex app-server for generation would introduce a second agent/tool loop
and process lifecycle. Updating the old hard-coded CLI version would leave the
original catalog dependency in place. Neither is part of this migration.

## Registration and identity

- Persist one stable host identity (`ext_agent_host_id`, a generated UUID URN)
  alongside the router's credential directory. Reuse it across connections and
  restarts. Creation is locked and atomic.
- First registration uses `client_id=dynamic_agent_client` and
  `agent_name_hint=Claude Router`; subsequent authorization reuses that
  registration's issued `client_id`, retained `id_token_hint` and host identity.
- Authorize at `https://auth.openai.com/api/accounts/authorize` with
  `resource=https://api.openai.com/v1` and scopes
  `openid profile email offline_access resource.invoke chatgpt.tokens.use.direct`.
- Bind fresh state, nonce and PKCE S256 values to a one-time pending login.
  The callback is `http://127.0.0.1:<port>/auth/callback`; authorization and code
  exchange use the identical URI. An error callback also requires valid state.
- A first-registration callback must supply an issued client ID, never
  `dynamic_agent_client`. A returning callback cannot replace its client ID.
- Exchange the code at
  `https://auth.openai.com/api/accounts/oauth/token`, using the issued client ID,
  verifier, callback URI and resource. No API key or client secret is involved.
- Verify the ID token signature using OpenAI's published JWKS and enforce issuer,
  issued client ID audience, subject, expiration and original nonce. Returning
  authorization must preserve the selected issuer/subject/client registration.
  Access-token authentication metadata remains opaque.
- Decide subscription permission from the token response's granted scopes.
  Retain a valid identity-only sign-in with subscription access marked disabled;
  do not send inference requests without `chatgpt.tokens.use.direct`.

## Credentials and renewal

Store mode, issuer, verified subject, issued client ID, host ID, display identity,
granted scopes, token response expiry, optional earliest refresh time and all
tokens in the existing private per-connection record. Files remain atomic and
owner-only (0600). Do not expose tokens, authorization hints or client IDs in
logs, history or the public UI state.

The new mode uses the issued client ID and resource when refreshing at the new
token endpoint. Persist rotating access/refresh tokens, expiry and granted
scopes together before adopting the result in memory. Use the existing
inter-process credential lock and single-flight rejected-token behavior. A
failed refresh does not replace usable stored credentials. Revoked access
requires sign-in; never retry generation after streaming has started.

Account and replay identity include the verified issuer, subject and issued
client ID. Two registrations with the same email remain separate. Pending
authorization does not change the active connection; a deleted or replaced
provider cannot adopt a completed login.

## Catalog and generation

New grants may authorize only pinned HTTPS requests to
`https://api.openai.com/v1/models` and
`https://api.openai.com/v1/responses`. Reject redirects and third-party targets.
Never send new grant tokens to the private ChatGPT backend, including quota
polling. Existing Codex tokens retain their separate endpoint restrictions.

Use `GET /v1/models` with the selected connection's bearer token. Preserve
server ordering, list-visible slugs, display names and supported reasoning
levels. Refresh after successful login and through the existing manual/hourly
updater. Preserve the last successful catalog on failure. Catalog presence is
not proof of entitlement; completed inference verifies model access.

For `POST /v1/responses`, retain the existing stateless mapping with `store=false`
and `stream=true`, full required context in `input`, and `instructions` rather
than system-role input items. Apply the documented ChatGPT plan restrictions:
omit unsupported fields and previous-response HTTP continuation; group local
function/custom tools into supported namespaces or additional-tool items.
Preserve call IDs, tool-choice semantics and names in the client-facing protocol.
Scope opaque reasoning replay to the new registration and retain privacy
provenance checks. Private Codex session/turn-state headers are not part of the
new public HTTP contract.

Reuse the existing SSE parser and require `response.completed` for success.
Interrupted, incomplete and failed streams remain failures. A subscription
limit error after output begins must not restart generation through failover.
User-Agent identifies `claude-router` and its actual build revision/version;
there is no fabricated CLI `client_version` query in the new mode.

## User experience and transition

Use the existing connection screen and browser login interaction. Label the
new action Continue with ChatGPT and show whether subscription permission was
granted. A known older connection can be reauthorized without deleting models,
pools or history; its old token stays active until the new login succeeds.
CLI credential import continues to identify a legacy Codex connection and
cannot masquerade as the new OAuth grant.

The existing private Codex quota endpoint is not a documented quota API for
new grants. Show quota data as unavailable for that mode and link to ChatGPT's
Usage settings; do not invent percentages or poll with an incompatible token.
Expose the router's own build version and last successful catalog refresh,
rather than a Codex CLI version that the router is not running.

## Verification

Use deterministic local OAuth/JWKS, catalog and Responses fixtures, with no
production credentials or network in tests. Add failing tests before changes.
Cover first registration and reauthorization, wrong state/nonce/signature/
issuer/audience, callback client-ID substitution, missing subscription scope,
identity changes, host-ID persistence, atomic rotation and refresh concurrency.

Exercise the actual provider probe, catalog persistence and request adapter:
the new model and supported efforts must appear in UI state; public targets
must receive the matching token; legacy tokens, quota requests and redirects
must remain isolated. Cover function tool round trips, namespaced forced tool
choice, streaming failure after partial output, account switches, restart and
privacy boundaries. Existing tests are changed only where the new sign-in
contract intentionally replaces their previous requirement, with the reason
recorded before editing.

Run focused checks first, adjacent auth/catalog/provider checks next, and the
repository suite once for the concrete cross-cutting regression risk. Obtain an
independent final review and happ diagnostics on touched source files. Deploy
locally only after verification; then let the user complete OAuth consent and
verify live catalog and inference before claiming the original issue resolved.

## Sources

- [Registration and sign-in](https://developers.openai.com/siwc/token-sharing-open-source/sign-in)
- [Accounts and sessions](https://developers.openai.com/siwc/token-sharing-open-source/profiles-and-sessions)
- [ID token verification](https://developers.openai.com/siwc/website)
- [Models and inference](https://developers.openai.com/siwc/token-sharing-open-source/models-and-inference)
- [Preview limitations](https://developers.openai.com/siwc/token-sharing-open-source/preview-limitations)
- [Errors and recovery](https://developers.openai.com/siwc/token-sharing-open-source/errors-and-recovery)
