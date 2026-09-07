'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');

const dashboardSource = fs.readFileSync(
  path.join(__dirname, '..', 'web', 'static', 'js', 'pages', 'dashboard.js'),
  'utf8',
);

test('dashboard trend controls expose a minute-precision custom range', () => {
  assert.match(dashboardSource, /<option value="month">本月<\/option>/);
  assert.match(dashboardSource, /<option value="custom">自定义<\/option>/);
  assert.match(dashboardSource, /type="datetime-local"[^>]*id="dashboard-trend-start"[^>]*step="60"/);
  assert.match(dashboardSource, /type="datetime-local"[^>]*id="dashboard-trend-end"[^>]*step="60"/);
  assert.match(dashboardSource, /id="dashboard-trend-apply"/);
});

test('dashboard custom range validates ordering before loading', () => {
  assert.match(dashboardSource, /结束时间必须晚于开始时间/);
  assert.match(dashboardSource, /dashboardTrendState\.customStart = current\.start/);
  assert.match(dashboardSource, /dashboardTrendState\.customEnd = current\.end/);
});

test('dashboard trend tooltip lists all site names or only the selected site', () => {
  assert.match(dashboardSource, /site_series/);
  assert.match(dashboardSource, /dashboardTrendState\.siteId === 'all'/);
  assert.match(dashboardSource, /dashboardTrendMetricLine/);
  assert.match(dashboardSource, /series\.site_name/);
  assert.match(dashboardSource, /selectedOption\?\.textContent/);
});

test('dashboard realtime trend keeps historical points after a page refresh', () => {
  assert.match(dashboardSource, /function dashboardTrendRealtimeOffset\(\)/);
  assert.match(dashboardSource, /historicalPoints\.slice\(0, offset\)\.concat\(realtimePoints\)/);
  assert.match(dashboardSource, /if \(!realtimePoints\.length \|\| !historicalPoints\.length\) return realtimePoints\.length \? realtimePoints : historicalPoints/);
});

test('dashboard loads one bootstrap snapshot before relying on SSE', () => {
  assert.doesNotMatch(dashboardSource, /loadDashboardInitialSnapshot/);
  assert.doesNotMatch(dashboardSource, /API\.dashboard\(signal\)/);
  assert.match(dashboardSource, /API\.dashboardBootstrap\(signal\)/);
});

test('dashboard orders snapshots and uses Agent sample timestamps', () => {
  assert.match(dashboardSource, /dashboardLastSnapshotMS/);
  assert.match(dashboardSource, /generated_at_ms/);
  assert.match(dashboardSource, /sampled_at_ms/);
  assert.match(dashboardSource, /current\.timestamp === previous\.timestamp/);
});

test('dashboard coalesces bootstrap and aborts stale trend requests', () => {
  assert.match(dashboardSource, /if \(dashboardBootstrapPromise\) return dashboardBootstrapPromise/);
  assert.match(dashboardSource, /dashboardTrendAbortController\.abort\(\)/);
  assert.match(dashboardSource, /dashboardTrends\([\s\S]*signal/);
  assert.match(dashboardSource, /setInterval\(\(\) => \{[\s\S]*60000/);
  assert.match(dashboardSource, /dashboardTrendState\.range === 'realtime'[\s\S]*15000/);
});
