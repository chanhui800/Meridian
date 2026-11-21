'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const vm = require('node:vm');

function loadHelpers() {
  const source = fs.readFileSync(path.join(__dirname, '..', 'web', 'static', 'js', 'pages', 'sites.js'), 'utf8');
  const sandbox = { window: {}, URL, esc: value => String(value) };
  vm.createContext(sandbox);
  vm.runInContext(source, sandbox, { filename: 'sites.js' });
  return sandbox;
}

function siteModalHarness() {
  const elements = new Map();
  const saved = [];
  const decode = value => value.replaceAll('&amp;', '&').replaceAll('&quot;', '"').replaceAll('&#39;', "'");
  const attributes = text => Object.fromEntries([...text.matchAll(/([\w-]+)(?:="([^"]*)")?/g)]
    .map(([, name, value]) => [name, value === undefined ? true : decode(value)]));
  const input = attrs => ({
    value: attrs.value || '', checked: attrs.checked === true, disabled: attrs.disabled === true,
    dataset: Object.fromEntries(Object.entries(attrs).filter(([name]) => name.startsWith('data-'))
      .map(([name, value]) => [name.slice(5).replace(/-([a-z])/g, (_, letter) => letter.toUpperCase()), value])),
    listeners: new Map(), focus() {}, classList: { toggle() {} },
    addEventListener(name, handler) { this.listeners.set(name, handler); },
  });
  const parseControls = html => {
    for (const [, tag, text, contents] of html.matchAll(/<(select|textarea)\b([^>]*)>([\s\S]*?)<\/\1>/g)) {
      const attrs = attributes(text);
      if (!attrs.id) continue;
      const control = element(attrs.id);
      if (tag === 'textarea') control.value = contents;
      else {
        const options = [...contents.matchAll(/<option\b([^>]*)>/g)].map(([, option]) => attributes(option));
        control.value = (options.find(option => option.selected === true) || options[0] || {}).value || '';
      }
    }
    for (const [, text] of html.matchAll(/<input\b([^>]*)>/g)) {
      const attrs = attributes(text);
      if (attrs.id) Object.assign(element(attrs.id), input(attrs));
    }
  };
  const element = id => {
    if (elements.has(id)) return elements.get(id);
    const item = input({});
    item.rows = [];
    let markup = '';
    Object.defineProperty(item, 'innerHTML', {
      get: () => markup,
      set(html) {
        markup = html;
        parseControls(html);
        if (id !== 'm-failover-lines') return;
        this.rows = [...html.matchAll(/<div class="upstream-line" data-line-index="(\d+)">([\s\S]*?)(?=<div class="upstream-line" data-line-index="|$)/g)]
          .map(([, index, contents]) => {
            const controls = new Map();
            for (const [, text] of contents.matchAll(/<(?:input|button)\b([^>]*)>/g)) {
              const attrs = attributes(text);
              const control = input(attrs);
              String(attrs.class || '').split(' ').forEach(name => controls.set(`.${name}`, control));
            }
            return { dataset: { lineIndex: index }, querySelector: selector => controls.get(selector) || null };
          });
      },
    });
    item.querySelectorAll = selector => selector === '.upstream-line' ? item.rows
      : item.rows.map(row => row.querySelector(selector)).filter(Boolean);
    elements.set(id, item);
    return item;
  };
  const sandbox = {
    window: {}, URL, console, Router: { current: 'sites' },
    document: { getElementById: id => id === 'page-sites' ? null : element(id) },
    esc: value => String(value ?? ''), openModal() {}, closeModal() {},
    Toast: { error(message) { throw new Error(message); }, success() {} },
    API: {
      async ingressCapabilities() { return { host_only_available: false, upstream_headers_available: true }; },
      async getSiteIconPack() { return { icons: [] }; },
      async createSite(payload) { saved.push(JSON.parse(JSON.stringify(payload))); },
      async updateSite(id, payload) { saved.push(JSON.parse(JSON.stringify(payload))); },
    },
  };
  vm.createContext(sandbox);
  vm.runInContext(fs.readFileSync(path.join(__dirname, '../web/static/js/app.js'), 'utf8').split('(function()')[0], sandbox);
  vm.runInContext(fs.readFileSync(path.join(__dirname, '..', 'web', 'static', 'js', 'pages', 'sites.js'), 'utf8'), sandbox);
  const emit = (control, name, value) => {
    control.value = value;
    const event = { target: control };
    control[`on${name}`]?.(event);
    control.listeners.get(name)?.(event);
  };
  return { sandbox, element, saved, emit };
}

test('create and edit forms open without reading optional icon pack contents', async () => {
  const { sandbox, element } = siteModalHarness();
  let reads = 0;
  sandbox.API.getSiteIconPack = async () => {
    reads++;
    throw new Error('optional icon pack endpoint unavailable');
  };
  await sandbox.showSiteModal();
  assert.equal(element('modal-title').textContent, '添加站点');
  await sandbox.showSiteModal({ id: 9, name: 'Existing site', ingress_mode: 'port', listen_port: 8096, target_url: 'http://origin.example.test:8096', ua_mode: 'passthrough' });
  assert.equal(element('modal-title').textContent, '编辑站点');
  assert.equal(element('m-name').value, 'Existing site');
  assert.equal(reads, 0);
});

test('site address autofill completes scheme and splits the port', () => {
  const { autoFillTargetAddress } = loadHelpers();
  const fill = raw => {
    const address = { value: raw };
    const port = { value: '' };
    autoFillTargetAddress(address, port);
    return { address: address.value, port: port.value };
  };
  assert.deepEqual(fill('emby.example.com'), { address: 'https://emby.example.com', port: '443' });
  assert.deepEqual(fill('emby.example.com/emby'), { address: 'https://emby.example.com/emby', port: '443' });
  assert.deepEqual(fill('media.example.com:8096'), { address: 'http://media.example.com', port: '8096' });
  assert.deepEqual(fill('media.example.com:443'), { address: 'https://media.example.com', port: '443' });
  assert.deepEqual(fill('lingj.k:80'), { address: 'http://lingj.k', port: '80' });
  assert.deepEqual(fill('https://cdn.example.com:8443/base?x=1'), { address: 'https://cdn.example.com/base?x=1', port: '8443' });
  assert.deepEqual(fill('https://cdn.example.com:443/base'), { address: 'https://cdn.example.com/base', port: '443' });
  assert.deepEqual(fill('http://plain.example.com'), { address: 'http://plain.example.com', port: '80' });
  assert.deepEqual(fill('http://plain.example.com:80'), { address: 'http://plain.example.com', port: '80' });
  assert.deepEqual(fill('[::1]:8096'), { address: 'http://[::1]', port: '8096' });
  assert.deepEqual(fill('media.example.com：8096'), { address: 'http://media.example.com', port: '8096' });
  assert.deepEqual(fill('not a url with spaces'), { address: 'not a url with spaces', port: '' });
  assert.deepEqual(fill('   '), { address: '   ', port: '' });
});

test('editing an origin path preserves the populated standalone port', () => {
  const { autoFillTargetAddress } = loadHelpers();
  const address = { value: 'http://origin.example.com/newbase' };
  const port = { value: '8096' };

  autoFillTargetAddress(address, port);

  assert.deepEqual({ address: address.value, port: port.value }, {
    address: 'http://origin.example.com/newbase', port: '8096',
  });
});

test('an automatically filled default follows a later explicit protocol', () => {
  const { autoFillTargetAddress } = loadHelpers();
  const address = { value: 'origin.example.com' };
  const port = { value: '', dataset: {} };
  autoFillTargetAddress(address, port);

  address.value = 'http://origin.example.com/emby';
  autoFillTargetAddress(address, port);

  assert.deepEqual({ address: address.value, port: port.value }, {
    address: 'http://origin.example.com/emby', port: '80',
  });
});

test('saving the site form preserves manually entered ports after protocol edits', async () => {
  const { sandbox, element, saved, emit } = siteModalHarness();
  await sandbox.showSiteModal();
  element('m-name').value = 'Origin port regression';
  element('m-port').value = '8001';
  emit(element('m-target-address'), 'change', 'origin.example.com');
  emit(element('m-target-port'), 'input', '443');
  emit(element('m-target-address'), 'change', 'http://origin.example.com/newbase');

  element('m-add-failover-line').onclick();
  const backup = element('m-failover-lines').rows[0];
  emit(backup.querySelector('.upstream-line-address'), 'change', 'backup.example.com');
  emit(backup.querySelector('.upstream-line-port'), 'input', '443');
  emit(backup.querySelector('.upstream-line-address'), 'change', 'http://backup.example.com/newbase');
  await element('m-submit').onclick();

  assert.equal(saved[0].target_url, 'http://origin.example.com:443/newbase');
  assert.deepEqual(saved[0].failover_lines, [{ name: '线路2', url: 'http://backup.example.com:443/newbase', enabled: true }]);
});

test('backup defaults still follow the protocol after adding and removing another line', async () => {
  const { sandbox, element, saved, emit } = siteModalHarness();
  await sandbox.showSiteModal();
  element('m-name').value = 'Backup default regression';
  element('m-port').value = '8001';
  emit(element('m-target-address'), 'change', 'origin.example.com');
  element('m-add-failover-line').onclick();
  emit(element('m-failover-lines').rows[0].querySelector('.upstream-line-address'), 'change', 'backup.example.com');
  element('m-add-failover-line').onclick();
  element('m-failover-lines').rows[1].querySelector('.upstream-line-remove').onclick();

  emit(element('m-failover-lines').rows[0].querySelector('.upstream-line-address'), 'change', 'http://backup.example.com/newbase');
  await element('m-submit').onclick();

  assert.deepEqual(saved[0].failover_lines, [{ name: '线路2', url: 'http://backup.example.com/newbase', enabled: true }]);
});

test('explicit inline ports replace standalone ports and select the unschemed protocol', () => {
  const { autoFillTargetAddress } = loadHelpers();
  for (const [raw, expectedAddress, expectedPort] of [
    ['origin.example.com:80/base', 'http://origin.example.com/base', '80'],
    ['origin.example.com:443/base', 'https://origin.example.com/base', '443'],
    ['origin.example.com:8096/base', 'http://origin.example.com/base', '8096'],
    ['https://origin.example.com:8443/base', 'https://origin.example.com/base', '8443'],
    ['https://origin.example.com:80/base', 'https://origin.example.com/base', '80'],
    ['[::1]:80/base', 'http://[::1]/base', '80'],
  ]) {
    const address = { value: raw };
    const port = { value: '9443', dataset: {} };
    autoFillTargetAddress(address, port);
    assert.deepEqual({ address: address.value, port: port.value }, { address: expectedAddress, port: expectedPort });
  }
});

test('saved standalone ports survive host, path and explicit scheme edits', () => {
  const { autoFillTargetAddress } = loadHelpers();
  for (const [raw, savedPort, expectedAddress] of [
    ['https://new.example.com/base?x=1', '8443', 'https://new.example.com/base?x=1'],
    ['new.example.com/base', '8096', 'http://new.example.com/base'],
    ['new.example.com/base', '443', 'https://new.example.com/base'],
    ['https://new.example.com/base', '80', 'https://new.example.com/base'],
    ['http://new.example.com/base', '443', 'http://new.example.com/base'],
  ]) {
    const address = { value: raw };
    const port = { value: savedPort, dataset: {} };
    autoFillTargetAddress(address, port);
    assert.deepEqual({ address: address.value, port: port.value }, { address: expectedAddress, port: savedPort });
  }
});

test('saving an edited site retains both existing nondefault origin ports', async () => {
  const { sandbox, element, saved, emit } = siteModalHarness();
  await sandbox.showSiteModal({
    id: 7, name: 'Saved origin regression', ingress_mode: 'port', listen_port: 8001,
    target_url: 'http://origin.example.com:8096/emby', ua_mode: 'passthrough',
    failover_lines: [{ name: 'Backup', url: 'https://backup.example.com:8443/emby', enabled: true }],
  });
  emit(element('m-target-address'), 'change', 'http://origin.example.com/newbase');
  emit(element('m-failover-lines').rows[0].querySelector('.upstream-line-address'), 'change', 'https://backup.example.com/newbase');
  await element('m-submit').onclick();

  assert.equal(saved[0].target_url, 'http://origin.example.com:8096/newbase');
  assert.deepEqual(saved[0].failover_lines, [{ name: 'Backup', url: 'https://backup.example.com:8443/newbase', enabled: true }]);
});

test('editing IPv6 origin URLs preserves their brackets and standalone ports', async () => {
  const { sandbox, element, saved } = siteModalHarness();
  await sandbox.showSiteModal({
    id: 7, name: 'IPv6 origin regression', ingress_mode: 'port', listen_port: 8001,
    target_url: 'http://[2001:db8::1]:8096/emby', ua_mode: 'passthrough',
    failover_lines: [{ name: 'IPv6 backup', url: 'https://[2001:db8::2]:8443/base', enabled: true }],
  });
  assert.equal(element('m-target-address').value, 'http://[2001:db8::1]/emby');
  assert.equal(element('m-target-port').value, '8096');
  await element('m-submit').onclick();

  assert.equal(saved[0].target_url, 'http://[2001:db8::1]:8096/emby');
  assert.deepEqual(saved[0].failover_lines, [{ name: 'IPv6 backup', url: 'https://[2001:db8::2]:8443/base', enabled: true }]);
});

test('editing a site name preserves populated ingress and advanced policies', async () => {
  const { sandbox, element, saved } = siteModalHarness();
  await sandbox.showSiteModal({
    id: 7, name: 'Existing site', ingress_mode: 'path', path_prefix: 'library', listen_port: 0,
    target_url: 'https://origin.example.com:8443/emby',
    ua_mode: 'custom', custom_user_agent: 'Player/1.0', custom_client: 'Player', custom_version: '1.0',
    client_ip_mode: 'real_ip', main_video_stream_mode: 'direct', watch_history_enabled: true,
    asset_cache_enabled: true, asset_cache_ttl_sec: 172800, asset_cache_max_bytes: 1073741824,
    asset_cache_rules: '/Images/*\n/assets/*', traffic_quota: 45097156608, speed_limit: 30,
    account_retention_days: 14, playback_target_url: 'https://stream.example.com',
    playback_mode: 'proxy', stream_hosts: ['stream.example.com'],
  });
  element('m-name').value = 'Renamed site';
  await element('m-submit').onclick();
  const payload = saved[0];

  assert.deepEqual({
    name: payload.name, target_url: payload.target_url, ingress_mode: payload.ingress_mode, path_prefix: payload.path_prefix,
    ua_mode: payload.ua_mode, custom_user_agent: payload.custom_user_agent, custom_client: payload.custom_client,
    custom_version: payload.custom_version, client_ip_mode: payload.client_ip_mode, main_video_stream_mode: payload.main_video_stream_mode,
    watch_history_enabled: payload.watch_history_enabled, asset_cache_enabled: payload.asset_cache_enabled,
    asset_cache_ttl_sec: payload.asset_cache_ttl_sec, asset_cache_max_bytes: payload.asset_cache_max_bytes,
    asset_cache_rules: payload.asset_cache_rules, traffic_quota: payload.traffic_quota, speed_limit: payload.speed_limit,
    account_retention_days: payload.account_retention_days, playback_target_url: payload.playback_target_url,
    playback_mode: payload.playback_mode, stream_hosts: payload.stream_hosts,
  }, {
    name: 'Renamed site', target_url: 'https://origin.example.com:8443/emby', ingress_mode: 'path', path_prefix: 'library',
    ua_mode: 'custom', custom_user_agent: 'Player/1.0', custom_client: 'Player', custom_version: '1.0', client_ip_mode: 'real_ip',
    main_video_stream_mode: 'direct', watch_history_enabled: true, asset_cache_enabled: true,
    asset_cache_ttl_sec: 172800, asset_cache_max_bytes: 1073741824, asset_cache_rules: '/Images/*\n/assets/*',
    traffic_quota: 45097156608, speed_limit: 30, account_retention_days: 14,
    playback_target_url: 'https://stream.example.com', playback_mode: 'proxy', stream_hosts: ['stream.example.com'],
  });
});
