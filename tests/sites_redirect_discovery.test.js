'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const vm = require('node:vm');

const STATIC_JS = path.join(__dirname, '..', 'web', 'static', 'js');
const ATTACK = '\"><img src=x onerror=alert(1)>';

function loadScripts(...relativePaths) {
  const sandbox = { window: {}, URL };
  vm.createContext(sandbox);
  for (const relativePath of relativePaths) {
    vm.runInContext(
      fs.readFileSync(path.join(STATIC_JS, relativePath), 'utf8'),
      sandbox,
      { filename: relativePath },
    );
  }
  return sandbox;
}

function plain(value) {
  return JSON.parse(JSON.stringify(value));
}

function clone(value) {
  return JSON.parse(JSON.stringify(value));
}

function discoveryProfile(id, overrides = {}) {
  const values = {
    safe: [3, 256, 4096, 256, 4 * 1024 * 1024, 16, 60, 32, 30 * 60, 8 * 60 * 60],
    compatible: [5, 1024, 16384, 1024, 16 * 1024 * 1024, 32, 300, 128, 2 * 60 * 60, 24 * 60 * 60],
    extreme: [10, 4096, 65536, 4096, 64 * 1024 * 1024, 64, 1200, 512, 24 * 60 * 60, 7 * 24 * 60 * 60],
  }[id];
  const anyPort = id !== 'safe';
  return {
    id,
    label: id[0].toUpperCase() + id.slice(1),
    recommended: id === 'compatible',
    limits: {
      allowed_schemes: anyPort ? ['http', 'https'] : ['https'],
      allowed_ports: anyPort ? [] : [443],
      allow_any_port: anyPort,
      max_redirects: values[0],
      max_authorities: values[1],
      max_active_capabilities: values[2],
      max_urls_per_response: values[3],
      max_body_bytes: values[4],
      max_dns_ips: values[5],
      max_new_authorities_per_minute: values[6],
      max_streams: values[7],
      idle_expiry_seconds: values[8],
      absolute_lifetime_seconds: values[9],
    },
    features: {
      redirect_discovery: true,
      playback_info: true,
      hls: id !== 'safe',
      dash: id !== 'safe',
      private_targets: false,
      custom_ca: false,
      raw_fallback: false,
    },
    ...overrides,
  };
}

function structuredDiscoveryResponse(overrides = {}) {
  return {
    stage: 'structured-discovery',
    available: true,
    key_configured: true,
    profiles: [
      discoveryProfile('safe'),
      discoveryProfile('compatible'),
      discoveryProfile('extreme'),
    ],
    global_limits: {
      max_authorities: 16384,
      max_active_capabilities: 131072,
      max_streams: 1024,
      max_new_authorities_per_minute: 2400,
      max_dns_workers: 32,
      max_concurrent_parses: 8,
      max_site_concurrent_parses: 2,
      max_parse_memory_bytes: 256 * 1024 * 1024,
      max_site_parse_memory_bytes: 64 * 1024 * 1024,
      max_capability_memory_bytes: 256 * 1024 * 1024,
      max_site_capability_memory_bytes: 64 * 1024 * 1024,
      max_parse_depth: 64,
      max_string_bytes: 1024 * 1024,
      max_target_url_bytes: 4096,
    },
    ...overrides,
  };
}

class FakeElement {
  constructor(ownerDocument, id = '') {
    this.ownerDocument = ownerDocument;
    this.id = id;
    this._innerHTML = '';
    this._children = [];
    this.classNames = new Set();
    this.dataset = {};
    this.style = {};
    this.value = '';
    this.textContent = '';
    this.checked = false;
    this.disabled = false;
    this.hidden = false;
    this.required = false;
    this.title = '';
  }

  set innerHTML(value) {
    this._innerHTML = String(value);
    this._children = this.ownerDocument.parseElements(this._innerHTML);
  }

  get innerHTML() {
    return this._innerHTML;
  }

  querySelectorAll(selector) {
    if (!selector.startsWith('.')) return [];
    const className = selector.slice(1);
    return this._children.filter(element => element.classNames.has(className));
  }

  addEventListener(type, handler) {
    this[`on${type}`] = handler;
  }

  focus() {}
}

class FakeDocument {
  constructor() {
    this.elements = new Map();
    for (const id of ['modal-title', 'modal-body', 'modal-footer']) {
      this.elements.set(id, new FakeElement(this, id));
    }
  }

  getElementById(id) {
    return this.elements.get(id) || null;
  }

  parseElements(html) {
    const elements = [];
    const tags = /<([a-z][a-z0-9-]*)\b([^>]*)>/gi;
    let tag;
    while ((tag = tags.exec(html)) !== null) {
      const attributes = tag[2];
      const id = /\bid="([^"]+)"/.exec(attributes)?.[1] || '';
      const element = id && this.elements.has(id)
        ? this.elements.get(id)
        : new FakeElement(this, id);
      if (id) this.elements.set(id, element);

      const classValue = /\bclass="([^"]*)"/.exec(attributes)?.[1] || '';
      element.classNames = new Set(classValue.split(/\s+/).filter(Boolean));
      element.value = /\bvalue="([^"]*)"/.exec(attributes)?.[1] || '';
      element.checked = /(?:^|\s)checked(?:\s|$)/.test(attributes);
      element.disabled = /(?:^|\s)disabled(?:\s|$)/.test(attributes);
      for (const match of attributes.matchAll(/\bdata-([a-z0-9-]+)="([^"]*)"/gi)) {
        const key = match[1].replace(/-([a-z])/g, (_, character) => character.toUpperCase());
        element.dataset[key] = match[2];
      }
      elements.push(element);
    }
    return elements;
  }
}

function loadModalHarness() {
  const document = new FakeDocument();
  const state = {
    confirmationResult: false,
    confirmations: [],
    observationGets: [],
    observationDeletes: [],
    creates: [],
    updates: [],
    successes: [],
    errors: [],
    opened: 0,
  };
  const sandbox = {
    document,
    window: {
      confirm(message) {
        state.confirmations.push(message);
        return state.confirmationResult;
      },
    },
    URL,
    API: {
      ingressCapabilities: async () => ({
        host_only_available: true,
        upstream_headers_available: true,
        max_playback_addresses: 128,
      }),
      getDynamicProfiles: async () => structuredDiscoveryResponse(),
      getDynamicObservations: async siteId => {
        state.observationGets.push(siteId);
        return {
          observations: [{
            canonical_authority: 'https://media.example:443',
            source: 'redirect',
            decision: 'allowed',
            reason_code: 'redirect_allowed',
            first_seen_ms: 0,
            last_seen_ms: 1,
            count: 2,
          }],
          dropped_observations: 3,
        };
      },
      deleteDynamicObservations: async siteId => {
        state.observationDeletes.push(siteId);
        return { observations: [], dropped_observations: 0 };
      },
      createSite: async payload => { state.creates.push(clone(payload)); },
      updateSite: async (siteId, payload) => { state.updates.push({ siteId, payload: clone(payload) }); },
    },
    Toast: {
      success(message) { state.successes.push(message); },
      error(message) { state.errors.push(message); },
    },
    esc(value) {
      return String(value).replace(/[&<>"']/g, character => ({
        '&': '&amp;',
        '<': '&lt;',
        '>': '&gt;',
        '"': '&quot;',
        "'": '&#39;',
      })[character]);
    },
    openModal() { state.opened++; },
    closeModal() {},
    loadSites() {},
    formatBytes(value) { return String(value); },
    uaClassMap: {},
    uaNameMap: {},
  };
  vm.createContext(sandbox);
  vm.runInContext(
    fs.readFileSync(path.join(STATIC_JS, 'pages', 'sites.js'), 'utf8'),
    sandbox,
    { filename: 'pages/sites.js' },
  );
  sandbox.loadSites = () => {};
  return { sandbox, document, state };
}

test('site modal presents proxy and direct main-video choices without discovery or security controls', async () => {
  const { sandbox, document } = loadModalHarness();
  await sandbox.showSiteModal(null);
  const body = document.getElementById('modal-body').innerHTML;

  assert.match(body, /主视频流策略/);
  assert.match(body, /反代/);
  assert.match(body, /直连/);
  assert.match(body, /网盘或 CDN 的 302/);
  assert.match(body, /面板、API、PlaybackInfo、HLS \/ DASH、字幕、图片和必要静态资源/);
  assert.equal(document.getElementById('m-main-video-mode').value, 'proxy');
  assert.equal(document.getElementById('m-account-retention-days').value, '');
  assert.equal(document.getElementById('m-account-retention-days').required, false);
  assert.equal(document.getElementById('m-account-retention'), null);
  assert.doesNotMatch(body, /视频连续播放|1 分钟/);
  assert.match(body, /5 次有效播放状态同步/);
  assert.doesNotMatch(body, /播放回源/);
  assert.equal(document.getElementById('m-dynamic-enabled'), null);
  assert.equal(document.getElementById('m-dynamic-profile'), null);
  assert.equal(document.getElementById('m-dynamic-source-hls'), null);
  assert.equal(document.getElementById('m-dynamic-source-dash'), null);
  assert.equal(document.getElementById('m-playback-list'), null);
  assert.doesNotMatch(body, /Safe|Compatible|Extreme|高级选项/);
});

test('site retention period is enabled by a value and disabled by an empty field', async () => {
  const { sandbox, document, state } = loadModalHarness();
  await sandbox.showSiteModal(null);
  document.getElementById('m-name').value = 'Retention';
  document.getElementById('m-target-address').value = 'https://origin.example';
  document.getElementById('m-target-port').value = '443';
  document.getElementById('m-ingress-mode').value = 'port';
  document.getElementById('m-ingress-mode').onchange();
  document.getElementById('m-port').value = '8097';
  document.getElementById('m-account-retention-days').value = '45';

  await document.getElementById('m-submit').onclick();

  assert.equal(state.errors.length, 0, state.errors.join('; '));
  assert.equal(state.creates.length, 1);
  assert.equal(state.creates[0].account_retention_days, 45);
});

test('edit modal hides legacy dynamic observations and configuration controls', async () => {
  const { sandbox, document, state } = loadModalHarness();
  const site = {
    id: 17,
    name: 'Media',
    target_url: 'https://origin.example',
    ingress_mode: 'port',
    listen_port: 8096,
    public_host: '',
    ua_mode: 'infuse',
    client_ip_mode: 'real_ip',
    playback_target_url: '',
    main_video_stream_mode: 'direct',
    stream_hosts: [],
    upstream_headers: [],
    dynamic_discovery_enabled: true,
    dynamic_profile: 'safe',
    dynamic_discovery_sources: ['redirect', 'playback_info'],
    dynamic_domain_rules: [{ type: 'exact', value: 'media.example' }],
    dynamic_allow_https_downgrade: false,
    dynamic_policy_revision: 2,
    account_retention_days: 30,
  };

  await sandbox.showSiteModal(site);
  assert.equal(state.opened, 1);
  assert.equal(document.getElementById('m-refresh-dynamic-observations'), null);
  assert.equal(document.getElementById('m-clear-dynamic-observations'), null);
  assert.equal(document.getElementById('m-dynamic-observations'), null);
  assert.deepEqual(state.observationGets, []);
  assert.deepEqual(state.observationDeletes, []);
  assert.deepEqual(state.confirmations, []);
  assert.deepEqual(state.errors, []);
  assert.equal(document.getElementById('m-main-video-mode').value, 'direct');
  assert.equal(document.getElementById('m-client-ip-mode').value, 'real_ip');
  assert.equal(document.getElementById('m-account-retention-days').value, '30');
  assert.equal(document.getElementById('m-account-retention'), null);
});

test('client IP forwarding selector exposes only the three node-level modes', () => {
  const source = fs.readFileSync(path.join(STATIC_JS, 'pages', 'sites.js'), 'utf8');
  for (const value of ['both', 'real_ip', 'none']) {
    assert.match(source, new RegExp(`<option value="${value}"`));
  }
  assert.doesNotMatch(source, /client_ip_mode[^\n]*(inherit|global)|继承全局/i);
  assert.match(source, /clientIPModeSelect\.value = isEdit[^\n]+: 'both'/);
  assert.match(source, /client_ip_mode: clientIPModeSelect\.value/);
});

test('main video strategy uses a compact selector and defaults to proxy', () => {
  const source = fs.readFileSync(path.join(STATIC_JS, 'pages', 'sites.js'), 'utf8');
  assert.match(source, /<select[^>]+id="m-main-video-mode"/);
  assert.match(source, /<option value="proxy"[^>]*>反代<\/option>/);
  assert.match(source, /<option value="direct"[^>]*>直连<\/option>/);
  assert.doesNotMatch(source, /m-main-video-(?:proxy|direct)|main-video-mode-control/);
  assert.match(source, /mainVideoModeSelect\.value = isEdit[^\n]+: 'proxy'/);
  assert.match(source, /main_video_stream_mode: mainVideoModeSelect\.value/);
});

test('advanced settings use independent policy, cache, and limits columns', () => {
  const source = fs.readFileSync(path.join(STATIC_JS, 'pages', 'sites.js'), 'utf8');
  const style = fs.readFileSync(path.join(STATIC_JS, '..', 'css', 'style.css'), 'utf8');
  assert.match(source, /class="site-form-column site-form-column-policy"/);
  assert.match(source, /class="site-form-column site-form-column-cache"/);
  assert.match(source, /class="site-form-column site-form-column-limits"/);
  assert.match(source, /class="form-group cache-limit-group site-field-cache-limits"/);
  assert.match(source, /class="cache-limit-grid"/);
  assert.match(style, /\.site-form-columns\s*\{[\s\S]*?grid-template-columns:\s*repeat\(3,/);
  assert.match(style, /\.site-form-column\s*\{[\s\S]*?flex-direction:\s*column/);
  assert.doesNotMatch(style, /\.cache-limit-group\s*\{\s*grid-area:/);
});
