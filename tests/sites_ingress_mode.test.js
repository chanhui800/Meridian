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
  for (const action of ['access', 'copy', 'latency', 'toggle', 'edit', 'delete', 'details']) {
    assert.match(row, new RegExp(`data-site-action="${action}"`));
  }
  assert.match(row, /媒体库数量/);
  assert.doesNotMatch(row, /播放回源/);
  const unset = sandbox.siteManagementRowHTML({ id: 2, name: 'unset', ingress_mode: 'unset', ua_mode: 'passthrough' }, new Set());
  assert.match(unset, /待配置/);
  assert.match(unset, /disabled title="请先编辑并配置入口"/);
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
