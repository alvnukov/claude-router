#!/usr/bin/env bash
# Regression entrypoint: bash internal/regression/run.sh from the router worktree.
# Stage A is authoring only. Neither an environment flag nor a proxy proves
# isolation of Go, Node, router and browser descendants.
set -euo pipefail

printf '%s\n' 'status=blocked' \
  'reason=no independently proven network-none and filesystem-write boundary for the whole process tree' \
  'oracle_sensitivity_evidence=not-run' \
  'product_evidence=not-run' \
  'phase_B=requires container preflight and separate authorization'

# Stage B must replace this guard with an independently attested container
# adapter BEFORE any subprocess or write. It must verify the same image/source
# SHA and all descendants: dynamic loopback only, no host network, real home,
# secrets or privileged sockets; original source/dependencies read-only and
# writes confined to fresh scratch/home/cache (use an isolated writable copy
# or overlay for the UI dist and browser scripts). A caller-supplied variable,
# proxy, or /proc inspection alone is not an authorization or attestation.
# The testBinary/startRouter guard must be replaced separately at that gate.
# No run.json is written in A: the CI validator treats absence as failure.
exit 78

# Stage B implementation sketch below the non-bypassable A guard. None of it
# has been built, run, or validated. Keep raw logs in private scratch; publish
# only the allowlisted summary after a complete -json stream on the SAME HEAD.
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
scratch="${ROUTER_TEST_SCRATCH:?isolated scratch required}"
modules="${ROUTER_TEST_MODULE_CACHE:?read-only offline module cache required}"
[[ "$scratch" = /* && "$modules" = /* && "$scratch" != / && -d "$scratch" && ! -L "$scratch" ]] || exit 78
umask 077
report="$scratch/report"
mkdir -- "$report" # Refuse to overwrite a previous run.
mkdir -- "$report/private" "$scratch/home" "$scratch/gopath" "$scratch/gocache"

# Only the independently isolated parent can reach this point. The child
# environment carries no inherited credentials, router paths or proxy claims.
clean_env=(env -i "PATH=$PATH" "HOME=$scratch/home" "TMPDIR=$scratch" \
  "XDG_CONFIG_HOME=$scratch/home" "GOPATH=$scratch/gopath" \
  "GOCACHE=$scratch/gocache" "GOMODCACHE=$modules" \
  "ROUTER_TEST_SCRATCH=$scratch" "ROUTER_TEST_MODULE_CACHE=$modules" \
  GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local \
  NPM_CONFIG_OFFLINE=true PLAYWRIGHT_BROWSERS_PATH="${PLAYWRIGHT_BROWSERS_PATH:-$scratch/browsers}")
commit_sha="$(cd "$root" && "${clean_env[@]}" git rev-parse HEAD)"
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
web="$root/internal/ui/web"
run_step ui-check 'npm run check' "$web" npm run check
run_step ui-browser 'npm run test:browser' "$web" npm run test:browser
run_step ui-usage 'npm run test:usage' "$web" npm run test:usage

# Node is already required by the browser suite. Parse only actions, packages
# and allowlisted test names; never copy the Output field into a public file.
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
};
const goStep = steps.find(step => step.id === 'go-test');
const packages = new Map();
const tests = new Map();
let complete = goStep?.status === 'pass';
try {
  const source = fs.readFileSync(path.join(reportDir, 'private/go-test.jsonl'), 'utf8');
  if (!source.endsWith('\n')) complete = false;
  for (const line of source.trimEnd().split('\n')) {
    const event = JSON.parse(line);
    if (!event.Package || !event.Action) throw Error('missing Go test event fields');
    const namedObserver = event.Test === 'TestPrivacyHTTPProtectedObserver/SyntheticProtectedObserver';
    if (event.Test && (!event.Test.includes('/') || namedObserver) &&
        ['run', 'pass', 'fail', 'skip'].includes(event.Action)) {
      const key = `${event.Package}:${event.Test}`;
      const current = tests.get(key) || {runs: 0, terminal: null};
      if (event.Action === 'run') current.runs++;
      else if (current.terminal !== null) complete = false;
      else current.terminal = event.Action;
      tests.set(key, current);
    }
    if (!event.Test && ['pass', 'fail', 'skip'].includes(event.Action)) {
      if (packages.has(event.Package)) complete = false;
      packages.set(event.Package, event.Action);
    }
  }
  if (packages.size === 0) complete = false;
} catch {
  complete = false;
}
function verdict(names) {
  if (!complete) return 'blocked';
  for (const [pkg, name] of names) {
    const matching = [...tests].filter(([key]) => key.endsWith(`/internal/${pkg}:${name}`));
    if (matching.length !== 1) return 'blocked';
    const [key, result] = matching[0];
    if (packages.get(key.split(':')[0]) !== 'pass') return 'blocked';
    if (result.runs !== 1 || result.terminal !== 'pass') return 'fail';
  }
  return 'pass';
}
const checks = [...new Set(Object.values(testIDs).flat().map(([pkg, name]) => `${pkg}:${name}`))]
  .map(entry => {
    const [pkg, name] = entry.split(':');
    const matching = [...tests].filter(([key]) => key.endsWith(`/internal/${pkg}:${name}`));
    const [key, result] = matching.length === 1 ? matching[0] : [null, null];
    return {package: `internal/${pkg}`, test: name, runs: result?.runs ?? 0,
      terminal: result?.terminal ?? null, package_status: key ? packages.get(key.split(':')[0]) ?? null : null};
  });
const summary = {schema_version: 1, complete, commit_sha: process.env.RR_COMMIT_SHA,
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
  'privacy-lab', 'privacy-p1-a', 'privacy-p1-b', 'privacy-p1-explicit-exception'];
const productStatus = productIDs.every(id => byID[id]?.status === 'pass') ? 'pass' :
  productIDs.some(id => byID[id]?.status === 'fail') ? 'fail' : 'blocked';
const report = {
  schema_version: 1, status,
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
