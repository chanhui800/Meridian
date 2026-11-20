'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const test = require('node:test');
const { pathToFileURL } = require('node:url');

const verifier = import(pathToFileURL(path.join(__dirname, '../scripts/codeql-policy.mjs')).href);
const NOW = new Date('2026-11-20T12:00:00Z');
const SHA = 'a'.repeat(40);
const OLD_SHA = 'b'.repeat(40);
const FINGERPRINT = 'b62a3d30847d4376:1';

function emptyPolicy() {
  return { schemaVersion: 1, hashAlgorithm: 'sha256-lf', reviews: [] };
}

function finding(ruleId = 'go/reflected-xss', fingerprint = FINGERPRINT) {
  return {
    ruleId, level: 'warning',
    partialFingerprints: fingerprint ? { primaryLocationLineHash: fingerprint } : {},
    locations: [{ physicalLocation: { artifactLocation: { uri: 'response.go' }, region: { startLine: 12 } } }],
  };
}

function sarif(results = [finding()], score = '7.8') {
  const ids = [...new Set(results.map(result => result.ruleId))];
  return {
    version: '2.1.0',
    runs: [{
      tool: { driver: { name: 'CodeQL', rules: ids.map(id => ({ id, properties: { 'security-severity': score, tags: ['security'] } })) } },
      results,
      versionControlProvenance: [{ revisionId: SHA }],
    }],
  };
}

async function fixture(t) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'meridian-codeql-policy-'));
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));
  fs.writeFileSync(path.join(root, 'response.go'), 'package main\n// protected response boundary\n');
  fs.writeFileSync(path.join(root, 'response_test.go'), '// verifies response isolation\n');
  fs.mkdirSync(path.join(root, 'docs'));
  fs.writeFileSync(path.join(root, 'docs/review.md'), 'The exact response has a tested restrictive sandbox policy.\n');
  const { sourceHash } = await verifier;
  const review = {
    id: 'review-response-isolation', ruleId: 'go/reflected-xss', path: 'response.go',
    partialFingerprints: { primaryLocationLineHash: FINGERPRINT },
    reason: 'The response has a restrictive sandbox and active content is rejected; the boundary is covered by tests.',
    reviewedBy: 'Project maintainers via tests and review',
    reviewedAt: '2026-11-20T00:00:00Z', expiresAt: '2027-02-18T00:00:00Z',
    evidence: 'docs/review.md#response',
    files: ['response.go', 'response_test.go', 'docs/review.md'].map(filename => ({ path: filename, sha256: sourceHash(root, filename) })),
  };
  return { root, review, policy: { ...emptyPolicy(), reviews: [review] }, options: { root, now: NOW } };
}

test('CodeQL gate blocks unreviewed critical/high security scores regardless of SARIF display level', async () => {
  const { evaluateSarif } = await verifier;
  for (const score of ['7.0', '7.8', '9.1']) {
    const report = evaluateSarif(sarif([finding()], score), emptyPolicy(), { now: NOW });
    assert.equal(report.passed, false);
    assert.equal(report.highRisk, 1);
    assert.equal(report.blocked[0].score, Number(score));
    assert.equal(report.blocked[0].partialFingerprints.primaryLocationLineHash, FINGERPRINT);
  }
});

test('CodeQL gate leaves medium and low findings visible without blocking', async () => {
  const { evaluateSarif } = await verifier;
  const document = sarif([finding()], '6.9');
  document.runs[0].results[0].level = 'error';
  const report = evaluateSarif(document, emptyPolicy(), { now: NOW });
  assert.equal(report.passed, true);
  assert.equal(report.nonBlocking, 1);
  assert.equal(report.highRisk, 0);
});

test('an exact attributed fingerprint review accepts the unchanged protected implementation', async t => {
  const f = await fixture(t);
  const { evaluateSarif } = await verifier;
  const report = evaluateSarif(sarif(), f.policy, f.options);
  assert.equal(report.passed, true);
  assert.equal(report.reviewed[0].review, f.review.id);
  assert.equal(report.reviewed[0].expiresAt, f.review.expiresAt);
});

test('review cannot accept another fingerprint, rule, or source path', async t => {
  const f = await fixture(t);
  const { evaluateSarif } = await verifier;
  for (const result of [finding('go/reflected-xss', 'abcdef0123456789:1'), finding('go/request-forgery')]) {
    assert.equal(evaluateSarif(sarif([result]), f.policy, f.options).passed, false);
  }
  const relocated = finding();
  relocated.locations[0].physicalLocation.artifactLocation.uri = 'another.go';
  assert.equal(evaluateSarif(sarif([relocated]), f.policy, f.options).passed, false);
  assert.equal(evaluateSarif(sarif([finding('go/reflected-xss', null)]), f.policy, f.options).passed, false);
});

test('safety implementation and evidence changes invalidate the review', async t => {
  const f = await fixture(t);
  const { evaluateSarif } = await verifier;
  fs.appendFileSync(path.join(f.root, 'response_test.go'), '// safety assertion removed\n');
  assert.throws(() => evaluateSarif(sarif(), f.policy, f.options), /safety implementation changed/);
  f.review.files.find(file => file.path === 'response_test.go').sha256 = (await verifier).sourceHash(f.root, 'response_test.go');
  fs.appendFileSync(path.join(f.root, 'docs/review.md'), 'Changed assumptions.\n');
  assert.throws(() => evaluateSarif(sarif(), f.policy, f.options), /safety implementation changed/);
});

test('source hash is portable across CRLF checkouts and rejects paths outside the repository', async t => {
  const f = await fixture(t);
  const { sourceHash, evaluateSarif } = await verifier;
  const before = sourceHash(f.root, 'response.go');
  fs.writeFileSync(path.join(f.root, 'response.go'), fs.readFileSync(path.join(f.root, 'response.go'), 'utf8').replaceAll('\n', '\r\n'));
  assert.equal(sourceHash(f.root, 'response.go'), before);
  assert.equal(evaluateSarif(sarif(), f.policy, f.options).passed, true);
  assert.throws(() => sourceHash(f.root, '../outside.go'), /repository-relative/);
});

test('policy rejects expired, future-dated, overly long, broad, and unauditable exceptions', async t => {
  const f = await fixture(t);
  const { validatePolicy } = await verifier;
  const mutations = [
    review => { review.expiresAt = '2027-02-19T00:00:00Z'; },
    review => { review.ruleId = 'go/*'; },
    review => { review.partialFingerprints.primaryLocationLineHash = '*'; },
    review => { delete review.partialFingerprints.primaryLocationLineHash; },
    review => { review.reason = 'false positive'; },
    review => { review.files = review.files.filter(file => file.path !== 'response.go'); },
    review => { review.files = review.files.filter(file => file.path !== 'docs/review.md'); },
    review => { review.ignore = true; },
  ];
  for (const mutate of mutations) {
    const copy = structuredClone(f.policy);
    mutate(copy.reviews[0]);
    assert.throws(() => validatePolicy(copy, f.options));
  }
  assert.throws(() => validatePolicy({ ...f.policy, ignoreRules: ['go/reflected-xss'] }, f.options), /unsupported fields/);
  const duplicate = structuredClone(f.policy);
  duplicate.reviews.push({ ...duplicate.reviews[0], id: 'another-review-id' });
  assert.throws(() => validatePolicy(duplicate, f.options), /Duplicate reviewed finding/);
});

test('only applicable high-risk reviews enforce expiration and existing safety-file hashes', async t => {
  const f = await fixture(t);
  const { evaluateSarif } = await verifier;
  f.review.expiresAt = '2026-11-20T11:59:59Z';
  assert.throws(() => evaluateSarif(sarif(), f.policy, f.options), /Applicable review expired/);
  fs.unlinkSync(path.join(f.root, 'response.go'));
  fs.unlinkSync(path.join(f.root, 'docs/review.md'));
  const noFindings = sarif();
  noFindings.runs[0].results = [];
  assert.equal(evaluateSarif(noFindings, f.policy, f.options).passed, true, 'a resolved finding must not be held hostage by its old record');
  assert.equal(evaluateSarif(sarif([finding()], '4.3'), f.policy, f.options).passed, true, 'an unused old high-risk record does not block medium findings');
});

test('future-dated applicable review is rejected even if its source files are unchanged', async t => {
  const f = await fixture(t);
  f.review.reviewedAt = '2026-11-21T00:00:00Z';
  const { evaluateSarif } = await verifier;
  assert.throws(() => evaluateSarif(sarif(), f.policy, f.options), /future-dated/);
});

test('post-processed SARIF extension metadata and original fingerprints are understood', async t => {
  const f = await fixture(t);
  const { evaluateSarif } = await verifier;
  const document = sarif();
  const run = document.runs[0];
  run.tool.extensions = [{ name: 'codeql/go-queries', rules: run.tool.driver.rules }];
  run.tool.driver = { name: 'GitHub Code Scanning' };
  run.results[0].rule = { id: 'go/reflected-xss', index: 0, toolComponent: { index: 0 } };
  assert.equal(evaluateSarif(document, f.policy, f.options).passed, true);
});

test('suppression markers cannot bypass a high finding and invalid scans fail closed', async () => {
  const { evaluateSarif } = await verifier;
  const suppressed = sarif();
  suppressed.runs[0].results[0].suppressions = [{ kind: 'external', status: 'accepted' }];
  assert.equal(evaluateSarif(suppressed, emptyPolicy(), { now: NOW }).passed, false);
  for (const invalid of [{ version: '2.1.0', runs: [] }, { runs: [] },
    ...['not-a-number', null, true, ''].map(score => sarif([finding()], score))]) {
    assert.throws(() => evaluateSarif(invalid, emptyPolicy(), { now: NOW }));
  }
  const failed = sarif();
  failed.runs[0].invocations = [{ executionSuccessful: false }];
  assert.throws(() => evaluateSarif(failed, emptyPolicy(), { now: NOW }), /execution failure/);
});

function releaseAPI({ runs, analyses, document, failStatus, invalidJSON, networkFailure } = {}) {
  const calls = [];
  const successfulRun = { id: 21, head_sha: SHA, head_branch: 'main', event: 'push', path: '.github/workflows/codeql.yml',
    status: 'completed', conclusion: 'success', run_number: 3, run_attempt: 1 };
  const analysis = { id: 33, commit_sha: SHA, ref: 'refs/heads/main', tool: { name: 'CodeQL' },
    category: '.github/workflows/codeql.yml:analyze', created_at: '2026-11-20T11:00:00Z', error: '', warning: '' };
  const fetchImpl = async (address, options) => {
    calls.push([address, options]);
    if (networkFailure) throw new Error('network unavailable');
    if (failStatus) return { ok: false, status: failStatus };
    if (invalidJSON) return { ok: true, status: 200, json: async () => { throw new Error('invalid JSON'); } };
    let result;
    const url = new URL(address);
    if (url.pathname.endsWith('/actions/workflows/codeql.yml/runs')) result = { workflow_runs: runs ?? [successfulRun] };
    else if (url.pathname.endsWith('/code-scanning/analyses')) result = analyses ?? [analysis];
    else if (url.pathname.endsWith('/code-scanning/analyses/33')) result = document ?? sarif();
    else throw new Error('unexpected test endpoint');
    return { ok: true, status: 200, json: async () => structuredClone(result) };
  };
  return { fetchImpl, calls, successfulRun, analysis };
}

test('release verifies exact main commit success and re-evaluates downloaded SARIF', async t => {
  const f = await fixture(t);
  const api = releaseAPI();
  const { verifyReleaseScan } = await verifier;
  const report = await verifyReleaseScan({ repo: 'owner/repo', sha: SHA, token: 'test-only-token', ...f.options, policy: f.policy, fetchImpl: api.fetchImpl });
  assert.equal(report.passed, true);
  assert.equal(report.commit, SHA);
  assert.equal(report.workflowRun, 21);
  assert.equal(report.analysis, 33);
  assert.equal(api.calls.length, 3);
  assert.ok(api.calls[0][0].includes('head_sha=' + SHA));
  assert.equal(api.calls[2][1].headers.Accept, 'application/sarif+json');
  assert.equal(api.calls[2][1].redirect, 'error');
  const unreviewed = await verifyReleaseScan({ repo: 'owner/repo', sha: SHA, token: 'test-only-token', policy: emptyPolicy(), now: NOW, fetchImpl: api.fetchImpl });
  assert.equal(unreviewed.passed, false);
});

test('release rejects old SHA, failed or in-progress latest CodeQL runs', async t => {
  const f = await fixture(t);
  const { verifyReleaseScan } = await verifier;
  const base = releaseAPI().successfulRun;
  for (const runs of [
    [{ ...base, head_sha: OLD_SHA }],
    [{ ...base, conclusion: 'failure' }],
    [{ ...base, id: 22, run_number: 4, status: 'in_progress', conclusion: null }, base],
    [{ ...base, event: 'pull_request' }],
  ]) {
    await assert.rejects(verifyReleaseScan({ repo: 'owner/repo', sha: SHA, token: 'test-only-token', ...f.options, policy: f.policy, fetchImpl: releaseAPI({ runs }).fetchImpl }), /no successful completed main CodeQL gate/);
  }
});

test('release rejects stale, incomplete, or mismatched analysis even when workflow is green', async t => {
  const f = await fixture(t);
  const { verifyReleaseScan } = await verifier;
  const base = releaseAPI().analysis;
  for (const analysis of [{ ...base, commit_sha: OLD_SHA }, { ...base, error: 'incomplete extraction' }, { ...base, warning: 'missing package' }]) {
    await assert.rejects(verifyReleaseScan({ repo: 'owner/repo', sha: SHA, token: 'test-only-token', ...f.options, policy: f.policy, fetchImpl: releaseAPI({ analyses: [analysis] }).fetchImpl }), /no complete main CodeQL analysis/);
  }
  const wrongCommit = sarif();
  wrongCommit.runs[0].versionControlProvenance[0].revisionId = OLD_SHA;
  await assert.rejects(verifyReleaseScan({ repo: 'owner/repo', sha: SHA, token: 'test-only-token', ...f.options, policy: f.policy, fetchImpl: releaseAPI({ document: wrongCommit }).fetchImpl }), /SARIF commit does not match/);
});

test('release cannot treat API denial, network failure, invalid JSON, or missing token as success', async t => {
  const f = await fixture(t);
  const { verifyReleaseScan } = await verifier;
  for (const behavior of [{ failStatus: 403 }, { networkFailure: true }, { invalidJSON: true }]) {
    await assert.rejects(verifyReleaseScan({ repo: 'owner/repo', sha: SHA, token: 'test-only-token', ...f.options, policy: f.policy, fetchImpl: releaseAPI(behavior).fetchImpl }));
  }
  const api = releaseAPI();
  await assert.rejects(verifyReleaseScan({ repo: 'owner/repo', sha: SHA, ...f.options, policy: f.policy, fetchImpl: api.fetchImpl }), /token missing/);
  assert.equal(api.calls.length, 0);
});

function pullAPI({ permission = { permission: 'admin', role_name: 'admin' }, head = SHA, failStatus } = {}) {
  const calls = [];
  return {
    calls,
    fetchImpl: async address => {
      calls.push(address);
      if (failStatus) return { ok: false, status: failStatus };
      const result = address.endsWith('/pulls/42')
        ? { head: { sha: head }, base: { ref: 'main', repo: { full_name: 'owner/repo' } } }
        : address.endsWith('/collaborators/owner/permission') ? permission : null;
      if (!result) throw new Error('unexpected PR test endpoint');
      return { ok: true, status: 200, json: async () => structuredClone(result) };
    },
  };
}

test('ordinary unchanged PR policy uses the base records without a maintenance API request', async t => {
  const f = await fixture(t);
  const api = pullAPI({ failStatus: 403 });
  const { verifyPullRequestScan } = await verifier;
  const report = await verifyPullRequestScan({ documents: sarif(), basePolicy: f.policy, candidatePolicy: structuredClone(f.policy), ...f.options, fetchImpl: api.fetchImpl });
  assert.equal(report.passed, true);
  assert.equal(report.policySource, 'base');
  assert.equal(api.calls.length, 0);
});

test('exact-head admin or maintain actor can refresh changed safety hashes without a second reviewer', async t => {
  const f = await fixture(t);
  const { sourceHash, verifyPullRequestScan } = await verifier;
  const candidatePolicy = structuredClone(f.policy);
  fs.appendFileSync(path.join(f.root, 'response.go'), '// legitimate safety implementation update\n');
  candidatePolicy.reviews[0].files.find(file => file.path === 'response.go').sha256 = sourceHash(f.root, 'response.go');
  for (const permission of [{ permission: 'admin', role_name: 'admin' }, { permission: 'write', role_name: 'maintain' }]) {
    const api = pullAPI({ permission });
    const report = await verifyPullRequestScan({ repo: 'owner/repo', number: 42, headSHA: SHA, actor: 'owner', token: 'test-only-token',
      documents: sarif(), basePolicy: f.policy, candidatePolicy, ...f.options, fetchImpl: api.fetchImpl });
    assert.equal(report.passed, true);
    assert.equal(report.policySource, 'candidate-maintainer-review');
    assert.equal(report.reviewedCommit, SHA);
    assert.equal(report.reviewedActor, 'owner');
    assert.equal(api.calls.length, 2);
  }
});

test('ordinary write actor, stale PR head and API failure cannot self-expand a review policy', async t => {
  const f = await fixture(t);
  const { verifyPullRequestScan } = await verifier;
  for (const behavior of [{ permission: { permission: 'write', role_name: 'write' } }, { head: OLD_SHA }, { failStatus: 403 }]) {
    const api = pullAPI(behavior);
    await assert.rejects(verifyPullRequestScan({ repo: 'owner/repo', number: 42, headSHA: SHA, actor: 'owner', token: 'test-only-token',
      documents: sarif(), basePolicy: emptyPolicy(), candidatePolicy: f.policy, ...f.options, fetchImpl: api.fetchImpl }));
  }
});

test('verifier or workflow control changes also require the trusted maintenance actor', async t => {
  const f = await fixture(t);
  const { verifyPullRequestScan } = await verifier;
  await assert.rejects(verifyPullRequestScan({ repo: 'owner/repo', number: 42, headSHA: SHA, actor: 'owner', token: 'test-only-token',
    documents: sarif(), basePolicy: f.policy, candidatePolicy: f.policy, controlsChanged: true, ...f.options,
    fetchImpl: pullAPI({ permission: { permission: 'write', role_name: 'write' } }).fetchImpl }), /Only a repository admin or maintainer/);
  const report = await verifyPullRequestScan({ repo: 'owner/repo', number: 42, headSHA: SHA, actor: 'owner', token: 'test-only-token',
    documents: sarif(), basePolicy: f.policy, candidatePolicy: f.policy, controlsChanged: true, ...f.options, fetchImpl: pullAPI().fetchImpl });
  assert.equal(report.passed, true);
});
