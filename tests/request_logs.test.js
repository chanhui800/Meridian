const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

function loadRequestLogHelpers() {
  const sandbox = { console, Date, Number, String, Math };
  vm.createContext(sandbox);
  vm.runInContext(
    fs.readFileSync(path.join(__dirname, '..', 'web', 'static', 'js', 'pages', 'request-logs.js'), 'utf8'),
    sandbox,
  );
  return sandbox;
}

test('request log table keeps P2 fields without COLO columns', () => {
  const source = fs.readFileSync(
    path.join(__dirname, '..', 'web', 'static', 'js', 'pages', 'request-logs.js'),
    'utf8',
  );
  for (const heading of ['节点', '资源类别', '状态', '客户端 IP', '客户端 UA', '上游 UA', '后端地址', '时间线']) {
    assert.match(source, new RegExp(`<th[^>]*>${heading}</th>`));
  }
  assert.doesNotMatch(source, /<th>入站机房<\/th>/);
  assert.doesNotMatch(source, /<th>出站机房<\/th>/);
  assert.match(source, /class="request-log-table"/);
});

test('global log write settings cover every visible request log column', () => {
  const source = fs.readFileSync(
    path.join(__dirname, '..', 'web', 'static', 'js', 'pages', 'global-settings.js'),
    'utf8',
  );
  for (const [id, property] of [
    ['setting-write-node', 'log_write_node'],
    ['setting-write-category', 'log_write_category'],
    ['setting-write-status', 'log_write_status'],
    ['setting-write-ip', 'log_write_client_ip'],
    ['setting-write-ua', 'log_write_ua'],
    ['setting-write-upstream-ua', 'log_write_upstream_ua'],
    ['setting-write-backend-address', 'log_write_backend_address'],
    ['setting-write-timeline', 'log_write_timeline'],
  ]) {
    assert.match(source, new RegExp(id));
    assert.match(source, new RegExp(`s\\.${property} = checkedSetting`));
  }
});

test('global log resource settings cover the complete request taxonomy', () => {
  const source = fs.readFileSync(
    path.join(__dirname, '..', 'web', 'static', 'js', 'pages', 'global-settings.js'),
    'utf8',
  );
  for (const [id, property] of [
    ['setting-write-playback', 'log_write_playback'],
    ['setting-write-video', 'log_write_video'],
    ['setting-write-image', 'log_write_image'],
    ['setting-write-metadata', 'log_write_metadata'],
    ['setting-write-subtitle', 'log_write_subtitle'],
    ['setting-write-asset', 'log_write_asset'],
    ['setting-write-websocket', 'log_write_websocket'],
    ['setting-write-api', 'log_write_api'],
    ['setting-write-auth', 'log_write_auth'],
  ]) {
    assert.match(source, new RegExp(id));
    assert.match(source, new RegExp(`s\\.${property} = checkedSetting`));
  }
});

test('global settings rendering stays scoped to the active page and keeps cached content visible', () => {
  const source = fs.readFileSync(
    path.join(__dirname, '..', 'web', 'static', 'js', 'pages', 'global-settings.js'),
    'utf8',
  );
  assert.match(source, /function bindGlobalSettingsNav\(root = document\)/);
  assert.match(source, /if \(globalSettingsCache\) paintGlobalSettings\(page\);/);
  assert.match(source, /const content = page\.querySelector\('\.settings-content'\);/);
  assert.match(source, /const nav = page\.querySelector\('\.settings-section-nav'\);/);
  assert.doesNotMatch(source, /const content = document\.querySelector\('\.settings-content'\);/);
  assert.match(source, /generation !== globalSettingsLoadGeneration/);
});

test('request log helpers map categories and status colors', () => {
  const sandbox = loadRequestLogHelpers();
  assert.equal(sandbox.requestLogCategoryLabel('playback'), '播放信息');
  assert.equal(sandbox.requestLogCategoryLabel('playback_sync'), '播放状态同步');
  assert.equal(sandbox.requestLogCategoryLabel('video'), '视频流');
  assert.equal(sandbox.requestLogCategoryLabel('stream'), '主视频流');
  assert.equal(sandbox.requestLogCategoryLabel('manifest'), '播放清单');
  assert.equal(sandbox.requestLogCategoryLabel('segment'), '媒体分片');
  assert.equal(sandbox.requestLogCategoryLabel('image'), '图片海报');
  assert.equal(sandbox.requestLogCategoryLabel('metadata'), '媒体元数据');
  assert.equal(sandbox.requestLogCategoryLabel('subtitle'), '字幕');
  assert.equal(sandbox.requestLogCategoryLabel('asset'), '静态资源');
  assert.equal(sandbox.requestLogCategoryLabel('websocket'), 'WebSocket');
  assert.equal(sandbox.requestLogCategoryLabel('api'), '常规 API');
  assert.equal(sandbox.requestLogCategoryLabel('auth'), '用户认证');
  assert.equal(sandbox.requestLogCategoryLabel(''), '—');
  assert.equal(sandbox.requestLogStatusClass(200), 'request-log-status-ok');
  assert.equal(sandbox.requestLogStatusClass(404), 'request-log-status-client');
  assert.equal(sandbox.requestLogStatusClass(503), 'request-log-status-server');
});

test('request log panel exposes only concrete resource-category filters and live refresh', () => {
  const source = fs.readFileSync(
    path.join(__dirname, '..', 'web', 'static', 'js', 'pages', 'request-logs.js'),
    'utf8',
  );
  for (const category of ['playback', 'playback_sync', 'stream', 'manifest', 'segment', 'image', 'metadata', 'subtitle', 'asset', 'websocket', 'api', 'auth']) {
    assert.match(source, new RegExp(`data-category="${category}"`));
  }
  assert.doesNotMatch(source, /data-category="video"/);
  assert.match(source, /<details class="management-log-filters" id="request-log-advanced-filters">/);
  assert.match(source, /id="request-log-filter-summary"/);
  assert.match(source, /requestLogRefreshTimer = setInterval/);
  assert.match(source, /Router\.current === 'request-logs'/);
  assert.match(source, /class="request-log-ip mono"/);
  assert.match(source, /class="request-log-region"/);
  assert.match(source, /requestLogLoading/);
  assert.doesNotMatch(source, /if \(Router\.current === 'request-logs'\) loadRequestLogs\(\);/);
  assert.match(source, /id="request-cache-clear"/);
  assert.match(source, /API\.clearAssetCache\(\)/);
});

test('node-name search is retried with the latest value after an automatic refresh is in flight', async () => {
  const page = { innerHTML: '' };
  const body = {
    innerHTML: '',
    closest() { return scroller; },
    querySelector() { return null; },
  };
  const scroller = { scrollTop: 0, scrollHeight: 0 };
  const elements = {
    'page-request-logs': page,
    'request-log-from': { value: '2026-08-05' },
    'request-log-to': { value: '2026-08-06' },
    'request-log-search': { value: '', oninput: null },
    'request-log-body': body,
    'request-log-summary': { textContent: '' },
    'request-log-refresh': { onclick: null },
    'request-log-clear': { onclick: null },
    'request-cache-clear': { onclick: null },
  };
  const timers = [];
  const calls = [];
  let resolveInitial;
  const initial = new Promise(resolve => { resolveInitial = resolve; });
  let callCount = 0;
  const sandbox = {
    console,
    Date,
    Number,
    String,
    Math,
    document: {
      getElementById(id) { return elements[id] || null; },
      querySelectorAll() { return []; },
    },
    Router: { current: 'request-logs' },
    API: {
      getRequestLogs(filters) {
        calls.push({ ...filters });
        callCount += 1;
        return callCount === 1 ? initial : Promise.resolve({ logs: [], dropped_logs: 0 });
      },
      clearRequestLogs() { return Promise.resolve(); },
      clearAssetCache() { return Promise.resolve(); },
    },
    Toast: { error() {}, success() {} },
    esc(value) { return String(value); },
    confirm() { return true; },
    setTimeout(callback) { timers.push(callback); return timers.length; },
    clearTimeout() {},
    setInterval() { return 1; },
    clearInterval() {},
  };
  vm.createContext(sandbox);
  vm.runInContext(
    fs.readFileSync(path.join(__dirname, '..', 'web', 'static', 'js', 'pages', 'request-logs.js'), 'utf8'),
    sandbox,
  );

  vm.runInContext('renderRequestLogs()', sandbox);
  await Promise.resolve();
  assert.equal(calls.length, 1);
  assert.equal(calls[0].q, '');

  elements['request-log-search'].value = 'edge-renamed';
  elements['request-log-search'].oninput();
  assert.equal(timers.length, 1);
  timers.shift()();

  resolveInitial({ logs: [], dropped_logs: 0 });
  await new Promise(resolve => setImmediate(resolve));
  await Promise.resolve();

  assert.equal(calls.length, 2);
  assert.equal(calls[1].q, 'edge-renamed');
});

test('request log date range covers the selected local days', () => {
  const sandbox = loadRequestLogHelpers();
  const range = sandbox.requestLogRangeMilliseconds('2026-08-04', '2026-08-05');
  assert.ok(Number.isFinite(range.from_ms));
  assert.ok(Number.isFinite(range.to_ms));
  assert.equal(range.to_ms - range.from_ms, (2 * 24 * 60 * 60 * 1000) - 1);
});

test('request log timeline heading aligns with timeline values', () => {
  const source = fs.readFileSync(
    path.join(__dirname, '..', 'web', 'static', 'css', 'style.css'),
    'utf8',
  );
  assert.match(source, /\.request-log-table th\[data-log-field="timeline"\],\s*\.request-log-table td\[data-log-field="timeline"\]\s*\{\s*text-align:\s*right;\s*\}/);
});

test('request log UA width is applied to the live table and handles drag completion', () => {
  const source = fs.readFileSync(
    path.join(__dirname, '..', 'web', 'static', 'js', 'pages', 'request-logs.js'),
    'utf8',
  );
  assert.match(source, /table\.style\.setProperty\('--request-log-ua-width', cssWidth\)/);
  assert.match(source, /col\.request-log-col-ua, col\.request-log-col-upstream-ua, th\[data-log-field="ua"\], th\[data-log-field="upstream-ua"\]/);
  assert.match(source, /uaWidthInput\.oninput = applyUAWidth/);
  assert.match(source, /uaWidthInput\.onchange = applyUAWidth/);
});

test('request log UA width slider stays compact', () => {
  const source = fs.readFileSync(
    path.join(__dirname, '..', 'web', 'static', 'css', 'style.css'),
    'utf8',
  );
  assert.match(source, /\.request-log-ua-width-control\s*\{[^}]*max-width:\s*380px;/s);
  assert.match(source, /\.request-log-ua-width-control input\[type="range"\]\s*\{[^}]*max-width:\s*220px;/s);
});

test('global log settings expose only runtime-backed storage controls', () => {
  const source = fs.readFileSync(
    path.join(__dirname, '..', 'web', 'static', 'js', 'pages', 'global-settings.js'),
    'utf8',
  );
  for (const control of ['setting-log-retention', 'setting-log-batch', 'setting-log-retries', 'setting-log-backoff']) {
    assert.match(source, new RegExp(control));
  }
  for (const retired of ['setting-log-delay', 'setting-log-threshold', 'setting-log-lease', 'log-search-choice', 'FTS 分词查询']) {
    assert.doesNotMatch(source, new RegExp(retired));
  }
});

test('request log filter chips keep centered labels and balanced spacing', () => {
  const source = fs.readFileSync(
    path.join(__dirname, '..', 'web', 'static', 'css', 'style.css'),
    'utf8',
  );
  assert.match(source, /\.request-log-filter-row\s*\{[^}]*padding:\s*10px 0;/s);
  assert.match(source, /\.request-log-pill\s*\{[^}]*display:\s*inline-flex;[^}]*align-items:\s*center;[^}]*justify-content:\s*center;[^}]*padding:\s*7px 16px;[^}]*text-align:\s*center;/s);
});

test('mobile request log table uses fixed readable columns without overlap', () => {
  const source = fs.readFileSync(
    path.join(__dirname, '..', 'web', 'static', 'css', 'style.css'),
    'utf8',
  );
  assert.match(source, /\.request-log-table col\.request-log-col-node,\s*\.request-log-table th:nth-child\(1\)\s*\{\s*width:\s*112px !important;/s);
  assert.match(source, /\.request-log-table col\.request-log-col-category,\s*\.request-log-table th:nth-child\(2\)\s*\{\s*width:\s*144px !important;/s);
  assert.match(source, /\.request-log-table col\.request-log-col-status,\s*\.request-log-table th:nth-child\(3\)\s*\{\s*width:\s*76px !important;/s);
  assert.match(source, /\.request-log-table col\.request-log-col-ip,\s*\.request-log-table th:nth-child\(4\)\s*\{\s*width:\s*170px !important;/s);
  assert.match(source, /\.request-log-table tbody tr\s*\{\s*height:\s*58px;/s);
  assert.match(source, /\.request-log-table th,\s*\.request-log-table td\s*\{[^}]*overflow:\s*hidden;[^}]*padding:\s*10px 12px;[^}]*font-size:\s*13px;[^}]*line-height:\s*1\.3;/s);
  assert.match(source, /\.request-log-category,\s*\.request-log-node,\s*\.request-log-status\s*\{\s*white-space:\s*nowrap;/s);
  assert.match(source, /\.request-log-table th\s*\{[^}]*font-size:\s*12px;[^}]*white-space:\s*nowrap;/s);
});

test('request log timeline uses concise Chinese relative time', () => {
  const sandbox = loadRequestLogHelpers();
  const now = Date.parse('2026-08-05T12:00:00Z');
  assert.equal(sandbox.requestLogRelativeTime(0, now), '—');
  assert.equal(sandbox.requestLogRelativeTime(now - 30_000, now), '刚刚');
  assert.equal(sandbox.requestLogRelativeTime(now - 5 * 60_000, now), '5 分钟前');
  assert.equal(sandbox.requestLogRelativeTime(now - 2 * 60 * 60_000, now), '2 小时前');
  assert.equal(sandbox.requestLogRelativeTime(now - 3 * 24 * 60 * 60_000, now), '3 天前');
});

test('request details escape values and respect hidden display fields', () => {
  const sandbox = loadRequestLogHelpers();
  vm.runInContext(fs.readFileSync(path.join(__dirname, '..', 'web', 'static', 'js', 'api.js'), 'utf8'), sandbox);
  const elements = {
    'modal-title': { textContent: '' },
    'modal-body': { innerHTML: '' },
    'modal-footer': { innerHTML: '' },
    'request-log-detail-close': {},
  };
  let modalClass;
  sandbox.document = { getElementById(id) { return elements[id]; } };
  sandbox.openModal = options => { modalClass = options.modalClass; };
  sandbox.closeModal = () => {};
  vm.runInContext('requestLogDisplaySettings.ua = false;', sandbox);
  sandbox.showRequestLogDetails({
    site_name: '<img src=x onerror=alert(1)>',
    method: 'GET', path: '/<script>synthetic</script>', status_code: 200,
    recorded_at_ms: 1760000000000, user_agent: 'operator-hidden-UA',
    upstream_user_agent: 'upstream-demo', backend_address: 'https://origin.example.com',
  });
  assert.equal(modalClass, 'management-log-modal');
  assert.match(elements['modal-body'].innerHTML, /&lt;img/);
  assert.doesNotMatch(elements['modal-body'].innerHTML, /<img|<script>|operator-hidden-UA/);
  assert.match(elements['modal-body'].innerHTML, /upstream-demo/);
});

test('request diagnostics distinguish reported zero from unknown legacy measurements', () => {
  const sandbox = loadRequestLogHelpers();
  assert.equal(sandbox.requestLogDiagnosticMilliseconds(0), '0 ms');
  for (const unknown of [undefined, null, '', false, -1, Infinity, '<svg>']) {
    assert.equal(sandbox.requestLogDiagnosticMilliseconds(unknown), '未报告');
  }
  assert.equal(sandbox.requestLogDiagnosticBytes(0), '0 B');
  assert.equal(sandbox.requestLogDiagnosticBytes(null), '未报告');
  assert.match(sandbox.requestLogDiagnosticFields({})[0][1], /未报告/);
  assert.equal(sandbox.requestLogDiagnosticFields({ diagnostics_version: 1, duration_ms: 0 }).find(([label]) => label === '请求总耗时')[1], '0 ms');
  assert.equal(sandbox.requestLogDiagnosticEnum('__proto__', {}), '未报告');
});

test('canceled partial content keeps the compact actual response while legacy 499 stays unchanged', () => {
  const sandbox = loadRequestLogHelpers();
  const entry = { diagnostics_version: 1, status_code: 499, response_status_code: 206, termination_reason: 'client_canceled' };
  assert.equal(sandbox.requestLogStatusLabel(entry), '206·已取消');
  assert.equal(sandbox.requestLogStatusLabel({ ...entry, diagnostics_version: undefined }), '499');
  assert.equal(sandbox.requestLogStatusLabel({ diagnostics_version: 1, status_code: 206, response_status_code: 206, termination_reason: 'completed' }), '206');
});

test('request diagnostics escape unsupported values and honor status, timeline, category and backend visibility', () => {
  const sandbox = loadRequestLogHelpers();
  vm.runInContext(fs.readFileSync(path.join(__dirname, '..', 'web', 'static', 'js', 'api.js'), 'utf8'), sandbox);
  const elements = {
    'modal-title': { textContent: '' }, 'modal-body': { innerHTML: '' }, 'modal-footer': { innerHTML: '' }, 'request-log-detail-close': {},
  };
  sandbox.document = { getElementById(id) { return elements[id]; } };
  sandbox.openModal = () => {};
  sandbox.closeModal = () => {};
  const malicious = '<img src=x onerror=alert(1)>';
  sandbox.showRequestLogDetails({
    diagnostics_version: 1, status_code: 499, response_status_code: 206, termination_reason: malicious,
    duration_ms: 0, body_bytes_written: 1234, connection_state: malicious, cache_lookup: malicious,
    cache_lookup_reason: malicious, cache_write: malicious, cache_write_reason: malicious, response_type: malicious,
  });
  const visible = elements['modal-body'].innerHTML;
  assert.match(visible, /实际响应状态/);
  assert.match(visible, /0 ms/);
  assert.match(visible, /1,234 B/);
  assert.match(visible, /未报告/);
  assert.doesNotMatch(visible, /<img|onerror|alert\(1\)/);
  vm.runInContext('requestLogDisplaySettings.status = false; requestLogDisplaySettings.timeline = false; requestLogDisplaySettings.category = false; requestLogDisplaySettings.backend_address = false;', sandbox);
  sandbox.showRequestLogDetails({ diagnostics_version: 1, status_code: 499, response_status_code: 206, termination_reason: 'client_canceled', duration_ms: 0, cache_lookup: 'read_error', cache_write: 'write_error', connection_state: 'new', body_bytes_written: 1234 });
  assert.doesNotMatch(elements['modal-body'].innerHTML, /实际响应状态|日志状态|结束原因|请求总耗时|缓存查找|缓存写入|连接使用|1,234 B|0 ms/);
});

function loadRequestLogRenderer() {
  const sandbox = loadRequestLogHelpers();
  vm.runInContext(fs.readFileSync(path.join(__dirname, '../web/static/js/api.js'), 'utf8'), sandbox);
  return sandbox;
}

function originFixture() {
  return {
    diagnostics_version: 1, redirect_count: 1, retry_count: 0,
    connection_state: 'mixed', final_connection_state: 'reused',
    upstream_attempts: [
      { attempt_index: 1, source_role: 'configured', authority: 'https://origin.example.test:8096', started_ms: 0, first_byte_ms: 4, response_headers_ms: 9, duration_ms: 10, response_status_code: 302, connection_state: 'new', dns_duration_ms: 1, tcp_duration_ms: 2, tls_duration_ms: 3, outcome: 'response' },
      { attempt_index: 2, source_role: 'dynamic', authority: 'https://media.example.test', started_ms: 12, first_byte_ms: 5, response_headers_ms: 7, duration_ms: 8, response_status_code: 206, connection_state: 'reused', outcome: 'response' },
    ],
  };
}

test('origin steps keep their order, redirect totals, final connection and per-attempt phase clocks', () => {
  const sandbox = loadRequestLogRenderer();
  const html = sandbox.requestLogDetailsHTML(originFixture());
  assert.match(html, /回源过程/);
  assert.ok(html.indexOf('第 1 次回源') < html.indexOf('第 2 次回源'));
  assert.ok(html.indexOf('配置上游') < html.indexOf('动态回源'));
  assert.match(html, /跳转次数<\/dt><dd>1 次/);
  assert.match(html, /重试次数<\/dt><dd>0 次/);
  assert.match(html, /最后连接<\/dt><dd>复用连接/);
  assert.match(html, /连接使用<\/dt><dd>新建与复用均有/);
  assert.match(html, /响应 302 · 已收到响应/);
  assert.match(html, /响应 206 · 已收到响应/);
  assert.match(html, /首个响应字节<\/dt><dd>4 ms/);
  assert.match(html, /最终响应头<\/dt><dd>9 ms/);
  assert.match(html, /请求内起点<\/dt><dd>12 ms/);
  assert.match(html, /首个响应字节可能是信息响应/);
  assert.doesNotMatch(html, /播放器首帧|CDN|<a\b/);
});

test('old diagnostics never infer the final connection, hop timings or zero redirects', () => {
  const sandbox = loadRequestLogRenderer();
  const old = sandbox.requestLogOriginProcessHTML({ diagnostics_version: 1, connection_state: 'mixed', upstream_first_byte_ms: 41 });
  assert.match(old, /最后连接<\/dt><dd>未报告/);
  assert.match(old, /跳转次数<\/dt><dd>未报告/);
  assert.match(old, /逐次回源未报告/);
  assert.doesNotMatch(old, /新建与复用均有|41 ms|0 次/);
  for (const version of [undefined, 0, 2]) {
    const html = sandbox.requestLogOriginProcessHTML({ ...originFixture(), diagnostics_version: version });
    assert.match(html, /未报告（旧 Agent 或历史记录）/);
    assert.doesNotMatch(html, /origin\.example|第 1 次回源/);
  }
  for (const invalid of [undefined, null, false, '', '0', -1, 1.5, Infinity, '<img>']) {
    assert.equal(sandbox.requestLogDiagnosticCount(invalid), '未报告');
  }
});

test('a long redirect chain retains the first seven and actual final step without borrowing a prior response', () => {
  const sandbox = loadRequestLogRenderer();
  const attempts = Array.from({ length: 12 }, (_, index) => ({
    attempt_index: index + 1, authority: `https://hop-${index + 1}.example.test`, response_status_code: 302,
    source_role: index === 0 ? 'configured' : 'dynamic', first_byte_ms: 3, duration_ms: 5, outcome: 'response',
  }));
  attempts[11] = { attempt_index: 12, authority: 'https://last.example.test', source_role: 'dynamic', duration_ms: 200, outcome: 'error' };
  const html = sandbox.requestLogOriginProcessHTML({ diagnostics_version: 1, upstream_attempts: attempts });
  assert.equal((html.match(/class="management-log-origin-step"/g) || []).length, 8);
  assert.match(html, /中间 4 次回源未展开/);
  assert.match(html, /value="12"/);
  assert.doesNotMatch(html, /hop-(?:8|9|10|11)\.example/);
  const last = html.slice(html.indexOf('第 12 次回源'));
  assert.match(last, /响应 未报告 · 回源失败/);
  assert.match(last, /首个响应字节<\/dt><dd>未报告/);
  assert.doesNotMatch(last, /响应 302|3 ms/);
});

test('origin diagnostics reject malformed schema values and never render credentials or executable markup', () => {
  const sandbox = loadRequestLogRenderer();
  const attack = '\"><img src=x onerror=alert(1)>';
  const fields = ['attempt_index', 'source_role', 'authority', 'started_ms', 'first_byte_ms', 'response_headers_ms', 'duration_ms', 'response_status_code', 'connection_state', 'dns_duration_ms', 'tcp_duration_ms', 'tls_duration_ms', 'outcome'];
  const attempt = Object.fromEntries(fields.map(field => [field, attack]));
  const html = sandbox.requestLogOriginProcessHTML({ diagnostics_version: 1, redirect_count: attack, retry_count: attack, final_connection_state: attack, upstream_attempts: [null, [], attack, attempt] });
  assert.match(html, /回源次序未报告/);
  assert.match(html, /未报告/);
  assert.doesNotMatch(html, /<img|onerror|alert\(1\)|value="0"/);
  for (const authority of ['https://user:secret@origin.test', 'https://origin.test/path', 'https://origin.test?token=secret', 'https://origin.test#secret', 'javascript:alert(1)', '\\evil.test']) {
    assert.equal(sandbox.requestLogDiagnosticAuthority(authority), '未报告');
  }
  assert.equal(sandbox.requestLogDiagnosticAuthority('http://[2001:db8::1]:8096'), 'http://[2001:db8::1]:8096');
});

test('all combinations of diagnostic display controls independently gate hop information', () => {
  const sandbox = loadRequestLogRenderer();
  for (let mask = 0; mask < 16; mask += 1) {
    const settings = { backend_address: !!(mask & 1), timeline: !!(mask & 2), status: !!(mask & 4), category: !!(mask & 8) };
    sandbox.visibility = settings;
    vm.runInContext('Object.assign(requestLogDisplaySettings, visibility)', sandbox);
    const html = sandbox.requestLogOriginProcessHTML(originFixture());
    const includes = (pattern, enabled) => enabled ? assert.match(html, pattern, `mask ${mask}`) : assert.doesNotMatch(html, pattern, `mask ${mask}`);
    includes(/origin\.example|最后连接|跳转次数|重试次数|新建连接|复用连接/, settings.backend_address);
    includes(/回源耗时|请求内起点|首个响应字节|最终响应头|\d+ ms/, settings.timeline);
    includes(/<dt>DNS<\/dt>|<dt>TCP<\/dt>|<dt>TLS<\/dt>/, settings.backend_address && settings.timeline);
    includes(/响应 302|响应 206|已收到响应/, settings.status);
    includes(/配置上游|动态回源/, settings.category);
    if (mask === 0) assert.equal(html, '');
  }
});

test('request details honor every display setting including when all diagnostic data is hidden', () => {
  const sandbox = loadRequestLogRenderer();
  const entry = { ...originFixture(), site_name: 'Visible Site', final_node: 'private-node', resource_category: 'manifest', status_code: 499, recorded_at_ms: 1760000000000, client_ip: 'hidden-client-ip', user_agent: 'hidden-client-UA', upstream_user_agent: 'hidden-upstream-UA', backend_address: 'https://hidden-origin.test' };
  vm.runInContext('Object.keys(requestLogDisplaySettings).forEach(key => { requestLogDisplaySettings[key] = false; })', sandbox);
  const html = sandbox.requestLogDetailsHTML(entry);
  assert.match(html, /Visible Site/);
  assert.doesNotMatch(html, /private-node|播放清单|499|hidden-client|hidden-upstream|hidden-origin|记录时间|回源过程|请求与缓存/);
  assert.doesNotMatch(sandbox.requestLogDetailsHTML({ ...entry, diagnostics_version: undefined }), /诊断信息|回源过程|请求与缓存/);
});

function requestDetailsHarness() {
  const elements = new Map();
  let document;
  function element(id) {
    if (elements.has(id)) return elements.get(id);
    const classes = new Set();
    const item = {
      id, parentElement: null, dataset: {}, style: {}, attributes: {},
      isConnected: true, disabled: false, tabIndex: 0,
      classList: {
        add(...values) { values.forEach(value => classes.add(value)); },
        remove(...values) { values.forEach(value => classes.delete(value)); },
        contains(value) { return classes.has(value); },
        toggle(value, active) { if (active) classes.add(value); else classes.delete(value); },
      },
      get className() { return [...classes].join(' '); },
      set className(value) { classes.clear(); String(value).split(/\s+/).filter(Boolean).forEach(value => classes.add(value)); },
      setAttribute(name, value) { this.attributes[name] = String(value); },
      removeAttribute(name) { delete this.attributes[name]; },
      addEventListener() {},
      getClientRects() { return [{}]; },
      matches() { return false; },
      focus() { document.activeElement = this; },
      blur() { if (document.activeElement === this) document.activeElement = document.body; },
      contains(other) { for (; other; other = other.parentElement) if (other === this) return true; return false; },
      querySelectorAll() { return this.id === 'modal' ? [element('modal-close'), element('request-log-detail-close')] : []; },
    };
    elements.set(id, item);
    return item;
  }
  document = {
    getElementById: element,
    querySelector: element,
    querySelectorAll(selector) { return selector === '.page' ? [element('page-request-logs'), element('page-account')] : []; },
    addEventListener() {},
    body: element('body'),
  };
  document.activeElement = document.body;
  element('modal').parentElement = element('modal-overlay');
  element('modal-close').parentElement = element('modal');
  element('request-log-detail-close').parentElement = element('modal');
  element('log-detail-trigger').parentElement = element('page-request-logs');
  const sandbox = {
    document, console, location: { hash: '#request-logs' },
    matchMedia() { return { matches: false }; },
    addEventListener() {}, scrollTo() {},
    requestAnimationFrame(callback) { callback(); },
    fetch() { return new Promise(() => {}); },
    clearInterval() {}, clearTimeout() {},
  };
  sandbox.window = sandbox;
  vm.createContext(sandbox);
  for (const relative of ['api.js', 'app.js', 'router.js', 'pages/request-logs.js']) {
    vm.runInContext(fs.readFileSync(path.join(__dirname, '../web/static/js', relative), 'utf8'), sandbox, { filename: relative });
  }
  vm.runInContext('Router.resolve()', sandbox);
  element('log-detail-trigger').focus();
  sandbox.showRequestLogDetails({ id: 1, site_name: 'Home', method: 'GET', path: '/emby/Items', status_code: 200 });
  return {
    element, document, sandbox,
    navigate(route) { sandbox.location.hash = '#' + route; vm.runInContext('Router.resolve()', sandbox); },
  };
}

test('leaving request logs dismisses its details and releases scrolling without focusing the departed row', () => {
  const h = requestDetailsHarness();
  assert.equal(h.element('modal-overlay').classList.contains('active'), true);
  h.navigate('account');
  assert.equal(h.element('modal-overlay').classList.contains('active'), false);
  assert.equal(h.document.body.classList.contains('modal-open'), false);
  assert.equal(h.element('page-account').classList.contains('active'), true);
  assert.equal(h.element('page-request-logs').contains(h.document.activeElement), false);
  assert.equal(h.element('modal-overlay').contains(h.document.activeElement), false);
});

test('request-log cleanup leaves another owner of the shared modal open', () => {
  const h = requestDetailsHarness();
  h.sandbox.openModal({ modalClass: 'site-config-modal' });
  const focused = h.document.activeElement;
  h.navigate('account');
  assert.equal(h.element('modal-overlay').classList.contains('active'), true);
  assert.equal(h.document.body.classList.contains('modal-open'), true);
  assert.equal(h.element('modal').classList.contains('site-config-modal'), true);
  assert.equal(h.document.activeElement, focused);
});

test('manual request-details close restores focus when invoked as a click handler', () => {
  const h = requestDetailsHarness();
  h.element('request-log-detail-close').onclick({ type: 'click' });
  assert.equal(h.element('modal-overlay').classList.contains('active'), false);
  assert.equal(h.document.body.classList.contains('modal-open'), false);
  assert.equal(h.document.activeElement, h.element('log-detail-trigger'));
});

test('late display settings redact the current request details without changing focus or its footer', () => {
  const h = requestDetailsHarness();
  h.sandbox.showRequestLogDetails({ ...originFixture(), site_name: 'Current', user_agent: 'hide-late-UA' });
  h.element('request-log-detail-close').focus();
  const focused = h.document.activeElement, footer = h.element('modal-footer').innerHTML;
  h.sandbox.requestLogApplyDisplaySettings({ log_display_ua: false, log_display_backend_address: false, log_display_timeline: false, log_display_status: false, log_display_category: false });
  assert.doesNotMatch(h.element('modal-body').innerHTML, /hide-late-UA|回源过程|origin\.example/);
  assert.equal(h.document.activeElement, focused);
  assert.equal(h.element('modal-footer').innerHTML, footer);
  assert.equal(h.element('modal-overlay').classList.contains('active'), true);
});

test('late request-log settings cannot repaint a newer shared modal or steal its focus', () => {
  const h = requestDetailsHarness();
  h.sandbox.openModal({ modalClass: 'site-config-modal' });
  h.element('modal-body').innerHTML = '<input value="Unsaved new owner">';
  h.element('modal-footer').innerHTML = 'Save new owner';
  h.element('new-owner-input').focus();
  const focused = h.document.activeElement;
  h.sandbox.requestLogApplyDisplaySettings({ log_display_backend_address: false });
  assert.equal(h.element('modal-body').innerHTML, '<input value="Unsaved new owner">');
  assert.equal(h.element('modal-footer').innerHTML, 'Save new owner');
  assert.equal(h.document.activeElement, focused);
});

function requestLogPollingHarness() {
  const stored = [], calls = [], writes = [], errors = [];
  const body = {
    rows: [],
    querySelector() { return this.rows[0] || null; },
    querySelectorAll() { return this.rows; },
    get lastElementChild() { return this.rows[this.rows.length - 1]; },
    parseRows(html) {
      return [...html.matchAll(/<tr\b[^>]*data-log-id="(\d+)"[\s\S]*?<\/tr>/g)].map(([markup, id]) => ({
        markup, dataset: { logId: id },
        remove() { body.rows.splice(body.rows.indexOf(this), 1); },
      }));
    },
    get innerHTML() { return this.rows.map(row => row.markup).join(''); },
    set innerHTML(html) { writes.push(html); this.rows = this.parseRows(html); },
    insertAdjacentHTML(position, html) {
      assert.equal(position, 'afterbegin');
      writes.push(html);
      this.rows.unshift(...this.parseRows(html));
    },
  };
  const elements = {
    'request-log-from': { value: '2026-08-05' },
    'request-log-to': { value: '2026-08-06' },
    'request-log-search': { value: '' },
    'request-log-body': body,
    'request-log-summary': { textContent: '' },
  };
  const timestamp = Date.parse('2026-08-05T12:00:00Z');
  const encodeCursor = entry => Buffer.from(`${entry.cursor_at_ms}:${entry.id}`).toString('base64url');
  const state = { holdNext: null };
  const sandbox = {
    console, Date, Number, String, Math,
    btoa: value => Buffer.from(value).toString('base64'),
    document: { getElementById: id => elements[id] || null, querySelectorAll() { return []; } },
    Router: { current: 'request-logs' },
    Toast: { error(message) { errors.push(message); } },
    esc: value => String(value),
    API: {
      async getRequestLogs(filters) {
        calls.push({ ...filters });
        let logs = stored.filter(entry => !filters.q || entry.path.includes(filters.q));
        if (filters.after_cursor) {
          const [afterMS, afterID] = Buffer.from(filters.after_cursor, 'base64url').toString().split(':').map(Number);
          logs = logs.filter(entry => entry.cursor_at_ms > afterMS || entry.cursor_at_ms === afterMS && entry.id > afterID);
        } else if (filters.after_id) logs = logs.filter(entry => entry.id > filters.after_id);
        logs.sort((first, second) => second.cursor_at_ms - first.cursor_at_ms || second.id - first.id);
        const response = { logs: logs.slice(0, filters.limit), dropped_logs: 0 };
        if (state.holdNext) {
          const hold = state.holdNext;
          state.holdNext = null;
          await hold;
        }
        return response;
      },
    },
  };
  vm.createContext(sandbox);
  vm.runInContext(fs.readFileSync(path.join(__dirname, '../web/static/js/pages/request-logs.js'), 'utf8'), sandbox);
  return {
    sandbox, calls, writes, errors, elements, encodeCursor,
    ids: () => body.rows.map(row => Number(row.dataset.logId)),
    add(count, newestID, extra = {}) {
      stored.push(...Array.from({ length: count }, (_, index) => ({
        id: newestID - index, site_name: 'Polling fixture', path: `/Items/${newestID - index}`,
        cursor_at_ms: timestamp, recorded_at_ms: 0, ...extra,
      })));
    },
    holdNext() {
      let release;
      state.holdNext = new Promise(resolve => { release = resolve; });
      return release;
    },
  };
}

test('automatic polling retains every row from a 150-request burst before advancing the cursor', async () => {
  const h = requestLogPollingHarness();
  h.add(1, 1);
  await h.sandbox.loadRequestLogs();
  h.add(150, 151);
  await h.sandbox.loadRequestLogs({ incremental: true });
  await h.sandbox.loadRequestLogs({ incremental: true });
  assert.deepEqual(h.ids(), Array.from({ length: 151 }, (_, index) => 151 - index));
  assert.equal(h.calls[1].limit, 500);
  assert.equal(h.calls[2].after_cursor, h.encodeCursor({ cursor_at_ms: Date.parse('2026-08-05T12:00:00Z'), id: 151 }));
  assert.deepEqual(h.errors, []);
});

test('a burst over 500 keeps the newest display window and polls correctly across tied timestamps', async () => {
  const h = requestLogPollingHarness();
  h.add(1, 1);
  await h.sandbox.loadRequestLogs();
  h.add(650, 651);
  await h.sandbox.loadRequestLogs({ incremental: true });
  assert.deepEqual(h.ids(), Array.from({ length: 500 }, (_, index) => 651 - index));
  h.add(1, 652);
  await h.sandbox.loadRequestLogs({ incremental: true });
  assert.deepEqual(h.ids(), Array.from({ length: 500 }, (_, index) => 652 - index));
  assert.equal(h.calls[2].after_cursor, h.encodeCursor({ cursor_at_ms: Date.parse('2026-08-05T12:00:00Z'), id: 651 }));
  assert.deepEqual(h.errors, []);
});

test('a poll starting during search debounce reads a fresh window for the new filter', async () => {
  const h = requestLogPollingHarness();
  h.add(1, 1, { path: '/new/1' });
  h.add(1, 2, { path: '/old/2' });
  h.elements['request-log-search'].value = '/old/';
  await h.sandbox.loadRequestLogs();
  h.elements['request-log-search'].value = '/new/';
  h.add(1, 3, { path: '/new/3' });
  await h.sandbox.loadRequestLogs({ incremental: true });
  assert.equal(h.calls[1].after_cursor, undefined);
  assert.equal(h.calls[1].after_id, undefined);
  assert.deepEqual(h.ids(), [3, 1]);
  assert.deepEqual(h.errors, []);
});

for (const reloadQueued of [true, false]) {
  test(`a pending poll ignores a changed search ${reloadQueued ? 'after a reload is queued' : 'before debounce runs'}`, async () => {
    const h = requestLogPollingHarness();
    h.add(1, 1, { path: '/old/1' });
    await h.sandbox.loadRequestLogs();
    h.add(1, 2, { path: '/old/2' });
    h.add(1, 3, { path: '/new/3' });
    const release = h.holdNext();
    const pending = h.sandbox.loadRequestLogs({ incremental: true });
    h.elements['request-log-search'].value = '/new/';
    if (reloadQueued) await h.sandbox.loadRequestLogs();
    const writesBefore = h.writes.length;
    release();
    await pending;
    await new Promise(resolve => setImmediate(resolve));
    assert.deepEqual(h.ids(), [3]);
    assert.equal(h.writes.length, writesBefore + 1, 'only the response for the new filter may render');
    assert.equal(h.calls[2].q, '/new/');
    assert.equal(h.calls[2].after_cursor, undefined);
    assert.deepEqual(h.errors, []);
  });
}
