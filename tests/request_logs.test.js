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
  for (const heading of ['节点', '资源类别', '状态', '客户端 IP', 'UA', '时间线']) {
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
    ['setting-write-timeline', 'log_write_timeline'],
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
  assert.equal(sandbox.requestLogCategoryLabel('video'), '视频流');
  assert.equal(sandbox.requestLogCategoryLabel('image'), '图片海报');
  assert.equal(sandbox.requestLogCategoryLabel('api'), '常规 API');
  assert.equal(sandbox.requestLogCategoryLabel('auth'), '用户认证');
  assert.equal(sandbox.requestLogCategoryLabel(''), '—');
  assert.equal(sandbox.requestLogStatusClass(200), 'request-log-status-ok');
  assert.equal(sandbox.requestLogStatusClass(404), 'request-log-status-client');
  assert.equal(sandbox.requestLogStatusClass(503), 'request-log-status-server');
});

test('request log panel exposes video stream filtering and live refresh', () => {
  const source = fs.readFileSync(
    path.join(__dirname, '..', 'web', 'static', 'js', 'pages', 'request-logs.js'),
    'utf8',
  );
  assert.match(source, /data-category="video">只看视频流/);
  assert.match(source, /requestLogRefreshTimer = setInterval/);
  assert.match(source, /Router\.current === 'request-logs'/);
  assert.match(source, /class="request-log-ip mono"/);
  assert.match(source, /class="request-log-region"/);
  assert.match(source, /requestLogLoading/);
  assert.match(source, /previousScrollTop/);
  assert.match(source, /previousScrollTop \+ addedHeight/);
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

test('request log timeline uses concise Chinese relative time', () => {
  const sandbox = loadRequestLogHelpers();
  const now = Date.parse('2026-08-05T12:00:00Z');
  assert.equal(sandbox.requestLogRelativeTime(0, now), '—');
  assert.equal(sandbox.requestLogRelativeTime(now - 30_000, now), '刚刚');
  assert.equal(sandbox.requestLogRelativeTime(now - 5 * 60_000, now), '5 分钟前');
  assert.equal(sandbox.requestLogRelativeTime(now - 2 * 60 * 60_000, now), '2 小时前');
  assert.equal(sandbox.requestLogRelativeTime(now - 3 * 24 * 60 * 60_000, now), '3 天前');
});
