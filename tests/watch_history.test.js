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

test('viewing history layout reuses theme tokens and keeps a two-column mobile poster grid', () => {
  const css = source('web/static/css/style.css');
  assert.match(css, /\.watch-history-grid\s*\{[^}]*grid-template-columns:\s*repeat\(auto-fill, minmax\(170px, 1fr\)\)/s);
  assert.match(css, /\.watch-history-poster\s*\{[^}]*aspect-ratio:\s*2 \/ 3/s);
  assert.match(css, /@media \(max-width: 768px\)[\s\S]*?\.watch-history-grid\s*\{\s*grid-template-columns:\s*repeat\(2, minmax\(0, 1fr\)\)/s);
  assert.match(css, /\.watch-history-card\s*\{[^}]*background:\s*var\(--panel\)/s);
  assert.match(css, /\.watch-history-detail-dialog\s*\{[^}]*display:\s*flex;[^}]*overflow:\s*hidden;/s);
  assert.match(css, /\.watch-history-detail-dialog\s*>\s*#watch-history-detail-content\s*\{[^}]*overflow:\s*auto;/s);
  assert.match(css, /\.watch-history-detail-modal\s*\{[^}]*inset:\s*var\(--nav-h\) 0 0;/s);
  assert.match(css, /\.watch-history-detail-modal\s*\{[^}]*place-items:\s*center;[^}]*padding:\s*20px;/s);
  assert.match(css, /\.watch-history-detail-dialog\s*\{[^}]*width:\s*min\(68vw, 1080px\);[^}]*height:\s*84vh;[^}]*max-width:\s*calc\(100vw - var\(--sidebar-w\) - 40px\);[^}]*max-height:\s*calc\(100dvh - var\(--nav-h\) - 32px\);[^}]*height:\s*84dvh;[^}]*transform:\s*translateX\(calc\(var\(--sidebar-w\) \/ 2\)\);/s);
  assert.match(css, /\.watch-history-detail-dialog\s*\{[^}]*border-radius:\s*var\(--ui-radius\);[^}]*background:\s*var\(--panel\);[^}]*box-shadow:\s*0 18px 60px/s);
  assert.match(css, /\.watch-history-detail-dialog\s*\{[^}]*padding:\s*0;/s);
  assert.match(css, /\.watch-history-detail-shell\s*>\s*\.watch-history-detail-layout\s*\{[^}]*padding:\s*228px 24px 24px;/s);
  assert.match(css, /\.watch-history-detail-meta\s*\{[^}]*grid-template-columns:\s*repeat\(2, minmax\(0, 1fr\)\);[^}]*grid-auto-flow:\s*row;[^}]*align-items:\s*start;[^}]*gap:\s*10px 24px;/s);
  assert.match(css, /@media\s*\(max-width:\s*768px\)[\s\S]*?\.watch-history-detail-modal\s*\{[^}]*place-items:\s*stretch;[^}]*padding:\s*8px max\(8px, env\(safe-area-inset-right\)\) max\(8px, env\(safe-area-inset-bottom\)\) max\(8px, env\(safe-area-inset-left\)\);/s);
  assert.match(css, /@media\s*\(max-width:\s*768px\)[\s\S]*?\.watch-history-detail-dialog\s*\{[^}]*width:\s*100%;[^}]*max-width:\s*none;[^}]*height:\s*100%;[^}]*max-height:\s*none;[^}]*transform:\s*none;/s);
  assert.match(css, /@media\s*\(max-width:\s*768px\)[\s\S]*?\.watch-history-detail-shell\s*>\s*\.watch-history-detail-layout\s*\{[^}]*padding:\s*194px max\(18px, env\(safe-area-inset-right\)\) 24px max\(18px, env\(safe-area-inset-left\)\);/s);
  assert.match(css, /html\[data-theme="light"\] \.watch-history-detail-heading h2[\s\S]*?color:\s*#0f172a;/s);
  assert.match(css, /\.watch-history-detail-background\s*\{[^}]*inset:\s*0;[^}]*background:\s*var\(--panel\)/s);
  assert.match(css, /\.watch-history-detail-background\s*\{[^}]*cursor:\s*zoom-in;/s);
  assert.match(css, /\.watch-history-detail-background::after[\s\S]*?linear-gradient\(180deg,[\s\S]*?transparent 0%[\s\S]*?transparent 24%[\s\S]*?var\(--panel\) 100%/s);
  assert.match(css, /\.watch-history-image-modal\s*\{[^}]*inset:\s*var\(--nav-h\) 0 0 var\(--sidebar-w\);[^}]*z-index:\s*1300;/s);
  assert.match(css, /\.watch-history-image-dialog\s*\{[^}]*width:\s*min\(68vw, 960px\);[^}]*max-width:\s*calc\(100vw - var\(--sidebar-w\) - 64px\);[^}]*max-height:\s*calc\(100dvh - 96px\);/s);
  assert.match(css, /@media \(max-width: 768px\)[\s\S]*?\.watch-history-image-dialog\s*\{[^}]*width:\s*calc\(100vw - 48px\);[^}]*max-width:\s*calc\(100vw - 48px\);[^}]*max-height:\s*calc\(100dvh - 48px\);/s);
  assert.match(css, /@media \(max-width: 768px\)[\s\S]*?\.watch-history-image-modal\s*\{[^}]*inset:\s*0;/s);
  assert.match(css, /\.watch-history-toolbar button:focus-visible[\s\S]*?outline:\s*2px solid var\(--blue\)/s);
  assert.match(css, /"video cache-limits retention"\s*"history \. \.":?/s);
  assert.match(css, /@media \(max-width: 768px\)[\s\S]*?\.watch-history-filters \.form-select,[\s\S]*?\.tmdb-settings-actions \.telegram-btn \{ min-height: 44px; \}/s);
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
  assert.match(history, /function watchHistoryRefreshOpenDetails\(\)[\s\S]*?if \(!item\) \{[\s\S]*?watchHistoryCloseImageViewer\(\);[\s\S]*?watchHistoryCloseDetails\(\);[\s\S]*?\}[\s\S]*?content\.innerHTML = watchHistoryDetailsHTML\(item\);/s);
  assert.doesNotMatch(history.match(/function watchHistoryRefreshOpenDetails\(\)[\s\S]*?\n\}/)?.[0] || '', /content\.innerHTML[\s\S]*?watchHistoryCloseImageViewer\(\)/);
});
