'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const vm = require('node:vm');

function deferred() {
  let resolve, reject;
  const promise = new Promise((fulfill, fail) => { resolve = fulfill; reject = fail; });
  return { promise, resolve, reject };
}

function settingsHarness() {
  const elements = new Map(), styles = new Map(), saved = [], messages = [], reads = [];
  const settings = { ui_radius: 6, traffic_billing_mode: 'bidirectional', traffic_reset_day: 1,
    probe_timeout_ms: 8000, ping_cache_minutes: 5, schedule_timezone_offset: 480,
    log_enabled: true, log_level: 'info', log_retention_days: 30, log_batch_size: 50,
    log_retry_count: 3, log_retry_backoff_ms: 100 };
  const attrs = text => Object.fromEntries([...text.matchAll(/([\w-]+)(?:="([^"]*)")?/g)]
    .map(([, key, value]) => [key, value === undefined ? true : value]));
  const createElement = (tag, attributes = {}) => {
    const listeners = new Map(), classes = new Set(String(attributes.class || '').split(/\s+/));
    const element = {
      tagName: tag.toUpperCase(), id: attributes.id || '', isConnected: true,
      value: String(attributes.value ?? ''), checked: attributes.checked === true,
      disabled: attributes.disabled === true, textContent: '', dataset: {}, children: [],
      classList: { toggle(name, active) { if (active) classes.add(name); else classes.delete(name); }, contains: name => classes.has(name) },
      setAttribute() {},
      addEventListener(name, listener) { if (!listeners.has(name)) listeners.set(name, []); listeners.get(name).push(listener); },
      dispatch(name) { for (const listener of listeners.get(name) || []) listener({ target: this }); },
      closest() { return null; },
      querySelector(selector) {
        if (selector.startsWith('#')) return elements.get(selector.slice(1)) || null;
        return this.children.find(child => child.classList.contains(selector.slice(1))) || null;
      },
      querySelectorAll(selector) {
        if (selector.includes('[data-settings-section]')) return this.children.filter(child => child.dataset.settingsSection);
        if (selector.includes('[data-setting-choice]')) return [];
        if (selector.includes('input') || selector.includes('select') || selector.includes('textarea')) return [...elements.values()].filter(child => ['INPUT', 'SELECT', 'TEXTAREA'].includes(child.tagName));
        return [];
      },
    };
    for (const [key, value] of Object.entries(attributes)) if (key.startsWith('data-')) element.dataset[key.slice(5).replace(/-([a-z])/g, (_, letter) => letter.toUpperCase())] = value;
    let markup = '';
    const parse = html => {
      markup = String(html);
      element.children = [];
      for (const [, childTag, text] of markup.matchAll(/<([a-z][\w-]*)\b([^>]*)>/gi)) {
        const attributes = attrs(text);
        if (!attributes.id && !attributes['data-settings-section'] && !String(attributes.class || '').includes('settings-content') && !String(attributes.class || '').includes('settings-section-nav')) continue;
        const child = createElement(childTag, attributes);
        element.children.push(child);
      }
      for (const [, text, contents] of markup.matchAll(/<select\b([^>]*)>([\s\S]*?)<\/select>/g)) {
        const child = elements.get(attrs(text).id);
        if (!child) continue;
        const options = [...contents.matchAll(/<option\b([^>]*)>/g)].map(([, attributes]) => attrs(attributes));
        child.value = String((options.find(option => option.selected === true) || options[0] || {}).value || '');
      }
    };
    Object.defineProperty(element, 'innerHTML', { get: () => markup, set: parse });
    Object.defineProperty(element, 'outerHTML', { set: parse });
    if (element.id) {
      const old = elements.get(element.id);
      if (old) old.isConnected = false;
      elements.set(element.id, element);
    }
    return element;
  };
  const page = createElement('div', { id: 'page-global-settings' });
  const sandbox = {
    console, Router: { current: 'global-settings' }, esc: value => String(value ?? ''),
    document: {
      getElementById: id => elements.get(id) || null,
      querySelector: () => null,
      documentElement: { style: { setProperty(name, value) { styles.set(name, value); } } },
    },
    API: {
      async getSystemSettings() { return reads.length ? reads.shift() : { ...settings }; },
      async saveSystemSettings(payload) { saved.push(JSON.parse(JSON.stringify(payload))); Object.assign(settings, payload); return { ...settings }; },
    },
    Toast: { success: message => messages.push(message), error: message => messages.push(message) },
  };
  vm.createContext(sandbox);
  vm.runInContext(fs.readFileSync(path.join(__dirname, '../web/static/js/pages/global-settings.js'), 'utf8'), sandbox);
  return { sandbox, page, settings, saved, messages, reads, styles, input: id => elements.get(id),
    edit(id, value) { const input = elements.get(id); input.value = String(value); input.dispatch('input'); },
    chooseSection(section) {
      const button = page.querySelectorAll('[data-settings-section]').find(button => button.dataset.settingsSection === section);
      assert.ok(button, `missing ${section} navigation`); button.onclick();
    },
  };
}

const flush = async () => { for (let index = 0; index < 5; index++) await Promise.resolve(); };

test('a delayed settings read cannot undo a successful save or leak its old UI value into a logs save', async () => {
  const h = settingsHarness(), pending = deferred();
  await h.sandbox.loadAppliedSystemSettings();
  h.reads.push(pending.promise);
  h.sandbox.renderGlobalSettings();
  h.edit('setting-ui-radius', 18);
  await h.input('settings-save').onclick();
  assert.equal(h.settings.ui_radius, 18);
  pending.resolve({ ...h.settings, ui_radius: 6 });
  await flush();
  assert.equal(h.input('setting-ui-radius').value, '18');
  assert.equal(h.styles.get('--ui-radius'), '18px');
  h.chooseSection('logs');
  await h.input('settings-save').onclick();
  assert.equal(h.settings.ui_radius, 18);
  assert.equal(Object.hasOwn(h.saved[1], 'ui_radius'), false, 'the logs form must not overwrite unrelated settings');
});

test('an unsaved UI draft survives a status read and a round trip through the logs section', async () => {
  const h = settingsHarness(), pending = deferred();
  await h.sandbox.loadAppliedSystemSettings();
  h.reads.push(pending.promise);
  h.sandbox.renderGlobalSettings();
  h.edit('setting-ui-radius', 18);
  pending.resolve({ ...h.settings, log_retention_days: 60 });
  await flush();
  assert.equal(h.input('setting-ui-radius').value, '18');
  h.chooseSection('logs');
  assert.equal(h.input('setting-log-retention').value, '60');
  h.chooseSection('system-ui');
  assert.equal(h.input('setting-ui-radius').value, '18');
  await h.input('settings-save').onclick();
  assert.equal(Object.hasOwn(h.saved[0], 'log_retention_days'), false);
});

test('an edit made during a save remains a draft after that save completes', async () => {
  const h = settingsHarness(), pending = deferred();
  await h.sandbox.loadAppliedSystemSettings();
  h.sandbox.renderGlobalSettings();
  await flush();
  h.edit('setting-ui-radius', 18);
  h.sandbox.API.saveSystemSettings = payload => { h.saved.push({ ...payload }); return pending.promise; };
  const save = h.input('settings-save').onclick();
  h.edit('setting-ui-radius', 20);
  pending.resolve({ ...h.settings, ui_radius: 18 });
  await save;
  assert.equal(h.input('setting-ui-radius').value, '20');
  assert.equal(h.styles.get('--ui-radius'), '18px');
});

test('an old bootstrap settings response cannot replace the newer settings page snapshot', async () => {
  const h = settingsHarness(), pending = deferred();
  h.reads.push(pending.promise, { ...h.settings, ui_radius: 18 });
  const bootstrap = h.sandbox.loadAppliedSystemSettings();
  h.sandbox.renderGlobalSettings();
  await flush();
  pending.resolve({ ...h.settings, ui_radius: 6 });
  await bootstrap;
  assert.equal(h.input('setting-ui-radius').value, '18');
  assert.equal(h.styles.get('--ui-radius'), '18px');
});

test('the logs form submits every visible policy checkbox without unrelated UI fields', async () => {
  const h = settingsHarness();
  await h.sandbox.loadAppliedSystemSettings();
  h.sandbox.renderGlobalSettings();
  await flush();
  h.chooseSection('logs');
  const policies = [
    ['node', 'node'], ['category', 'category'], ['status', 'status'], ['ip', 'client_ip'],
    ['ua', 'ua'], ['upstream-ua', 'upstream_ua'], ['backend-address', 'backend_address'], ['timeline', 'timeline'],
  ];
  const resourceTypes = ['playback', 'video', 'image', 'metadata', 'api', 'auth', 'subtitle', 'asset', 'websocket'];
  for (const [id] of policies) {
    for (const action of ['write', 'display']) h.input(`setting-${action}-${id}`).checked = false;
  }
  for (const id of resourceTypes) h.input(`setting-write-${id}`).checked = false;
  await h.input('settings-save').onclick();
  for (const [, field] of policies) {
    for (const action of ['write', 'display']) assert.equal(h.saved[0][`log_${action}_${field}`], false, `${action} ${field}`);
  }
  for (const id of resourceTypes) assert.equal(h.saved[0][`log_write_${id}`], false, id);
  assert.equal(Object.hasOwn(h.saved[0], 'ui_radius'), false);
});

test('saving a no-reset billing cycle submits zero and retains the current log policy', async () => {
  const h = settingsHarness();
  await h.sandbox.loadAppliedSystemSettings();
  h.sandbox.renderGlobalSettings();
  await flush();
  h.edit('setting-traffic-reset-day', 0);
  await h.input('settings-save').onclick();
  assert.equal(h.saved[0].traffic_reset_day, 0);
  assert.equal(h.settings.traffic_reset_day, 0);
  assert.equal(h.settings.log_retention_days, 30);
  assert.equal(Object.hasOwn(h.saved[0], 'log_retention_days'), false);
});
