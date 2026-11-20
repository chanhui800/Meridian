'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const vm = require('node:vm');

function copyHarness(writeText) {
  const listeners = new Map();
  const timers = new Map();
  const writes = [];
  let timerID = 0;
  let selectedCode = null;
  const copyBtn = {
    textContent: '复制', disabled: false,
    addEventListener(name, handler) { listeners.set(name, handler); },
  };
  const installCode = {
    textContent: 'services:\n  meridian:\n    image: ghcr.io/chanhui800/meridian:latest\n',
    focus() { this.focused = true; },
  };
  const copyFeedback = { textContent: '', hidden: true };
  const selection = { removeAllRanges() {}, addRange(range) { selectedCode = range.target; } };
  const navigator = writeText === null ? {} : {
    clipboard: { writeText(text) { writes.push(text); return writeText(text); } },
  };
  const lightbox = { addEventListener() {}, hidden: true };
  const elements = { copyBtn, installCode, copyFeedback, lightbox, lightboxImg: {}, lightboxClose: { addEventListener() {} } };
  const sandbox = {
    navigator,
    window: { getSelection: () => selection },
    setTimeout(callback) { timers.set(++timerID, callback); return timerID; },
    clearTimeout(id) { timers.delete(id); },
    document: {
      body: { style: {} },
      addEventListener(name, handler) { if (name === 'DOMContentLoaded') handler(); },
      getElementById(id) { return elements[id]; },
      querySelectorAll() { return []; },
      createRange() { return { selectNodeContents(target) { this.target = target; } }; },
    },
  };
  vm.createContext(sandbox);
  vm.runInContext(fs.readFileSync(path.join(__dirname, '../website/app.js'), 'utf8'), sandbox);
  return {
    copyBtn, installCode, copyFeedback, writes, navigator,
    async click() { await listeners.get('click')(); await new Promise(resolve => setImmediate(resolve)); },
    selectedCode: () => selectedCode,
    settleTimers() { for (const callback of timers.values()) callback(); timers.clear(); },
  };
}

test('website copy confirms success only after the exact compose text is written', async () => {
  let resolveWrite;
  const h = copyHarness(() => new Promise(resolve => { resolveWrite = resolve; }));
  const pendingClick = h.click();
  assert.notEqual(h.copyBtn.textContent, '已复制');
  resolveWrite();
  await pendingClick;
  assert.deepEqual(h.writes, [h.installCode.textContent]);
  assert.equal(h.copyBtn.textContent, '已复制');
  assert.equal(h.copyFeedback.hidden, true);
  assert.equal(h.selectedCode(), null);
  assert.equal(h.copyBtn.disabled, false);
  h.settleTimers();
  assert.equal(h.copyBtn.textContent, '复制');
});

test('website copy permission denial offers manual selection without reporting success', async () => {
  const h = copyHarness(() => Promise.reject(new Error('NotAllowedError')));
  await h.click();
  assert.notEqual(h.copyBtn.textContent, '已复制');
  assert.equal(h.copyFeedback.hidden, false);
  assert.match(h.copyFeedback.textContent, /手动复制/);
  assert.equal(h.selectedCode(), h.installCode);
  assert.equal(h.copyBtn.disabled, false);
});

test('website copy supports manual selection when the Clipboard API is unavailable', async () => {
  const h = copyHarness(null);
  await h.click();
  assert.deepEqual(h.writes, []);
  assert.notEqual(h.copyBtn.textContent, '已复制');
  assert.equal(h.copyFeedback.hidden, false);
  assert.equal(h.selectedCode(), h.installCode);
  assert.equal(h.copyBtn.disabled, false);
});

test('website copy catches a synchronous clipboard failure and remains retryable', async () => {
  const h = copyHarness(() => { throw new Error('SecurityError'); });
  await h.click();
  assert.notEqual(h.copyBtn.textContent, '已复制');
  assert.equal(h.copyFeedback.hidden, false);
  assert.equal(h.selectedCode(), h.installCode);
  assert.equal(h.copyBtn.disabled, false);
  h.navigator.clipboard.writeText = () => Promise.resolve();
  await h.click();
  assert.equal(h.copyBtn.textContent, '已复制');
  assert.equal(h.copyFeedback.hidden, true);
});
