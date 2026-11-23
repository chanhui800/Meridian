'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const vm = require('node:vm');

test('website uses a versioned script path with the current app source', () => {
  const websiteRoot = path.join(__dirname, '../website');
  const html = fs.readFileSync(path.join(websiteRoot, 'index.html'), 'utf8');
  const script = html.match(/<script src="(app-v[^"]+\.js)"><\/script>/);

  assert.ok(script, 'index.html should use a versioned app script path');
  assert.equal(
    fs.readFileSync(path.join(websiteRoot, script[1]), 'utf8'),
    fs.readFileSync(path.join(websiteRoot, 'app.js'), 'utf8'),
  );
});

test('website release links and cache versions match the announced release', () => {
  const html = fs.readFileSync(path.join(__dirname, '../website/index.html'), 'utf8');
  const release = html.match(/class="hero-badge"[^>]*>[\s\S]*?(v\d+\.\d+\.\d+) 已发布/);
  assert.ok(release, 'the release badge should announce a version');
  assert.match(html, new RegExp(`<script src="app-${release[1].replaceAll('.', '\\.')}\\.js">`));
  assert.ok(html.includes(`href="style.css?v=${release[1].slice(1)}"`));
  const releaseLinks = [...html.matchAll(/<a\b[^>]*href="([^\"]+)"[^>]*>(下载正式版|Release 与校验文件)<\/a>/g)];
  assert.equal(releaseLinks.length, 2);
  for (const link of releaseLinks) assert.ok(link[1].endsWith(`/releases/tag/${release[1]}`));
});

test('website generic proxy client fields produce the documented request and canonical route', () => {
  const html = fs.readFileSync(path.join(__dirname, '../website/index.html'), 'utf8');
  assert.match(html, /<a href="#generic-proxy">通用反代<\/a>/);
  const section = html.match(/<section id="generic-proxy"[^>]*>([\s\S]*?)<\/section>/)?.[1];
  assert.ok(section, 'generic proxy should have its own navigable section');
  const fields = section.match(/<dl class="client-fields">([\s\S]*?)<\/dl>/)?.[1];
  const server = fields?.match(/<dt>服务器地址<\/dt><dd><code>([^<]+)<\/code>/)?.[1];
  const upstream = fields?.match(/<dt>路径<\/dt><dd><code>([^<]+)<\/code>/)?.[1];
  assert.ok(server && upstream, 'both client fields should be visible');
  const serverURL = new URL(server);
  const upstreamURL = new URL(upstream);
  assert.equal(serverURL.protocol, 'https:');
  assert.ok(serverURL.port, 'the gateway example should preserve its explicit port');
  assert.equal(serverURL.pathname, '/');
  assert.ok(['http:', 'https:'].includes(upstreamURL.protocol));
  assert.ok(upstreamURL.port, 'the upstream example should preserve its explicit port');
  assert.equal(upstreamURL.pathname, '/emby');
  const request = section.match(/<code class="request-example">([^<]+)<\/code>/)?.[1];
  assert.equal(request, `${server}/${upstream}/Items`);
  const canonicalPaths = [...section.matchAll(/<code>(\/_proxy\/v1\/(https?)\/([^/]+)(\/[^<]*))<\/code>/g)];
  assert.equal(canonicalPaths.length, 2);
  const httpPath = canonicalPaths.find(match => match[2] === upstreamURL.protocol.slice(0, -1));
  assert.ok(httpPath);
  assert.equal(`${httpPath[2]}://${httpPath[3]}${httpPath[4]}`, upstream);
  assert.ok(canonicalPaths.some(match => match[2] === 'https'));
  const address = section.match(/<dl class="source-address">[\s\S]*?<code>([^<]+)<\/code>/)?.[1];
  assert.ok(address, 'multi-source history should include a server address example');
  assert.equal(address, new URL(address).origin, 'the displayed server address should exclude mount paths');
});

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
