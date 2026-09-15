'use strict';

// Regression tests for the v1.10.0 full-project audit (frontend half). Each test
// is written so that reverting its fix makes it fail.

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

// --- watch history: a live card must delete the live item --------------------

function loadWatchHistorySandbox() {
  const sandbox = {
    console, Date, Number, String, Math, Promise, Set, Map, JSON,
    API: {
      watchHistoryPosterURL: () => '',
      watchHistoryBackdropURL: () => '',
      watchHistoryStillURL: () => '',
      watchHistoryCastURL: () => '',
      deleteWatchHistory: async () => ({ ok: true }),
    },
    esc: escapeHTML,
    Toast: { error() {}, success() {}, info() {} },
    document: { getElementById: () => null, querySelectorAll: () => [] },
    window: { location: { protocol: 'http:', hostname: '127.0.0.1', port: '' } },
    Router: { current: 'watch-history' },
  };
  vm.createContext(sandbox);
  vm.runInContext(source('web/static/js/pages/watch-history.js'), sandbox, { filename: 'watch-history.js' });
  return sandbox;
}

test('audit: a 正在观看 card resolves its delete target from the live collection', () => {
  const sandbox = loadWatchHistorySandbox();
  vm.runInContext(
    'watchHistoryState.items = [{ id: 11, title: "history-row" }];' +
    'watchHistoryState.liveItems = [{ id: 22, title: "live-row" }];',
    sandbox,
  );
  // The card index is an index into liveItems, so the source resolver is the only
  // correct lookup; indexing state.items here deleted an unrelated history row.
  const live = vm.runInContext('watchHistoryItemsForSource("live")[0]', sandbox);
  const history = vm.runInContext('watchHistoryItemsForSource("history")[0]', sandbox);
  assert.equal(live.id, 22);
  assert.equal(history.id, 11);
  assert.equal(vm.runInContext('watchHistoryItemsForSource(undefined)[0].id', sandbox), 11);

  const page = source('web/static/js/pages/watch-history.js');
  assert.match(
    page,
    /watchHistoryDeleteItem\(watchHistoryItemsForSource\(card\.dataset\.historySource\)\[index\]\)/,
    'the delete handler must resolve the collection from the card source',
  );
  assert.doesNotMatch(
    page,
    /watchHistoryDeleteItem\(watchHistoryState\.items\[index\]\)/,
    'the delete handler must not index the history collection for a live card',
  );
});

// --- sites: the render signature must cover every edited field ---------------

function loadSitesSandbox() {
  const sandbox = {
    console, Date, Number, String, Math, Promise, Set, Map, JSON, RegExp, Array, Object,
    esc: escapeHTML,
    Toast: { error() {}, success() {}, info() {} },
    API: {},
    Router: { current: 'sites' },
    document: { getElementById: () => null, querySelectorAll: () => [], querySelector: () => null },
    window: { location: { protocol: 'http:', hostname: '127.0.0.1', port: '', origin: 'http://127.0.0.1' } },
  };
  vm.createContext(sandbox);
  const page = source('web/static/js/pages/sites.js');
  // Only the pure helpers are needed; the page module also binds DOM globals, so
  // evaluate just the signature function together with its retention helper.
  const signature = page.slice(page.indexOf('function siteCardsRenderSignature'), page.indexOf('function renderSites()'));
  const retention = page.slice(
    page.indexOf('function siteAccountRetentionStatus'),
    page.indexOf('function renderSiteAccountRetention'),
  );
  vm.runInContext(`${retention}\n${signature}`, sandbox, { filename: 'sites-signature.js' });
  return sandbox;
}

test('audit: every site field the edit modal reads is part of the card render signature', () => {
  const sandbox = loadSitesSandbox();
  const base = { id: 1, name: 'A', watch_history_enabled: false };
  const signatureOf = site => vm.runInContext(
    `siteCardsRenderSignature([${JSON.stringify(site)}], new Set())`,
    sandbox,
  );
  const baseline = signatureOf(base);

  // Fields the edit modal pre-fills from the site object: a change to any of them
  // must repaint, otherwise re-opening 编辑 shows the pre-save value and the next
  // save writes it back.
  const editedFields = [
    ['speed_limit', 50],
    ['watch_history_enabled', true],
    ['asset_cache_enabled', true],
    ['asset_cache_ttl_sec', 7200],
    ['asset_cache_max_bytes', 1073741824],
    ['asset_cache_rules', '*/file/*'],
    ['main_video_stream_mode', 'direct'],
    ['client_ip_mode', 'real_ip'],
    ['path_prefix', 'emby'],
    ['primary_line_name', '备用'],
    ['playback_target_url', 'http://playback.example.test'],
    ['playback_mode', 'proxy'],
    ['custom_user_agent', 'UA/1'],
    ['custom_client', 'Client'],
    ['custom_version', '1.0'],
    ['public_host', 'host.example.test'],
    ['failover_lines', [{ name: 'L', url: 'http://line.example.test', enabled: true }]],
    ['stream_hosts', ['stream.example.test']],
  ];
  for (const [field, value] of editedFields) {
    const changed = signatureOf({ ...base, [field]: value });
    assert.notEqual(changed, baseline, `changing ${field} must change the render signature`);
  }
});

test('audit: the card render signature still tracks the fields it always tracked', () => {
  const sandbox = loadSitesSandbox();
  const base = { id: 1, name: 'A', running: true, traffic_used: 10 };
  const baseline = vm.runInContext(`siteCardsRenderSignature([${JSON.stringify(base)}], new Set())`, sandbox);
  for (const [field, value] of [['name', 'B'], ['running', false], ['traffic_used', 11]]) {
    const changed = vm.runInContext(
      `siteCardsRenderSignature([${JSON.stringify({ ...base, [field]: value })}], new Set())`,
      sandbox,
    );
    assert.notEqual(changed, baseline, `changing ${field} must still change the signature`);
  }
  const activeChanged = vm.runInContext(
    `siteCardsRenderSignature([${JSON.stringify(base)}], new Set(['1']))`,
    sandbox,
  );
  assert.notEqual(activeChanged, baseline, 'an active playback entry must change the signature');
});

// --- dashboard: periodic refreshes must survive a re-entry -------------------

test('audit: dashboard re-entry restarts the periodic refresh timers', () => {
  const dashboard = source('web/static/js/pages/dashboard.js');
  // The intervals are owned by the page render, because stopDashSSE runs on every
  // entry while app.js only starts them once per page load.
  const renderBody = dashboard.slice(
    dashboard.indexOf('function renderDashboard()'),
    dashboard.indexOf('function applyDashboardInsights('),
  );
  const startSSE = renderBody.indexOf('startDashSSE();');
  const startTimers = renderBody.indexOf('startDashboardRefreshTimers();');
  assert.ok(startSSE >= 0, 'renderDashboard must start SSE');
  assert.ok(startTimers > startSSE, 'renderDashboard must start the refresh timers after starting SSE');

  const stopBody = dashboard.slice(
    dashboard.indexOf('function stopDashSSE()'),
    dashboard.indexOf('function startDashboardRefreshTimers()'),
  );
  assert.doesNotMatch(
    stopBody,
    /clearInterval\(dashboardSecondaryRefreshTimer\)/,
    'stopDashSSE must not clear the secondary refresh interval',
  );
  assert.doesNotMatch(
    stopBody,
    /clearInterval\(dashboardTrendRefreshTimer\)/,
    'stopDashSSE must not clear the trend refresh interval',
  );
  // Teardown still has to stop them.
  const stopTimersBody = dashboard.slice(
    dashboard.indexOf('function stopDashboardRefreshTimers()'),
    dashboard.indexOf('const uaClassMap'),
  );
  assert.match(stopTimersBody, /clearInterval\(dashboardSecondaryRefreshTimer\)/);
  assert.match(stopTimersBody, /clearInterval\(dashboardTrendRefreshTimer\)/);
  assert.match(source('web/static/js/app.js'), /stopDashboardRefreshTimers\(\)/);
});

// --- request logs: hidden columns survive a full re-render -------------------

test('audit: a full request-log re-render re-applies the column display settings', () => {
  const page = source('web/static/js/pages/request-logs.js');
  const renderBody = page.slice(
    page.indexOf('function renderRequestLogRows(logs)'),
    page.indexOf('function requestLogRowHTML(entry)'),
  );
  assert.match(
    renderBody,
    /requestLogApplyDisplaySettings\(requestLogLastDisplaySettings\)/,
    'renderRequestLogRows must re-apply the operator column settings after replacing innerHTML',
  );
  // The incremental path keeps doing the same thing.
  assert.match(page, /body\.insertAdjacentHTML\('afterbegin', incoming\.map\(requestLogRowHTML\)\.join\(''\)\);/);
});

// --- telegram report settings: never save an unhydrated form -----------------

function loadTelegramSandbox() {
  const elements = new Map();
  const classList = { add() {}, remove() {}, toggle() {}, contains: () => false };
  const element = id => {
    if (!elements.has(id)) {
      elements.set(id, {
        id, classList, style: {}, dataset: {}, value: '', textContent: '', checked: false,
        hidden: false, disabled: false, title: '', innerHTML: '',
        querySelector: () => null, querySelectorAll: () => [],
      });
    }
    return elements.get(id);
  };
  const toasts = [];
  const sandbox = {
    console, Date, Number, String, Math, Promise, Set, Map, JSON, Array, Object, RegExp,
    esc: escapeHTML,
    Toast: {
      error: message => toasts.push(['error', message]),
      success: message => toasts.push(['success', message]),
      info: message => toasts.push(['info', message]),
    },
    API: { getTelegramReportSettings: async () => null, saveTelegramReportSettings: async () => null },
    Router: { current: 'telegram-report' },
    document: {
      getElementById: id => element(id),
      querySelectorAll: () => [],
    },
    window: { location: { protocol: 'http:', hostname: '127.0.0.1', port: '' } },
    setTimeout,
    clearTimeout,
  };
  vm.createContext(sandbox);
  vm.runInContext(source('web/static/js/pages/telegram-report.js'), sandbox, { filename: 'telegram-report.js' });
  return { sandbox, element, toasts };
}

test('audit: rendering the telegram settings page clears the loaded guard', () => {
  const { sandbox } = loadTelegramSandbox();
  vm.runInContext('telegramReportLoaded = true', sandbox);
  // Normalize line endings so the assertion does not depend on the checkout's
  // CRLF/LF setting.
  const page = source('web/static/js/pages/telegram-report.js').replace(/\r\n/g, '\n');
  const renderBody = page.slice(
    page.indexOf('function renderTelegramReport()'),
    page.indexOf('function updateTelegramFrequencyFields()'),
  );
  // The guard has to be cleared by the render itself, before the form is shown,
  // so a click during the settings read cannot submit the template defaults.
  assert.ok(
    renderBody.includes('telegramReportLoaded = false;\n  setTelegramReportButtonsPending(true);'),
    'renderTelegramReport must clear the guard when it rebuilds the form',
  );
  assert.ok(
    renderBody.indexOf('telegramReportLoaded = false;') < renderBody.indexOf('loadTelegramReportSettings()'),
    'the guard must be cleared before the settings read is requested',
  );
});

test('audit: submitting an unhydrated telegram form refuses and explains why', async () => {
  const { sandbox, toasts } = loadTelegramSandbox();
  const page = source('web/static/js/pages/telegram-report.js');
  const submitStart = page.indexOf('async function submitTelegramReport(testOnly)');
  assert.ok(submitStart > 0, 'submitTelegramReport must exist');
  const submit = page.slice(submitStart);
  assert.match(submit, /if \(!telegramReportLoaded\) \{[\s\S]{0,200}?Toast\.error\(/, 'the refusal must be visible');
  const body = sandbox.submitTelegramReport;
  assert.equal(typeof body, 'function');
  await body.call(sandbox, false);
  assert.equal(toasts.length, 1);
  assert.equal(toasts[0][0], 'error');
  assert.match(toasts[0][1], /尚未读取完成/);
  // The saved configuration must not have been overwritten.
  assert.equal(vm.runInContext('telegramReportLoaded', sandbox), false);
});

test('audit: the telegram guard is only raised after the form is hydrated', () => {
  const page = source('web/static/js/pages/telegram-report.js');
  const loader = page.slice(
    page.indexOf('async function loadTelegramReportSettings()'),
    page.indexOf('function telegramTrafficWarningPercent()'),
  );
  const guardIndex = loader.indexOf('telegramReportLoaded = true');
  const lastFieldIndex = loader.lastIndexOf('document.getElementById(');
  assert.ok(guardIndex > 0, 'the loader must raise the guard');
  assert.ok(
    lastFieldIndex < guardIndex,
    'the guard must be raised only after every field has been filled from the server',
  );
});
