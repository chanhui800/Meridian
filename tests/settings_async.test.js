'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const vm = require('node:vm');

function deferred() {
  let resolve, reject;
  const promise = new Promise((accept, fail) => { resolve = accept; reject = fail; });
  return { promise, resolve, reject };
}
const flush = async () => { for (let index = 0; index < 12; index++) await Promise.resolve(); };

// Model form DOM ownership, edits and subtree replacement. API transport is
// synthetic, while rendering, event handlers, payloads and response sequencing
// execute the real page scripts, just as the browser regression does.
function settingsHarness(route = 'settings-tmdb') {
  const fields = new Map(), roots = [], messages = [];
  let document;
  const decode = value => String(value).replaceAll('&quot;', '"').replaceAll('&#39;', "'")
    .replaceAll('&lt;', '<').replaceAll('&gt;', '>').replaceAll('&amp;', '&');
  const attrs = source => Object.fromEntries([...source.matchAll(/([\w-]+)(?:="([^"]*)")?/g)]
    .map(([, name, value]) => [name, value === undefined ? true : decode(value)]));
  function element(tag, attributes = {}) {
    const listeners = new Map(), classes = new Set(String(attributes.class || '').split(/\s+/).filter(Boolean));
    const node = { tagName: tag.toUpperCase(), id: attributes.id || '', type: attributes.type || '',
      value: String(attributes.value ?? ''), checked: attributes.checked === true, disabled: attributes.disabled === true,
      hidden: attributes.hidden === true, isConnected: true, parentElement: null, children: [], dataset: {}, style: {},
      textContent: '', title: '',
      classList: { add(...values) { values.forEach(value => classes.add(value)); },
        remove(...values) { values.forEach(value => classes.delete(value)); }, contains: value => classes.has(value),
        toggle(value, active) { if (active === undefined) active = !classes.has(value); if (active) classes.add(value); else classes.delete(value); return active; } },
      get className() { return [...classes].join(' '); },
      set className(value) { classes.clear(); String(value).split(/\s+/).filter(Boolean).forEach(item => classes.add(item)); },
      setAttribute(name, value) { attributes[name] = String(value); }, getAttribute(name) { return attributes[name] ?? null; },
      addEventListener(name, listener) { if (!listeners.has(name)) listeners.set(name, []); listeners.get(name).push(listener); },
      dispatch(name) { const event = { target: this, preventDefault() {}, stopPropagation() {} };
        const pending = (listeners.get(name) || []).map(listener => listener(event));
        if (typeof this['on' + name] === 'function') pending.push(this['on' + name](event));
        return Promise.all(pending); },
      focus() { document.activeElement = this; },
      contains(other) { for (; other; other = other.parentElement) if (other === this) return true; return false; },
      querySelectorAll(selector) {
        const descendants = [], visit = parent => parent.children.forEach(child => { descendants.push(child); visit(child); });
        visit(this);
        const matches = (child, part) => {
          const last = part.trim().split(/\s+/).at(-1);
          if (last.startsWith('#')) return child.id === last.slice(1);
          if (last.startsWith('.')) return child.classList.contains(last.slice(1));
          const attribute = last.match(/^\[([^=\]]+)(?:="([^"]*)")?\]$/);
          if (attribute) return child.getAttribute(attribute[1]) !== null && (attribute[2] === undefined || child.getAttribute(attribute[1]) === attribute[2]);
          return child.tagName.toLowerCase() === last;
        };
        return descendants.filter(child => selector.split(',').some(part => matches(child, part)));
      },
      querySelector(selector) { return this.querySelectorAll(selector)[0] || null; },
    };
    for (const [name, value] of Object.entries(attributes)) if (name.startsWith('data-')) node.dataset[name.slice(5).replace(/-([a-z])/g, (_, letter) => letter.toUpperCase())] = value;
    const disconnect = parent => parent.children.forEach(child => {
      disconnect(child); child.isConnected = false;
      if (fields.get(child.id) === child) fields.delete(child.id);
    });
    let markup = '';
    Object.defineProperty(node, 'innerHTML', { get: () => markup, set(value) {
      markup = String(value); disconnect(node); node.children = [];
      const stack = [node];
      const voidTags = new Set(['input', 'img', 'br', 'hr', 'meta', 'link', 'path']);
      for (const token of markup.matchAll(/<\/?([a-z][\w-]*)\b([^>]*)>|([^<]+)/gi)) {
        if (!token[1]) { stack.at(-1).textContent += decode(token[3] || ''); continue; }
        const tag = token[1].toLowerCase();
        if (token[0].startsWith('</')) {
          for (let index = stack.length - 1; index > 0; index--) {
            if (stack[index].tagName.toLowerCase() === tag) { stack.length = index; break; }
          }
          continue;
        }
        const child = element(tag, attrs(token[2]));
        child.parentElement = stack.at(-1); child.parentElement.children.push(child);
        if (!voidTags.has(tag) && !token[0].endsWith('/>')) stack.push(child);
      }
      for (const select of node.querySelectorAll('select')) {
        const options = select.children.filter(child => child.tagName === 'OPTION');
        select.value = (options.find(option => option.getAttribute('selected') !== null) || options[0])?.value || '';
      }
    } });
    if (node.id) fields.set(node.id, node);
    return node;
  }
  for (const page of ['settings-tmdb', 'telegram-report', 'settings-tls', 'global-settings']) roots.push(element('div', { id: 'page-' + page }));
  document = { activeElement: null, getElementById: id => fields.get(id) || null,
    querySelectorAll: selector => roots.flatMap(root => root.querySelectorAll(selector)),
    querySelector: selector => document.querySelectorAll(selector)[0] || null,
    documentElement: { style: { setProperty() {} } } };
  const tmdb = { enabled: true, configured: true, secret_stable: true, language: 'zh-CN',
    history_retention_days: 90, credential_state: 'ready', cache_entries: 31, cache_size_bytes: 4096 };
  const telegram = { enabled: true, configured: true, secret_stable: true, bot_token: 'synthetic-token',
    chat_id: 'saved-chat', frequency: 'daily', weekday: 1, schedule_time: '20:00', traffic_warning_percent: 80 };
  const tls = { available: true, configured: true, settings_configured: true, panel_domain: 'panel.example.test',
    wildcard_domain: '*.example.test', route_domain: 'example.test', listen_port: 9090, active_listen_port: 9090,
    acme_email: 'admin@example.test', dns_api_token: 'synthetic-dns-token' };
  const API = { getTMDBSettings: async () => ({ ...tmdb }), getTelegramReportSettings: async () => ({ ...telegram }),
    panelCertificate: async () => ({ ...tls }), saveTMDBSettings: async data => ({ ...tmdb, ...data }),
    saveTelegramReportSettings: async data => ({ ...telegram, ...data }),
    savePanelSettings: async data => ({ ...tls, ...data }),
    requestPanelCertificate: async () => ({ staging_tested: true }) };
  const sandbox = { console, document, API, Router: { current: route, navigate(next) { this.current = next; } },
    Toast: { error: message => messages.push(message), success: message => messages.push(message) },
    esc: value => String(value ?? ''), setInterval() { return 1; }, clearInterval() {},
    window: { location: { protocol: 'https:', hostname: 'panel.example.test', port: '9090' }, confirm: () => true },
    confirm: () => true, renderPanelCertificateStatus: () => '<span>synthetic TLS status</span>',
    panelHTTPSPreview: domain => 'https://' + domain + ':9090',
  };
  vm.createContext(sandbox);
  for (const file of ['global-settings.js', 'tmdb-settings.js', 'telegram-report.js']) {
    vm.runInContext(fs.readFileSync(path.join(__dirname, '../web/static/js/pages', file), 'utf8'), sandbox, { filename: file });
  }
  return { sandbox, API, tmdb, telegram, tls, document, messages, field: id => fields.get(id),
    edit(id, value) { const control = fields.get(id); control.value = String(value); return control.dispatch('input'); } };
}

test('TMDB save retains fields and a replacement token edited after submission', async () => {
  const h = settingsHarness(), pending = deferred();
  h.sandbox.renderTMDBSettings(); await flush();
  await h.edit('tmdb-history-retention', '120');
  let submitted;
  h.API.saveTMDBSettings = payload => { submitted = payload; return pending.promise; };
  const save = h.field('tmdb-save').onclick();
  await h.edit('tmdb-history-retention', '240');
  await h.edit('tmdb-api-token', 'synthetic-new-token-draft');
  h.tmdb.history_retention_days = 120;
  pending.resolve({ ...h.tmdb }); await save; await flush();
  assert.equal(submitted.history_retention_days, 120);
  assert.equal(h.field('tmdb-history-retention').value, '240');
  assert.equal(h.field('tmdb-api-token').value, 'synthetic-new-token-draft');
});

test('a TMDB post-save read does not replace fields edited while the read is pending', async () => {
  const h = settingsHarness(), pendingRead = deferred();
  h.sandbox.renderTMDBSettings(); await flush();
  await h.edit('tmdb-history-retention', '120');
  h.API.getTMDBSettings = () => pendingRead.promise;
  const save = h.field('tmdb-save').onclick();
  await flush();
  await h.edit('tmdb-history-retention', '240');
  await h.edit('tmdb-api-token', 'synthetic-post-save-token-draft');
  pendingRead.resolve({ ...h.tmdb, history_retention_days: 120 });
  await save; await flush();
  assert.equal(h.field('tmdb-history-retention').value, '240');
  assert.equal(h.field('tmdb-api-token').value, 'synthetic-post-save-token-draft');
});

test('a failed TMDB post-save read leaves the edited form available', async () => {
  const h = settingsHarness(), pendingRead = deferred();
  h.sandbox.renderTMDBSettings(); await flush();
  await h.edit('tmdb-history-retention', '120');
  h.API.getTMDBSettings = () => pendingRead.promise;
  const save = h.field('tmdb-save').onclick();
  await flush();
  await h.edit('tmdb-history-retention', '240');
  pendingRead.reject(new Error('synthetic read failure'));
  await save; await flush();
  assert.equal(h.field('tmdb-history-retention').value, '240');
  assert.equal(h.field('tmdb-save').isConnected, true);
});

test('TMDB cache clearing updates only cache statistics while keeping a newer form draft', async () => {
  const h = settingsHarness(), pending = deferred();
  h.sandbox.renderTMDBSettings(); await flush();
  h.API.clearTMDBCache = () => pending.promise;
  const clear = h.field('tmdb-cache-clear-all').onclick();
  await h.edit('tmdb-history-retention', '240');
  await h.edit('tmdb-api-token', 'synthetic-cache-draft');
  pending.resolve({ settings: { ...h.tmdb, cache_entries: 0, cache_size_bytes: 0 } });
  await clear; await flush();
  assert.equal(h.field('tmdb-history-retention').value, '240');
  assert.equal(h.field('tmdb-api-token').value, 'synthetic-cache-draft');
  assert.equal(h.field('tmdb-cache-summary').textContent, '0 条 · 0 B');
});

test('a late Telegram read cannot replace a reopened form or its draft', async () => {
  const h = settingsHarness('telegram-report'), pending = deferred();
  h.API.getTelegramReportSettings = () => pending.promise;
  h.sandbox.renderTelegramReport(); await flush();
  h.API.getTelegramReportSettings = async () => ({ ...h.telegram, chat_id: 'new-page-chat' });
  h.sandbox.renderTelegramReport(); await flush();
  await h.edit('telegram-chat-id', 'new-page-draft');
  pending.resolve({ ...h.telegram, chat_id: 'old-page-chat' });
  await flush();
  assert.equal(h.field('telegram-chat-id').value, 'new-page-draft');
});

test('a Telegram save response keeps a chat draft edited while saving', async () => {
  const h = settingsHarness('telegram-report'), pending = deferred();
  h.sandbox.renderTelegramReport(); await flush();
  h.API.saveTelegramReportSettings = payload => { assert.equal(payload.chat_id, 'saved-chat'); return pending.promise; };
  const save = h.field('telegram-save').onclick();
  await h.edit('telegram-chat-id', 'replacement-chat-draft');
  pending.resolve({ ...h.telegram });
  await save; await flush();
  assert.equal(h.field('telegram-chat-id').value, 'replacement-chat-draft');
});

test('a Telegram post-save read merges fields edited while the read is pending', async () => {
  const h = settingsHarness('telegram-report'), pendingRead = deferred();
  h.sandbox.renderTelegramReport(); await flush();
  h.API.getTelegramReportSettings = () => pendingRead.promise;
  const save = h.field('telegram-save').onclick();
  await flush();
  await h.edit('telegram-chat-id', 'post-save-chat-draft');
  await h.edit('telegram-schedule-time', '21:30');
  pendingRead.resolve({ ...h.telegram });
  await save; await flush();
  assert.equal(h.field('telegram-chat-id').value, 'post-save-chat-draft');
  assert.equal(h.field('telegram-schedule-time').value, '21:30');
  assert.equal(h.field('telegram-save').disabled, false);
});

test('a failed Telegram post-save read keeps the edited form usable', async () => {
  const h = settingsHarness('telegram-report'), pendingRead = deferred();
  h.sandbox.renderTelegramReport(); await flush();
  h.API.getTelegramReportSettings = () => pendingRead.promise;
  const save = h.field('telegram-save').onclick();
  await flush();
  await h.edit('telegram-chat-id', 'post-save-chat-draft');
  pendingRead.reject(new Error('synthetic read failure'));
  await save; await flush();
  assert.equal(h.field('telegram-chat-id').value, 'post-save-chat-draft');
  assert.equal(h.field('telegram-save').disabled, false);
  assert.ok(!h.messages.some(message => message.includes('读取 Telegram 日报设置失败')));
});

test('a late TLS read cannot replace a reopened page or its domain draft', async () => {
  const h = settingsHarness('settings-tls'), pending = deferred();
  h.API.panelCertificate = () => pending.promise;
  const first = h.sandbox.renderTLSSettings(); await flush();
  h.API.panelCertificate = async () => ({ ...h.tls, panel_domain: 'new-page.example.test' });
  const second = h.sandbox.renderTLSSettings(); await second;
  await h.edit('p-panel-domain', 'new-page-draft.example.test');
  pending.resolve({ ...h.tls, panel_domain: 'old-page.example.test' });
  await first; await flush();
  assert.equal(h.field('p-panel-domain').value, 'new-page-draft.example.test');
});

test('a TLS save response keeps newer domain edits in the same form', async () => {
  const h = settingsHarness('settings-tls'), pending = deferred();
  await h.sandbox.renderTLSSettings();
  h.API.savePanelSettings = payload => { assert.equal(payload.panel_domain, 'panel.example.test'); return pending.promise; };
  const save = h.field('p-cert-save').onclick();
  await h.edit('p-panel-domain', 'draft.example.test');
  pending.resolve({});
  await save; await flush();
  assert.equal(h.field('p-panel-domain').value, 'draft.example.test');
  assert.equal(h.field('p-cert-save').disabled, false);
  assert.equal(h.field('p-cert-save').textContent, '保存设置');
});

test('TLS certificate issue compares drafts with the latest successfully saved settings', async () => {
  const h = settingsHarness('settings-tls'), pending = deferred();
  await h.sandbox.renderTLSSettings();
  let issueCalls = 0;
  h.API.savePanelSettings = () => pending.promise;
  h.API.requestPanelCertificate = async () => { issueCalls++; return { staging_tested: true }; };
  await h.edit('p-panel-domain', 'new-panel.example.test');
  const save = h.field('p-cert-save').onclick();
  await h.edit('p-panel-domain', 'panel.example.test');
  pending.resolve({ ...h.tls, panel_domain: 'new-panel.example.test' });
  await save; await flush();
  await h.field('p-cert-issue').onclick();
  assert.equal(issueCalls, 0);
  assert.ok(h.messages.includes('请先点击“保存设置”，再申请证书'));
});

test('a TLS certificate response keeps newer token edits and restores its action button', async () => {
  const h = settingsHarness('settings-tls'), pending = deferred();
  await h.sandbox.renderTLSSettings();
  h.API.requestPanelCertificate = () => pending.promise;
  const issue = h.field('p-cert-issue').onclick();
  await h.edit('p-acme-token', 'replacement-dns-token-draft');
  pending.resolve({ staging_tested: true });
  await issue; await flush();
  assert.equal(h.field('p-acme-token').value, 'replacement-dns-token-draft');
  assert.equal(h.field('p-cert-issue').disabled, false);
  assert.equal(h.field('p-cert-issue').textContent, '申请证书');
});

for (const action of ['save', 'issue']) {
  test(`an older TLS ${action} response cannot reset a reopened form's pending action`, async () => {
    const h = settingsHarness('settings-tls'), oldPending = deferred(), newPending = deferred();
    const method = action === 'save' ? 'savePanelSettings' : 'requestPanelCertificate';
    const buttonID = action === 'save' ? 'p-cert-save' : 'p-cert-issue';
    const busyText = action === 'save' ? '保存中…' : '申请中…';
    await h.sandbox.renderTLSSettings();
    h.API[method] = () => oldPending.promise;
    const oldAction = h.field(buttonID).onclick();
    await h.sandbox.renderTLSSettings();
    h.API[method] = () => newPending.promise;
    const newAction = h.field(buttonID).onclick();
    oldPending.resolve({ staging_tested: true });
    await oldAction; await flush();
    assert.equal(h.field(buttonID).disabled, true);
    assert.equal(h.field(buttonID).textContent, busyText);
    await h.edit('p-acme-token', 'current-form-draft');
    newPending.resolve({ staging_tested: true });
    await newAction; await flush();
    assert.equal(h.field(buttonID).disabled, false);
  });
}
