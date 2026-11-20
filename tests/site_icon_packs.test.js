'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const vm = require('node:vm');

const clone = value => JSON.parse(JSON.stringify(value));

function deferred() {
  let resolve;
  let reject;
  const promise = new Promise((fulfill, fail) => { resolve = fulfill; reject = fail; });
  return { promise, resolve, reject };
}

function iconPackHarness() {
  const elements = new Map();
  const calls = { fetch: [], refresh: [], activate: [], create: [], updateIcon: [], opened: [], closed: 0, success: [] };
  const hooks = {};
  const packs = [
    { id: 1, name: '离歌', description: '离歌说明', source_url: 'https://raw.example.test/lige.json', built_in: true, updated_at: '2026-11-20T09:00:00Z', icons: [{ name: 'Existing', url: 'https://cdn.example.test/existing.png' }] },
    { id: 2, name: 'Sakura', description: 'Sakura 说明', source_url: 'https://raw.example.test/sakura.json?branch=main', built_in: true, updated_at: '2026-11-20T09:00:00Z', icons: [{ name: 'Sakura Old', url: 'https://cdn.example.test/sakura-old.png' }] },
    { id: 3, name: '良友', description: '良友说明', source_url: 'https://raw.example.test/liangyou.json', built_in: true, updated_at: '2026-11-20T09:00:00Z', icons: [{ name: 'LiangYou', url: 'https://cdn.example.test/liangyou.png' }] },
  ];
  const state = { packs, activeID: 1 };
  const remote = new Map(packs.map(pack => [pack.source_url, { name: pack.name, description: pack.description, icons: clone(pack.icons) }]));
  const decode = value => String(value || '').replaceAll('&quot;', '"').replaceAll('&#39;', "'").replaceAll('&lt;', '<').replaceAll('&gt;', '>').replaceAll('&amp;', '&');
  const attributes = text => Object.fromEntries([...text.matchAll(/([\w-]+)(?:="([^"]*)")?/g)].map(([, name, value]) => [name, value === undefined ? true : decode(value)]));
  const classList = names => {
    const values = new Set(String(names || '').split(' ').filter(Boolean));
    return {
      contains: value => values.has(value),
      toggle(value, enabled) {
        const next = enabled === undefined ? !values.has(value) : enabled;
        if (next) values.add(value); else values.delete(value);
        return next;
      },
    };
  };
  const createElement = attrs => {
    const element = {
      id: attrs.id || '', value: attrs.value || '', disabled: attrs.disabled === true,
      textContent: '', classList: classList(attrs.class), listeners: new Map(), buttons: [], ownedIDs: [],
      dataset: Object.fromEntries(Object.entries(attrs).filter(([name]) => name.startsWith('data-')).map(([name, value]) => [name.slice(5).replace(/-([a-z])/g, (_, letter) => letter.toUpperCase()), value])),
      addEventListener(name, handler) { this.listeners.set(name, handler); },
      focus() {},
      querySelectorAll(selector) {
        if (selector === '.site-icon-image') return [];
        if (selector === '.site-icon-option') return this.buttons.filter(button => button.classList.contains('site-icon-option'));
        if (selector === '[data-icon-pack-action]') return this.buttons.filter(button => button.dataset.iconPackAction);
        const action = selector.match(/^\[data-icon-pack-action="([^"]+)"\]$/);
        return action ? this.buttons.filter(button => button.dataset.iconPackAction === action[1]) : [];
      },
    };
    let markup = '';
    Object.defineProperty(element, 'innerHTML', {
      get: () => markup,
      set(html) {
        markup = String(html);
        element.ownedIDs.forEach(id => elements.delete(id));
        element.ownedIDs = [];
        for (const [, tag, attributeText] of markup.matchAll(/<([a-z][\w-]*)\b([^>]*)>/gi)) {
          const childAttrs = attributes(attributeText);
          if (!childAttrs.id) continue;
          const child = createElement(childAttrs);
          child.tagName = tag;
          elements.set(childAttrs.id, child);
          element.ownedIDs.push(childAttrs.id);
        }
        element.buttons = [...markup.matchAll(/<button\b([^>]*)>([\s\S]*?)<\/button>/g)].map(([, attributeText, content]) => {
          const buttonAttrs = attributes(attributeText);
          const button = buttonAttrs.id && elements.get(buttonAttrs.id) || createElement(buttonAttrs);
          button.textContent = decode(content.replace(/<[^>]+>/g, ''));
          return button;
        });
        for (const [, attributeText, contents] of markup.matchAll(/<select\b([^>]*)>([\s\S]*?)<\/select>/g)) {
          const selectAttrs = attributes(attributeText);
          const select = elements.get(selectAttrs.id);
          if (select) select.innerHTML = contents;
        }
        const options = [...markup.matchAll(/<option\b([^>]*)>/g)].map(([, attributeText]) => attributes(attributeText));
        if (options.length) element.value = String((options.find(option => option.selected === true) || options[0]).value || '');
      },
    });
    return element;
  };
  for (const id of ['modal-title', 'modal-body', 'modal-footer']) elements.set(id, createElement({ id }));
  const fullPack = pack => ({ ...clone(pack), active: pack.id === state.activeID });
  const sandbox = {
    window: { confirm: () => true }, URL, TextEncoder, console, Router: { current: 'sites' },
    document: { getElementById: id => elements.get(id) || null },
    esc: value => String(value ?? '').replace(/[&<>"']/g, char => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[char]),
    openModal(options) { calls.opened.push(options); }, closeModal() { calls.closed++; },
    Toast: { success(message) { calls.success.push(message); }, error(message) { throw new Error(message); } },
    async fetch(url, options) {
      calls.fetch.push({ url, options: clone(options) });
      if (hooks.fetch) return hooks.fetch(url, options);
      return { ok: true, async text() { return JSON.stringify(remote.get(url)); } };
    },
    API: {
      async listSiteIconPacks() {
        return { packs: state.packs.map(pack => ({ ...fullPack(pack), icons: undefined, icon_count: pack.icons.length })) };
      },
      async getSiteIconPack() {
        if (hooks.getActive) return hooks.getActive();
        const active = state.packs.find(pack => pack.id === state.activeID);
        return active ? fullPack(active) : { icons: [] };
      },
      async activateSiteIconPack(id) {
        calls.activate.push(Number(id));
        if (hooks.activate) await hooks.activate(Number(id));
        state.activeID = Number(id);
        return { id: Number(id), active: true };
      },
      async refreshSiteIconPack(id, pack) {
        calls.refresh.push({ id: Number(id), pack: clone(pack) });
        if (hooks.refresh) await hooks.refresh(Number(id), pack);
        const existing = state.packs.find(item => item.id === Number(id));
        Object.assign(existing, clone(pack), { updated_at: '2026-11-20T10:00:00Z' });
        return fullPack(existing);
      },
      async createSiteIconPack(pack, sourceURL) {
        calls.create.push({ pack: clone(pack), sourceURL });
        const created = { ...clone(pack), id: 4, source_url: sourceURL, built_in: false };
        state.packs.push(created);
        return fullPack(created);
      },
      async deleteSiteIconPack(id) { state.packs = state.packs.filter(pack => pack.id !== Number(id)); },
      async updateSiteIcon(id, name, url) {
        calls.updateIcon.push({ id, name, url });
        return { icon_name: name, icon_url: url };
      },
    },
  };
  vm.createContext(sandbox);
  vm.runInContext(fs.readFileSync(path.join(__dirname, '..', 'web', 'static', 'js', 'pages', 'sites.js'), 'utf8'), sandbox);
  const element = id => elements.get(id);
  const packButton = (action, id) => element('m-site-icon-pack-list').querySelectorAll(`[data-icon-pack-action="${action}"]`).find(button => Number(button.dataset.iconPackId) === id);
  return { sandbox, element, packButton, calls, hooks, state, remote, fullPack };
}

test('icon pack list refresh reads its complete source and replaces the same inactive pack', async () => {
  const harness = iconPackHarness();
  const refreshed = { name: 'Sakura 最新', description: '链接中的新说明', icons: [{ name: 'Sakura New', url: 'https://cdn.example.test/sakura-new.png' }, { name: 'Added', url: 'https://cdn.example.test/added.png' }] };
  harness.remote.set(harness.state.packs[1].source_url, refreshed);
  await harness.sandbox.showSiteIconPackModal();
  assert.ok(harness.element('m-site-icon-pack-list').innerHTML.includes('内置'));
  await harness.packButton('refresh', 2).onclick();
  assert.equal(harness.calls.fetch[0].url, 'https://raw.example.test/sakura.json?branch=main');
  assert.deepEqual(harness.calls.fetch[0].options, { credentials: 'omit', redirect: 'error', cache: 'no-store' });
  assert.deepEqual(harness.calls.refresh, [{ id: 2, pack: refreshed }]);
  assert.equal(harness.state.activeID, 1);
  assert.equal(harness.state.packs.length, 3);
  assert.equal(harness.state.packs[1].source_url, 'https://raw.example.test/sakura.json?branch=main');
  assert.ok(harness.element('m-site-icon-pack-list').innerHTML.includes('Sakura 最新'));
  assert.ok(harness.element('m-site-icon-pack-list').innerHTML.includes('2 个图标'));
  assert.deepEqual(harness.calls.activate, []);
  assert.deepEqual(harness.calls.create, []);
});

test('a failed icon pack refresh leaves the previous pack and a retryable action', async () => {
  const harness = iconPackHarness();
  await harness.sandbox.showSiteIconPackModal();
  const before = clone(harness.state.packs[1]);
  harness.hooks.refresh = async () => { throw new Error('来源更新校验失败'); };
  await harness.packButton('refresh', 2).onclick();
  assert.deepEqual(harness.state.packs[1], before);
  assert.ok(harness.element('m-site-icon-pack-list').innerHTML.includes('Sakura 说明'));
  assert.equal(harness.element('m-icon-pack-status').textContent, '来源更新校验失败');
  assert.equal(harness.element('m-icon-pack-status').classList.contains('is-error'), true);
  assert.equal(harness.packButton('refresh', 2).disabled, false);
  assert.equal(harness.packButton('refresh', 2).textContent, '刷新');
});

test('icon pack refresh prevents overlapping refresh, activation and import submissions', async () => {
  const harness = iconPackHarness();
  await harness.sandbox.showSiteIconPackModal();
  const response = deferred();
  harness.hooks.fetch = () => response.promise;
  const refresh = harness.packButton('refresh', 2);
  const operation = refresh.onclick();
  assert.equal(harness.element('m-icon-pack-import').disabled, true);
  assert.equal(harness.packButton('activate', 3).disabled, true);
  await refresh.onclick();
  await harness.packButton('activate', 3).onclick();
  harness.element('m-icon-pack-url').value = 'https://raw.example.test/new.json';
  harness.element('m-icon-pack-import-form').onsubmit({ preventDefault() {} });
  assert.equal(harness.calls.fetch.length, 1);
  assert.equal(harness.calls.create.length, 0);
  assert.equal(harness.calls.activate.length, 0);
  response.resolve({ ok: true, async text() { return JSON.stringify(harness.remote.get('https://raw.example.test/sakura.json?branch=main')); } });
  await operation;
  assert.equal(harness.calls.refresh.length, 1);
  assert.equal(harness.element('m-icon-pack-import').disabled, false);
});

test('icon picker switches and refreshes packs in place before explicitly saving a new icon', async () => {
  const harness = iconPackHarness();
  const site = { id: 9, name: '我的站点', icon_name: 'Existing', icon_url: 'https://cdn.example.test/existing.png' };
  const before = clone(site);
  await harness.sandbox.showSiteIconManager(site);
  const selector = harness.element('m-site-icon-pack-select');
  const search = harness.element('m-site-icon-search');
  search.value = 'Existing';
  selector.value = '2';
  await selector.onchange();
  assert.equal(harness.calls.opened.length, 1);
  assert.equal(harness.calls.closed, 0);
  assert.deepEqual(harness.calls.activate, [2]);
  assert.equal(selector.value, '2');
  assert.equal(search.value, '');
  assert.ok(harness.element('m-site-icon-manager-results').innerHTML.includes('Sakura Old'));
  assert.deepEqual(site, before);
  const refreshed = { name: 'Sakura', description: '更新后的说明', icons: [{ name: 'Added', url: 'https://cdn.example.test/added.png' }] };
  harness.remote.set(harness.state.packs[1].source_url, refreshed);
  search.value = 'Added';
  search.oninput();
  await harness.element('m-site-icon-pack-refresh').onclick();
  assert.deepEqual(harness.calls.activate, [2]);
  assert.equal(harness.calls.refresh[0].id, 2);
  assert.equal(harness.calls.closed, 0);
  assert.deepEqual(site, before);
  const results = harness.element('m-site-icon-manager-results').querySelectorAll('.site-icon-option');
  assert.equal(results.length, 1);
  await results[0].onclick();
  assert.deepEqual(harness.calls.updateIcon, [{ id: 9, name: 'Added', url: 'https://cdn.example.test/added.png' }]);
  assert.equal(site.icon_name, 'Added');
  assert.equal(harness.calls.closed, 1);
});

test('a rejected inline pack switch preserves the original options and selection', async () => {
  const harness = iconPackHarness();
  await harness.sandbox.showSiteIconManager({ id: 9, name: '站点' });
  harness.hooks.activate = async () => { throw new Error('切换暂时不可用'); };
  const selector = harness.element('m-site-icon-pack-select');
  selector.value = '2';
  await selector.onchange();
  assert.equal(selector.value, '1');
  assert.equal(harness.state.activeID, 1);
  assert.ok(harness.element('m-site-icon-manager-results').innerHTML.includes('Existing'));
  assert.equal(harness.element('m-site-icon-status').textContent, '切换暂时不可用');
  assert.equal(harness.calls.closed, 0);
});

test('the icon picker can activate the only remaining pack when the previous active pack was deleted', async () => {
  const harness = iconPackHarness();
  harness.state.packs = [harness.state.packs[1]];
  harness.state.activeID = 0;
  const site = { id: 9, name: '站点', icon_name: 'Removed', icon_url: 'https://cdn.example.test/removed.png' };
  const before = clone(site);
  await harness.sandbox.showSiteIconManager(site);
  const selector = harness.element('m-site-icon-pack-select');
  assert.equal(selector.disabled, false);
  assert.equal(selector.value, '');
  assert.ok(selector.innerHTML.includes('选择图标包'));
  selector.value = '2';
  await selector.onchange();
  assert.equal(harness.state.activeID, 2);
  assert.equal(selector.value, '2');
  assert.ok(harness.element('m-site-icon-manager-results').innerHTML.includes('Sakura Old'));
  assert.deepEqual(site, before);
  assert.equal(harness.calls.opened.length, 1);
  assert.equal(harness.calls.closed, 0);
});

test('inline switch locks competing requests and ignores a stale global icon pack during search', async () => {
  const harness = iconPackHarness();
  await harness.sandbox.showSiteIconManager({ id: 9, name: '站点' });
  const activation = deferred();
  harness.hooks.activate = () => activation.promise;
  const selector = harness.element('m-site-icon-pack-select');
  selector.value = '2';
  const operation = selector.onchange();
  assert.equal(selector.disabled, true);
  assert.equal(harness.element('m-site-icon-pack-refresh').disabled, true);
  selector.value = '3';
  await selector.onchange();
  assert.deepEqual(harness.calls.activate, [2]);
  activation.resolve();
  await operation;
  assert.equal(selector.value, '2');
  vm.runInContext('siteIconPack = { icons: [{name:"STALE",url:"https://cdn.example.test/stale.png"}] }', harness.sandbox);
  harness.element('m-site-icon-search').oninput();
  const displayed = harness.element('m-site-icon-manager-results').innerHTML;
  assert.ok(displayed.includes('Sakura Old'));
  assert.equal(displayed.includes('STALE'), false);
  assert.equal(selector.disabled, false);
});

test('a pack read failure after activation prevents choosing stale icons and can recover by refresh', async () => {
  const harness = iconPackHarness();
  await harness.sandbox.showSiteIconManager({ id: 9, name: '站点' });
  harness.hooks.getActive = async () => { throw new Error('读取更新失败'); };
  const selector = harness.element('m-site-icon-pack-select');
  selector.value = '2';
  await selector.onchange();
  assert.equal(selector.value, '2');
  assert.equal(harness.state.activeID, 2);
  assert.equal(harness.element('m-site-icon-manager-results').querySelectorAll('.site-icon-option').length, 0);
  assert.equal(harness.element('m-site-icon-pack-refresh').disabled, false);
  await harness.element('m-site-icon-pack-refresh').onclick();
  assert.equal(harness.element('m-site-icon-manager-results').querySelectorAll('.site-icon-option').length, 1);
  assert.ok(harness.element('m-site-icon-manager-results').innerHTML.includes('Sakura Old'));
});

test('icon pack reader rejects oversized UTF-8 content and malformed JSON before updating storage', async () => {
  const harness = iconPackHarness();
  harness.hooks.fetch = async () => ({ ok: true, async text() { return '汉'.repeat(1500000); } });
  await assert.rejects(harness.sandbox.loadSiteIconPackFromSource('https://raw.example.test/large.json'), /4 MiB/);
  harness.hooks.fetch = async () => ({ ok: true, async text() { return '{broken'; } });
  await assert.rejects(harness.sandbox.loadSiteIconPackFromSource('https://raw.example.test/broken.json'), /有效的 JSON/);
  assert.equal(harness.calls.refresh.length, 0);
});

test('refresh API sends only the fetched pack to the encoded existing package path', async () => {
  const sandbox = {};
  vm.createContext(sandbox);
  vm.runInContext(fs.readFileSync(path.join(__dirname, '..', 'web', 'static', 'js', 'api.js'), 'utf8'), sandbox);
  const api = vm.runInContext('API', sandbox);
  const requests = [];
  api.request = async (...args) => { requests.push(clone(args)); };
  const pack = { name: 'latest', icons: [{ name: 'New', url: 'https://cdn.example.test/new.png' }] };
  await api.refreshSiteIconPack('17/extra', pack);
  assert.deepEqual(requests, [['PUT', '/api/site-icon-packs/17%2Fextra', { pack }]]);
});
