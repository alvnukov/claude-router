# Synthetic Codex loopback build

This is a **test-only build tag**, not a runtime flag or a production configuration. The ordinary build still authorizes Codex only against `https://chatgpt.com/backend-api/codex/...` and uses its normal transport. Never install or deploy a tagged binary. Use only synthetic request bodies and dummy credentials in separate temporary homes; do not send real client transcripts or provider requests.

`router_codex_loopback` substitutes a fail-closed `http.RoundTripper` **after** the unchanged production Codex origin authorization. It accepts only `POST https://chatgpt.com/backend-api/codex/responses` with the exact dummy access token and account below. It sends that body and allowlisted headers to `ROUTER_CODEX_TEST_STUB_URL=http://127.0.0.1:<port>` (or an explicit IPv6 loopback IP). An absent, non-IP-loopback, credential-bearing, non-HTTP, or non-origin-only stub URL fails closed. OAuth refresh targets fail closed; startup config watching, model checker/catalog probes, and usage refresh are suppressed in this test build. HTTP redirects are not followed. These restrictions apply to the Codex test path; the binary is **not** a universal network sandbox for arbitrary non-Codex routes or manual UI operations.

## Build and execute (only in an authorized isolated test environment)

From this source worktree, with the tester's `notes/readiness-e2e/{stub.py,privacy-on.json,privacy-off.json}` available:

```sh
TESTER_FIXTURES=/path/to/tester/notes/readiness-e2e
TEST_BIN="$(mktemp -d)/localrouter-codex-loopback"
go build -tags router_codex_loopback -o "$TEST_BIN" .
go version -m "$TEST_BIN" | grep -- '-tags=router_codex_loopback'
PYTHONDONTWRITEBYTECODE=1 python3 notes/codex-loopback/verify.py \
  --binary "$TEST_BIN" --fixture-dir "$TESTER_FIXTURES"
```

`TESTER_FIXTURES` is the directory containing those three files; set it to that directory before running the command. The verifier **refuses a binary without the exact build tag before starting any router or issuing a client request**. It starts the tester's stub on an ephemeral `127.0.0.1` port; launches each router with separate fresh `HOME`, `CODEX_HOME`, `ROUTER_HOME`, auth/profile files, and ephemeral API/UI ports; forces `ROUTER_ENV_FILE` to a nonexistent file; and sets HTTP(S) proxies to a dead loopback port as an additional guard. It stops its child processes and removes the temporary homes. It checks one observed canonical Codex stub call per client request, the status/Retry-After matrix, and absence of the upstream canary/token in the client body. This is a local synthetic HTTP-error test, not a real-provider, installed-client, or valid native SSE fixture.

The runner's exact temporary manifest is:

`ROUTER_PROVIDERS_FILE=$ROUTER_HOME/providers.json`:
```json
{"providers":[{"name":"codex","type":"codex","base_url":"https://chatgpt.com/backend-api/codex"}],"models":[{"provider":"codex","model":"gpt-5"}]}
```

`$ROUTER_HOME/providers.json.active-profile` contains the JSON string `"default"`; `$ROUTER_HOME/providers.json.profiles/default.json` contains:
```json
{"family_routes":{"opus":{"default":{"mode":"model","model":"codex/gpt-5"}}},"routes":{},"model_pools":{}}
```

`ROUTER_CODEX_AUTH_FILE=$ROUTER_HOME/codex-auth.json` (mode `0600`) contains **only**:
```json
{"auth_mode":"chatgpt","tokens":{"id_token":"RR_SYNTHETIC_ID","access_token":"RR_SYNTHETIC.eyJleHAiOjQxMDI0NDQ4MDB9.sig","refresh_token":"RR_SYNTHETIC_REFRESH","account_id":"RR_SYNTHETIC_ACCOUNT"}}
```

Copy `privacy-on.json` or `privacy-off.json` from the tester fixtures to `$ROUTER_HOME/privacy-profiles.json` **in separate fresh homes**: privacy-on leaves a sticky required marker, so toggling one home off is not a valid comparison. Set `ROUTER_CODEX_TEST_STUB_URL=http://127.0.0.1:<printed-stub-port>`, `ROUTER_UPSTREAM_URL` to that same local URL, `ROUTER_LISTEN=127.0.0.1:<free-api-port>`, `ROUTER_UI_LISTEN=127.0.0.1:<free-ui-port>`, and `ROUTER_LOCAL_FAILOVER=0`. Start with `"$TEST_BIN" serve` under those variables. A synthetic client posts to `http://127.0.0.1:<free-api-port>/v1/messages` with `Content-Type: application/json`, `X-Api-Key: RR_SYNTHETIC_CLIENT_KEY`, and:
```json
{"model":"claude-opus-5","max_tokens":16,"messages":[{"role":"user","content":"RR_SYNTHETIC_SAFE_PROMPT"}]}
```

For the tester's HTTP-error modes `transient`, `quota`, `unknown`, `no-code-429`, and `mapped-503`, expect respectively protected HTTP 429/429/429/429/502 and off HTTP 429/429/429/429/429. Only `transient` has `Retry-After: 2`; no upstream canary or token appears in the client response. `GET /_state` from the stub must show exactly one `/backend-api/codex/responses` call per request. The tester's `stream-*` fixture speaks OpenAI Chat SSE, **not native Codex Responses SSE**, and cannot prove Codex SSE termination or postcommit-frame delivery.
