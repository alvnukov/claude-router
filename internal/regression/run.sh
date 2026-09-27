#!/usr/bin/env bash
# Regression entrypoint for the trusted synthetic CI job. The image-owned
# wrapper validates the baked source tree and event SHA before calling this
# script; path and environment checks below do not attest its network boundary.
set -euo pipefail

blocked() {
  printf '%s\n' 'status=blocked' "reason=$1" \
    'oracle_sensitivity_evidence=not-run' 'product_evidence=not-run'
  exit 78
}

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
source_dir="${ROUTER_TEST_SOURCE:-}"
scratch="${ROUTER_TEST_SCRATCH:-}"
modules="${ROUTER_TEST_MODULE_CACHE:-}"
browsers="${PLAYWRIGHT_BROWSERS_PATH:-}"
commit_sha="${ROUTER_TEST_EXPECT_SHA:-}"
[[ "$root" == /workspace/source && "$source_dir" == "$root" &&
   "$scratch" == /workspace/output && "$modules" == /* && "$modules" != / &&
   "$browsers" == /* && "$browsers" != / && "$modules" != "$root" &&
   "$browsers" != "$root" && "$modules" != "$scratch" &&
   "$browsers" != "$scratch" && "$modules" != "$root"/* &&
   "$browsers" != "$root"/* && "$modules" != "$scratch"/* &&
   "$browsers" != "$scratch"/* && "$root" != "$modules"/* &&
   "$scratch" != "$modules"/* && "$root" != "$browsers"/* &&
   "$scratch" != "$browsers"/* && "$commit_sha" =~ ^[0-9a-f]{40}$ ]] ||
  blocked 'trusted image source/output/dependency adapter is unavailable'
[[ $EUID -ne 0 && -d "$scratch" && ! -L "$scratch" &&
   "$(cd "$scratch" && pwd -P)" == "$scratch" &&
   -d "$modules" && ! -L "$modules" && -d "$browsers" && ! -L "$browsers" &&
   -d "$root/internal/ui/web/node_modules" &&
   ! -e "$scratch/report" ]] || blocked 'fresh scratch or offline dependencies unavailable'
command -v go >/dev/null && command -v node >/dev/null && command -v npm >/dev/null ||
  blocked 'offline Go, Node and npm toolchains are required'

# The wrapper owns the immutable source and read-only dependencies. The runner
# owns every writable home/cache and keeps raw logs private in fresh scratch.
umask 077
report="$scratch/report"
mkdir -- "$report" "$report/private" "$scratch/home" "$scratch/home/router" \
  "$scratch/home/claude" "$scratch/home/codex" "$scratch/gopath" \
  "$scratch/gocache" "$scratch/tmp"
clean_env=(env -i "PATH=$PATH" "HOME=$scratch/home" "TMPDIR=$scratch/tmp" \
  "XDG_CONFIG_HOME=$scratch/home" "CLAUDE_CONFIG_DIR=$scratch/home/claude" \
  "CODEX_HOME=$scratch/home/codex" "ROUTER_HOME=$scratch/home/router" \
  "ROUTER_CODEX_AUTH_FILE=$scratch/home/codex/auth.json" \
  "ROUTER_PROVIDERS_FILE=$scratch/home/router/providers.json" \
  "ROUTER_ENV_FILE=$scratch/home/router/env" \
  "ROUTER_STATE_FILE=$scratch/home/router/state.json" \
  "ROUTER_UI_HISTORY_FILE=$scratch/home/router/history.jsonl" \
  "ROUTER_ANTHROPIC_LIMITS_FILE=$scratch/home/router/limits.json" \
  "GOPATH=$scratch/gopath" "GOCACHE=$scratch/gocache" "GOMODCACHE=$modules" \
  "ROUTER_TEST_SOURCE=$root" "ROUTER_TEST_EXPECT_SHA=$commit_sha" \
  "ROUTER_TEST_SCRATCH=$scratch" "ROUTER_TEST_MODULE_CACHE=$modules" \
  GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local NPM_CONFIG_OFFLINE=true \
  "PLAYWRIGHT_BROWSERS_PATH=$browsers")
go_version="$("${clean_env[@]}" go version)"
export RR_REPORT="$report" RR_COMMIT_SHA="$commit_sha" RR_GO_VERSION="$go_version"

# One record per command, even on failure. Logs and the unsanitized Go JSONL
# may contain test data and must NEVER be CI artifacts or sent to a service.
run_step() {
  local id="$1" label="$2" cwd="$3"; shift 3
  local log="$report/private/$id.log" status code started duration
  [[ "$id" != go-test ]] || log="$report/private/go-test.jsonl"
  started=$SECONDS
  if (cd "$cwd" && "${clean_env[@]}" "$@") >"$log" 2>&1; then
    code=0; status=pass
  else
    code=$?; status=fail
  fi
  duration=$(( (SECONDS - started) * 1000 ))
  printf '%s\t%s\t%s\t%s\t%s\n' "$id" "$label" "$status" "$code" "$duration" >>"$report/private/steps.tsv"
}

run_step go-build 'go build ./...' "$root" go build ./...
run_step go-test 'go test -count=1 -json ./...' "$root" go test -count=1 -json ./...
run_step go-race 'go test -count=1 -race ./...' "$root" env ROUTER_TEST_CHILD_RACE=1 go test -count=1 -race ./...
run_step go-vet 'go vet ./...' "$root" go vet ./...
# svelte-check may write working files, so run it from a writable web copy.
# Browser/usage scripts resolve the repository root from their own location;
# run them in the read-only source, where they write only to isolated TMPDIR.
web="$root/internal/ui/web"
web_work="$scratch/web"
mkdir -- "$web_work"
shopt -s dotglob nullglob
for item in "$web"/*; do
  [[ "${item##*/}" == node_modules ]] && continue
  cp -R -- "$item" "$web_work/"
done
shopt -u dotglob nullglob
ln -s -- "$web/node_modules" "$web_work/node_modules"
run_step ui-check 'npm run check' "$web_work" npm run check
run_step ui-browser 'npm run test:browser' "$web" npm run test:browser
run_step ui-usage 'npm run test:usage' "$web" npm run test:usage

# Node is already required by the browser suite. Inspect raw actions and
# packages privately; publish only allowlisted checks, never the Output field.
"${clean_env[@]}" RR_REPORT="$report" RR_COMMIT_SHA="$commit_sha" RR_GO_VERSION="$go_version" node <<'NODE'
const fs = require('node:fs');
const path = require('node:path');
const crypto = require('node:crypto');
const reportDir = process.env.RR_REPORT;
const baseSHA = '45aae67f51b64dc744c1b54a663a308a2632c659';
const evidenceName = 'steps-evidence.json';
const rows = fs.readFileSync(path.join(reportDir, 'private/steps.tsv'), 'utf8').trim().split('\n');
const steps = rows.map(row => {
  const [id, command, status, exitCode, duration] = row.split('\t');
  return {id, status, exit_code: Number(exitCode), command, artifact: evidenceName, duration_ms: Number(duration)};
});
const evidence = {schema_version: 1, base_sha: baseSHA, commit_sha: process.env.RR_COMMIT_SHA,
  steps: steps.map(({id, command, exit_code, duration_ms}) => ({
    id, command, exit_code, duration_ms,
    private_log_sha256: crypto.createHash('sha256').update(fs.readFileSync(path.join(
      reportDir, 'private', id === 'go-test' ? 'go-test.jsonl' : `${id}.log`))).digest('hex'),
    toolchain: {go: process.env.RR_GO_VERSION, node: process.version},
  }))};
fs.writeFileSync(path.join(reportDir, evidenceName), JSON.stringify(evidence) + '\n', {mode: 0o600});
const testIDs = {
  'privacy-lab': [
    ['privacy', 'TestLabOracleTextMaskRestore'],
    ['privacy', 'TestLabOracleJSONStructure'],
    ['privacy', 'TestLabOracleRestoreRejection'],
    ['privacy', 'TestLabOracleInputRejection'],
    ['privacy', 'TestLabOracleSupportedFields'],
    ['ui', 'TestPrivacyHTTPProtectedObserver'],
    ['ui', 'TestPrivacyHTTPProtectedObserver/SyntheticProtectedObserver'], // named observer exception
  ],
  'privacy-p1-a': [['privacy', 'TestSessionOracleAutomaticPseudonymsA']],
  'privacy-p1-b': [['privacy', 'TestSessionOracleForeignRestorationB']],
  'privacy-p1-explicit-exception': [['privacy', 'TestSessionOracleExplicitPseudonymException']],
  'oracle-sensitivity': [
    ['regression', 'TestRegressionOracleRejectsCorruptedObservations'],
    ['regression', 'TestRegressionOracleSSERejectsMissingTerminalAndWrongTool'],
    ['regression', 'TestRegressionRouteOracleRejectsWrongDestinationAndExtraCall'],
    ['regression', 'TestRegressionCloudOracleRejectsCorruptedObservedBody'],
    ['regression', 'TestRegressionPoolOracleRejectsExtraRetryAndReordering'],
    ['regression', 'TestRegressionSSEOracleRejectsCorruptedObservation'],
    ['regression', 'TestRegressionConfigOracleRejectsCorruptedResult'],
    ['regression', 'TestRegressionHistoryOracleRejectsExtraSimilarModel'],
    ['regression', 'TestRegressionUIOracleRejectsForeignMutation'],
  ],
  'regression-full-path': [
    ['regression', 'TestRegressionRouteMatrix'],
    ['regression', 'TestRegressionEffortMappingAndAbsentMapping'],
    ['regression', 'TestRegressionCloudPassthroughAndDisabled'],
    ['regression', 'TestRegressionProfileSwitchAndRestart'],
    ['regression', 'TestRegressionPoolAttemptOrderAndAffinity'],
    ['regression', 'TestRegressionMessageShapes'],
    ['regression', 'TestRegressionLocalOpenAIWire'],
    ['regression', 'TestRegressionSSEEventsAndFinal'],
    ['regression', 'TestRegressionStreamFailurePhases'],
    ['regression', 'TestRegressionCancelNoRetry'],
    ['regression', 'TestRegressionConfigCrashSafeRestart'],
    ['regression', 'TestRegressionProfileCloneAndDeletionGuards'],
    ['regression', 'TestRegressionAccountIdentityAndNoCrossCall'],
    ['regression', 'TestRegressionInterceptionRoundTrip'],
    ['regression', 'TestRegressionInterceptionRejectsExternalConflict'],
    ['regression', 'TestRegressionCatalogRollback'],
    ['regression', 'TestRegressionHistoryDetailAndExactFilter'],
    ['regression', 'TestRegressionHistoryToolOnlyDetail'],
    ['regression', 'TestRegressionHistoryLiteralJSONText'],
    ['regression', 'TestRegressionUIDetailCapsLongResponse'],
    ['regression', 'TestRegressionUIHostOriginEventsAndCap'],
    ['regression', 'TestRegressionLimitsAccountFreshness'],
    ['regression', 'TestRegressionHealthAndCancel'],
    ['regression', 'TestRegressionAccountIdentityAndNoCrossCall/RR-ACC-01-02-03/auth-refresh'],
    ['regression', 'TestRegressionCatalogRollback/RR-CAT-01/refresh'],
    ['regression', 'TestRegressionLimitsAccountFreshness/RR-LIM-01/account-window'],
    ['regression', 'TestRegressionHealthAndCancel/RR-HEA-01/recovery'],
  ],
};
const goStep = steps.find(step => step.id === 'go-test');
const required = [...new Set(Object.values(testIDs).flat().map(([pkg, name]) => `${pkg}:${name}`))];
const packages = new Map();
const tests = new Map();
let complete = goStep?.command === 'go test -count=1 -json ./...' &&
  goStep.status === 'pass' && goStep.exit_code === 0;
let duplicate = false;
try {
  const source = fs.readFileSync(path.join(reportDir, 'private/go-test.jsonl'), 'utf8');
  if (!source.endsWith('\n')) complete = false;
  const lines = (source.endsWith('\n') ? source.slice(0, -1) : source).split('\n');
  for (const line of lines) {
    const event = JSON.parse(line);
    if (typeof event.Package !== 'string' || !event.Package || !event.Action) {
      throw Error('missing Go test event fields');
    }
    if (event.Test && ['run', 'pass', 'fail', 'skip'].includes(event.Action)) {
      const key = `${event.Package}:${event.Test}`;
      const current = tests.get(key) || {runs: 0, terminal: null, duplicate: false};
      if (event.Action === 'run') {
        current.runs++;
        if (current.runs > 1 || current.terminal !== null) current.duplicate = duplicate = true;
      } else if (current.terminal !== null) {
        current.duplicate = duplicate = true;
      } else {
        current.terminal = event.Action;
      }
      tests.set(key, current);
    }
    if (!event.Test && ['pass', 'fail', 'skip'].includes(event.Action)) {
      if (packages.has(event.Package)) duplicate = true;
      packages.set(event.Package, event.Action);
    }
  }
  if (packages.size === 0 || duplicate) complete = false;
} catch {
  complete = false;
}
function verdict(names) {
  if (duplicate) return 'fail';
  let blocked = !complete;
  for (const [pkg, name] of names) {
    const key = `localrouter/internal/${pkg}:${name}`;
    const result = tests.get(key);
    if (result?.duplicate || result?.terminal === 'fail') return 'fail';
    if (!result || result.runs !== 1 || result.terminal !== 'pass' ||
        packages.get(`localrouter/internal/${pkg}`) !== 'pass') blocked = true;
  }
  return blocked ? 'blocked' : 'pass';
}
const checks = required.map(entry => {
  const [pkg, name] = entry.split(':');
  const result = tests.get(`localrouter/internal/${pkg}:${name}`);
  return {package: `internal/${pkg}`, test: name, runs: result?.runs ?? 0,
    terminal: result?.terminal ?? null,
    package_status: packages.get(`localrouter/internal/${pkg}`) ?? null};
});
const summary = {schema_version: 2, complete, commit_sha: process.env.RR_COMMIT_SHA,
  checks, verdicts: Object.fromEntries(Object.entries(testIDs).map(([id, names]) => [id, verdict(names)]))};
const summaryName = 'go-test-summary.json';
fs.writeFileSync(path.join(reportDir, summaryName), JSON.stringify(summary) + '\n', {mode: 0o600});
for (const id of Object.keys(testIDs)) {
  const status = summary.verdicts[id];
  steps.push({id, status, exit_code: null, command: 'derived:go-test',
    artifact: summaryName, duration_ms: null});
}
const byID = Object.fromEntries(steps.map(step => [step.id, step]));
const status = steps.every(step => step.status === 'pass') ? 'pass' :
  steps.some(step => step.status === 'fail') ? 'fail' : 'blocked';
const productIDs = ['go-build', 'go-test', 'go-race', 'go-vet', 'ui-check', 'ui-browser', 'ui-usage',
  'privacy-lab', 'privacy-p1-a', 'privacy-p1-b', 'privacy-p1-explicit-exception',
  'oracle-sensitivity', 'regression-full-path'];
const productStatus = productIDs.every(id => byID[id]?.status === 'pass') ? 'pass' :
  productIDs.some(id => byID[id]?.status === 'fail') ? 'fail' : 'blocked';
const report = {
  schema_version: 2, status,
  base_sha: baseSHA,
  commit_sha: process.env.RR_COMMIT_SHA,
  platform: {os: process.platform, arch: process.arch},
  toolchain: {go: process.env.RR_GO_VERSION, node: process.version},
  steps, oracle_sensitivity: {status: byID['oracle-sensitivity'].status,
    evidence: byID['oracle-sensitivity'].status === 'pass' ? summaryName : null},
  product: {status: productStatus, evidence: productStatus === 'pass' ? summaryName : null},
  external_gaps: {proxy_e2e: 'not-run', native_macos: 'not-run', native_windows: 'not-run'},
};
const temporary = path.join(reportDir, '.run.json.tmp');
fs.writeFileSync(temporary, JSON.stringify(report) + '\n', {mode: 0o600});
fs.renameSync(temporary, path.join(reportDir, 'run.json'));
process.stdout.write(`status=${status}\nreport=${path.join(reportDir, 'run.json')}\n`);
process.exitCode = status === 'pass' ? 0 : 1;
NODE
