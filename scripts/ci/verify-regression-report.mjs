import { constants, closeSync, fstatSync, openSync, readSync } from 'node:fs';
import { dirname, isAbsolute, join } from 'node:path';

const BASE_SHA = '45aae67f51b64dc744c1b54a663a308a2632c659';
const REPORT_PATH = '/workspace/output/report/run.json';
const SUMMARY_NAME = 'go-test-summary.json';
const STEPS_NAME = 'steps-evidence.json';
const COMMANDS = new Map([
  ['go-build', 'go build ./...'],
  ['go-test', 'go test -count=1 -json ./...'],
  ['go-race', 'go test -count=1 -race ./...'],
  ['go-vet', 'go vet ./...'],
  ['ui-check', 'npm run check'],
  ['ui-browser', 'npm run test:browser'],
  ['ui-usage', 'npm run test:usage'],
]);
const EXECUTED = [...COMMANDS.keys()];
const DERIVED = [
  'privacy-lab', 'privacy-p1-a', 'privacy-p1-b',
  'privacy-p1-explicit-exception', 'oracle-sensitivity',
];
const CHECKS = new Map([
  ['privacy-lab', [
    ['internal/privacy', 'TestLabOracleTextMaskRestore'],
    ['internal/privacy', 'TestLabOracleJSONStructure'],
    ['internal/privacy', 'TestLabOracleRestoreRejection'],
    ['internal/privacy', 'TestLabOracleInputRejection'],
    ['internal/privacy', 'TestLabOracleSupportedFields'],
    ['internal/ui', 'TestPrivacyHTTPProtectedObserver'],
    ['internal/ui', 'TestPrivacyHTTPProtectedObserver/SyntheticProtectedObserver'],
  ]],
  ['privacy-p1-a', [['internal/privacy', 'TestSessionOracleAutomaticPseudonymsA']]],
  ['privacy-p1-b', [['internal/privacy', 'TestSessionOracleForeignRestorationB']]],
  ['privacy-p1-explicit-exception', [['internal/privacy', 'TestSessionOracleExplicitPseudonymException']]],
  ['oracle-sensitivity', [
    ['internal/regression', 'TestRegressionOracleRejectsCorruptedObservations'],
    ['internal/regression', 'TestRegressionOracleSSERejectsMissingTerminalAndWrongTool'],
    ['internal/regression', 'TestRegressionRouteOracleRejectsWrongDestinationAndExtraCall'],
    ['internal/regression', 'TestRegressionCloudOracleRejectsCorruptedObservedBody'],
    ['internal/regression', 'TestRegressionPoolOracleRejectsExtraRetryAndReordering'],
    ['internal/regression', 'TestRegressionSSEOracleRejectsCorruptedObservation'],
    ['internal/regression', 'TestRegressionConfigOracleRejectsCorruptedResult'],
    ['internal/regression', 'TestRegressionHistoryOracleRejectsExtraSimilarModel'],
    ['internal/regression', 'TestRegressionUIOracleRejectsForeignMutation'],
  ]],
]);
const SHA = /^[0-9a-f]{40}$/;
const DIGEST = /^[0-9a-f]{64}$/;
const BASENAME = /^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$/;

function requireCondition(condition, reason) {
  if (!condition) throw new Error(reason);
}

function object(value) {
  return value !== null && typeof value === 'object' && !Array.isArray(value);
}

function readJSON(file, maxBytes) {
  let descriptor;
  try {
    descriptor = openSync(file, constants.O_RDONLY | constants.O_NOFOLLOW);
    const stat = fstatSync(descriptor);
    requireCondition(stat.isFile() && stat.size > 0 && stat.size <= maxBytes, 'invalid-file');
    const buffer = Buffer.alloc(stat.size);
    let offset = 0;
    while (offset < buffer.length) {
      const count = readSync(descriptor, buffer, offset, buffer.length - offset, null);
      requireCondition(count > 0, 'incomplete-file');
      offset += count;
    }
    return JSON.parse(buffer.toString('utf8'));
  } catch {
    throw new Error('invalid-report');
  } finally {
    if (descriptor !== undefined) closeSync(descriptor);
  }
}

function args() {
  const values = process.argv.slice(2);
  requireCondition(values.length === 6, 'invalid-arguments');
  const flags = new Map();
  for (let index = 0; index < values.length; index += 2) {
    requireCondition(!flags.has(values[index]), 'invalid-arguments');
    flags.set(values[index], values[index + 1]);
  }
  requireCondition(flags.size === 3, 'invalid-arguments');
  const report = flags.get('--report');
  const base = flags.get('--base-sha');
  const commit = flags.get('--commit-sha');
  requireCondition(report === REPORT_PATH && isAbsolute(report), 'invalid-arguments');
  requireCondition(base === BASE_SHA && SHA.test(commit ?? ''), 'invalid-arguments');
  return { report, base, commit };
}

function basename(value) {
  return typeof value === 'string' && BASENAME.test(value) &&
    value !== '.' && value !== '..' && !value.includes('..') &&
    value !== 'go-test.jsonl';
}

function version(value) {
  return typeof value === 'string' && value.length > 0 && value.length <= 128;
}

function verifySteps(report) {
  requireCondition(Array.isArray(report.steps) &&
    report.steps.length === EXECUTED.length + DERIVED.length, 'incomplete-gates');
  const seen = new Set();
  for (const step of report.steps) {
    requireCondition(object(step) && typeof step.id === 'string' &&
      !seen.has(step.id), 'duplicate-or-invalid-gate');
    seen.add(step.id);
    requireCondition(step.status === 'pass' &&
      typeof step.command === 'string' && step.command.length > 0 &&
      (step.duration_ms === null ||
        (Number.isSafeInteger(step.duration_ms) && step.duration_ms >= 0)), 'failed-gate');
    requireCondition(step.artifact === null || basename(step.artifact), 'unsafe-artifact');
    if (EXECUTED.includes(step.id)) {
      requireCondition(step.exit_code === 0 &&
        step.command === COMMANDS.get(step.id) &&
        step.artifact === STEPS_NAME &&
        Number.isSafeInteger(step.duration_ms) && step.duration_ms >= 0,
      'failed-gate');
    } else {
      requireCondition(DERIVED.includes(step.id) &&
        step.exit_code === null && step.command === 'derived:go-test' &&
        step.artifact === SUMMARY_NAME, 'failed-derived-gate');
    }
  }
  for (const id of [...EXECUTED, ...DERIVED]) {
    requireCondition(seen.has(id), 'missing-gate');
  }
}

function verifyStepEvidence(evidence, report) {
  requireCondition(object(evidence) && evidence.schema_version === 1 &&
    evidence.base_sha === report.base_sha &&
    evidence.commit_sha === report.commit_sha &&
    Array.isArray(evidence.steps) &&
    evidence.steps.length === EXECUTED.length, 'invalid-step-evidence');
  const recorded = new Map();
  for (const step of evidence.steps) {
    requireCondition(object(step) && COMMANDS.has(step.id) &&
      !recorded.has(step.id) &&
      step.command === COMMANDS.get(step.id) && step.exit_code === 0 &&
      Number.isSafeInteger(step.duration_ms) && step.duration_ms >= 0 &&
      DIGEST.test(step.private_log_sha256 ?? '') &&
      object(step.toolchain) &&
      step.toolchain.go === report.toolchain.go &&
      step.toolchain.node === report.toolchain.node, 'invalid-step-evidence');
    recorded.set(step.id, step);
  }
  for (const step of report.steps) {
    if (!COMMANDS.has(step.id)) continue;
    const matched = recorded.get(step.id);
    requireCondition(matched && matched.duration_ms === step.duration_ms &&
      matched.command === step.command && matched.exit_code === step.exit_code,
    'mismatched-step-evidence');
  }
}

function verifySummary(summary, commit) {
  requireCondition(object(summary) && summary.schema_version === 1 &&
    summary.complete === true && summary.commit_sha === commit &&
    Array.isArray(summary.checks) && object(summary.verdicts), 'invalid-summary');
  const checks = new Map();
  for (const check of summary.checks) {
    requireCondition(object(check) && typeof check.package === 'string' &&
      typeof check.test === 'string', 'invalid-summary');
    const key = `${check.package}\u0000${check.test}`;
    requireCondition(!checks.has(key), 'duplicate-check');
    checks.set(key, check);
  }
  for (const [gate, required] of CHECKS) {
    requireCondition(summary.verdicts[gate] === 'pass', 'failed-verdict');
    for (const [pkg, test] of required) {
      const check = checks.get(`${pkg}\u0000${test}`);
      requireCondition(check && check.runs === 1 && check.terminal === 'pass' &&
        check.package_status === 'pass', 'missing-or-failed-check');
    }
  }
  for (const verdict of Object.values(summary.verdicts)) {
    requireCondition(verdict === 'pass', 'failed-verdict');
  }
  for (const check of summary.checks) {
    requireCondition(check.terminal !== 'fail' &&
      check.package_status !== 'fail', 'failed-check');
  }
}

function verify() {
  const { report: file, base, commit } = args();
  const report = readJSON(file, 1024 * 1024);
  requireCondition(object(report) && report.schema_version === 1 &&
    report.status === 'pass' && report.base_sha === base &&
    report.commit_sha === commit && SHA.test(report.commit_sha) &&
    object(report.platform) && version(report.platform.os) &&
    version(report.platform.arch) && object(report.toolchain) &&
    version(report.toolchain.go) && version(report.toolchain.node), 'invalid-report');
  requireCondition(object(report.product) && report.product.status === 'pass' &&
    report.product.evidence === SUMMARY_NAME &&
    object(report.oracle_sensitivity) &&
    report.oracle_sensitivity.status === 'pass' &&
    report.oracle_sensitivity.evidence === SUMMARY_NAME, 'missing-evidence');
  verifySteps(report);
  verifyStepEvidence(readJSON(join(dirname(file), STEPS_NAME), 1024 * 1024), report);
  verifySummary(readJSON(join(dirname(file), SUMMARY_NAME), 2 * 1024 * 1024), commit);
}

try {
  verify();
  process.stdout.write('status=pass\n');
} catch (error) {
  const reasons = new Set([
    'invalid-arguments', 'invalid-report', 'incomplete-gates',
    'duplicate-or-invalid-gate', 'failed-gate', 'unsafe-artifact',
    'failed-derived-gate', 'missing-gate', 'invalid-summary',
    'duplicate-check', 'failed-verdict', 'missing-or-failed-check',
    'failed-check', 'missing-evidence', 'invalid-step-evidence',
    'mismatched-step-evidence',
  ]);
  const reason = reasons.has(error.message) ? error.message : 'invalid-report';
  process.stderr.write(`status=fail reason=${reason}\n`);
  process.exitCode = 1;
}
