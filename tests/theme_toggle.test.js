'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const vm = require('node:vm');

const script = fs.readFileSync(path.join(__dirname, '..', 'web', 'static', 'js', 'theme.js'), 'utf8');

function makeHarness(stored, systemLight = false) {
  const attributes = new Map();
  const buttonAttributes = new Map();
  const listeners = new Map();
  const mediaListeners = new Map();
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
    matchMedia(query) {
      assert.equal(query, '(prefers-color-scheme: light)');
      return {
        get matches() { return systemLight; },
        addEventListener(name, handler) { mediaListeners.set(name, handler); },
      };
    },
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
  return {
    window,
    documentElement,
    button,
    buttonAttributes,
    listeners,
    writes,
    setSystemLight(value) {
      systemLight = value;
      mediaListeners.get('change')?.({ matches: value });
    },
  };
}

test('stored light theme is applied before interaction and updates the toggle label', () => {
  const harness = makeHarness('light');
  assert.equal(harness.documentElement.getAttribute('data-theme'), 'light');
  assert.equal(harness.documentElement.getAttribute('data-theme-mode'), 'light');
  assert.equal(harness.documentElement.style.colorScheme, 'light');
  assert.equal(harness.buttonAttributes.get('aria-pressed'), 'true');
  assert.equal(harness.buttonAttributes.get('aria-label'), '主题：浅色；点击切换到深色主题');
  assert.equal(harness.button.dataset.themeBound, 'true');
});

test('theme toggle switches to dark and persists the choice', () => {
  const harness = makeHarness('light');
  harness.listeners.get('click')();
  assert.equal(harness.documentElement.getAttribute('data-theme'), 'dark');
  assert.equal(harness.documentElement.getAttribute('data-theme-mode'), 'dark');
  assert.equal(harness.buttonAttributes.get('aria-pressed'), 'false');
  assert.equal(harness.buttonAttributes.get('aria-label'), '主题：深色；点击切换到跟随设备');
  assert.deepEqual(harness.writes, [['meridian-theme', 'dark']]);
});

test('missing or unknown stored values follow the device theme', () => {
  const harness = makeHarness('unexpected', true);
  assert.equal(harness.window.MeridianTheme.mode(), 'system');
  assert.equal(harness.window.MeridianTheme.current(), 'light');
  assert.equal(harness.documentElement.getAttribute('data-theme-mode'), 'system');
  assert.equal(harness.documentElement.getAttribute('data-theme'), 'light');
  assert.equal(harness.buttonAttributes.get('aria-pressed'), 'mixed');
  assert.match(harness.buttonAttributes.get('aria-label'), /跟随设备（当前浅色）/);
  assert.deepEqual(harness.writes, []);
});

test('system mode reacts to device theme changes without persisting', () => {
  const harness = makeHarness(null, false);
  assert.equal(harness.window.MeridianTheme.current(), 'dark');
  harness.setSystemLight(true);
  assert.equal(harness.window.MeridianTheme.mode(), 'system');
  assert.equal(harness.window.MeridianTheme.current(), 'light');
  assert.equal(harness.documentElement.style.colorScheme, 'light');
  assert.deepEqual(harness.writes, []);
});

test('stored manual mode ignores later device theme changes', () => {
  const harness = makeHarness('dark', false);
  harness.setSystemLight(true);
  assert.equal(harness.window.MeridianTheme.current(), 'dark');
  assert.equal(harness.documentElement.getAttribute('data-theme'), 'dark');
  assert.deepEqual(harness.writes, []);
});

test('theme toggle cycles through system, light and dark modes', () => {
  const harness = makeHarness(null, false);
  harness.listeners.get('click')();
  harness.listeners.get('click')();
  harness.listeners.get('click')();
  assert.equal(harness.window.MeridianTheme.mode(), 'system');
  assert.equal(harness.window.MeridianTheme.current(), 'dark');
  assert.deepEqual(harness.writes, [
    ['meridian-theme', 'light'],
    ['meridian-theme', 'dark'],
    ['meridian-theme', 'system'],
  ]);
});

test('theme toggle starts with the opposite manual theme when the device is light', () => {
  const harness = makeHarness(null, true);
  assert.match(harness.buttonAttributes.get('aria-label'), /点击切换到深色主题/);
  harness.listeners.get('click')();
  harness.listeners.get('click')();
  harness.listeners.get('click')();
  assert.equal(harness.window.MeridianTheme.mode(), 'system');
  assert.equal(harness.window.MeridianTheme.current(), 'light');
  assert.deepEqual(harness.writes, [
    ['meridian-theme', 'dark'],
    ['meridian-theme', 'light'],
    ['meridian-theme', 'system'],
  ]);
});
