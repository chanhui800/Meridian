'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '../web/static/js/pages/dashboard.js'), 'utf8');

function harness() {
  let now = Date.now(), stored = null, writes = 0, fail = false, encodes = 0, timerID = 0;
  const timers = new Map(), events = new Map();
  const sandbox = {
    console, Date: class extends Date { static now() { return now; } },
    JSON: { parse: JSON.parse, stringify(value) { encodes++; return JSON.stringify(value); } },
    window: {
      localStorage: { getItem() { return stored; }, setItem(_key, value) { writes++; if (fail) throw new Error('QuotaExceededError'); stored = value; }, removeItem() { stored = null; } },
      addEventListener(name, fn) { events.set(name, fn); },
    },
    setTimeout(fn, delay) { const id = ++timerID; timers.set(id, { fn, delay }); return id; },
    clearTimeout(id) { timers.delete(id); }, Router: { current: 'dashboard' },
  };
  vm.createContext(sandbox);
  vm.runInContext(source, sandbox);
  const run = expression => vm.runInContext(expression, sandbox);
  function seed(sites = 11, count = 150) {
    sandbox.fixture = Array.from({ length: count }, (_, index) => ({
      timestamp_ms: now - (count - index - 1) * 2000, download_bps: sites * 1000, upload_bps: 12,
      bytes_in: 40, bytes_out: 8000, requests: 2, realtime_revision: index + 1,
      site_contributions: Object.fromEntries(Array.from({ length: sites }, (_, site) => [String(site + 1), {
        download_bps: 1000, upload_bps: 12, bytes_in: 40, bytes_out: 8000, requests: 2, traffic_bytes: 8000,
        cumulative_bytes_in: 1000000 + index * 40, cumulative_bytes_out: 100000000 + index * 8000,
        cumulative_requests: index * 2,
      }])),
    }));
    run(`dashboardRealtimeEpoch='epoch'; dashboardRealtimeRevision=150; dashboardBillingMode='outbound'; fixture.forEach(point => dashboardAppendServerRealtimeTrendSample(point, false));`);
  }
  return { sandbox, run, seed, timers, events,
    get stored() { return stored; }, get writes() { return writes; }, get encodes() { return encodes; },
    setFail(value) { fail = value; }, advance(ms) { now += ms; },
    setStored(value) { stored = value; },
  };
}

test('dashboard persistence coalesces revisions and flushes once on pagehide', () => {
  const h = harness(); h.seed();
  for (let index = 0; index < 100; index++) h.run('persistDashboardRealtimeSamples()');
  assert.equal(h.writes, 0);
  assert.equal(h.encodes, 0, 'SSE admission should not serialize the whole window');
  assert.equal(h.timers.size, 1);
  assert.equal([...h.timers.values()][0].delay, 15000);
  h.events.get('pagehide')();
  assert.equal(h.writes, 1);
  assert.equal(h.timers.size, 0);
  assert.equal(JSON.parse(h.stored).version, 2);
  h.events.get('pagehide')();
  assert.equal(h.writes, 1, 'clean pagehide should not rewrite storage');
});

test('compact storage reconstructs shared site samples and preserves selected-site corrections', () => {
  const h = harness(); h.seed(11, 10);
  h.run(`persistDashboardRealtimeSamples(); flushDashboardRealtimeSamples();`);
  assert.deepEqual(JSON.parse(h.stored).sites, {}, 'identical aggregate contributions should not be stored twice');
  h.run(`dashboardRealtimeTrendSamples=new Map();dashboardRealtimeTrendSiteSamples=new Map();restoreDashboardRealtimeSamples();`);
  assert.equal(h.run(`dashboardRealtimeTrendSiteSamples.size`), 11);
  assert.equal(h.run(`dashboardRealtimeTrendSiteSamples.get('1').length`), 10);
  assert.equal(h.run(`dashboardRealtimeTrendSiteSamples.get('1')[9].bytes_out`), 8000);
  h.run(`dashboardRealtimeTrendSiteSamples.get('1')[9]={...dashboardRealtimeTrendSiteSamples.get('1')[9],bytes_out:9000,realtime_revision:151};persistDashboardRealtimeSamples();flushDashboardRealtimeSamples();`);
  const payload = JSON.parse(h.stored);
  assert.equal(payload.sites['1'].length, 1);
  assert.equal(payload.sites['1'][0].bytes_out, 9000);
  h.run(`dashboardRealtimeTrendSamples=new Map();dashboardRealtimeTrendSiteSamples=new Map();restoreDashboardRealtimeSamples();`);
  assert.equal(h.run(`dashboardRealtimeTrendSiteSamples.get('1')[9].bytes_out`), 9000);
  assert.equal(h.run(`dashboardRealtimeEpoch`), 'epoch');
  assert.equal(h.run(`dashboardRealtimePersistedBillingMode`), 'outbound');
});

test('large dashboard history stays below storage byte budget and keeps latest sample', () => {
  const h = harness(); h.seed(100, 150);
  h.run(`persistDashboardRealtimeSamples();flushDashboardRealtimeSamples();`);
  assert.ok(h.stored.length <= 1024 * 1024);
  const payload = JSON.parse(h.stored);
  assert.ok(payload.all.length > 0 && payload.all.length < 150);
  assert.equal(payload.all.at(-1).realtime_revision, 150);
  assert.equal(Object.keys(payload.all.at(-1).site_contributions).length, 100);
  assert.equal(h.run(`dashboardRealtimeTrendSamples.get('all').length`), 150, 'storage pruning must not change live state');
});

test('quota failure backs off before expensive serialization and reduces next attempt size', () => {
  const h = harness(); h.seed(11, 150); h.setFail(true);
  h.run(`persistDashboardRealtimeSamples();flushDashboardRealtimeSamples();`);
  const initialEncodes = h.encodes;
  for (let index = 0; index < 50; index++) h.run(`persistDashboardRealtimeSamples();flushDashboardRealtimeSamples();`);
  assert.equal(h.writes, 1);
  assert.equal(h.encodes, initialEncodes, 'failed persistence must not reserialize on every SSE tick');
  h.advance(60001); h.setFail(false);
  assert.equal(h.run(`flushDashboardRealtimeSamples()`), true);
  assert.equal(h.writes, 2);
  assert.ok(h.stored.length <= 512 * 1024);
});

test('clearing an epoch invalidates a scheduled save and old v1 storage remains readable', () => {
  const h = harness(); h.seed(1, 2);
  h.run(`persistDashboardRealtimeSamples();clearDashboardRealtimeSamplesStorage();`);
  assert.equal(h.timers.size, 0);
  assert.equal(h.run(`flushDashboardRealtimeSamples()`), false);
  assert.equal(h.writes, 0);
  h.setStored(JSON.stringify({ version: 1, all: [{ timestamp_ms: Date.now(), bytes_out: 8 }], sites: {} }));
  assert.equal(h.run('restoreDashboardRealtimeSamples()'), true);
  assert.equal(h.run(`dashboardRealtimeTrendSamples.get('all')[0].bytes_out`), 8);
});
