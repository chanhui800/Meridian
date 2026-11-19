'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const vm = require('node:vm');

function shellHarness(stored, mobile = false) {
  const elements = new Map();
  const windowListeners = new Map();
  const writes = [];
  const element = id => {
    if (elements.has(id)) return elements.get(id);
    const classes = new Set(), attributes = new Map(), listeners = new Map();
    const item = {
      id, attributes, listeners, style: {}, dataset: {},
      classList: {
        add(...values) { values.forEach(value => classes.add(value)); },
        remove(...values) { values.forEach(value => classes.delete(value)); },
        contains(value) { return classes.has(value); },
        toggle(value, active) { if (active) classes.add(value); else classes.delete(value); },
      },
      setAttribute(name, value) { attributes.set(name, String(value)); },
      removeAttribute(name) { attributes.delete(name); },
      addEventListener(name, handler) { listeners.set(name, handler); },
      querySelectorAll() { return []; },
    };
    elements.set(id, item);
    return item;
  };
  const document = {
    getElementById: element,
    querySelector(selector) { return element(selector); },
    querySelectorAll() { return []; },
    addEventListener() {},
    body: element('body'),
  };
  const window = {
    matchMedia(query) { return { matches: query === '(max-width: 768px)' && mobile }; },
    addEventListener(name, handler) { windowListeners.set(name, handler); },
    localStorage: {
      getItem() { return stored; },
      setItem(key, value) { writes.push([key, value]); stored = value; },
    },
  };
  const sandbox = { window, document, console, closeModal() {}, API: { checkSetup: () => new Promise(() => {}) } };
  vm.createContext(sandbox);
  vm.runInContext(fs.readFileSync(path.join(__dirname, '../web/static/js/app.js'), 'utf8'), sandbox);
  return { element, writes, resize: () => windowListeners.get('resize')(), setMobile: value => { mobile = value; } };
}

test('desktop starts expanded and an explicit collapse persists across resize', () => {
  const h = shellHarness(null);
  const shell = h.element('app-shell'), toggle = h.element('sidebar-toggle');
  assert.equal(shell.classList.contains('sidebar-expanded'), true);
  assert.equal(toggle.attributes.get('aria-label'), '收起侧边栏');
  toggle.listeners.get('click')();
  assert.equal(shell.classList.contains('sidebar-expanded'), false);
  assert.equal(toggle.attributes.get('aria-label'), '展开侧边栏');
  h.resize();
  assert.equal(shell.classList.contains('sidebar-expanded'), false);
  assert.deepEqual(h.writes, [['meridian-sidebar-expanded', 'false']]);
});

test('a stored collapsed rail stays collapsed until the toggle is used', () => {
  const h = shellHarness('false');
  const shell = h.element('app-shell');
  assert.equal(shell.classList.contains('sidebar-expanded'), false);
  assert.equal(h.element('.sidebar').listeners.has('pointerenter'), false);
  h.element('sidebar-toggle').listeners.get('click')();
  assert.equal(shell.classList.contains('sidebar-expanded'), true);
  assert.deepEqual(h.writes, [['meridian-sidebar-expanded', 'true']]);
});

test('mobile drawer changes do not overwrite the desktop sidebar choice', () => {
  const h = shellHarness('true', true);
  assert.equal(h.element('app-shell').classList.contains('sidebar-expanded'), false);
  h.element('sidebar-toggle').listeners.get('click')();
  assert.equal(h.element('app-shell').classList.contains('sidebar-expanded'), true);
  assert.equal(h.element('sidebar-toggle').attributes.get('aria-label'), '关闭导航栏');
  assert.deepEqual(h.writes, []);
  h.setMobile(false);
  h.resize();
  assert.equal(h.element('app-shell').classList.contains('sidebar-expanded'), true);
});
