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

test('request log table keeps the requested six columns without COLO fields', () => {
  const source = fs.readFileSync(
    path.join(__dirname, '..', 'web', 'static', 'js', 'pages', 'request-logs.js'),
    'utf8',
  );
  for (const heading of ['节点', '资源类别', '状态', '客户端 IP', 'UA', '时间线']) {
    assert.match(source, new RegExp(`<th>${heading}</th>`));
  }
  assert.doesNotMatch(source, /入站机房|出站机房|COLO/);
});

test('request log helpers map categories and status colors', () => {
  const sandbox = loadRequestLogHelpers();
  assert.equal(sandbox.requestLogCategoryLabel('playback'), '播放信息');
  assert.equal(sandbox.requestLogCategoryLabel('video'), '视频流');
  assert.equal(sandbox.requestLogCategoryLabel('image'), '图片海报');
  assert.equal(sandbox.requestLogCategoryLabel('api'), '常规 API');
  assert.equal(sandbox.requestLogCategoryLabel('auth'), '用户认证');
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
  assert.equal(sandbox.requestLogRelativeTime(now - 30_000, now), '刚刚');
  assert.equal(sandbox.requestLogRelativeTime(now - 5 * 60_000, now), '5 分钟前');
  assert.equal(sandbox.requestLogRelativeTime(now - 2 * 60 * 60_000, now), '2 小时前');
  assert.equal(sandbox.requestLogRelativeTime(now - 3 * 24 * 60 * 60_000, now), '3 天前');
});
