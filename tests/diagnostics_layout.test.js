'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const vm = require('node:vm');

const script = fs.readFileSync(path.join(__dirname, '../web/static/js/pages/diag.js'), 'utf8');
const shared = { window: {} };
vm.createContext(shared);
vm.runInContext(fs.readFileSync(path.join(__dirname, '../web/static/js/api.js'), 'utf8'), shared);

function upstream(url, changes = {}) {
  return {
    configured: true, configured_url: url, effective_url: url,
    using_fallback: false, same_as_primary: false, show_health: true, show_tls: true,
    health: {
      status: 'online', emby_version: '4.9.0.30', latency_ms: 84,
      probe: { kind: 'metadata_api', method: 'GET', url: `${url}/System/Info/Public`, http_status: 200 },
    },
    tls: { enabled: true, valid: true, issuer: 'Example CA', expires_at: '2027-02-20', days_left: 91 },
    ...changes,
  };
}

function resultFixture(changes = {}) {
  const primary = upstream('https://primary.example/emby');
  return {
    upstreams: {
      primary,
      playback: { ...primary, configured: false, configured_url: '', using_fallback: true, same_as_primary: true, show_health: false, show_tls: false },
      failovers: [],
    },
    health: primary.health, tls: primary.tls,
    headers: { passthrough: false, ua_applied: true, current_ua: 'Example Client/1.0', client_field: 'Example Client', version_field: '1.0' },
    proxy: { running: true, ingress_mode: 'both', public_host: 'media.example', port_listening: true, listen_port: 8096, total_requests: 1234 },
    ...changes,
  };
}

function renderer() {
  const sandbox = { esc: shared.esc };
  vm.createContext(sandbox);
  vm.runInContext(script, sandbox);
  return sandbox;
}

// Match the complete section so assertions verify which upstream owns a value,
// including its nested health and TLS sections.
function sectionContaining(html, attribute) {
  let start = -1, depth = 0;
  for (const token of html.matchAll(/<section\b[^>]*>|<\/section>/g)) {
    if (start < 0) {
      if (token[0].startsWith('<section') && token[0].includes(attribute)) {
        start = token.index;
        depth = 1;
      }
    } else if (token[0].startsWith('</')) {
      if (--depth === 0) return html.slice(start, token.index + token[0].length);
    } else {
      depth++;
    }
  }
  return '';
}

function deferred() {
  let resolve, reject;
  const promise = new Promise((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}

function pageHarness() {
  const fields = new Map(), calls = [], pending = [], errors = [];
  const page = {
    set innerHTML(html) {
      this.html = html;
      fields.forEach(field => { field.isConnected = false; });
      fields.clear();
      const classes = new Set();
      fields.set('btn-scan', {
        disabled: true, textContent: '开始诊断', isConnected: true,
        classList: { add: value => classes.add(value), remove: value => classes.delete(value), contains: value => classes.has(value) },
      });
      fields.set('diag-select', {
        disabled: false, isConnected: true, value: '',
        set innerHTML(value) { this.html = value; this.value = value.match(/<option value="([^"]*)"/)?.[1] || ''; },
      });
      fields.set('diag-grid', {
        innerHTML: '选择一个站点，查看连接与代理诊断结果。', isConnected: true,
        attributes: new Map([['aria-busy', 'false']]),
        setAttribute(name, value) { this.attributes.set(name, value); },
      });
    },
  };
  const sandbox = {
    esc: shared.esc,
    document: { getElementById: id => id === 'page-diagnostics' ? page : fields.get(id) },
    Router: { current: 'diagnostics' },
    settingsPageFrame: (active, title, description, content) => content,
    bindGlobalSettingsNav() {},
    Toast: { error: message => errors.push(message) },
    API: {
      listSites: async () => [{ id: 101, name: 'Example primary' }, { id: 102, name: 'Example secondary' }],
      diagSite(siteID) { calls.push(siteID); const request = deferred(); pending.push(request); return request.promise; },
    },
  };
  vm.createContext(sandbox);
  vm.runInContext(script, sandbox);
  return {
    sandbox, calls, pending, errors,
    field: id => fields.get(id),
    async open() { sandbox.Router.current = 'diagnostics'; sandbox.renderDiag(); await new Promise(resolve => setImmediate(resolve)); },
  };
}

test('main-only diagnostics keep the target, latency, probe and TLS in one upstream group', () => {
  const html = renderer().renderDiagResult(resultFixture());
  const primary = sectionContaining(html, 'data-diag-upstream="primary"');
  assert.equal((html.match(/data-diag-upstream=/g) || []).length, 1);
  assert.match(primary, /https:\/\/primary\.example\/emby/);
  assert.match(primary, /84 ms/);
  assert.match(primary, /4\.9\.0\.30/);
  assert.match(primary, /Example CA/);
  assert.match(primary, /2027-02-20 \(91 天\)/);
  assert.match(primary, /Metadata \/ API 探针/);
  assert.match(primary, /GET https:\/\/primary\.example\/emby\/System\/Info\/Public/);
  assert.match(primary, /播放回源未单独配置/);
  assert.ok(html.indexOf('代理与请求头') > html.indexOf('data-diag-upstream="primary"'));
  assert.ok(html.indexOf('诊断结果说明') > html.indexOf('代理与请求头'));
});

test('independent playback owns its own target, latency and TLS instead of mixing with the primary', () => {
  const fixture = resultFixture();
  fixture.upstreams.playback = upstream('https://playback.example/stream', {
    health: { status: 'error', emby_version: '', latency_ms: 312, probe: { kind: 'reachability_fallback', method: 'GET', url: 'https://playback.example/stream', http_status: 404 } },
    tls: { enabled: true, valid: false, issuer: 'Playback CA', expires_at: '2026-11-18', days_left: -3, error: 'certificate expired' },
  });
  const html = renderer().renderDiagResult(fixture);
  const primary = sectionContaining(html, 'data-diag-upstream="primary"');
  const playback = sectionContaining(html, 'data-diag-upstream="playback"');
  assert.equal((html.match(/data-diag-upstream=/g) || []).length, 2);
  assert.match(playback, /独立播放回源/);
  assert.match(playback, /https:\/\/playback\.example\/stream/);
  assert.match(playback, /312 ms/);
  assert.match(playback, /Playback CA/);
  assert.match(playback, /certificate expired/);
  assert.match(playback, /探针异常/);
  assert.match(playback, /可达性回退探针/);
  assert.doesNotMatch(primary, /playback\.example|312 ms|Playback CA/);
  assert.doesNotMatch(playback, /primary\.example|84 ms|Example CA/);
});

test('independent HTTP playback retains its health without inventing a TLS result', () => {
  const fixture = resultFixture();
  fixture.upstreams.playback = upstream('http://playback.example:8096', { show_tls: false, tls: { enabled: false, valid: false } });
  const playback = sectionContaining(renderer().renderDiagResult(fixture), 'data-diag-upstream="playback"');
  assert.match(playback, /http:\/\/playback\.example:8096/);
  assert.match(playback, /健康与延迟/);
  assert.doesNotMatch(playback, /TLS 证书|证书有效/);
});

test('same-target playback is explained without duplicating a diagnostic group', () => {
  const fixture = resultFixture();
  fixture.upstreams.playback.configured = true;
  fixture.upstreams.playback.using_fallback = false;
  fixture.upstreams.primary.health.probe.kind = 'reachability_fallback';
  const html = renderer().renderDiagResult(fixture);
  assert.equal((html.match(/data-diag-upstream=/g) || []).length, 1);
  assert.match(html, /播放回源与主回源相同/);
  assert.match(html, /主回源元数据接口未命中/);
});

test('multiple backup lines preserve configured order and expose long targets, errors and TLS state', () => {
  const fixture = resultFixture();
  const longTarget = `https://backup-one.example/${'very-long-segment'.repeat(45)}`;
  fixture.upstreams.failovers = [
    upstream(longTarget),
    upstream('http://backup-two.example:8096', { health: { status: 'offline', latency_ms: 5000, error: 'dial tcp: connection timed out' }, tls: { enabled: false, valid: false } }),
    upstream('https://backup-three.example', { tls: { enabled: true, valid: false, error: 'x509: expired certificate' } }),
  ];
  const backup = sectionContaining(renderer().renderDiagResult(fixture), 'diag-backup-report');
  assert.ok(backup.indexOf(longTarget) < backup.indexOf('http://backup-two.example:8096'));
  assert.ok(backup.indexOf('http://backup-two.example:8096') < backup.indexOf('https://backup-three.example'));
  assert.match(backup, /3 条线路/);
  assert.match(backup, /备用 1/);
  assert.match(backup, /备用 2/);
  assert.match(backup, /备用 3/);
  assert.match(backup, /离线/);
  assert.match(backup, /5000 ms/);
  assert.match(backup, /connection timed out/);
  assert.match(backup, /未使用 HTTPS/);
  assert.match(backup, /证书异常/);
  assert.match(backup, /x509: expired certificate/);
});

test('diagnostic result rendering escapes upstream, probe, certificate, backup and identity data', () => {
  const payload = '"><img src=x onerror=alert(1)>';
  const malicious = upstream(payload, {
    health: { status: 'error', emby_version: payload, latency_ms: 42, error: payload, probe: { kind: payload, method: payload, url: payload, http_status: 502 } },
    tls: { enabled: true, valid: false, issuer: payload, expires_at: payload, error: payload },
  });
  const html = renderer().renderDiagResult(resultFixture({
    upstreams: { primary: malicious, playback: { ...malicious, show_health: true, show_tls: true }, failovers: [malicious] },
    headers: { current_ua: payload, client_field: payload, version_field: payload, profile_error: payload },
    proxy: { ingress_mode: payload, public_host: payload, path_prefix: payload, listen_port: payload },
  }));
  assert.doesNotMatch(html, /<img\b/);
  assert.ok(!html.includes(payload));
  assert.ok(html.includes(shared.esc(payload)));
  assert.match(html, /身份配置/);
  assert.match(html, /TLS 结果/);
});

test('a running scan keeps the selected site stable and makes one diagnostic request', async () => {
  const h = pageHarness();
  await h.open();
  const scan = h.sandbox.runDiag();
  assert.deepEqual(h.calls, ['101']);
  assert.equal(h.field('btn-scan').disabled, true);
  assert.equal(h.field('diag-select').disabled, true);
  assert.equal(h.field('diag-grid').attributes.get('aria-busy'), 'true');
  assert.match(h.field('diag-grid').innerHTML, /role="status"/);
  await h.sandbox.runDiag();
  assert.equal(h.calls.length, 1);
  h.pending[0].resolve(resultFixture());
  await scan;
  assert.equal(h.field('btn-scan').disabled, false);
  assert.equal(h.field('diag-select').disabled, false);
  assert.equal(h.field('diag-grid').attributes.get('aria-busy'), 'false');
  assert.match(h.field('diag-grid').innerHTML, /data-diag-upstream="primary"/);
  h.field('diag-select').value = '102';
  h.field('diag-select').onchange();
  assert.match(h.field('diag-grid').innerHTML, /站点已切换/);
  assert.doesNotMatch(h.field('diag-grid').innerHTML, /primary\.example/);
});

for (const outcome of ['success', 'failure']) {
  test(`an old scan ${outcome} cannot replace a new page's results or unlock its active scan`, async () => {
    const h = pageHarness();
    await h.open();
    const oldScan = h.sandbox.runDiag();
    h.sandbox.Router.current = 'settings';
    await h.open();
    h.field('diag-select').value = '102';
    const newScan = h.sandbox.runDiag();
    const currentGrid = h.field('diag-grid');
    const loading = currentGrid.innerHTML;
    if (outcome === 'success') h.pending[0].resolve(resultFixture());
    else h.pending[0].reject(new Error('old site failed'));
    await oldScan;
    assert.equal(currentGrid.innerHTML, loading);
    assert.equal(h.field('btn-scan').disabled, true);
    assert.equal(h.field('diag-select').disabled, true);
    assert.equal(currentGrid.attributes.get('aria-busy'), 'true');
    assert.deepEqual(h.errors, []);
    const current = resultFixture();
    current.upstreams.primary = upstream('https://current.example');
    h.pending[1].resolve(current);
    await newScan;
    assert.match(currentGrid.innerHTML, /https:\/\/current\.example/);
    assert.equal(h.field('btn-scan').disabled, false);
    assert.equal(currentGrid.attributes.get('aria-busy'), 'false');
  });
}

test('a current request error remains escaped and leaves diagnostics available to retry', async () => {
  const h = pageHarness();
  await h.open();
  const scan = h.sandbox.runDiag();
  h.pending[0].reject(new Error('<script>failed</script>'));
  await scan;
  assert.match(h.field('diag-grid').innerHTML, /role="alert"/);
  assert.match(h.field('diag-grid').innerHTML, /&lt;script&gt;failed&lt;\/script&gt;/);
  assert.doesNotMatch(h.field('diag-grid').innerHTML, /<script>/);
  assert.equal(h.field('btn-scan').disabled, false);
  assert.equal(h.field('diag-select').disabled, false);
  assert.equal(h.field('diag-grid').attributes.get('aria-busy'), 'false');
});
