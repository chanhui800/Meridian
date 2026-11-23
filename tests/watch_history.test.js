'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const vm = require('node:vm');

const root = path.join(__dirname, '..');
const staticJS = path.join(root, 'web', 'static', 'js');

function source(relativePath) {
  return fs.readFileSync(path.join(root, relativePath), 'utf8');
}

function escapeHTML(value) {
  return String(value).replace(/[&<>"']/g, character => ({
    '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;',
  })[character]);
}

function loadWatchHistoryHelpers() {
  const sandbox = {
    console,
    Date,
    Number,
    String,
    Math,
    Promise,
    URL,
    API: {
      watchHistoryPosterURL(id) {
        const numeric = Number(id);
        return Number.isSafeInteger(numeric) && numeric > 0 ? `/api/watch-history/posters/${numeric}` : '';
      },
      watchHistoryBackdropURL(id) {
        const numeric = Number(id);
        return Number.isSafeInteger(numeric) && numeric > 0 ? `/api/watch-history/backdrops/${numeric}` : '';
      },
      watchHistoryStillURL(id, index) {
        const numeric = Number(id);
        const position = Number(index);
        return Number.isSafeInteger(numeric) && numeric > 0 && Number.isSafeInteger(position) && position >= 0 && position < 12
          ? `/api/watch-history/stills/${numeric}/${position}` : '';
      },
      watchHistoryCastURL(id, index) {
        const numeric = Number(id);
        const position = Number(index);
        return Number.isSafeInteger(numeric) && numeric > 0 && Number.isSafeInteger(position) && position >= 0 && position < 20
          ? `/api/watch-history/cast/${numeric}/${position}` : '';
      },
    },
    esc: escapeHTML,
  };
  vm.createContext(sandbox);
  vm.runInContext(source('web/static/js/pages/watch-history.js'), sandbox, { filename: 'watch-history.js' });
  return sandbox;
}

function loadTMDBHelpers() {
  const sandbox = { console, Number, String, esc: escapeHTML };
  vm.createContext(sandbox);
  vm.runInContext(source('web/static/js/pages/tmdb-settings.js'), sandbox, { filename: 'tmdb-settings.js' });
  return sandbox;
}

test('watch history is a first-level page between request logs and global settings', () => {
  const html = source('web/static/index.html');
  const logs = html.indexOf('data-page="request-logs"');
  const history = html.indexOf('data-page="watch-history"');
  const settings = html.indexOf('data-page="global-settings"');
  assert.ok(logs >= 0 && history > logs && settings > history);
  assert.match(html, /id="page-watch-history"/);
  assert.match(html, /id="page-settings-tmdb"/);
  assert.match(html, /pages\/watch-history\.js/);
  assert.match(html, /pages\/tmdb-settings\.js/);

  const router = source('web/static/js/router.js');
  const app = source('web/static/js/app.js');
  assert.match(router, /'watch-history': \['观看历史', '查看各站点最近播放记录与媒体信息'\]/);
  assert.match(router, /parentRoutes: new Set\(\[[^\]]*'settings-tmdb'/s);
  assert.match(app, /Router\.register\('watch-history', renderWatchHistory\)/);
  assert.match(app, /Router\.register\('settings-tmdb', renderTMDBSettings\)/);
});

test('watch history and TMDB API methods use encoded authenticated same-origin paths', async () => {
  const requests = [];
  const sandbox = { window: {}, URLSearchParams };
  vm.createContext(sandbox);
  vm.runInContext(source('web/static/js/api.js'), sandbox, { filename: 'api.js' });
  sandbox.fetch = async (url, options) => {
    requests.push({ url: String(url), options });
    return { status: 200, ok: true, statusText: 'OK', json: async () => ({ items: [] }) };
  };

  await vm.runInContext('API.getWatchHistory({ site_id: "site/42 ?", media_type: "movie", from_ms: 123, limit: 24 })', sandbox);
  await vm.runInContext('API.getActiveWatchHistory({ site_id: "site/42 ?", media_type: "movie" })', sandbox);
  await vm.runInContext('API.clearWatchHistory("site/42 ?")', sandbox);
  await vm.runInContext('API.getTMDBSettings()', sandbox);
  await vm.runInContext('API.saveTMDBSettings({ language: "zh-CN", history_retention_days: 90 })', sandbox);
  await vm.runInContext('API.testTMDBSettings({ token: "typed-only" })', sandbox);
  await vm.runInContext('API.deleteWatchHistory(42)', sandbox);
  await vm.runInContext('API.clearTMDBCache("stale")', sandbox);

  assert.deepEqual(requests.map(request => [request.options.method, request.url]), [
    ['GET', '/api/watch-history?site_id=site%2F42+%3F&media_type=movie&from_ms=123&limit=24'],
    ['GET', '/api/watch-history/active?site_id=site%2F42+%3F&media_type=movie'],
    ['DELETE', '/api/watch-history?site_id=site%2F42+%3F'],
    ['GET', '/api/tmdb-settings'],
    ['POST', '/api/tmdb-settings'],
    ['POST', '/api/tmdb-settings/test'],
    ['DELETE', '/api/watch-history/42'],
    ['POST', '/api/tmdb-settings/cache/clear'],
  ]);
  assert.equal(requests.every(request => request.options.credentials === 'same-origin'), true);
  assert.deepEqual(JSON.parse(requests[4].options.body), { language: 'zh-CN', history_retention_days: 90 });
  assert.deepEqual(JSON.parse(requests[5].options.body), { token: 'typed-only' });
  assert.deepEqual(JSON.parse(requests[7].options.body), { scope: 'stale' });
});

test('poster URLs accept only positive numeric media IDs and remain same-origin', () => {
  const sandbox = { window: {}, URLSearchParams };
  vm.createContext(sandbox);
  vm.runInContext(source('web/static/js/api.js'), sandbox, { filename: 'api.js' });
  assert.equal(vm.runInContext('API.watchHistoryPosterURL(42)', sandbox), '/api/watch-history/posters/42');
  assert.equal(vm.runInContext('API.watchHistoryPosterURL("42")', sandbox), '/api/watch-history/posters/42');
  assert.equal(vm.runInContext('API.watchHistoryPosterURL("https://evil.example/poster.jpg")', sandbox), '');
  assert.equal(vm.runInContext('API.watchHistoryPosterURL("1/../../x")', sandbox), '');
  assert.equal(vm.runInContext('API.watchHistoryPosterURL(-1)', sandbox), '');
  assert.equal(vm.runInContext('API.watchHistoryBackdropURL(42)', sandbox), '/api/watch-history/backdrops/42');
  assert.equal(vm.runInContext('API.watchHistoryStillURL(42, 2)', sandbox), '/api/watch-history/stills/42/2');
  assert.equal(vm.runInContext('API.watchHistoryStillURL(42, 12)', sandbox), '');
  assert.equal(vm.runInContext('API.watchHistoryCastURL(42, 2)', sandbox), '/api/watch-history/cast/42/2');
  assert.equal(vm.runInContext('API.watchHistoryCastURL(42, 20)', sandbox), '');
});

test('watch history card escapes media data and never interpolates a remote poster path', () => {
  const sandbox = loadWatchHistoryHelpers();
  const attack = '\"><img src=x onerror=alert(1)>';
  const html = sandbox.watchHistoryCardHTML({
    id: 1,
    media_item_id: 7,
    poster_available: true,
    poster_path: `https://evil.example/${attack}`,
    title: attack,
    media_type: 'episode',
    series_name: attack,
    season_number: 1,
    episode_number: 2,
    site_name: attack,
    last_watched_at_ms: Date.parse('2026-08-24T01:02:03Z'),
    progress_percent: 42,
    position_ticks: 7540000000,
    runtime_ticks: 36000000000,
  }, 0);

  assert.ok(!html.includes(attack));
  assert.match(html, /src="\/api\/watch-history\/posters\/7"/);
  assert.doesNotMatch(html, /evil\.example/);
  assert.match(html, /S01E02/);
  assert.match(html, /aria-valuenow="42"/);
  assert.match(html, /&lt;img src=x onerror=alert\(1\)&gt;/);
  assert.match(html, /role="button"/);
  assert.match(html, /tabindex="0"/);
  assert.match(html, /data-history-index="0"/);
  assert.match(html, /aria-haspopup="dialog"/);
  assert.match(html, /watch-history-progress-time/);
  assert.match(html, />12:34<\/span>/);
  assert.doesNotMatch(html, /12:34 \/ 1:00:00/);
});

test('live watch cards match the detail view by preferring a horizontal backdrop', () => {
  const sandbox = loadWatchHistoryHelpers();
  const html = sandbox.watchHistoryCardHTML({
    id: 2,
    media_item_id: 8,
    poster_available: true,
    poster_path: '/portrait.jpg',
    backdrop_path: '/landscape.jpg',
    stills: ['/still.jpg'],
    title: '正在播放',
    media_type: 'movie',
  }, 0, { source: 'live', live: true });

  assert.match(html, /src="\/api\/watch-history\/backdrops\/8"/);
  assert.match(html, /data-watch-history-card-fallbacks=/);
  assert.match(html, /watch-history-live-badge/);
});

test('generic history and live cards identify server authorities while keeping artwork same-origin', () => {
  const sandbox = loadWatchHistoryHelpers();
  for (const [sourceKey, address, authority] of [
    ['a'.repeat(64), 'http://192.0.2.10:8096', 'http://192.0.2.10:8096'],
    ['b'.repeat(64), 'https://emby.example:8920/private-mount/emby', 'https://emby.example:8920'],
    ['c'.repeat(64), 'https://[2001:db8::10]:8920/private-mount/emby', 'https://[2001:db8::10]:8920'],
    ['d'.repeat(64), 'http://default.example:80/private-mount', 'http://default.example:80'],
    ['e'.repeat(64), 'https://default.example/private-mount', 'https://default.example:443'],
  ]) {
    const item = {
      id: 1, site_id: 4, site_name: '通用站点', title: '同一影片',
      source_key: sourceKey, source_address: address,
      media_item_id: 7, poster_available: true, backdrop_path: '/landscape.jpg',
    };
    for (const context of [{}, { source: 'live', live: true }]) {
      const html = sandbox.watchHistoryCardHTML(item, 0, context);
      assert.match(html, /服务器地址/);
      assert.ok(html.includes(`<span class="watch-history-source-address-value">${authority}</span>`));
      assert.doesNotMatch(html, /(?:src|href|data-watch-history-image)="https?:/);
      assert.doesNotMatch(html, /private-mount/);
      assert.ok(!html.includes(sourceKey), 'the hash is not a readable server identifier');
    }
    const details = sandbox.watchHistoryDetailsHTML(item);
    assert.ok(details.includes(`<dt>服务器地址</dt><dd>${authority}</dd>`));
    assert.doesNotMatch(details, /(?:src|href|data-watch-history-image)="https?:/);
    assert.doesNotMatch(details, /private-mount/);
  }
});

test('source address credentials, paths and executable content never render in history, live cards or details', () => {
  const sandbox = loadWatchHistoryHelpers();
  const address = 'https://private-user:private-password@emby.example:8920/private-mount/\"><img src=x onerror=alert(1)>?api_key=private-token#private-fragment';
  const item = {
    id: 1, site_id: 4, site_name: '通用站点', title: '安全显示',
    source_key: 'a'.repeat(64), source_address: address,
    media_item_id: 7, poster_available: true,
    poster_path: 'https://other.example/poster.jpg',
  };
  for (const html of [
    sandbox.watchHistoryCardHTML(item, 0),
    sandbox.watchHistoryCardHTML(item, 0, { source: 'live', live: true }),
    sandbox.watchHistoryDetailsHTML(item),
  ]) {
    assert.ok(html.includes('https://emby.example:8920'), 'only the server authority is readable');
    assert.ok(!html.includes(address));
    assert.doesNotMatch(html, /private-user|private-password|private-mount|private-token|private-fragment|onerror|alert\(1\)|(?:src|href|data-watch-history-image)="https?:/);
    assert.match(html, /src="\/api\/watch-history\/posters\/7"/);
    assert.doesNotMatch(html, /other\.example/);
  }
});

test('server authority text stays HTML-escaped after URL normalization', () => {
  const sandbox = loadWatchHistoryHelpers();
  const item = {
    id: 1, title: '安全显示', source_key: 'a'.repeat(64),
    source_address: 'https://media&partners.example:8920/private-mount?api_key=private-token',
  };
  for (const html of [
    sandbox.watchHistoryCardHTML(item, 0),
    sandbox.watchHistoryCardHTML(item, 0, { source: 'live', live: true }),
    sandbox.watchHistoryDetailsHTML(item),
  ]) {
    assert.ok(html.includes('https://media&amp;partners.example:8920'));
    assert.doesNotMatch(html, /https:\/\/media&partners|private-mount|private-token/);
  }
});

test('long server addresses remain complete visible text with wrapping styles for cards and details', () => {
  const sandbox = loadWatchHistoryHelpers();
  const hostname = `${'a'.repeat(60)}.${'b'.repeat(60)}.${'c'.repeat(60)}.emby.example`;
  const address = `https://${hostname}:8920`;
  const item = { id: 1, title: '长地址', source_key: 'a'.repeat(64), source_address: address };
  for (const context of [{}, { source: 'live', live: true }]) {
    const html = sandbox.watchHistoryCardHTML(item, 0, context);
    assert.ok(html.includes(`<span class="watch-history-source-address-value">${address}</span>`));
    assert.doesNotMatch(html, /title="https?:|href="https?:/);
  }
  assert.ok(sandbox.watchHistoryDetailsHTML(item).includes(`<dt>服务器地址</dt><dd>${address}</dd>`));
  const css = source('web/static/css/history-console.css');
  assert.match(css, /\.watch-history-card-body\s*\{[^}]*min-width:\s*0/);
  assert.match(css, /\.watch-history-source-address\s*\{[^}]*overflow-wrap:\s*anywhere;[^}]*white-space:\s*normal/);
  assert.match(css, /\.watch-history-detail-source-address\s*\{[^}]*grid-column:\s*1\s*\/\s*-1;[^}]*min-width:\s*0/);
  assert.match(css, /\.watch-history-detail-source-address dd\s*\{[^}]*white-space:\s*normal/);
});

test('missing or invalid addresses show an update message only for generic source keys', () => {
  const sandbox = loadWatchHistoryHelpers();
  for (const sourceKey of ['a'.repeat(64), 'ABCDEF01'.repeat(8)]) {
    for (const address of [undefined, null, '', '   ', 42, {}, 'not a URL', 'javascript:alert(1)', 'https://', 'https:///private-token', 'https://server.example\\private-token', 'https://server.example:0', 'https://server.example:65536', 'https://\"><img src=x onerror=alert(1)>']) {
      const item = { id: 1, title: '待补齐记录', source_key: sourceKey, source_address: address };
      for (const html of [
        sandbox.watchHistoryCardHTML(item, 0),
        sandbox.watchHistoryCardHTML(item, 0, { source: 'live', live: true }),
        sandbox.watchHistoryDetailsHTML(item),
      ]) {
        assert.match(html, /服务器地址/);
        assert.match(html, /来源地址待更新/);
        assert.doesNotMatch(html, /private-token|onerror|javascript:|alert\(1\)/);
        assert.ok(!html.includes(sourceKey));
      }
    }
  }
  for (const sourceKey of [undefined, null, '', 'legacy-source', 'g'.repeat(64), 'a'.repeat(63), 'a'.repeat(65), ` ${'a'.repeat(64)} `]) {
    for (const address of [undefined, 'https://fixed.example:8920/private-mount?api_key=private-token']) {
      const item = { id: 1, title: '固定站点记录', site_name: '固定站点', source_key: sourceKey, source_address: address };
      for (const html of [
        sandbox.watchHistoryCardHTML(item, 0),
        sandbox.watchHistoryCardHTML(item, 0, { source: 'live', live: true }),
        sandbox.watchHistoryDetailsHTML(item),
      ]) {
        assert.doesNotMatch(html, /服务器地址|来源地址待更新|watch-history-(?:detail-)?source-address/);
        assert.doesNotMatch(html, /fixed\.example|private-mount|private-token/);
        assert.match(html, /固定站点/);
      }
    }
  }
});

test('watch history details escape synopsis and cast data while exposing the requested metadata', () => {
  const sandbox = loadWatchHistoryHelpers();
  const attack = '<script>alert(1)</script>';
  const html = sandbox.watchHistoryDetailsHTML({
    id: 4,
    media_item_id: 12,
    poster_available: true,
    title: '示例影片',
    media_type: 'movie',
    site_name: '测试站点',
    overview: attack,
    cast: [{ name: '演员甲', character: attack }, { name: '演员乙' }],
    last_seen_at_ms: Date.parse('2026-08-24T01:02:03Z'),
    progress_percent: 57,
  });
  assert.match(html, /简介/);
  assert.match(html, /演员表/);
  assert.match(html, /演员甲/);
  assert.match(html, /演员乙/);
  assert.match(html, /&lt;script&gt;alert\(1\)&lt;\/script&gt;/);
  assert.doesNotMatch(html, /<script>alert/);
  assert.match(html, /\/api\/watch-history\/posters\/12/);
});

test('watch history details show the playback client without exposing session credentials or device data', () => {
  const sandbox = loadWatchHistoryHelpers();
  const token = 'token-must-never-render';
  const html = sandbox.watchHistoryDetailsHTML({
    title: '正在播放的影片',
    site_name: '测试站点',
    user_name: 'Alice',
    user_id: 'alice-id',
    client_name: 'Emby for Android TV',
    device_name: 'Living Room TV',
    device_id: 'device-1',
    play_session_id: 'raw-play-session',
    token_stored: true,
    token_ciphertext: token,
  });
  assert.match(html, /观看用户/);
  assert.match(html, /Alice/);
  assert.match(html, /播放客户端/);
  assert.match(html, /Emby for Android TV/);
  assert.doesNotMatch(html, /设备名称|设备 ID|播放会话 ID|认证令牌|Living Room TV|raw-play-session/);
  assert.doesNotMatch(html, new RegExp(token));
});

test('client stop intent stays separate from upstream confirmation in cards and details', () => {
  const sandbox = loadWatchHistoryHelpers();
  const requested = Date.parse('2026-11-21T03:04:05Z');
  const item = { id: 1, title: '停止测试', client_stop_requested_at_ms: requested, stopped_at_ms: 0 };
  for (const outcome of ['pending', 'unconfirmed']) {
    const current = { ...item, client_stop_outcome: outcome };
    const html = sandbox.watchHistoryDetailsHTML(current);
    assert.match(html, /客户端已发起停止，源站未确认/);
    assert.match(html, /客户端停止请求/);
    assert.doesNotMatch(html, /源站确认停止<\/dt>|源站已确认|已退出|已经停止/);
    assert.match(sandbox.watchHistoryCardHTML(current, 0), /客户端已发起停止，源站未确认/);
    assert.equal(sandbox.watchHistoryItemIsActive(current), false);
  }
  const confirmed = { ...item, client_stop_outcome: 'confirmed', stopped_at_ms: requested + 10 };
  assert.match(sandbox.watchHistoryDetailsHTML(confirmed), /源站已确认/);
  assert.match(sandbox.watchHistoryDetailsHTML(confirmed), /源站确认停止<\/dt>/);
  assert.match(sandbox.watchHistoryCardHTML(confirmed, 0), /源站已确认/);
  assert.equal(sandbox.watchHistoryItemIsActive(confirmed), false);
  const unknown = sandbox.watchHistoryDetailsHTML({ ...item, client_stop_requested_at_ms: 0 });
  assert.match(unknown, /停止确认<\/dt><dd>未报告/);
  assert.doesNotMatch(unknown, /客户端停止请求|源站已确认|源站未确认/);
  const legacy = sandbox.watchHistoryDetailsHTML({ title: '历史停止', stopped_at_ms: requested });
  assert.match(legacy, /记录停止时间/);
  assert.doesNotMatch(legacy, /源站已确认|源站确认停止/);
});

test('denied stops remain active and a later successful progress can clear the stop intent', () => {
  const sandbox = loadWatchHistoryHelpers();
  const denied = { id: 1, title: '继续播放', client_stop_requested_at_ms: 1795212345000, client_stop_outcome: 'denied', stopped_at_ms: 0 };
  assert.equal(sandbox.watchHistoryItemIsActive(denied), true);
  const deniedCard = sandbox.watchHistoryCardHTML(denied, 0, { live: true, source: 'live' });
  assert.match(deniedCard, /正在观看/);
  assert.match(deniedCard, /源站拒绝停止/);
  const stopped = { ...denied, client_stop_outcome: 'unconfirmed' };
  assert.equal(sandbox.watchHistoryItemIsActive(stopped), false);
  assert.doesNotMatch(sandbox.watchHistoryCardHTML(stopped, 0, { live: true, source: 'live' }), /watch-history-live-badge/);
  const resumed = { ...stopped, client_stop_requested_at_ms: 0, client_stop_outcome: '', last_seen_at_ms: 1795212346000 };
  assert.equal(sandbox.watchHistoryItemIsActive(resumed), true);
  const resumedCard = sandbox.watchHistoryCardHTML(resumed, 0, { live: true, source: 'live' });
  assert.match(resumedCard, /watch-history-live-badge/);
  assert.doesNotMatch(resumedCard, /源站未确认|watch-history-stop-note/);
});

test('stop schema rejects invalid timestamps and unsupported outcome values without rendering executable data', () => {
  const sandbox = loadWatchHistoryHelpers();
  const attack = '\"><img src=x onerror=alert(1)>';
  for (const value of [undefined, null, false, '', '1795212345000', -1, Infinity, 1.5, attack]) {
    assert.equal(sandbox.watchHistoryStopTimestamp(value), 0);
    const html = sandbox.watchHistoryDetailsHTML({ title: 'Safe', client_stop_requested_at_ms: value, client_stop_outcome: attack, stopped_at_ms: value });
    assert.match(html, /停止确认<\/dt><dd>未报告/);
    assert.doesNotMatch(html, /<img|onerror|alert\(1\)|源站已确认/);
  }
  for (const outcome of [attack, '__proto__', 'toString', {}, null]) {
    const html = sandbox.watchHistoryCardHTML({ title: 'Safe', client_stop_requested_at_ms: 1795212345000, client_stop_outcome: outcome }, 0);
    assert.doesNotMatch(html, /watch-history-stop-note|onerror|alert\(1\)/);
  }
});

function historyDetailsHarness() {
  const sandbox = loadWatchHistoryHelpers();
  const writes = [], focuses = [];
  let document, controls = [];
  const close = { isConnected: true, focus() { document.activeElement = close; focuses.push(close); } };
  const trigger = { isConnected: true, focus() { document.activeElement = trigger; focuses.push(trigger); } };
  const content = {
    scrollTop: 0,
    contains(control) { return controls.includes(control); },
    querySelectorAll(selector) { return selector === '[data-watch-history-image]' ? controls : []; },
  };
  let markup = '';
  Object.defineProperty(content, 'innerHTML', {
    get: () => markup,
    set(value) {
      writes.push(String(value));
      controls.forEach(control => { control.isConnected = false; });
      if (controls.includes(document.activeElement)) document.activeElement = document.body;
      markup = String(value);
      controls = [...markup.matchAll(/data-watch-history-image="([^"]+)" data-watch-history-image-alt="([^"]+)"/g)].map(([, image, alt]) => {
        const control = {
          isConnected: true, addEventListener() {},
          getAttribute(name) { return name === 'data-watch-history-image' ? image : alt; },
          focus() { document.activeElement = control; focuses.push(control); },
        };
        return control;
      });
    },
  });
  const modal = { hidden: true, querySelector() { return close; } };
  const imageModal = { hidden: true };
  const elements = { 'watch-history-detail-modal': modal, 'watch-history-detail-content': content, 'watch-history-image-modal': imageModal };
  document = { activeElement: trigger, getElementById: id => elements[id] || null, body: { classList: { add() {}, remove() {} } } };
  sandbox.document = document;
  sandbox.Router = { current: 'watch-history' };
  return {
    sandbox, content, modal, close, trigger, writes, focuses, document,
    controls: () => controls,
    setItems(history, live = []) {
      sandbox.historyFixture = history;
      sandbox.liveFixture = live;
      vm.runInContext('watchHistoryState.items = historyFixture; watchHistoryState.liveItems = liveFixture;', sandbox);
    },
  };
}

for (const detailSource of ['history', 'live']) {
  test(`background address enrichment redraws history, live cards and the open ${detailSource} detail without changing progress`, async () => {
    const h = historyDetailsHarness();
    const makeGrid = () => {
      let markup = '';
      const writes = [];
      const grid = { setAttribute() {}, querySelectorAll() { return []; } };
      Object.defineProperty(grid, 'innerHTML', {
        get: () => markup,
        set(value) { markup = String(value); writes.push(markup); },
      });
      return { grid, writes };
    };
    const history = makeGrid(), live = makeGrid();
    const elements = {
      'page-watch-history': { isConnected: true },
      'watch-history-grid': history.grid,
      'watch-history-status': { textContent: '' },
      'watch-history-more': { hidden: true, disabled: false, textContent: '' },
      'watch-history-live-section': { hidden: true },
      'watch-history-live-grid': live.grid,
      'watch-history-live-count': { textContent: '' },
      'watch-history-live-description': { textContent: '' },
    };
    const getElementById = h.document.getElementById;
    h.document.getElementById = id => elements[id] || getElementById(id);
    h.sandbox.setTimeout = () => 1;
    h.sandbox.clearTimeout = () => {};
    h.sandbox.API.listSites = async () => [{ id: 4, name: '通用站点', watch_history_enabled: true }];
    h.sandbox.API.getTMDBSettings = async () => ({ enabled: false, configured: false });
    const item = {
      id: 1, site_id: 4, site_name: '通用站点', title: '地址补齐测试',
      source_key: 'a'.repeat(64), last_seen_at_ms: 1795212345000,
      position_ticks: 7540000000, runtime_ticks: 36000000000,
    };
    let address;
    const response = () => ({ items: [{ ...item, source_address: address }] });
    h.sandbox.API.getWatchHistory = async () => response();
    h.sandbox.API.getActiveWatchHistory = async () => response();

    await vm.runInContext('loadWatchHistory(true)', h.sandbox);
    h.sandbox.watchHistoryOpenDetails(0, detailSource);
    h.content.scrollTop = 97;
    const focused = h.document.activeElement;
    assert.match(history.grid.innerHTML, /来源地址待更新/);
    assert.match(live.grid.innerHTML, /来源地址待更新/);
    assert.match(h.content.innerHTML, /来源地址待更新/);

    for (const newAddress of ['https://server-a.example:8920', 'https://server-b.example:8920']) {
      const historyWrites = history.writes.length, liveWrites = live.writes.length, detailWrites = h.writes.length;
      address = newAddress;
      await vm.runInContext('loadWatchHistory(true, { background: true })', h.sandbox);
      assert.equal(history.writes.length, historyWrites + 1, 'address-only changes must update visible history rows');
      assert.equal(live.writes.length, liveWrites + 1, 'address-only changes must update visible live cards');
      assert.equal(h.writes.length, detailWrites + 1, 'the open detail must display the enriched address');
      for (const html of [history.grid.innerHTML, live.grid.innerHTML, h.content.innerHTML]) {
        assert.ok(html.includes(newAddress));
        assert.doesNotMatch(html, /来源地址待更新/);
      }
      assert.equal(h.document.activeElement, focused);
      assert.equal(h.content.scrollTop, 97);
      assert.equal(vm.runInContext('watchHistoryState.items[0].position_ticks', h.sandbox), item.position_ticks);
      assert.equal(vm.runInContext('watchHistoryState.liveItems[0].last_seen_at_ms', h.sandbox), item.last_seen_at_ms);

      await vm.runInContext('loadWatchHistory(true, { background: true })', h.sandbox);
      assert.equal(history.writes.length, historyWrites + 1, 'an unchanged response preserves the existing history DOM');
      assert.equal(live.writes.length, liveWrites + 1, 'an unchanged response preserves the existing live DOM');
      assert.equal(h.writes.length, detailWrites + 1, 'an unchanged response preserves the open detail DOM');
    }
  });
}

test('a live detail remains open on its historical stop record without stealing focus or fabricating a stop time', () => {
  const h = historyDetailsHarness();
  const item = { id: 1, title: 'Active', media_item_id: 7, poster_available: true };
  h.setItems([item], [item]);
  h.sandbox.watchHistoryOpenDetails(0, 'live');
  h.content.scrollTop = 85;
  const focused = h.document.activeElement;
  h.setItems([{ ...item, client_stop_requested_at_ms: 1795212345000, client_stop_outcome: 'unconfirmed', stopped_at_ms: 0 }]);
  h.sandbox.watchHistoryRefreshOpenDetails();
  assert.equal(h.modal.hidden, false);
  assert.match(h.content.innerHTML, /客户端已发起停止，源站未确认/);
  assert.doesNotMatch(h.content.innerHTML, /源站确认停止|源站已确认/);
  assert.equal(h.document.activeElement, focused);
  assert.equal(h.content.scrollTop, 85);
  const writes = h.writes.length;
  h.sandbox.watchHistoryRefreshOpenDetails();
  assert.equal(h.writes.length, writes, 'an unchanged snapshot must preserve its DOM');
});

test('a changing detail restores an equivalent image control and a missing record closes the old detail', () => {
  const h = historyDetailsHarness();
  const item = { id: 1, title: 'Old title', media_item_id: 7, poster_available: true };
  h.setItems([item]);
  h.sandbox.watchHistoryOpenDetails(0);
  const oldControl = h.controls()[0];
  oldControl.focus();
  h.setItems([{ ...item, title: 'Updated title', client_stop_requested_at_ms: 1795212345000, client_stop_outcome: 'pending' }]);
  h.sandbox.watchHistoryRefreshOpenDetails();
  assert.equal(oldControl.isConnected, false);
  assert.equal(h.document.activeElement, h.controls()[0]);
  assert.match(h.content.innerHTML, /Updated title/);
  h.setItems([]);
  h.sandbox.watchHistoryRefreshOpenDetails();
  assert.equal(h.modal.hidden, true);
  assert.equal(h.document.activeElement, h.trigger);
});

test('stop-only state changes invalidate the visible history card signature', () => {
  const sandbox = loadWatchHistoryHelpers();
  const item = { id: 1, title: 'Fixed progress', last_seen_at_ms: 1795212345000, position_ticks: 100 };
  const active = sandbox.watchHistoryItemsRenderSignature([item]);
  const intent = sandbox.watchHistoryItemsRenderSignature([{ ...item, client_stop_requested_at_ms: 1795212346000, client_stop_outcome: 'pending' }]);
  const confirmation = sandbox.watchHistoryItemsRenderSignature([{ ...item, client_stop_requested_at_ms: 1795212346000, client_stop_outcome: 'confirmed', stopped_at_ms: 1795212346100 }]);
  assert.notEqual(active, intent);
  assert.notEqual(intent, confirmation);
});

test('watch history details expose bounded TMDB media facts and same-origin gallery URLs', () => {
  const sandbox = loadWatchHistoryHelpers();
  const html = sandbox.watchHistoryDetailsHTML({
    id: 5,
    media_item_id: 13,
    poster_available: true,
    poster_path: '/poster.jpg',
    backdrop_path: '/backdrop.jpg',
    title: '示例剧集',
    media_type: 'episode',
    tmdb_type: 'tv',
    series_name: '示例剧集',
    season_number: 1,
    episode_number: 2,
    season_count: 3,
    episode_count: 36,
    release_date: '2024-03-04',
    genres: ['剧情', '悬疑'],
    vote_average: 8.7,
    status: 'Returning Series',
    last_air_date: '2026-08-14',
    next_air_date: '2026-08-21',
    next_season_number: 3,
    next_episode_number: 1,
    stills: ['/still-1.jpg', '/still-2.jpg'],
    cast: [{ name: '演员甲', character: '主角', profile_path: '/actor.jpg' }],
    position_ticks: 18000000000,
    runtime_ticks: 27000000000,
  });
  assert.match(html, /上映时间/);
  assert.match(html, /2024 年 03 月 04 日/);
  assert.match(html, /剧情 · 悬疑/);
  assert.match(html, /8\.7 \/ 10/);
  assert.match(html, /3 季 · 36 集 · 本次 S01E02/);
  assert.match(html, /下一集 S03E01/);
  assert.match(html, /\/api\/watch-history\/stills\/13\/0/);
  assert.match(html, /\/api\/watch-history\/stills\/13\/1/);
  assert.match(html, /watch-history-detail-still/);
  assert.match(html, /data-watch-history-image="\/api\/watch-history\/stills\/13\/0"/);
  assert.match(html, /watch-history-detail-background/);
  assert.match(html, /watch-history-detail-cast/);
  assert.match(html, /\/api\/watch-history\/cast\/13\/0/);
  assert.match(html, /30:00 \/ 45:00 · 67%/);
});

test('watch history helpers clamp progress and reject invalid poster/date values', () => {
  const sandbox = loadWatchHistoryHelpers();
  assert.equal(sandbox.watchHistoryProgress({ progress_percent: 130 }), 100);
  assert.equal(sandbox.watchHistoryProgress({ progress_percent: -9 }), 0);
  assert.equal(sandbox.watchHistoryProgress({ position_ticks: 25, runtime_ticks: 100 }), 25);
  assert.equal(sandbox.watchHistoryDurationLabel(7540000000), '12:34');
  assert.equal(sandbox.watchHistoryDurationLabel(36000000000), '1:00:00');
  assert.equal(sandbox.watchHistoryElapsedLabel({ position_ticks: 7540000000, runtime_ticks: 36000000000 }), '12:34');
  assert.equal(sandbox.watchHistoryTimeProgress({ position_ticks: 99900000000, runtime_ticks: 36000000000 }), '1:00:00 / 1:00:00');
  assert.equal(sandbox.watchHistoryPosterURL({ poster_available: true, media_item_id: '../../etc' }), '');
  assert.equal(sandbox.watchHistoryFormatTime(Number.MAX_VALUE), '时间未知');
});

test('background refresh preserves loaded pages while replacing series representatives', () => {
  const sandbox = loadWatchHistoryHelpers();
  const firstPage = Array.from({ length: 24 }, (_, index) => ({
    id: index + 1,
    site_id: 1,
    media_type: 'movie',
    title: `Movie ${index + 1}`,
  }));
  const secondPage = [
    { id: 25, site_id: 1, media_type: 'episode', series_name: 'Series A', title: 'Old Episode' },
    { id: 26, site_id: 1, media_type: 'movie', title: 'Later Movie' },
  ];
  const refreshed = [
    { id: 27, site_id: 1, media_type: 'episode', series_name: 'Series A', title: 'New Episode' },
    ...firstPage.slice(0, 23),
  ];
  const merged = sandbox.watchHistoryMergeBackgroundPage(firstPage.concat(secondPage), refreshed);
  assert.equal(merged[0].title, 'New Episode');
  assert.equal(merged.some(item => item.id === 25), false);
  assert.equal(merged.some(item => item.id === 26), true);
  assert.equal(merged.length, 26);
});

for (const [grouping, metadata] of [
  ['series name', { series_name: 'Shared Series' }],
  ['TMDB ID', { series_name: '', tmdb_type: 'tv', tmdb_id: 7001 }],
]) {
  test(`background refresh isolates ${grouping} across sources after loading more`, async () => {
    const sandbox = loadWatchHistoryHelpers();
    const sourceA = 'a'.repeat(64);
    const sourceB = 'b'.repeat(64);
    const firstPage = Array.from({ length: 24 }, (_, index) => ({
      id: index + 1,
      site_id: 1,
      media_type: 'movie',
      title: `Movie ${index + 1}`,
    }));
    const episode = { site_id: 1, media_type: 'episode', ...metadata };
    const laterPage = [
      { ...episode, id: 25, source_key: sourceA, title: 'Old source A episode' },
      { ...episode, id: 26, source_key: sourceB, title: 'Source B episode' },
      { ...episode, id: 27, title: 'Old fixed source episode' },
    ];
    const responses = [
      { items: firstPage, next_cursor: 'page-two', has_more: true },
      { items: laterPage, next_cursor: 'page-three', has_more: true },
      {
        items: [
          { ...episode, id: 28, source_key: sourceA, title: 'New source A episode' },
          { ...episode, id: 29, source_key: '', title: 'New fixed source episode' },
          ...firstPage.slice(0, 22),
        ],
        next_cursor: 'refreshed-first-page',
        has_more: false,
      },
    ];
    const requests = [];
    const grid = { innerHTML: '', setAttribute() {}, querySelectorAll() { return []; } };
    const elements = {
      'page-watch-history': { isConnected: true },
      'watch-history-grid': grid,
      'watch-history-status': { textContent: '' },
      'watch-history-more': { hidden: true, disabled: false, textContent: '' },
    };
    sandbox.document = { getElementById(id) { return elements[id] || null; } };
    sandbox.Router = { current: 'watch-history' };
    sandbox.setTimeout = () => 1;
    sandbox.clearTimeout = () => {};
    sandbox.API.listSites = async () => [{ id: 1, name: 'Shared site', watch_history_enabled: true }];
    sandbox.API.getTMDBSettings = async () => ({ enabled: false, configured: false });
    sandbox.API.getActiveWatchHistory = async () => ({ items: [] });
    sandbox.API.getWatchHistory = async filters => {
      requests.push(JSON.parse(JSON.stringify(filters)));
      return responses.shift();
    };

    await vm.runInContext('loadWatchHistory(true)', sandbox);
    await vm.runInContext('loadWatchHistory(false)', sandbox);
    assert.equal(vm.runInContext('watchHistoryState.items.length', sandbox), 27);
    assert.match(grid.innerHTML, /Source B episode/);

    await vm.runInContext('loadWatchHistory(true, { background: true })', sandbox);
    const state = JSON.parse(JSON.stringify(vm.runInContext('watchHistoryState', sandbox)));
    assert.equal(state.loading, false);
    assert.equal(state.items.some(item => item.id === 25), false, 'replace the old representative from source A');
    assert.equal(state.items.some(item => item.id === 28 && item.source_key === sourceA), true);
    assert.equal(state.items.some(item => item.id === 26 && item.source_key === sourceB), true, 'retain the loaded representative from source B');
    assert.equal(state.items.some(item => item.id === 27), false, 'missing and empty source keys identify the same fixed source');
    assert.equal(state.items.some(item => item.id === 29), true);
    assert.equal(state.items.length, 27);
    assert.equal(state.nextCursor, 'page-three');
    assert.equal(state.hasMore, true);
    assert.match(grid.innerHTML, /Source B episode/);
    assert.match(grid.innerHTML, /New source A episode/);
    assert.match(grid.innerHTML, /New fixed source episode/);
    assert.doesNotMatch(grid.innerHTML, /Old source A episode|Old fixed source episode/);
    assert.deepEqual(requests, [{ limit: 24 }, { limit: 24, cursor: 'page-two' }, { limit: 24 }]);
  });
}

test('TMDB settings never render a returned token and blank saves preserve it', () => {
  const sandbox = loadTMDBHelpers();
  const leaked = 'secret-token-must-not-render';
  const html = sandbox.renderTMDBSettingsForm({
    configured: true,
    enabled: true,
    secret_stable: true,
    credential_state: 'ready',
    api_token: leaked,
    token: leaked,
    language: 'zh-CN',
    history_retention_days: 90,
  });
  assert.ok(!html.includes(leaked));
  assert.match(html, /id="tmdb-api-token" type="password"/);
  assert.match(html, /autocomplete="new-password"/);
  assert.match(html, /已配置，留空保持不变/);
  assert.match(html, /src="\/tmdb-logo\.svg"/);
  assert.match(html, /This product uses the TMDB API but is not endorsed or certified by TMDB\./);

  assert.match(html, /id="tmdb-enabled"/);
  assert.match(html, /TMDB API Read Access Token/);
  assert.doesNotMatch(html, /Read Access Token \/ API Key/);

  const preserved = sandbox.buildTMDBSettingsPayload('', 'zh-CN', 90, true, false);
  assert.deepEqual(JSON.parse(JSON.stringify(preserved)), { enabled: true, language: 'zh-CN', history_retention_days: 90 });
  const replaced = sandbox.buildTMDBSettingsPayload(' new-token ', 'zh-TW', 120, true, false);
  assert.deepEqual(JSON.parse(JSON.stringify(replaced)), { enabled: true, language: 'zh-TW', history_retention_days: 120, token: 'new-token' });
  const removed = sandbox.buildTMDBSettingsPayload('', 'en-US', 30, true, true);
  assert.equal(removed.enabled, false);
  assert.equal(removed.clear_token, true);
  const removedWithTypedReplacement = sandbox.buildTMDBSettingsPayload('must-not-survive', 'en-US', 30, true, true);
  assert.equal(removedWithTypedReplacement.token, undefined);
});

test('watch history all filters are omitted and backend field names render correctly', () => {
  const sandbox = loadWatchHistoryHelpers();
  const filters = sandbox.watchHistoryFilters();
  assert.equal(filters.site_id, undefined);
  assert.equal(filters.media_type, undefined);
  assert.equal(filters.limit, 24);

  vm.runInContext('watchHistoryState.tmdbConfigured = true', sandbox);
  const html = sandbox.watchHistoryCardHTML({
    id: 9,
    media_item_id: 11,
    title: 'Example',
    media_type: 'movie',
    production_year: 2026,
    match_status: 'pending',
    last_seen_at_ms: Date.parse('2026-08-24T01:02:03Z'),
  }, 0);
  assert.match(html, />2026<\/p>/);
  assert.match(html, />补全中<\/span>/);
});

test('site viewing-history flags accept JSON booleans and legacy SQLite scalar values', () => {
  const sandbox = loadWatchHistoryHelpers();
  const sites = sandbox.watchHistoryNormalizeSites([
    { id: 1, name: '运行但关闭', enabled: true, watch_history_enabled: false },
    { id: 2, name: '明确开启', enabled: false, watch_history_enabled: true },
    { id: 3, name: '数值开启', watch_history_enabled: 1 },
    { id: 4, name: '字符串开启', watch_history_enabled: '1' },
    { id: 5, name: '文本开启', watch_history_enabled: 'true' },
    { id: 6, name: '关闭数值', watch_history_enabled: 0 },
  ]);
  assert.deepEqual(JSON.parse(JSON.stringify(sites)), [
    { id: '1', name: '运行但关闭', enabled: false },
    { id: '2', name: '明确开启', enabled: true },
    { id: '3', name: '数值开启', enabled: true },
    { id: '4', name: '字符串开启', enabled: true },
    { id: '5', name: '文本开启', enabled: true },
    { id: '6', name: '关闭数值', enabled: false },
  ]);
});

test('site cards show the playback entry only for active sessions and expose reduced-motion-safe heartbeat styling', () => {
  const sites = source('web/static/js/pages/sites.js');
  const css = source('web/static/css/style.css');
  assert.match(sites, /API\.getActiveWatchHistory\(\{\}\)\.catch/);
  assert.match(sites, /const sites = await API\.listSites\(\);/);
  assert.ok(sites.indexOf('API.getActiveWatchHistory({})') < sites.indexOf('const sites = await API.listSites();'));
  assert.match(sites, /const hasActivePlayback = activeSiteIDs\.has\(String\(s\.id\)\)/);
  assert.match(sites, /hasActivePlayback \? `<button[^`]*正在播放/);
  assert.match(css, /\.site-live-pulse\s*\{[\s\S]*?animation:\s*site-live-heartbeat/);
  assert.match(css, /@media \(prefers-reduced-motion: reduce\)\s*\{[\s\S]*?\.site-live-pulse\s*\{\s*animation:\s*none;/);
});

test('watch history and sites stay fresh while their pages are open and cancel timers on navigation', () => {
  const history = source('web/static/js/pages/watch-history.js');
  const sites = source('web/static/js/pages/sites.js');
  const router = source('web/static/js/router.js');
  assert.match(history, /const WATCH_HISTORY_REFRESH_INTERVAL_MS = 5000;/);
  assert.match(sites, /const SITE_REFRESH_INTERVAL_MS = 5000;/);
  assert.match(sites, /function scheduleSitesRefresh\(\)/);
  assert.match(sites, /void loadSites\(\{ background: true \}\);/);
  assert.match(sites, /function stopSitesRefresh\(\)/);
  assert.match(sites, /generation !== siteLoadGeneration \|\| Router\.current !== 'sites' \|\| !page\.isConnected/);
  assert.match(router, /previous === 'sites' && hash !== 'sites' && typeof stopSitesRefresh/);
});

test('watch history loader keeps the normalized enabled flag instead of normalizing twice', () => {
  const history = source('web/static/js/pages/watch-history.js');
  assert.match(history, /watchHistorySitesCache = Array\.isArray\(siteResult\.sites\) \? siteResult\.sites : \[\];/);
  assert.doesNotMatch(history, /watchHistorySitesCache = watchHistoryNormalizeSites\(siteResult\.sites\)/);
});

test('episode labels are emitted only for real episode rows', () => {
  const sandbox = loadWatchHistoryHelpers();
  assert.equal(sandbox.watchHistoryEpisodeLabel({ media_type: 'movie', season_number: 0, episode_number: 0 }), '');
  assert.equal(sandbox.watchHistoryEpisodeLabel({ media_type: 'episode', series_name: 'Specials', season_number: 0, episode_number: 1 }), 'Specials · S00E01');
  assert.equal(sandbox.watchHistoryEpisodeLabel({ media_type: 'episode', series_name: 'Unknown', season_number: 0, episode_number: 0 }), 'Unknown');
});

test('invalid TMDB credentials are unavailable and suppress the pending badge', () => {
  const sandbox = loadWatchHistoryHelpers();
  assert.deepEqual(JSON.parse(JSON.stringify(sandbox.watchHistoryTMDBAvailability({
    enabled: true,
    configured: true,
    credential_state: 'invalid',
  }))), { available: false, invalid: true });
  assert.deepEqual(JSON.parse(JSON.stringify(sandbox.watchHistoryTMDBAvailability({
    enabled: true,
    configured: true,
    credential_state: 'ready',
  }))), { available: true, invalid: false });

  vm.runInContext('watchHistoryState.tmdbConfigured = false; watchHistoryState.tmdbInvalid = true', sandbox);
  const html = sandbox.watchHistoryCardHTML({
    id: 1,
    media_item_id: 1,
    title: 'Pending title',
    media_type: 'movie',
    match_status: 'pending',
  }, 0);
  assert.doesNotMatch(html, /补全中/);
});

test('watch history notices expose auxiliary failures and invalid credentials accessibly', () => {
  const sandbox = loadWatchHistoryHelpers();
  const notice = { textContent: '', hidden: true };
  sandbox.document = { getElementById: id => id === 'watch-history-notice' ? notice : null };
  vm.runInContext(`
    watchHistoryState.sitesLoadError = '站点接口超时';
    watchHistoryState.tmdbLoadError = 'TMDB 接口超时';
    watchHistoryState.tmdbInvalid = false;
  `, sandbox);
  sandbox.watchHistoryRenderNotice();
  assert.match(notice.textContent, /站点列表读取失败/);
  assert.match(notice.textContent, /TMDB 设置读取失败/);
  assert.equal(notice.hidden, false);

  vm.runInContext(`
    watchHistoryState.sitesLoadError = '';
    watchHistoryState.tmdbLoadError = '';
    watchHistoryState.tmdbInvalid = true;
    watchHistoryState.tmdbConfigured = false;
  `, sandbox);
  sandbox.watchHistoryRenderNotice();
  assert.match(notice.textContent, /TMDB Token 无效/);

  const sourceText = source('web/static/js/pages/watch-history.js');
  assert.match(sourceText, /id="watch-history-notice" role="status" aria-live="polite" aria-atomic="true"/);
  assert.match(sourceText, /watchHistoryAuxiliaryError\(error, '请求失败'\)/);
});

test('site advanced settings default viewing history off and submit a boolean', () => {
  const sites = source('web/static/js/pages/sites.js');
  assert.match(sites, /<select class="form-select modal-select" id="m-watch-history">/);
  assert.match(sites, /option value="off" \$\{!isEdit \|\| !site\.watch_history_enabled \? 'selected' : ''\}>关闭/);
  assert.match(sites, /watchHistorySelect\.value = isEdit && site\.watch_history_enabled \? 'on' : 'off'/);
  assert.match(sites, /watch_history_enabled: watchHistorySelect\.value === 'on'/);
  assert.match(sites, /保存用户名、设备与原始 PlaySessionId 供管理员排查/);
  assert.match(sites, /令牌仅加密保存且绝不显示或回传/);
});

test('history console keeps the filter, live and detail interfaces in the new list layout', () => {
  const history = source('web/static/js/pages/watch-history.js');
  const css = source('web/static/css/history-console.css');
  for (const id of ['watch-history-site', 'watch-history-type', 'watch-history-range', 'watch-history-live-grid', 'watch-history-grid', 'watch-history-more', 'watch-history-detail-modal', 'watch-history-image-modal']) {
    assert.ok(history.includes(`id="${id}"`), `the existing ${id} interface must remain available`);
  }
  assert.match(css, /--console-surface/);
  assert.match(css, /--console-ink/);
  assert.match(css, /history-record-list[\s\S]*?grid-template-columns:\s*minmax\(0, 1fr\)/);
  assert.match(css, /watch-history-detail-dialog[\s\S]*?max-height:\s*calc\(100dvh - 40px\)/);
});

test('history dialog Tab navigation stays in the active modal', () => {
  const sandbox = loadWatchHistoryHelpers();
  const document = { activeElement: null, getElementById(id) { return id === 'watch-history-detail-modal' ? modal : null; } };
  const first = { hidden: false, offsetParent: {}, focus() { document.activeElement = first; } };
  const last = { hidden: false, offsetParent: {}, focus() { document.activeElement = last; } };
  const dialog = { querySelectorAll() { return [first, last]; }, contains(element) { return element === first || element === last; } };
  const modal = { hidden: false, querySelector() { return dialog; } };
  sandbox.document = document;
  document.activeElement = last;
  let prevented = 0;
  sandbox.watchHistoryTrapDialogFocus({ key: 'Tab', shiftKey: false, preventDefault() { prevented++; } });
  assert.equal(document.activeElement, first);
  sandbox.watchHistoryTrapDialogFocus({ key: 'Tab', shiftKey: true, preventDefault() { prevented++; } });
  assert.equal(document.activeElement, last);
  assert.equal(prevented, 2);
});

test('history delete control uses its own live source and cannot open details on Enter', () => {
  const sandbox = loadWatchHistoryHelpers();
  const cardEvents = {}, deleteEvents = {};
  const button = { addEventListener(name, handler) { deleteEvents[name] = handler; } };
  const card = {
    dataset: { historyIndex: '0', historySource: 'live' },
    querySelector() { return button; },
    addEventListener(name, handler) { cardEvents[name] = handler; },
    classList: { contains() { return false; }, add() {}, remove() {} },
  };
  const grid = { querySelectorAll(selector) { return selector.includes('data-history-index') ? [card] : []; } };
  sandbox.document = {};
  sandbox.confirm = () => true;
  const deleted = [];
  let opened = 0;
  sandbox.watchHistoryDeleteItem = item => deleted.push(item.id);
  sandbox.watchHistoryOpenDetails = () => { opened++; };
  vm.runInContext("watchHistoryState.items = [{id:1,title:'历史'}]; watchHistoryState.liveItems = [{id:2,title:'当前播放'}];", sandbox);
  sandbox.watchHistoryBindCards(grid);
  cardEvents.keydown({ key: 'Enter', target: { closest() { return button; } }, preventDefault() {} });
  deleteEvents.click({ preventDefault() {}, stopPropagation() {} });
  assert.equal(opened, 0);
  assert.deepEqual(deleted, [2]);
});

test('live deletion preserves pending and failed cards and reindexes surviving cards on success', async () => {
  const h = historyDetailsHarness();
  const section = { hidden: true };
  const count = { textContent: '' };
  const description = { textContent: '' };
  let markup = '', cards = [];
  const grid = {
    querySelectorAll(selector) { return selector === '.watch-history-card[data-history-index]' ? cards : []; },
  };
  Object.defineProperty(grid, 'innerHTML', {
    get: () => markup,
    set(value) {
      cards.forEach(card => { card.isConnected = false; });
      markup = String(value);
      cards = [...markup.matchAll(/<article\b[^>]*data-history-index="(\d+)" data-history-source="(live|history)"[^>]*>/g)].map(([, index, source]) => {
        const events = {}, deleteEvents = {};
        const button = { addEventListener(name, handler) { deleteEvents[name] = handler; } };
        return {
          isConnected: true,
          dataset: { historyIndex: index, historySource: source },
          events, deleteEvents,
          querySelector() { return button; },
          addEventListener(name, handler) { events[name] = handler; },
          classList: { contains() { return false; }, add() {}, remove() {} },
        };
      });
    },
  });
  const elements = {
    'watch-history-live-section': section,
    'watch-history-live-grid': grid,
    'watch-history-live-count': count,
    'watch-history-live-description': description,
  };
  const getElementById = h.document.getElementById;
  h.document.getElementById = id => elements[id] || getElementById(id);
  const requests = [], errors = [], successes = [], confirmations = [];
  h.sandbox.confirm = message => { confirmations.push(message); return true; };
  h.sandbox.Toast = { error: message => errors.push(message), success: message => successes.push(message) };
  h.sandbox.API.deleteWatchHistory = id => new Promise((resolve, reject) => { requests.push({ id, resolve, reject }); });
  let deletion;
  const deleteItem = h.sandbox.watchHistoryDeleteItem;
  h.sandbox.watchHistoryDeleteItem = item => { deletion = deleteItem(item); return deletion; };
  const clickDelete = card => {
    deletion = null;
    card.deleteEvents.click({ preventDefault() {}, stopPropagation() {} });
    assert.ok(deletion, 'the bound card control must call the real async deletion function');
    return deletion;
  };
  const firstLive = { id: 2, title: 'Live A' };
  const secondLive = { id: 3, title: 'Live B' };
  h.setItems([{ id: 1, title: 'Unrelated history' }, firstLive, secondLive], [firstLive, secondLive]);
  h.sandbox.watchHistoryRenderLive();
  const originalCards = cards;
  const originalMarkup = markup;
  assert.equal(cards.length, 2);
  assert.equal(count.textContent, '2');

  const failedDeletion = clickDelete(cards[0]);
  assert.equal(requests[0].id, 2);
  assert.equal(markup, originalMarkup, 'pending deletion must leave the visible cards intact');
  assert.equal(vm.runInContext('watchHistoryState.liveItems.length', h.sandbox), 2);
  assert.equal(vm.runInContext('watchHistoryState.items.length', h.sandbox), 3);
  requests[0].reject(new Error('Deletion failed'));
  await failedDeletion;
  assert.deepEqual(errors, ['Deletion failed']);
  assert.deepEqual(successes, []);
  assert.equal(cards, originalCards, 'failed deletion must leave the bound cards intact');
  assert.equal(markup, originalMarkup);
  assert.equal(vm.runInContext('watchHistoryState.liveItems.map(item => item.id).join(",")', h.sandbox), '2,3');
  assert.equal(vm.runInContext('watchHistoryState.items.map(item => item.id).join(",")', h.sandbox), '1,2,3');

  const firstDeletion = clickDelete(cards[0]);
  assert.equal(requests[1].id, 2);
  requests[1].resolve({ deleted: true, id: 2 });
  await firstDeletion;
  assert.equal(vm.runInContext('watchHistoryState.liveItems.map(item => item.id).join(",")', h.sandbox), '3');
  assert.equal(vm.runInContext('watchHistoryState.items.map(item => item.id).join(",")', h.sandbox), '1,3');
  assert.equal(cards.length, 1, 'redraw live cards immediately after a successful deletion');
  assert.equal(originalCards[0].isConnected, false);
  assert.equal(cards[0].dataset.historyIndex, '0');
  assert.equal(cards[0].dataset.historySource, 'live');
  assert.equal(count.textContent, '1');
  assert.match(markup, /Live B/);
  assert.doesNotMatch(markup, /Live A/);

  cards[0].events.click({ preventDefault() {} });
  assert.equal(vm.runInContext('watchHistoryDetailItemID', h.sandbox), '3');
  assert.match(h.content.innerHTML, /Live B/);
  const lastDeletion = clickDelete(cards[0]);
  assert.equal(requests[2].id, 3, 'the reindexed survivor must delete its own session');
  requests[2].resolve({ deleted: true, id: 3 });
  await lastDeletion;
  assert.deepEqual(requests.map(request => request.id), [2, 2, 3]);
  assert.equal(confirmations.length, 3);
  assert.match(confirmations[2], /Live B/);
  assert.equal(cards.length, 0);
  assert.equal(markup, '');
  assert.equal(count.textContent, '0');
  assert.equal(section.hidden, true);
  assert.equal(h.modal.hidden, true);
  assert.equal(vm.runInContext('watchHistoryState.items.map(item => item.id).join(",")', h.sandbox), '1');
  assert.equal(successes.length, 2);
});

test('TMDB official logo is stored locally for the panel CSP', () => {
  const logo = source('web/static/tmdb-logo.svg');
  assert.match(logo, /^<svg[^>]+viewBox="0 0 423\.04 35\.4"/);
  assert.match(logo, /The Movie Database/);
  assert.doesNotMatch(logo, /<script|onload=|(?:href|src)=["']https?:/i);
});

test('TMDB cache summary is rendered with a stable target for asynchronous refresh', () => {
  const tmdb = loadTMDBHelpers();
  const html = tmdb.renderTMDBSettingsForm({
    enabled: true,
    configured: true,
    language: 'zh-CN',
    history_retention_days: 90,
    cache_entries: 9,
    cache_size_bytes: 9705,
  });
  assert.match(html, /id="tmdb-cache-summary">9 条 · 9\.5 KB<\/b>/);
  const settings = source('web/static/js/pages/tmdb-settings.js');
  assert.match(settings, /setInterval\(refresh, 15000\)/);
  assert.match(settings, /const cacheEntriesValue = settings\.cache_entries \?\? settings\.cacheEntries/);
  assert.match(settings, /const cacheSizeValue = settings\.cache_size_bytes \?\? settings\.cacheSizeBytes/);
  assert.match(settings, /void refresh\(\);/);
  assert.match(settings, /querySelector\('#tmdb-cache-summary'\)/);
  assert.match(settings, /must not replace the form/);
});

test('watch history async loads are scoped to the active route and generation', () => {
  const history = source('web/static/js/pages/watch-history.js');
  assert.match(history, /generation !== watchHistoryLoadGeneration \|\| Router\.current !== 'watch-history'/);
  assert.match(history, /function stopWatchHistoryRefresh\(\)/);
  assert.match(history, /function watchHistoryItemsRenderSignature\(items, context = ''\)/);
  assert.match(history, /signature !== watchHistoryRenderedItemsSignature/);
  assert.match(history, /const showLoading = watchHistoryState\.loading && options\.background !== true;/);
  assert.match(history, /watchHistoryRenderItems\(undefined, \{ background \}\);/);
  assert.match(history, /role="status" aria-live="polite"/);
  assert.match(history, /aria-busy="true"/);
  assert.match(history, /loading="lazy" decoding="async"/);
  assert.match(history, /reset \|\| watchHistorySitesCache\.length === 0/);
  assert.match(history, /reset \|\| watchHistoryState\.tmdbConfigured === null/);
  assert.match(history, /WATCH_HISTORY_REFRESH_INTERVAL_MS = 5000/);
  assert.match(history, /\$\{WATCH_HISTORY_REFRESH_INTERVAL_MS \/ 1000\} 秒/);
  assert.match(history, /scheduleWatchHistoryRefresh\(\)/);
  assert.match(history, /loadWatchHistory\(true, \{ background: true \}\)/);
  assert.match(history, /watch-history-detail-modal/);
  assert.match(history, /watch-history-image-modal/);
  assert.match(history, /watchHistoryOpenImageViewer\(/);
  assert.match(history, /data-watch-history-image="\$\{esc\(backgroundURLs\[0\]\)\}"/);
  assert.match(history, /root\.querySelectorAll\('\[data-watch-history-image\]'\)/);
  assert.match(history, /data-watch-history-image-close/);
  assert.match(history, /role="dialog" aria-modal="true"/);
  assert.match(history, /event\.key === 'Escape'/);
  assert.match(history, /watchHistoryRefreshOpenDetails\(\)/);
  assert.match(history, /watchHistoryDetailItemID/);
});
