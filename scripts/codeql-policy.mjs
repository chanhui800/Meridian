import { createHash } from 'node:crypto';
import { readFileSync, readdirSync, realpathSync, statSync } from 'node:fs';
import { execFileSync } from 'node:child_process';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const DAY = 24 * 60 * 60 * 1000;
const HIGH_RISK_SCORE = 7;
const MAX_REVIEW_DAYS = 90;
const WORKFLOW = '.github/workflows/codeql.yml';
const CATEGORY = '.github/workflows/codeql.yml:analyze';

function requireValue(condition, message) {
  if (!condition) throw new Error(message);
}

function exactKeys(value, allowed, label) {
  requireValue(value && typeof value === 'object' && !Array.isArray(value), `${label} must be an object`);
  requireValue(Object.keys(value).every(key => allowed.includes(key)), `${label} contains unsupported fields`);
}

function sourcePath(value) {
  requireValue(typeof value === 'string' && /^[A-Za-z0-9_.-]+(?:\/[A-Za-z0-9_.-]+)*$/.test(value)
    && !value.split('/').some(part => part === '.' || part === '..'), 'Expected an exact repository-relative source path');
  return value;
}

// Go and workflow sources have LF in Git. Normalize checkout CRLF so a Windows
// review and the Linux CodeQL runner bind the same source bytes.
export function sourceHash(root, filename) {
  const relative = sourcePath(filename);
  const absoluteRoot = realpathSync(root);
  const absolute = realpathSync(path.join(absoluteRoot, relative));
  requireValue(absolute.startsWith(absoluteRoot + path.sep) && statSync(absolute).isFile(), `Review file escapes repository: ${relative}`);
  return createHash('sha256').update(readFileSync(absolute, 'utf8').replace(/\r\n/g, '\n')).digest('hex');
}

function validDate(value, label) {
  requireValue(typeof value === 'string' && /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$/.test(value), `${label} must be an explicit UTC timestamp`);
  const timestamp = Date.parse(value);
  requireValue(Number.isFinite(timestamp) && new Date(timestamp).toISOString() === value.replace('Z', '.000Z'), `${label} is invalid`);
  return timestamp;
}

export function validatePolicy(policy) {
  exactKeys(policy, ['schemaVersion', 'hashAlgorithm', 'reviews'], 'CodeQL policy');
  requireValue(policy.schemaVersion === 1 && policy.hashAlgorithm === 'sha256-lf' && Array.isArray(policy.reviews), 'Unsupported CodeQL policy schema');
  const identifiers = new Set();
  const findings = new Set();
  for (const review of policy.reviews) {
    exactKeys(review, ['id', 'ruleId', 'path', 'partialFingerprints', 'reason', 'reviewedBy', 'reviewedAt', 'expiresAt', 'evidence', 'files'], 'CodeQL review');
    requireValue(typeof review.id === 'string' && /^[a-z0-9][a-z0-9-]{3,79}$/.test(review.id) && !identifiers.has(review.id), 'Review ID must be exact and unique');
    identifiers.add(review.id);
    requireValue(typeof review.ruleId === 'string' && /^[a-z]+\/[a-z0-9-]+$/.test(review.ruleId), `Invalid exact rule ID: ${review.id}`);
    sourcePath(review.path);
    exactKeys(review.partialFingerprints, ['primaryLocationLineHash'], 'Review partialFingerprints');
    const fingerprint = review.partialFingerprints.primaryLocationLineHash;
    requireValue(typeof fingerprint === 'string' && /^[a-f0-9]{16,64}:[1-9][0-9]*$/.test(fingerprint), `Review must preserve the original CodeQL line fingerprint: ${review.id}`);
    const findingKey = [review.ruleId, review.path, fingerprint].join('\0');
    requireValue(!findings.has(findingKey), `Duplicate reviewed finding: ${review.id}`);
    findings.add(findingKey);
    requireValue(typeof review.reason === 'string' && review.reason.trim().length >= 40, `Review needs a specific safety rationale: ${review.id}`);
    requireValue(typeof review.reviewedBy === 'string' && review.reviewedBy.trim().length >= 5, `Review attribution missing: ${review.id}`);
    const reviewed = validDate(review.reviewedAt, 'reviewedAt');
    const expires = validDate(review.expiresAt, 'expiresAt');
    requireValue(expires > reviewed && expires - reviewed <= MAX_REVIEW_DAYS * DAY,
      `Review duration exceeds ${MAX_REVIEW_DAYS} days or is invalid: ${review.id}`);
    requireValue(typeof review.evidence === 'string', `Review evidence missing: ${review.id}`);
    const evidencePath = sourcePath(review.evidence.split('#')[0]);
    requireValue(Array.isArray(review.files) && review.files.length > 0, `Review must bind safety implementation files: ${review.id}`);
    const files = new Set();
    for (const file of review.files) {
      exactKeys(file, ['path', 'sha256'], 'Review safety file');
      sourcePath(file.path);
      requireValue(!files.has(file.path) && typeof file.sha256 === 'string' && /^[a-f0-9]{64}$/.test(file.sha256), `Invalid or duplicate review file: ${review.id}`);
      files.add(file.path);
    }
    requireValue(files.has(review.path), `Review must hash the finding source itself: ${review.id}`);
    requireValue(files.has(evidencePath), `Review must hash its evidence document: ${review.id}`);
  }
  return policy;
}

function validateApplicableReview(review, { root = process.cwd(), now = new Date() } = {}) {
  const current = new Date(now).getTime();
  requireValue(Number.isFinite(current), 'Invalid policy evaluation time');
  requireValue(Date.parse(review.reviewedAt) <= current && Date.parse(review.expiresAt) > current,
    `Applicable review expired or is future-dated: ${review.id}`);
  for (const file of review.files) {
    requireValue(sourceHash(root, file.path) === file.sha256, `Reviewed safety implementation changed: ${review.id} (${file.path})`);
  }
}

function findRule(run, result) {
  const component = result.rule?.toolComponent;
  const rules = Number.isInteger(component?.index)
    ? run.tool?.extensions?.[component.index]?.rules
    : run.tool?.driver?.rules;
  const index = result.rule?.index ?? result.ruleIndex;
  const indexed = Number.isInteger(index) ? rules?.[index] : undefined;
  const id = result.ruleId ?? result.rule?.id;
  if (indexed?.id === id) return indexed;
  const matches = [run.tool?.driver, ...(run.tool?.extensions ?? [])]
    .flatMap(tool => tool?.rules ?? []).filter(rule => rule.id === id);
  requireValue(matches.length === 1, `Missing or ambiguous SARIF rule metadata: ${id}`);
  return matches[0];
}

function findingPath(run, result) {
  const artifact = result.locations?.[0]?.physicalLocation?.artifactLocation;
  const uri = artifact?.uri ?? run.artifacts?.[artifact?.index]?.location?.uri;
  requireValue(typeof uri === 'string', `Finding has no source location: ${result.ruleId}`);
  return sourcePath(decodeURIComponent(uri));
}

export function evaluateSarif(documents, policy, options = {}) {
  validatePolicy(policy, options);
  const report = { passed: true, highRisk: 0, reviewed: [], blocked: [], nonBlocking: 0 };
  const docs = Array.isArray(documents) ? documents : [documents];
  requireValue(docs.length > 0, 'No CodeQL SARIF documents');
  for (const sarif of docs) {
    requireValue(sarif?.version === '2.1.0' && Array.isArray(sarif.runs) && sarif.runs.length > 0, 'Invalid or empty CodeQL SARIF');
    for (const run of sarif.runs) {
      const tools = [run.tool?.driver, ...(run.tool?.extensions ?? [])];
      requireValue(tools.some(tool => /codeql/i.test(tool?.name ?? '')) && tools.some(tool => tool?.rules?.length > 0), 'SARIF is missing CodeQL rule metadata');
      requireValue(Array.isArray(run.results), 'SARIF results missing');
      for (const invocation of run.invocations ?? []) {
        requireValue(invocation.executionSuccessful !== false && !(invocation.toolExecutionNotifications ?? []).some(note => note.level === 'error'), 'CodeQL analysis reported an execution failure');
      }
      if (options.expectedSHA) {
        const revisions = (run.versionControlProvenance ?? []).map(origin => origin.revisionId);
        requireValue(revisions.length > 0 && revisions.every(revision => revision === options.expectedSHA), 'SARIF commit does not match the release commit');
      }
      for (const result of run.results) {
        const rule = findRule(run, result);
        const scoreValue = rule.properties?.['security-severity'];
        const securityRule = rule.properties?.tags?.includes('security');
        if (scoreValue === undefined && !securityRule) { report.nonBlocking++; continue; }
        const score = Number(scoreValue);
        requireValue((typeof scoreValue === 'number' || (typeof scoreValue === 'string' && /^\d+(?:\.\d+)?$/.test(scoreValue)))
          && Number.isFinite(score) && score >= 0 && score <= 10, `Invalid security severity: ${rule.id}`);
        if (score < HIGH_RISK_SCORE) { report.nonBlocking++; continue; }
        report.highRisk++;
        const file = findingPath(run, result);
        const fingerprint = result.partialFingerprints?.primaryLocationLineHash;
        const review = policy.reviews.find(item => item.ruleId === rule.id && item.path === file
          && fingerprint && item.partialFingerprints.primaryLocationLineHash === fingerprint);
        const finding = { ruleId: rule.id, score, path: file,
          line: result.locations?.[0]?.physicalLocation?.region?.startLine,
          partialFingerprints: result.partialFingerprints ?? {} };
        if (review) {
          validateApplicableReview(review, options);
          report.reviewed.push({ ...finding, review: review.id, expiresAt: review.expiresAt });
        } else report.blocked.push(finding);
      }
    }
  }
  report.passed = report.blocked.length === 0;
  return report;
}

async function apiJSON(fetchImpl, apiURL, token, endpoint, accept = 'application/vnd.github+json') {
  const response = await fetchImpl(apiURL + endpoint, {
    headers: { Accept: accept, Authorization: `Bearer ${token}`, 'X-GitHub-Api-Version': '2022-11-28' },
    redirect: 'error', signal: AbortSignal.timeout(30000),
  });
  requireValue(response.ok, `GitHub CodeQL API failed (HTTP ${response.status})`);
  try { return await response.json(); } catch { throw new Error('GitHub CodeQL API returned invalid JSON'); }
}

async function paginated(fetchImpl, apiURL, token, endpoint, listKey) {
  const all = [];
  for (let page = 1; page <= 100; page++) {
    const body = await apiJSON(fetchImpl, apiURL, token, `${endpoint}${endpoint.includes('?') ? '&' : '?'}per_page=100&page=${page}`);
    const items = listKey ? body[listKey] : body;
    requireValue(Array.isArray(items), 'GitHub CodeQL API returned an invalid list');
    all.push(...items);
    if (items.length < 100) return all;
  }
  throw new Error('GitHub CodeQL API pagination limit exceeded');
}

// A single-maintainer repository cannot require someone else to approve the
// owner's own PR. Trust the actor's existing admin/maintain role, bound to the
// exact current PR head; ordinary write access cannot refresh its own records.
export async function verifyPullRequestScan({ repo, number, headSHA, actor, basePolicy, candidatePolicy,
  controlsChanged = false, documents, root = process.cwd(), token, now = new Date(),
  fetchImpl = fetch, apiURL = 'https://api.github.com' }) {
  validatePolicy(basePolicy);
  validatePolicy(candidatePolicy);
  const changed = controlsChanged || JSON.stringify(basePolicy) !== JSON.stringify(candidatePolicy);
  if (!changed) return { ...evaluateSarif(documents, basePolicy, { root, now }), policySource: 'base' };
  requireValue(/^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/.test(repo ?? '')
    && /^[1-9][0-9]*$/.test(String(number)) && /^[a-f0-9]{40}$/.test(headSHA ?? '')
    && /^[A-Za-z0-9-]+$/.test(actor ?? ''), 'Exact PR identity required for review-policy maintenance');
  requireValue(typeof token === 'string' && token.length > 0, 'GitHub token missing; review-policy maintenance cannot be authorized');
  const pull = await apiJSON(fetchImpl, apiURL, token, `/repos/${repo}/pulls/${number}`);
  requireValue(pull.head?.sha === headSHA && pull.base?.ref === 'main' && pull.base?.repo?.full_name?.toLowerCase() === repo.toLowerCase(),
    'Review-policy maintenance PR head or base changed');
  const permission = await apiJSON(fetchImpl, apiURL, token, `/repos/${repo}/collaborators/${encodeURIComponent(actor)}/permission`);
  requireValue(permission.permission === 'admin' || ['admin', 'maintain'].includes(permission.role_name),
    'Only a repository admin or maintainer may refresh a CodeQL review policy');
  return { ...evaluateSarif(documents, candidatePolicy, { root, now }), policySource: 'candidate-maintainer-review',
    reviewedCommit: headSHA, reviewedActor: actor };
}

export async function verifyReleaseScan({ repo, sha, policy, root = process.cwd(), token,
  now = new Date(), fetchImpl = fetch, apiURL = 'https://api.github.com' }) {
  requireValue(typeof repo === 'string' && /^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/.test(repo), 'Invalid GitHub repository');
  requireValue(typeof sha === 'string' && /^[a-f0-9]{40}$/.test(sha), 'An exact release commit SHA is required');
  requireValue(typeof token === 'string' && token.length > 0, 'GitHub token missing; release scan cannot be verified');
  requireValue(/^https:\/\/[A-Za-z0-9.-]+(?::\d+)?(?:\/[A-Za-z0-9_./-]+)?$/.test(apiURL), 'Invalid GitHub API origin');
  validatePolicy(policy, { root, now });
  const prefix = `/repos/${repo}`;
  const runs = await paginated(fetchImpl, apiURL, token,
    `${prefix}/actions/workflows/codeql.yml/runs?head_sha=${sha}&branch=main&event=push`, 'workflow_runs');
  const matchingRuns = runs.filter(run => run.head_sha === sha && run.head_branch === 'main'
    && run.event === 'push' && run.path === WORKFLOW)
    .sort((left, right) => (right.run_number - left.run_number) || ((right.run_attempt ?? 1) - (left.run_attempt ?? 1)));
  const latest = matchingRuns[0];
  requireValue(latest && latest.status === 'completed' && latest.conclusion === 'success', 'The exact release commit has no successful completed main CodeQL gate');
  const analyses = await paginated(fetchImpl, apiURL, token,
    `${prefix}/code-scanning/analyses?ref=refs%2Fheads%2Fmain&tool_name=CodeQL`);
  const matchingAnalyses = analyses.filter(analysis => analysis.commit_sha === sha && analysis.ref === 'refs/heads/main'
    && analysis.tool?.name === 'CodeQL' && analysis.category === CATEGORY)
    .sort((left, right) => Date.parse(right.created_at) - Date.parse(left.created_at));
  const analysis = matchingAnalyses[0];
  requireValue(analysis && Number.isSafeInteger(analysis.id) && !analysis.error && !analysis.warning, 'The exact release commit has no complete main CodeQL analysis');
  const sarif = await apiJSON(fetchImpl, apiURL, token, `${prefix}/code-scanning/analyses/${analysis.id}`, 'application/sarif+json');
  const report = evaluateSarif(sarif, policy, { root, now, expectedSHA: sha });
  return { ...report, commit: sha, workflowRun: latest.id, analysis: analysis.id };
}

function sarifFiles(target) {
  if (statSync(target).isFile()) return [target];
  return readdirSync(target, { withFileTypes: true }).flatMap(entry => {
    const filename = path.join(target, entry.name);
    return entry.isDirectory() ? sarifFiles(filename) : /\.sarif(?:\.json)?$/.test(entry.name) ? [filename] : [];
  });
}

async function main(args) {
  const [command, ...rest] = args;
  requireValue(['sarif', 'pr', 'release'].includes(command), 'Usage: codeql-policy.mjs sarif --sarif <path> --policy <file> | pr --sarif <path> --base-policy <file> --policy <file> --base-sha <commit> --repo <owner/repo> --pr <number> --head-sha <commit> --actor <actor> | release --sha <commit> --repo <owner/repo> --policy <file>');
  const options = {};
  for (let index = 0; index < rest.length; index += 2) {
    const name = rest[index];
    requireValue(['--sarif', '--policy', '--root', '--sha', '--repo', '--base-policy', '--base-sha', '--pr', '--head-sha', '--actor'].includes(name)
      && rest[index + 1] && !options[name], 'Invalid or duplicate CodeQL gate argument');
    options[name] = rest[index + 1];
  }
  requireValue(options['--policy'], 'CodeQL review policy missing');
  const policy = JSON.parse(readFileSync(options['--policy'], 'utf8').replace(/^\uFEFF/, ''));
  const root = options['--root'] ?? process.cwd();
  let report;
  if (command === 'sarif' || command === 'pr') {
    requireValue(options['--sarif'], 'CodeQL SARIF output missing');
    const documents = sarifFiles(options['--sarif']).map(filename => JSON.parse(readFileSync(filename, 'utf8').replace(/^\uFEFF/, '')));
    if (command === 'sarif') report = evaluateSarif(documents, policy, { root });
    else {
      requireValue(options['--base-policy'] && /^[a-f0-9]{40}$/.test(options['--base-sha'] ?? ''), 'Trusted PR base policy and commit missing');
      const basePolicy = JSON.parse(readFileSync(options['--base-policy'], 'utf8').replace(/^\uFEFF/, ''));
      const controls = ['scripts/codeql-policy.mjs', '.github/codeql-reviewed-alerts.json', '.github/codeql-review.schema.json',
        '.github/workflows/codeql.yml', '.github/workflows/release.yml'];
      const changes = execFileSync('git', ['diff', '--name-only', options['--base-sha'], '--', ...controls], { cwd: root, encoding: 'utf8' });
      report = await verifyPullRequestScan({ repo: options['--repo'], number: options['--pr'], headSHA: options['--head-sha'], actor: options['--actor'],
        basePolicy, candidatePolicy: policy, controlsChanged: changes.trim().length > 0, documents, root,
        token: process.env.GH_TOKEN || process.env.GITHUB_TOKEN, apiURL: process.env.GITHUB_API_URL || 'https://api.github.com' });
    }
  } else {
    report = await verifyReleaseScan({ repo: options['--repo'], sha: options['--sha'], policy, root,
      token: process.env.GH_TOKEN || process.env.GITHUB_TOKEN, apiURL: process.env.GITHUB_API_URL || 'https://api.github.com' });
  }
  console.log(JSON.stringify(report, null, 2));
  if (!report.passed) process.exitCode = 1;
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  main(process.argv.slice(2)).catch(error => {
    console.error(`CodeQL policy gate failed: ${error.message}`);
    process.exitCode = 1;
  });
}
