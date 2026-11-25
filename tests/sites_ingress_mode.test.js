'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const vm = require('node:vm');

function loadHelpers() {
  const source = loadSitesSource();
  const sandbox = { window: {}, URL, esc: value => String(value) };
  vm.createContext(sandbox);
  vm.runInContext(source, sandbox, { filename: 'sites.js' });
  return sandbox;
}

function loadSitesSource() {
  return fs.readFileSync(path.join(__dirname, '..', 'web', 'static', 'js', 'pages', 'sites.js'), 'utf8');
}

test('ingress form exposes the secure host-only mode without a listener', () => {
  const { ingressFormState } = loadHelpers();
  const host = ingressFormState('host');
  assert.equal(host.showPublicHost, true);
  assert.equal(host.requirePublicHost, true);
	assert.equal(host.requireListenPort, false);
	assert.match(host.portLabel, /可选/);
	assert.match(host.warning, /不会绑定/);
	assert.match(host.warning, /TLS .*\u8bc1\u4e66/);
	assert.equal(ingressFormState('port').requireListenPort, true);
	assert.equal(ingressFormState('path').requireListenPort, false);
	assert.equal(ingressFormState('path').requirePathPrefix, true);
	assert.match(ingressFormState('path').warning, /面板域名和端口/);
	assert.equal(ingressFormState('unset').requireListenPort, false);
	assert.match(ingressFormState('unset').warning, /请选择可用入口/);
});

test('ingress payload clears stale host for port mode and preserves it otherwise', () => {
  const { buildIngressPayload } = loadHelpers();
  assert.deepEqual(JSON.parse(JSON.stringify(buildIngressPayload('port', '8001', 'stale.example.com'))), {
    ingress_mode: 'port', listen_port: 8001, public_host: '', path_prefix: '',
  });
  assert.deepEqual(JSON.parse(JSON.stringify(buildIngressPayload('host', '8002', ' media.example.com '))), {
    ingress_mode: 'host', listen_port: 8002, public_host: 'media.example.com', path_prefix: '',
  });
	assert.deepEqual(JSON.parse(JSON.stringify(buildIngressPayload('host', '', ' media.example.com '))), {
		ingress_mode: 'host', listen_port: 0, public_host: 'media.example.com', path_prefix: '',
	});
  assert.deepEqual(JSON.parse(JSON.stringify(buildIngressPayload('both', '8003', 'media.example.com'))), {
    ingress_mode: 'both', listen_port: 8003, public_host: 'media.example.com', path_prefix: '',
  });
	assert.deepEqual(JSON.parse(JSON.stringify(buildIngressPayload('path', '', 'stale.example.com', '', '', ' Emby '))), {
		ingress_mode: 'path', listen_port: 0, public_host: '', path_prefix: 'Emby',
	});
});

test('new-site ingress defaults follow backend host-only capability', () => {
  const { defaultIngressMode } = loadHelpers();
  assert.equal(defaultIngressMode({ host_only_available: true, domain_prefix_available: true, panel_tls_enabled: true }), 'host');
  assert.equal(defaultIngressMode({ host_only_available: false }), 'port');
	assert.equal(defaultIngressMode({ host_only_available: true, domain_prefix_available: true, panel_tls_enabled: false }), 'port');
	assert.equal(defaultIngressMode({ host_only_available: true, domain_prefix_available: false, panel_tls_enabled: true }), 'port');
  assert.equal(defaultIngressMode(undefined), 'host');
});

test('ingress mode labels remain concise for the site card', () => {
  const { siteIngressModeLabel } = loadHelpers();
  assert.equal(siteIngressModeLabel({ ingress_mode: 'host' }), '域名前缀');
  assert.equal(siteIngressModeLabel({ ingress_mode: 'port' }), '独立端口');
	assert.equal(siteIngressModeLabel({ ingress_mode: 'path' }), '路径');
  assert.equal(siteIngressModeLabel({ ingress_mode: 'both' }), '域名前缀（兼容）');
  assert.equal(siteIngressModeLabel({ ingress_mode: 'unset' }), '入口未配置');
});

test('path access addresses use the current external origin behind a reverse proxy', () => {
  const sandbox = loadHelpers();
  const site = { ingress_mode: 'path', path_prefix: ' /emby/ ' };
  const capabilities = sandbox.normalizeSiteCapabilities({
    panel_tls_enabled: false,
    panel_access_url: 'http://internal.example.test:9090',
  });
  for (const origin of ['https://media.example.test', 'https://media.example.test:8443', 'https://[2001:db8::8]:8443']) {
    sandbox.window.location = new URL(`${origin}/#sites`);
    assert.equal(sandbox.siteAccessAddress(site, capabilities), `${origin}/emby/`);
  }
  assert.equal(sandbox.siteAccessAddress({ ...site, path_prefix: '' }, capabilities), '');
});

test('dedicated port addresses use the actual listener TLS state and bracket IPv6', () => {
  const sandbox = loadHelpers();
  const site = { ingress_mode: 'port', listen_port: 8096 };
  sandbox.window.location = new URL('https://panel.example.test/#sites');
  assert.equal(sandbox.siteAccessAddress(site, sandbox.normalizeSiteCapabilities({ panel_tls_enabled: false })), 'http://panel.example.test:8096');
  assert.equal(sandbox.siteAccessAddress(site, sandbox.normalizeSiteCapabilities({ panel_tls_enabled: true })), 'https://panel.example.test:8096');

  sandbox.window.location = { protocol: 'https:', hostname: '2001:db8::8', port: '8443', origin: 'https://[2001:db8::8]:8443' };
  assert.equal(sandbox.siteAccessAddress(site, sandbox.normalizeSiteCapabilities({ panel_tls_enabled: false })), 'http://[2001:db8::8]:8096');
  sandbox.window.location = new URL('http://[::1]:9090/#sites');
  assert.equal(sandbox.siteAccessAddress(site, sandbox.normalizeSiteCapabilities({ panel_tls_enabled: true })), 'https://[::1]:8096');
});

test('legacy ingress capabilities retain the browser protocol without changing domain mapping', () => {
  const sandbox = loadHelpers();
  sandbox.window.location = new URL('https://panel.example.test:8443/#sites');
  const legacy = sandbox.normalizeSiteCapabilities({});
  assert.equal(sandbox.siteAccessAddress({ ingress_mode: 'port', listen_port: 8096 }, legacy), 'https://panel.example.test:8096');
  for (const mode of ['host', 'both']) {
    assert.equal(sandbox.siteAccessAddress({ ingress_mode: mode, public_host: 'emby.example.test', listen_port: 8096 }, legacy), 'https://emby.example.test:8443');
    assert.equal(sandbox.siteAccessAddress({ ingress_mode: mode, public_host: 'emby.example.test', listen_port: 8096 }, sandbox.normalizeSiteCapabilities({ panel_tls_enabled: false })), 'https://emby.example.test:8443');
  }
  sandbox.window.location = new URL('http://panel.example.test:9090/#sites');
  assert.equal(sandbox.siteAccessAddress({ ingress_mode: 'port', listen_port: 8096 }, legacy), 'http://panel.example.test:8096');
});

for (const upstreamMode of ['fixed', 'path_target']) {
  for (const controllerPort of ['9090', '']) {
    test(`${upstreamMode} host access uses the applied Agent port after takeover from controller ${controllerPort || '443'}`, () => {
      const sandbox = loadHelpers();
      sandbox.window.location = new URL(`https://panel.example.test${controllerPort ? `:${controllerPort}` : ''}/#sites`);
      const capabilities = sandbox.normalizeSiteCapabilities({ panel_tls_enabled: true });
      const site = { ingress_mode: 'host', upstream_mode: upstreamMode, public_host: 'media.example.test',
        applied_node_id: 11, applied_node_port: 9443, controller_closed: true };
      assert.equal(sandbox.siteAccessAddress(site, capabilities), 'https://media.example.test:9443');
      assert.equal(sandbox.siteAccessAddress({ ...site, applied_node_port: Number(controllerPort || 443) }, capabilities), `https://media.example.test:${controllerPort || '443'}`);
      assert.equal(sandbox.siteAccessAddress({ ...site, controller_closed: false, desired_node_port: 9443 }, capabilities), `https://media.example.test:${controllerPort || '443'}`);
      assert.equal(sandbox.siteAccessAddress({ ...site, enabled: false }, capabilities), 'https://media.example.test:9443', 'pending cleanup retains the committed owner');
    });
  }
}

test('invalid or absent applied Agent entry waits without copying the controller address; dedicated listeners stay independent', () => {
  const sandbox = loadHelpers();
  sandbox.window.location = new URL('https://panel.example.test:9090/#sites');
  const capabilities = sandbox.normalizeSiteCapabilities({ panel_tls_enabled: true });
  for (const port of [undefined, 0, -1, 65536, 9443.5]) {
    assert.equal(sandbox.siteAccessAddress({ ingress_mode: 'host', public_host: 'media.example.test', controller_closed: true, applied_node_port: port }, capabilities), '');
  }
  assert.equal(sandbox.siteAccessAddress({ ingress_mode: 'port', listen_port: 8096, controller_closed: true, applied_node_port: 9443 }, capabilities), 'https://panel.example.test:8096');
});

test('takeover ownership and applied port changes invalidate the site row rendering signature', () => {
  const sandbox = loadHelpers();
  const site = { id: 1, name: 'scheduled', ingress_mode: 'host', public_host: 'media.example.test', controller_closed: false, applied_node_id: 0, applied_node_port: 0 };
  const signature = sandbox.siteCardsRenderSignature([site], new Set());
  for (const change of [{ controller_closed: true }, { applied_node_id: 11 }, { applied_node_port: 9443 }]) {
    assert.notEqual(sandbox.siteCardsRenderSignature([{ ...site, ...change }], new Set()), signature);
  }
});

test('same-count upstream header edits invalidate the site row rendering signature', () => {
  const sandbox = loadHelpers();
  const site = { id: 1, name: 'headers', upstream_headers: [{ name: 'X-Old', configured: true }] };
  const signature = sandbox.siteCardsRenderSignature([site], new Set());
  assert.notEqual(sandbox.siteCardsRenderSignature([{ ...site, upstream_headers: [{ name: 'X-New', configured: true }] }], new Set()), signature);
  assert.notEqual(sandbox.siteCardsRenderSignature([{ ...site, upstream_headers: [{ name: 'X-Old', configured: false }] }], new Set()), signature);
});

test('site rows retain ingress status, concealed access and secondary actions', () => {
  const sandbox = loadHelpers();
  sandbox.formatBytes = value => `${value} B`;
  sandbox.uaNameMap = { passthrough: '透传' };
  const row = sandbox.siteManagementRowHTML({
    id: 1, name: 'synthetic site', ingress_mode: 'host', public_host: 'media.example.com',
    target_url: 'https://origin.example.com', ua_mode: 'passthrough', enabled: true, running: true,
    traffic_used: 20, traffic_quota: 100, media_movie_count: 1, media_series_count: 2, media_episode_count: 3,
  }, new Set());
  assert.match(row, /域名前缀 · 透传/);
  assert.match(row, /运行中/);
  assert.match(row, /id="site-details-1" hidden/);
  assert.match(row, /data-access-address="[^" ]+"/);
  assert.match(row, /aria-label="显示访问地址" aria-pressed="false"/);
  for (const action of ['access', 'copy', 'latency', 'toggle', 'edit', 'delete']) {
    assert.match(row, new RegExp(`data-site-action="${action}"`));
  }
  assert.match(row, /data-site-row-toggle[^>]*aria-expanded="false"[^>]*aria-controls="site-details-1"/);
  assert.doesNotMatch(row, /data-site-action="details"/);
  assert.match(row, /媒体库数量/);
  assert.doesNotMatch(row, /播放回源/);
  const unset = sandbox.siteManagementRowHTML({ id: 2, name: 'unset', ingress_mode: 'unset', ua_mode: 'passthrough' }, new Set());
  assert.match(unset, /待配置/);
  assert.match(unset, /disabled title="请先编辑并配置入口"/);
});

test('site management shows current-cycle usage and quota progress while keeping lifetime usage in details', () => {
  const sandbox = loadHelpers();
  sandbox.formatBytes = value => `${value} B`;
  sandbox.uaNameMap = { passthrough: '透传' };
  const site = {
    id: 9, name: 'cycle usage', ingress_mode: 'unset', ua_mode: 'passthrough',
    traffic_used: 1000, monthly_traffic: 400, traffic_quota: 500,
  };
  const signature = sandbox.siteCardsRenderSignature([site], new Set());
  assert.notEqual(
    sandbox.siteCardsRenderSignature([{ ...site, monthly_traffic: 401 }], new Set()),
    signature,
    'a changed current-cycle total must repaint the site row',
  );

  vm.runInContext('siteManagementExpanded.add(9)', sandbox);
  const row = sandbox.siteManagementRowHTML(site, new Set());
  assert.match(row, /class="management-site-usage"><strong>400 B<\/strong>/);
  assert.match(row, /aria-valuenow="80"/);
  assert.match(row, /width:80\.00%/);
  assert.match(row, /累计流量<\/span><span class="console-mono">1000 B<\/span>/);
});

test('access address visibility uses closed and open eye icons with accessible state', () => {
  const sandbox = loadHelpers();
  assert.match(sandbox.renderSiteAccessVisibilityIcon(true), /site-access-eye-closed/);
  assert.match(sandbox.renderSiteAccessVisibilityIcon(false), /site-access-eye-open/);

  let hidden = true;
  const value = {
    dataset: { accessAddress: 'https://media.example.com:9090' },
    classList: { toggle: () => { hidden = !hidden; return hidden; } },
    textContent: '********',
  };
  const row = { querySelector: selector => selector === '[data-access-address]' ? value : null };
  const attributes = {};
  const button = {
    closest: () => row,
    innerHTML: '',
    setAttribute: (name, value) => { attributes[name] = value; },
  };

  sandbox.toggleSiteAccessAddress(button);
  assert.equal(value.textContent, 'https://media.example.com:9090');
  assert.match(button.innerHTML, /site-access-eye-open/);
  assert.equal(attributes['aria-label'], '隐藏访问地址');
  assert.equal(attributes['aria-pressed'], 'true');
  assert.equal(attributes.title, '隐藏访问地址');

  sandbox.toggleSiteAccessAddress(button);
  assert.equal(value.textContent, '********');
  assert.match(button.innerHTML, /site-access-eye-closed/);
  assert.equal(attributes['aria-label'], '显示访问地址');
  assert.equal(attributes['aria-pressed'], 'false');
  assert.equal(attributes.title, '显示访问地址');
});

test('site cards render media library counts and an accessible retention countdown', () => {
  const sandbox = loadHelpers();
  const counts = sandbox.renderSiteMediaLibraryCounts({
    media_movie_count: 19778,
    media_series_count: 28127,
    media_episode_count: 339044,
  });
  assert.match(counts, /电影 <strong>19,778<\/strong>/);
  assert.match(counts, /剧集 <strong>28,127<\/strong>/);
  assert.match(counts, /集 <strong>339,044<\/strong>/);
  assert.match(counts, /class="site-media-icon" aria-hidden="true"><svg/);
  assert.equal((counts.match(/class="site-media-icon"/g) || []).length, 1);
  assert.match(sandbox.renderSiteMediaLibraryCounts({}), /媒体库<\/span><strong>待同步/);
  assert.match(sandbox.renderSiteMediaLibraryCounts({}), /<\/svg><\/span><span>媒体库/);

  const now = Date.UTC(2026, 7, 24, 0, 0, 0);
  const status = sandbox.siteAccountRetentionStatus({
    account_retention_days: 30,
    account_retention_started_at_ms: now - 24 * 86400000,
  }, now);
  assert.deepEqual(JSON.parse(JSON.stringify(status)), {
    enabled: true,
    remainingDays: 6,
    urgent: true,
    overdue: false,
  });
  const retention = sandbox.renderSiteAccountRetention({
    account_retention_days: 30,
    account_retention_started_at_ms: Date.now() - 24 * 86400000,
  });
  assert.match(retention, /site-retention-row is-urgent/);
  assert.match(retention, /剩余 6 天/);
  assert.doesNotMatch(retention, /合格观看后自动重置|<small/);
});

test('switching a fixed upstream to generic proxy hides previous media counts and pending totals', () => {
  const sandbox = loadHelpers();
  sandbox.formatBytes = value => `${value} B`;
  sandbox.uaNameMap = { passthrough: '透传' };
  const site = { id: 3, name: '通用站点', upstream_mode: 'fixed', ingress_mode: 'port', listen_port: 8096,
    ua_mode: 'passthrough', media_movie_count: 15019, media_series_count: 120, media_episode_count: 840 };
  const fixedSignature = sandbox.siteCardsRenderSignature([site], new Set());
  const fixedRow = sandbox.siteManagementRowHTML(site, new Set());
  assert.match(fixedRow, /电影 <strong>15,019<\/strong>/);
  assert.match(fixedRow, /剧集 <strong>120<\/strong>/);
  assert.match(fixedRow, /集 <strong>840<\/strong>/);

  site.upstream_mode = 'path_target';
  assert.notEqual(sandbox.siteCardsRenderSignature([site], new Set()), fixedSignature, 'changing upstream mode must repaint the row');
  assert.equal(sandbox.renderSiteMediaLibraryCounts(site), '');
  assert.equal(sandbox.renderSiteMediaLibraryCounts({ upstream_mode: 'path_target' }), '', 'generic proxy has no pending media-library sync');
  const genericRow = sandbox.siteManagementRowHTML(site, new Set());
  assert.match(genericRow, /回源方式<\/span><span class="console-mono">通用反代/);
  assert.doesNotMatch(genericRow, /site-media-counts|媒体库数量|待同步|15,019/);
  assert.match(genericRow, /data-site-action="access"/);

  site.upstream_mode = 'fixed';
  assert.equal(sandbox.siteCardsRenderSignature([site], new Set()), fixedSignature);
  assert.equal(sandbox.siteManagementRowHTML(site, new Set()), fixedRow, 'switching back must retain all fixed-upstream counts');
});

test('site card actions stay bottom-aligned without fixed card heights', () => {
  const css = fs.readFileSync(path.join(__dirname, '..', 'web', 'static', 'css', 'style.css'), 'utf8');
  const cardRule = css.match(/\.site-card\s*\{([^}]*)\}/)?.[1] || '';
  const actionsRule = css.match(/\.site-actions\s*\{([^}]*)\}/)?.[1] || '';

  assert.match(cardRule, /display:\s*flex/);
  assert.match(cardRule, /flex-direction:\s*column/);
  assert.doesNotMatch(cardRule, /(?:min-)?height\s*:/);
  assert.match(actionsRule, /margin-top:\s*auto/);
});

test('site management uses responsive rows with touch-capable reorder controls', () => {
  const css = fs.readFileSync(path.join(__dirname, '..', 'web', 'static', 'css', 'management-console.css'), 'utf8');
  const source = loadSitesSource();
  assert.match(source, /site-card management-site-row/);
  assert.match(source, /management-site-identity/);
  assert.match(source, /data-site-drag-handle/);
  assert.match(css, /management-site-list\.sites-grid[^}]*display:\s*block/s);
  assert.match(css, /\.site-drag-handle[^}]*touch-action:\s*none/s);
  assert.match(css, /@media \(max-width: 768px\)/);
  assert.doesNotMatch(css, /(?:height|max-height):\s*100(?:vh|dvh)/);
});

test('site management exposes touch reorder targets before the first input event', () => {
  for (const { maxTouchPoints, coarse, touch } of [
    { maxTouchPoints: 0, coarse: false, touch: false },
    { maxTouchPoints: 5, coarse: false, touch: true },
    { maxTouchPoints: 0, coarse: true, touch: true },
  ]) {
    const sandbox = loadHelpers();
    const elements = new Map();
    const element = id => {
      if (!elements.has(id)) elements.set(id, { innerHTML: '', addEventListener() {} });
      return elements.get(id);
    };
    sandbox.document = { getElementById: element };
    sandbox.navigator = { maxTouchPoints };
    sandbox.window.matchMedia = query => ({ matches: query === '(any-pointer: coarse)' && coarse });
    sandbox.loadSites = async () => {};
    sandbox.renderSites();
    const page = element('page-sites').innerHTML;
    assert.equal(/class="console-page management-page[^\"]*\bis-touch-capable\b/.test(page), touch);
    assert.match(page, /class="sites-grid management-site-list"/);
  }
});

test('site identity icons remain supported in site cards and console rows', () => {
  const dashboard = fs.readFileSync(path.join(__dirname, '..', 'web', 'static', 'js', 'pages', 'dashboard.js'), 'utf8');
  const sites = loadSitesSource();
  const css = fs.readFileSync(path.join(__dirname, '..', 'web', 'static', 'css', 'style.css'), 'utf8');

  assert.match(sites, /renderSiteIconButton\(s\)/);
  assert.match(dashboard, /dashboardSiteIconMarkup\(s\)/);
  assert.match(dashboard, /dashboard-site-identity/);
  assert.match(css, /\.site-icon\s*\{[\s\S]*?border-radius:\s*50%;[\s\S]*?background:\s*transparent;/);
  assert.match(css, /\.site-icon-button\s*\{[\s\S]*?width:\s*60px;[\s\S]*?background:\s*transparent;/);
  assert.match(dashboard, /bindDashboardSiteIconFallbacks\(tbody\)/);
});

test('site configuration groups origins, ingress and advanced policies in the console layout', () => {
  const source = loadSitesSource();
  const css = fs.readFileSync(path.join(__dirname, '..', 'web', 'static', 'css', 'management-console.css'), 'utf8');
  const policyColumn = source.indexOf('site-form-column-policy');
  const secondaryColumn = source.indexOf('site-form-column-secondary');
  const cacheColumn = source.indexOf('site-form-column-cache');
  const limitsColumn = source.indexOf('site-form-column-limits');
  const cacheToggle = source.indexOf('id="m-asset-cache"');
  const cacheRules = source.indexOf('id="m-cache-rules"');
  const cacheLimits = source.indexOf('id="m-cache-ttl"');
  const retention = source.indexOf('id="m-account-retention-days"');
  const quota = source.indexOf('id="m-quota"');
  const speed = source.indexOf('id="m-speed"');

  assert.ok(policyColumn < secondaryColumn && secondaryColumn < cacheColumn && cacheColumn < limitsColumn);
  assert.ok(cacheColumn < cacheToggle && cacheToggle < cacheLimits && cacheLimits < cacheRules && cacheRules < limitsColumn);
  assert.ok(limitsColumn < quota && quota < speed && speed < retention);
  for (const section of ['site-section-basics', 'site-section-origins', 'site-section-ingress']) {
    assert.match(source, new RegExp(`aria-labelledby="${section}"`));
  }
  assert.match(css, /\.site-config-fields\s*\{[^}]*grid-template-columns:\s*repeat\(2,/);
  assert.match(css, /\.site-form-columns\s*\{[^}]*grid-template-columns:\s*minmax\(0, 1fr\)/);
  assert.match(css, /@media \(max-width: 768px\)[\s\S]*?\.site-config-fields\s*\{[^}]*grid-template-columns:\s*minmax\(0, 1fr\)/);
});

test('access addresses use the full card width without ellipsis wrapping', () => {
  const css = fs.readFileSync(path.join(__dirname, '..', 'web', 'static', 'css', 'style.css'), 'utf8');
  assert.match(css, /\.site-row\.site-access-row > \.site-access-value[\s\S]*?width:\s*100%/);
  assert.match(css, /\.site-row\.site-access-row \.site-access-address[\s\S]*?overflow-x:\s*auto/);
  assert.match(css, /\.site-row\.site-access-row \.site-access-address[\s\S]*?text-overflow:\s*clip/);
  assert.match(css, /\.site-row\.site-access-row \.site-access-address[\s\S]*?white-space:\s*nowrap/);
  assert.match(css, /\.site-row\.site-access-row \.site-access-address[\s\S]*?font-family:\s*inherit/);
  assert.match(css, /\.site-row\.site-access-row \.site-access-address[\s\S]*?border-radius:\s*999px/);
	assert.match(css, /\.site-access-toggle svg,\s*\.site-access-copy svg\s*\{[\s\S]*?width:\s*14px[\s\S]*?stroke:\s*currentColor/);
	assert.match(css, /\.site-access-toggle,\s*\.site-access-copy\s*\{[\s\S]*?-webkit-tap-highlight-color:\s*transparent[\s\S]*?touch-action:\s*manipulation/);
	assert.match(css, /\.site-access-toggle:active,\s*\.site-access-copy:active\s*\{[\s\S]*?background:\s*var\(--surface-hover\)/);
});

test('copySiteAccessAddress copies the raw address even while it is hidden', async () => {
  const sandbox = loadHelpers();
  let copied = '';
  let success = '';
  sandbox.navigator = { clipboard: { writeText: async value => { copied = value; } } };
  sandbox.Toast = { success: value => { success = value; }, error: () => {} };
  const value = { dataset: { accessAddress: 'https://123.divine.de5.net:9090' } };
  const row = { querySelector: selector => selector === '[data-access-address]' ? value : null };
  const button = { closest: () => row };

  await sandbox.copySiteAccessAddress(button);

  assert.equal(copied, 'https://123.divine.de5.net:9090');
  assert.equal(success, '访问地址已复制');
});

test('target authority comparison ignores path and explicit default ports', () => {
  const { normalizedTargetAuthority } = loadHelpers();
	assert.equal(normalizedTargetAuthority('https://origin.example.com/emby'), 'https://origin.example.com:443');
	assert.equal(normalizedTargetAuthority('https://origin.example.com:443/other'), 'https://origin.example.com:443');
	assert.equal(normalizedTargetAuthority('origin.example.com:443/other'), 'https://origin.example.com:443');
  assert.notEqual(normalizedTargetAuthority('https://origin.example.com'), normalizedTargetAuthority('https://other.example.com'));
});

test('site modal always loads deployment capabilities for create and edit flows', () => {
  const source = loadSitesSource();
  const start = source.indexOf('async function showSiteModal(site)');
  const end = source.indexOf('// Global actions', start);
  const modalSource = source.slice(start, end);

  assert.match(modalSource, /normalizeSiteCapabilities\(await API\.ingressCapabilities\(\)\)/);
  assert.doesNotMatch(modalSource, /if \(!isEdit\)[\s\S]{0,200}ingressCapabilities/);
	assert.doesNotMatch(modalSource, /id="m-port"[^>]*\srequired(?:\s|>)/);
	assert.match(modalSource, /portInput\.required = state\.requireListenPort/);
	assert.match(modalSource, /panelTLSReady = siteCapabilities\.panel_tls_enabled === true/);
	assert.match(modalSource, /PANEL_ROUTE_DOMAIN/);
	assert.match(modalSource, /TLS .*\u8bc1\u4e66/);
});

test('stream host normalization accepts the array API and legacy JSON strings', () => {
  const { normalizeStreamHosts } = loadHelpers();
  assert.deepEqual(JSON.parse(JSON.stringify(normalizeStreamHosts([' one.example ', '', 42, 'two.example']))), [
    'one.example',
    'two.example',
  ]);
  assert.deepEqual(JSON.parse(JSON.stringify(normalizeStreamHosts('[" legacy-one.example ","legacy-two.example"]'))), [
    'legacy-one.example',
    'legacy-two.example',
  ]);
  assert.deepEqual(JSON.parse(JSON.stringify(normalizeStreamHosts('{'))), []);

});

test('playback limit follows backend capabilities and has a safe compatibility default', () => {
  const { normalizeSiteCapabilities, canAddPlaybackAddress } = loadHelpers();
  const configured = normalizeSiteCapabilities({
    host_only_available: false,
    upstream_headers_available: false,
    max_playback_addresses: 100,
  });
  assert.deepEqual(JSON.parse(JSON.stringify(configured)), {
    host_only_available: false,
    upstream_headers_available: false,
    max_playback_addresses: 100,
  });
  assert.equal(canAddPlaybackAddress(99, configured.max_playback_addresses), true);
  assert.equal(canAddPlaybackAddress(100, configured.max_playback_addresses), false);

  const fallback = normalizeSiteCapabilities({});
  assert.equal(fallback.host_only_available, true);
  assert.equal(fallback.upstream_headers_available, true);
  assert.equal(fallback.max_playback_addresses, 128);
});

test('missing upstream header key disables edits but leaves deletion available', () => {
  const { renderUpstreamHeaderRows } = loadHelpers();
  const disabled = renderUpstreamHeaderRows([
    { name: 'X-Origin-Secret', configured: true },
  ], false);
  assert.equal((disabled.match(/ disabled/g) || []).length, 2, 'name and value inputs must be disabled');
  const removeButton = disabled.match(/<button[^>]*m-upstream-header-remove[^>]*>/)?.[0] || '';
  assert.ok(removeButton, 'configured row must retain a delete control');
  assert.ok(!removeButton.includes('disabled'), 'delete control must remain enabled');

  const enabled = renderUpstreamHeaderRows([
    { name: 'X-Origin-Secret', configured: true },
  ], true);
  assert.ok(!enabled.includes(' disabled'), 'configured key must keep inputs editable');
});
