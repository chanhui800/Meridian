'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const vm = require('node:vm');

const script = fs.readFileSync(path.join(__dirname, '..', 'web', 'static', 'js', 'theme.js'), 'utf8');

function makeHarness(stored) {
  const attributes = new Map();
  const buttonAttributes = new Map();
  const listeners = new Map();
  const writes = [];
  const documentElement = {
    style: {},
    getAttribute(name) { return attributes.get(name) || null; },
    setAttribute(name, value) { attributes.set(name, String(value)); },
  };
  const button = {
    dataset: {},
    title: '',
    setAttribute(name, value) { buttonAttributes.set(name, String(value)); },
    addEventListener(name, handler) { listeners.set(name, handler); },
  };
  const document = {
    readyState: 'complete',
    documentElement,
    getElementById(id) { return id === 'theme-toggle' ? button : null; },
    addEventListener() {},
  };
  const window = {
    document,
    localStorage: {
      getItem(key) { return key === 'meridian-theme' ? stored : null; },
      setItem(key, value) { writes.push([key, value]); stored = value; },
    },
    CustomEvent: function CustomEvent(type, options) { this.type = type; this.detail = options.detail; },
    dispatchEvent() {},
  };
  const sandbox = { window };
  vm.createContext(sandbox);
  vm.runInContext(script, sandbox, { filename: 'theme.js' });
  return { window, documentElement, button, buttonAttributes, listeners, writes };
}

test('stored light theme is applied before interaction and updates the toggle label', () => {
  const harness = makeHarness('light');
  assert.equal(harness.documentElement.getAttribute('data-theme'), 'light');
  assert.equal(harness.documentElement.style.colorScheme, 'light');
  assert.equal(harness.buttonAttributes.get('aria-pressed'), 'true');
  assert.equal(harness.buttonAttributes.get('aria-label'), '切换到黑色背景');
  assert.equal(harness.button.dataset.themeBound, 'true');
});

test('theme toggle switches to dark and persists the choice', () => {
  const harness = makeHarness('light');
  harness.listeners.get('click')();
  assert.equal(harness.documentElement.getAttribute('data-theme'), 'dark');
  assert.equal(harness.buttonAttributes.get('aria-pressed'), 'false');
  assert.equal(harness.buttonAttributes.get('aria-label'), '切换到白色背景');
  assert.deepEqual(harness.writes, [['meridian-theme', 'dark']]);
});

test('unknown stored values fall back to the original dark theme', () => {
  const harness = makeHarness('unexpected');
  assert.equal(harness.window.MeridianTheme.current(), 'dark');
  assert.equal(harness.documentElement.getAttribute('data-theme'), 'dark');
  assert.deepEqual(harness.writes, []);
});
