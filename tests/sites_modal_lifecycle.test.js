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

function modalHarness() {
  const elements = new Map(), frames = [], errors = [], infos = [], focuses = [];
  let scrollY = 0;
  let document;
  const decode = value => String(value || '').replaceAll('&amp;', '&').replaceAll('&quot;', '"').replaceAll('&#39;', "'");
  const attributes = text => Object.fromEntries([...text.matchAll(/([\w-]+)(?:="([^"]*)")?/g)]
    .map(([, name, value]) => [name, value === undefined ? true : decode(value)]));
  function createElement(id, attrs = {}) {
    const classes = new Set(String(attrs.class || '').split(/\s+/).filter(Boolean));
    const listeners = new Map();
    const item = {
      id, parentElement: null, ownedIDs: [], tagName: '', type: attrs.type || '',
      value: attrs.value || '', textContent: '', disabled: attrs.disabled === true,
      checked: attrs.checked === true, isConnected: true, tabIndex: 0, style: {}, dataset: {},
      classList: {
        add(...values) { values.forEach(value => classes.add(value)); },
        remove(...values) { values.forEach(value => classes.delete(value)); },
        contains(value) { return classes.has(value); },
        toggle(value, active) { if (active) classes.add(value); else classes.delete(value); },
      },
      get className() { return [...classes].join(' '); },
      set className(value) { classes.clear(); String(value).split(/\s+/).filter(Boolean).forEach(value => classes.add(value)); },
      setAttribute() {}, removeAttribute() {},
      addEventListener(type, listener) { if (!listeners.has(type)) listeners.set(type, []); listeners.get(type).push(listener); },
      click() { let result; for (const listener of listeners.get('click') || []) result = listener({ target: this }); return result; },
      replaceChildren() { this.innerHTML = ''; },
      append(...values) { values.forEach(value => { if (typeof value === 'object') value.parentElement = this; else this.textContent += String(value); }); },
      appendChild(child) { child.parentElement = this; return child; },
      getClientRects() { return this.isConnected ? [{}] : []; },
      matches() { return ['INPUT', 'SELECT', 'TEXTAREA'].includes(this.tagName) && this.type !== 'hidden'; },
      contains(other) { for (; other; other = other.parentElement) if (other === this) return true; return false; },
      focus(options) {
        document.activeElement = this; focuses.push(this);
        if (!options?.preventScroll && Number.isFinite(this.fixtureTop)) scrollY = this.fixtureTop;
      },
      blur() { if (document.activeElement === this) document.activeElement = document.body; },
      querySelectorAll(selector) {
        if (selector.includes('input,') || selector.includes('button,')) {
          return [...elements.values()].filter(other => other !== this && this.contains(other)
            && ['INPUT', 'SELECT', 'TEXTAREA', 'BUTTON'].includes(other.tagName));
        }
        return [];
      },
    };
    let markup = '';
    Object.defineProperty(item, 'innerHTML', {
      get: () => markup,
      set(html) {
        markup = String(html);
        item.ownedIDs.forEach(ownedID => {
          const old = elements.get(ownedID);
          if (old) old.isConnected = false;
          elements.delete(ownedID);
        });
        item.ownedIDs = [];
        for (const [, tag, text] of markup.matchAll(/<([a-z][\w-]*)\b([^>]*)>/gi)) {
          const attrs = attributes(text);
          if (!attrs.id) continue;
          const child = createElement(attrs.id, attrs);
          child.tagName = tag.toUpperCase();
          child.parentElement = item;
          item.ownedIDs.push(child.id);
        }
        for (const [, text, contents] of markup.matchAll(/<(?:select|textarea)\b([^>]*)>([\s\S]*?)<\/(?:select|textarea)>/g)) {
          const control = elements.get(attributes(text).id);
          if (!control) continue;
          if (control.tagName === 'TEXTAREA') control.value = contents;
          else {
            const options = [...contents.matchAll(/<option\b([^>]*)>/g)].map(([, attrs]) => attributes(attrs));
            control.value = String((options.find(option => option.selected === true) || options[0] || {}).value || '');
          }
        }
      },
    });
    elements.set(id, item);
    return item;
  }
  const element = id => elements.get(id) || createElement(id);
  document = {
    getElementById: element, querySelector: element, addEventListener() {},
    createElement(tag) { const result = createElement(`fixture-${elements.size}`); result.tagName = tag.toUpperCase(); return result; },
    querySelectorAll(selector) { return selector === '.page' ? [element('page-sites'), element('page-account')] : []; },
    body: element('body'),
  };
  document.activeElement = document.body;
  element('modal').parentElement = element('modal-overlay');
  element('modal-body').parentElement = element('modal');
  element('modal-footer').parentElement = element('modal');
  const sandbox = {
    document, console, URL, location: { hash: '#sites', origin: 'https://controller.example.test:9090', port: '9090' },
    getComputedStyle() { return { visibility: 'visible' }; },
    confirm() { return true; },
    matchMedia() { return { matches: false }; }, addEventListener() {},
    scrollTo({ top }) { scrollY = top; }, get scrollY() { return scrollY; },
    clearTimeout() {}, clearInterval() {}, setTimeout() { return 1; },
    requestAnimationFrame(callback) { frames.push(callback); },
    API: {
      checkSetup() { return new Promise(() => {}); },
      async ingressCapabilities() { return { host_only_available: false, upstream_headers_available: true }; },
      async getActiveWatchHistory() { return { items: [] }; },
      async listSiteIconPacks() { return { packs: [] }; },
      async getSiteIconPack() { return { icons: [] }; },
      async updateSite() {},
    },
    Toast: { error(message) { errors.push(message); }, info(message) { infos.push(message); }, success() {} },
    esc: value => String(value ?? ''),
    watchHistoryNormalizeItems: response => response.items,
    watchHistoryDetailsHTML: item => `<p data-fixture-live="${item.id}">${item.name}</p>`,
    watchHistoryBindDetailImages() {},
  };
  sandbox.window = sandbox;
  vm.createContext(sandbox);
  for (const relative of ['router.js', 'pages/sites.js', 'app.js']) {
    vm.runInContext(fs.readFileSync(path.join(__dirname, '../web/static/js', relative), 'utf8'), sandbox, { filename: relative });
  }
  vm.runInContext('Router.resolve()', sandbox);
  sandbox.loadSites = async () => {};
  const site = id => ({ id, name: `Site ${id}`, target_url: 'http://origin.example.test:8096', listen_port: 8096, ingress_mode: 'port', ua_mode: 'passthrough' });
  return {
    sandbox, element, document, frames, errors, infos, focuses, site,
    liveResult: id => ({ items: [{ id, name: `Media ${id}` }] }),
    navigate(route) { sandbox.location.hash = '#' + route; vm.runInContext('Router.resolve()', sandbox); },
  };
}

function bindRealHistoryDetails(h) {
  vm.runInContext(fs.readFileSync(path.join(__dirname, '../web/static/js/pages/watch-history.js'), 'utf8'), h.sandbox, { filename: 'watch-history.js' });
}

test('a live snapshot refresh follows stop intent and confirmation without reopening the modal', async () => {
  const h = modalHarness();
  bindRealHistoryDetails(h);
  const item = { id: 11, site_id: 1, title: 'Current playback', stopped_at_ms: 0 };
  h.sandbox.API.getActiveWatchHistory = async () => ({ items: [item] });
  let outcome = 'unconfirmed', calls = 0;
  h.sandbox.API.getWatchHistory = async () => {
    calls += 1;
    return { items: [{ ...item, client_stop_requested_at_ms: 1795212345000, client_stop_outcome: outcome, stopped_at_ms: outcome === 'confirmed' ? 1795212345010 : 0 }] };
  };
  await h.sandbox.showSiteLiveDetails(h.site(1));
  h.element('modal-close').focus();
  const focused = h.document.activeElement, frames = h.frames.length;
  await h.sandbox.refreshSiteLiveDetails([]);
  assert.equal(h.element('modal-title').textContent, 'Site 1 · 播放详情');
  assert.match(h.element('modal-body').innerHTML, /客户端已发起停止，源站未确认/);
  assert.doesNotMatch(h.element('modal-body').innerHTML, /源站确认停止<\/dt>|源站已确认/);
  assert.equal(h.document.activeElement, focused);
  assert.equal(h.frames.length, frames, 'snapshot updates must not reopen the modal or schedule autofocus');
  outcome = 'confirmed';
  await h.sandbox.refreshSiteLiveDetails([]);
  assert.match(h.element('modal-body').innerHTML, /源站已确认/);
  assert.match(h.element('modal-body').innerHTML, /源站确认停止<\/dt>/);
  await h.sandbox.refreshSiteLiveDetails([]);
  assert.equal(calls, 2, 'confirmed records need no further history lookup');
});

test('a newer active snapshot defeats a delayed old stop-history response', async () => {
  const h = modalHarness(), pending = deferred();
  bindRealHistoryDetails(h);
  const item = { id: 11, site_id: 1, title: 'Original playback', stopped_at_ms: 0 };
  h.sandbox.API.getActiveWatchHistory = async () => ({ items: [item] });
  h.sandbox.API.getWatchHistory = () => pending.promise;
  await h.sandbox.showSiteLiveDetails(h.site(1));
  const stopRead = h.sandbox.refreshSiteLiveDetails([]);
  await h.sandbox.refreshSiteLiveDetails([{ ...item, title: 'Resumed playback', client_stop_requested_at_ms: 0, client_stop_outcome: '' }]);
  const body = h.element('modal-body').innerHTML;
  pending.resolve({ items: [{ ...item, client_stop_requested_at_ms: 1795212345000, client_stop_outcome: 'unconfirmed' }] });
  await stopRead;
  assert.equal(h.element('modal-title').textContent, 'Site 1 · 正在播放');
  assert.equal(h.element('modal-body').innerHTML, body);
  assert.match(body, /Resumed playback/);
  assert.doesNotMatch(body, /客户端已发起停止，源站未确认/);
});

for (const nextOwner of ['edit', 'live', 'icons']) {
  test(`a late stop-history update leaves a newer ${nextOwner} modal, its footer and focus intact`, async () => {
    const h = modalHarness(), pending = deferred();
    bindRealHistoryDetails(h);
    h.sandbox.API.getActiveWatchHistory = async filters => ({ items: [{ id: Number(filters.site_id) + 10, site_id: Number(filters.site_id), title: `Media ${filters.site_id}` }] });
    h.sandbox.API.getWatchHistory = () => pending.promise;
    await h.sandbox.showSiteLiveDetails(h.site(1));
    const stopRead = h.sandbox.refreshSiteLiveDetails([]);
    if (nextOwner === 'edit') await h.sandbox.showSiteModal(h.site(2));
    else if (nextOwner === 'live') await h.sandbox.showSiteLiveDetails(h.site(2));
    else await h.sandbox.showSiteIconManager(h.site(2));
    const focus = h.element(nextOwner === 'edit' ? 'm-name' : 'modal-close');
    focus.value = 'Unsaved next owner';
    focus.focus();
    const title = h.element('modal-title').textContent, body = h.element('modal-body').innerHTML, footer = h.element('modal-footer').innerHTML;
    pending.resolve({ items: [{ id: 11, site_id: 1, title: 'Old stopped media', client_stop_requested_at_ms: 1795212345000, client_stop_outcome: 'unconfirmed' }] });
    await stopRead;
    assert.equal(h.element('modal-title').textContent, title);
    assert.equal(h.element('modal-body').innerHTML, body);
    assert.equal(h.element('modal-footer').innerHTML, footer);
    assert.equal(h.document.activeElement, focus);
    assert.equal(focus.value, 'Unsaved next owner');
  });
}

test('site live reads exclude pending stop intent but retain denied intent as active', async () => {
  const h = modalHarness();
  bindRealHistoryDetails(h);
  const item = { id: 11, site_id: 1, title: 'Stopping', client_stop_requested_at_ms: 1795212345000, stopped_at_ms: 0 };
  h.sandbox.API.getActiveWatchHistory = async () => ({ items: [{ ...item, client_stop_outcome: 'pending' }] });
  await h.sandbox.showSiteLiveDetails(h.site(1));
  assert.equal(h.element('modal-overlay').classList.contains('active'), false);
  assert.match(h.infos.at(-1), /没有正在播放/);
  h.sandbox.API.getActiveWatchHistory = async () => ({ items: [{ ...item, client_stop_outcome: 'denied' }] });
  await h.sandbox.showSiteLiveDetails(h.site(1));
  assert.equal(h.element('modal-overlay').classList.contains('active'), true);
  assert.equal(h.element('modal-title').textContent, 'Site 1 · 正在播放');
  assert.match(h.element('modal-body').innerHTML, /源站拒绝停止/);
});

test('a late live response preserves a newer site edit, its input, footer and focus', async () => {
  const h = modalHarness(), pending = deferred();
  h.sandbox.API.getActiveWatchHistory = () => pending.promise;
  const live = h.sandbox.showSiteLiveDetails(h.site(1), h.element('live-trigger'));
  await h.sandbox.showSiteModal(h.site(2));
  const name = h.element('m-name'), body = h.element('modal-body').innerHTML, footer = h.element('modal-footer').innerHTML;
  name.value = 'Unsaved Site 2';
  name.focus();
  pending.resolve(h.liveResult(1));
  await live;
  assert.equal(h.element('modal-title').textContent, '编辑站点');
  assert.equal(h.element('modal-body').innerHTML, body);
  assert.equal(h.element('modal-footer').innerHTML, footer);
  assert.equal(h.element('m-name'), name);
  assert.equal(name.value, 'Unsaved Site 2');
  assert.equal(h.document.activeElement, name);
});

test('starting an edit invalidates an older live request before its capability read completes', async () => {
  const h = modalHarness(), liveRead = deferred(), capabilityRead = deferred();
  h.sandbox.API.getActiveWatchHistory = () => liveRead.promise;
  h.sandbox.API.ingressCapabilities = () => capabilityRead.promise;
  const live = h.sandbox.showSiteLiveDetails(h.site(1));
  const edit = h.sandbox.showSiteModal(h.site(2));
  liveRead.resolve(h.liveResult(1));
  await live;
  assert.equal(h.element('modal-overlay').classList.contains('active'), false);
  capabilityRead.resolve({ host_only_available: false, upstream_headers_available: true });
  await edit;
  assert.equal(h.element('modal-title').textContent, '编辑站点');
});

test('a newer live intent wins even when requests finish in reverse order', async () => {
  const h = modalHarness(), first = deferred(), second = deferred();
  h.sandbox.API.getActiveWatchHistory = filters => filters.site_id === 1 ? first.promise : second.promise;
  const oldLive = h.sandbox.showSiteLiveDetails(h.site(1));
  const newLive = h.sandbox.showSiteLiveDetails(h.site(2));
  second.resolve(h.liveResult(2));
  await newLive;
  first.resolve(h.liveResult(1));
  await oldLive;
  assert.equal(h.element('modal-title').textContent, 'Site 2 · 正在播放');
  assert.match(h.element('modal-body').innerHTML, /Media 2/);
});

for (const method of ['showSiteIconManager', 'showSiteIconPackModal']) {
  test(`a late live request cannot overwrite ${method}`, async () => {
    const h = modalHarness(), pending = deferred();
    h.sandbox.API.getActiveWatchHistory = () => pending.promise;
    const live = h.sandbox.showSiteLiveDetails(h.site(1));
    await h.sandbox[method](h.site(2));
    const title = h.element('modal-title').textContent, footer = h.element('modal-footer').innerHTML;
    pending.resolve(h.liveResult(1));
    await live;
    assert.equal(h.element('modal-title').textContent, title);
    assert.equal(h.element('modal-footer').innerHTML, footer);
  });
}

test('a pending edit cannot overwrite a newer icon dialog', async () => {
  const h = modalHarness(), pending = deferred();
  h.sandbox.API.ingressCapabilities = () => pending.promise;
  const edit = h.sandbox.showSiteModal(h.site(1));
  await h.sandbox.showSiteIconManager(h.site(2));
  pending.resolve({ host_only_available: false, upstream_headers_available: true });
  await edit;
  assert.equal(h.element('modal-title').textContent, '设置站点图标');
});

test('leaving and returning to sites invalidates the prior pending live request', async () => {
  const h = modalHarness(), pending = deferred();
  h.sandbox.API.getActiveWatchHistory = () => pending.promise;
  const live = h.sandbox.showSiteLiveDetails(h.site(1));
  h.navigate('account');
  h.navigate('sites');
  pending.resolve(h.liveResult(1));
  await live;
  assert.equal(h.element('modal-overlay').classList.contains('active'), false);
  assert.equal(h.document.body.classList.contains('modal-open'), false);
});

test('a stale live error does not interrupt a newer edit or move its focus', async () => {
  const h = modalHarness(), pending = deferred();
  h.sandbox.API.getActiveWatchHistory = () => pending.promise;
  const live = h.sandbox.showSiteLiveDetails(h.site(1), h.element('live-trigger'));
  await h.sandbox.showSiteModal(h.site(2));
  const name = h.element('m-name');
  name.focus();
  pending.reject(new Error('Old Site 1 request failed'));
  await live;
  assert.deepEqual(h.errors, []);
  assert.equal(h.document.activeElement, name);
  assert.equal(h.element('modal-title').textContent, '编辑站点');
  assert.equal(h.element('live-trigger').disabled, false);
});

test('an old deferred focus callback cannot move focus within a newer modal', async () => {
  const h = modalHarness();
  h.sandbox.openModal({ modalClass: 'site-live-modal' });
  const oldFocus = h.frames.shift();
  await h.sandbox.showSiteModal(h.site(2));
  const target = h.element('m-target-address');
  target.focus();
  oldFocus();
  assert.equal(h.document.activeElement, target);
});

test('leaving sites dismisses its active modal without focusing a departed trigger', async () => {
  const h = modalHarness();
  h.element('edit-trigger').parentElement = h.element('page-sites');
  h.element('edit-trigger').focus();
  await h.sandbox.showSiteModal(h.site(2));
  h.frames.shift()();
  h.navigate('account');
  assert.equal(h.element('modal-overlay').classList.contains('active'), false);
  assert.equal(h.document.body.classList.contains('modal-open'), false);
  assert.equal(h.element('page-sites').contains(h.document.activeElement), false);
  assert.equal(h.element('modal-overlay').contains(h.document.activeElement), false);
});

test('closing invalidates live and edit intents that are still waiting for network reads', async () => {
  const h = modalHarness(), liveRead = deferred(), capabilityRead = deferred();
  h.sandbox.API.getActiveWatchHistory = () => liveRead.promise;
  const live = h.sandbox.showSiteLiveDetails(h.site(1));
  h.sandbox.closeModal();
  liveRead.resolve(h.liveResult(1));
  await live;
  assert.equal(h.element('modal-overlay').classList.contains('active'), false);
  h.sandbox.API.ingressCapabilities = () => capabilityRead.promise;
  const edit = h.sandbox.showSiteModal(h.site(2));
  h.sandbox.closeModal();
  capabilityRead.resolve({ host_only_available: false, upstream_headers_available: true });
  await edit;
  assert.equal(h.element('modal-overlay').classList.contains('active'), false);
  assert.equal(h.document.body.classList.contains('modal-open'), false);
});

test('leaving sites closes its shared artwork viewer without restoring focus to the departed route', () => {
  const h = modalHarness();
  bindRealHistoryDetails(h);
  const viewer = h.element('watch-history-image-modal');
  viewer.hidden = false;
  h.element('watch-history-image-viewer-image').src = '/api/watch-history/posters/7';
  h.document.body.classList.add('watch-history-image-open');
  h.sandbox.departedImageTrigger = h.element('site-artwork-trigger');
  vm.runInContext('watchHistoryImagePreviousFocus = departedImageTrigger', h.sandbox);
  const routeFocus = h.element('account-navigation-target');
  routeFocus.focus();

  h.navigate('account');

  assert.equal(viewer.hidden, true, 'the shared image layer must close when leaving sites');
  assert.equal(h.document.body.classList.contains('watch-history-image-open'), false);
  assert.equal(h.document.activeElement, routeFocus, 'cleanup must preserve the newly selected route focus');
});

test('the legacy edit action owns its intent before reading the site list', async () => {
  const h = modalHarness(), liveRead = deferred(), siteRead = deferred();
  h.sandbox.API.getActiveWatchHistory = () => liveRead.promise;
  h.sandbox.API.listSites = () => siteRead.promise;
  const live = h.sandbox.showSiteLiveDetails(h.site(1));
  const edit = h.sandbox.editSiteAction(2);
  liveRead.resolve(h.liveResult(1));
  await live;
  assert.equal(h.element('modal-overlay').classList.contains('active'), false);
  siteRead.resolve([h.site(2)]);
  await edit;
  assert.equal(h.element('modal-title').textContent, '编辑站点');
});

test('an older live request cannot restore a button still owned by a newer request', async () => {
  const h = modalHarness(), first = deferred(), second = deferred();
  h.sandbox.API.getActiveWatchHistory = filters => filters.site_id === 1 ? first.promise : second.promise;
  const button = h.element('live-trigger');
  button.textContent = '正在播放';
  const oldLive = h.sandbox.showSiteLiveDetails(h.site(1), button);
  const newLive = h.sandbox.showSiteLiveDetails(h.site(2), button);
  first.resolve(h.liveResult(1));
  await oldLive;
  assert.equal(button.disabled, true);
  assert.equal(button.textContent, '…');
  second.resolve(h.liveResult(2));
  await newLive;
  assert.equal(button.disabled, false);
  assert.equal(button.textContent, '正在播放');
});

test('a completed save cannot close the newer edit or discard its unsaved input', async () => {
  const h = modalHarness(), pending = deferred();
  h.sandbox.API.updateSite = () => pending.promise;
  await h.sandbox.showSiteModal(h.site(1));
  const save = h.element('m-submit').onclick();
  await h.sandbox.showSiteModal(h.site(2));
  const name = h.element('m-name'), footer = h.element('modal-footer').innerHTML;
  name.value = 'Unsaved Site 2';
  name.focus();
  pending.resolve();
  await save;
  assert.equal(h.element('modal-overlay').classList.contains('active'), true);
  assert.equal(h.element('modal-footer').innerHTML, footer);
  assert.equal(h.element('m-name'), name);
  assert.equal(name.value, 'Unsaved Site 2');
  assert.equal(h.document.activeElement, name);
});

test('a generic modal focus callback stays invalid after leaving and returning to its route', () => {
  const h = modalHarness();
  h.element('modal-body').innerHTML = '<input id="generic-modal-input" type="text">';
  h.sandbox.openModal();
  const oldFocus = h.frames.shift();
  h.navigate('account');
  h.navigate('sites');
  const current = h.element('current-page-focus');
  current.focus();
  oldFocus();
  assert.equal(h.document.activeElement, current);
});

test('a completed node save leaves a later node draft open while refreshing the list', async () => {
  const h = modalHarness(), pending = deferred();
  vm.runInContext(fs.readFileSync(path.join(__dirname, '../web/static/js/pages/nodes.js'), 'utf8'), h.sandbox);
  h.navigate('nodes');
  let refreshes = 0;
  h.sandbox.loadNodes = async () => { refreshes += 1; };
  h.sandbox.API.updateNode = () => pending.promise;
  h.sandbox.openNodeForm({ id: 1, name: 'First node', address: '', enabled: true, dns_publish: 'auto', port: 9443, reset_day: 1, priority: 100, billing_mode: 'outbound' });
  const save = h.element('node-form').onsubmit({ preventDefault() {} });
  h.sandbox.closeModal();
  h.sandbox.openNodeForm(null);
  const name = h.element('node-name'), footer = h.element('modal-footer').innerHTML;
  name.value = 'Unsaved second node';
  name.focus();
  pending.resolve({ id: 1 });
  await save;
  assert.equal(h.element('modal-overlay').classList.contains('active'), true);
  assert.equal(h.element('modal-footer').innerHTML, footer);
  assert.equal(h.element('node-name'), name);
  assert.equal(name.value, 'Unsaved second node');
  assert.equal(h.document.activeElement, name);
  assert.equal(refreshes, 1);
});

test('a completed site deletion leaves a later site draft open while refreshing the list', async () => {
  const h = modalHarness(), pending = deferred();
  let refreshes = 0;
  h.sandbox.loadSites = async () => { refreshes += 1; };
  h.sandbox.API.deleteSite = () => pending.promise;
  h.sandbox.deleteSiteAction(1, 'First site');
  const deletion = h.element('delete-confirm').click();
  h.sandbox.closeModal();
  await h.sandbox.showSiteModal(h.site(2));
  const name = h.element('m-name'), footer = h.element('modal-footer').innerHTML;
  name.value = 'Unsaved second site';
  name.focus();
  pending.resolve({ status: 'deleted' });
  await deletion;
  assert.equal(h.element('modal-overlay').classList.contains('active'), true);
  assert.equal(h.element('modal-footer').innerHTML, footer);
  assert.equal(h.element('m-name'), name);
  assert.equal(name.value, 'Unsaved second site');
  assert.equal(h.document.activeElement, name);
  assert.equal(refreshes, 1);
});

for (const operation of ['create', 'enrollment']) {
  for (const outcome of ['success', 'error']) {
    test(`a late node ${operation} ${outcome} cannot replace a newer node form`, async () => {
      const h = modalHarness(), pending = deferred();
      vm.runInContext(fs.readFileSync(path.join(__dirname, '../web/static/js/pages/nodes.js'), 'utf8'), h.sandbox);
      h.navigate('nodes');
      let refreshes = 0;
      h.sandbox.API.getNodes = async () => ({ nodes: [{ id: 1, name: 'First node', port: 9443 }], scheduler: { mode: 'auto' } });
      h.sandbox.API.getSiteNodeSchedules = async () => ({ sites: [] });
      await h.sandbox.loadNodes();
      h.sandbox.loadNodes = async () => { refreshes += 1; };
      let request;
      if (operation === 'create') {
        h.sandbox.API.createNode = () => pending.promise;
        h.sandbox.openNodeForm(null);
        h.element('node-name').value = 'First node';
        request = h.element('node-form').onsubmit({ preventDefault() {} });
      } else {
        h.sandbox.API.refreshNodeEnrollment = () => pending.promise;
        const button = { dataset: { action: 'enroll', id: '1' }, disabled: false };
        request = h.sandbox.handleNodeAction({ target: { closest: () => button } });
      }
      h.sandbox.closeModal();
      h.sandbox.openNodeForm(null);
      const name = h.element('node-name'), footer = h.element('modal-footer').innerHTML;
      name.value = 'Unsaved second node'; name.focus();
      if (outcome === 'success') pending.resolve({ install_script: 'SYNTHETIC_SCRIPT', install_command: 'SYNTHETIC_COMMAND' });
      else pending.reject(new Error('synthetic old request failure'));
      await request;
      assert.equal(h.element('modal-overlay').classList.contains('active'), true);
      assert.equal(h.element('modal-title').textContent, '创建节点');
      assert.equal(h.element('modal-footer').innerHTML, footer);
      assert.equal(h.element('node-name'), name);
      assert.equal(name.value, 'Unsaved second node');
      assert.equal(h.document.activeElement, name);
      assert.deepEqual(h.errors, []);
      assert.equal(refreshes, outcome === 'success' ? 1 : 0);
    });
  }
}

test('a node save failure leaves its own form editable and never reports success', async () => {
  const h = modalHarness();
  vm.runInContext(fs.readFileSync(path.join(__dirname, '../web/static/js/pages/nodes.js'), 'utf8'), h.sandbox);
  h.navigate('nodes');
  let successes = 0;
  h.sandbox.Toast.success = () => { successes += 1; };
  h.sandbox.API.createNode = async () => { throw new Error('synthetic failure'); };
  h.sandbox.openNodeForm(null);
  h.element('node-name').value = 'Unsaved node';
  await h.element('node-form').onsubmit({ preventDefault() {} });
  assert.equal(h.element('modal-overlay').classList.contains('active'), true);
  assert.equal(h.element('node-name').value, 'Unsaved node');
  assert.equal(h.element('node-form-save').disabled, false);
  assert.equal(successes, 0);
  assert.deepEqual(h.errors, ['synthetic failure']);
});

test('leaving nodes dismisses its modal and defeats its pending save and focus callbacks', async () => {
  const h = modalHarness(), pending = deferred();
  vm.runInContext(fs.readFileSync(path.join(__dirname, '../web/static/js/pages/nodes.js'), 'utf8'), h.sandbox);
  h.navigate('nodes');
  let refreshes = 0;
  h.sandbox.API.createNode = () => pending.promise;
  h.sandbox.loadNodes = async () => { refreshes += 1; };
  h.sandbox.openNodeForm(null);
  const save = h.element('node-form').onsubmit({ preventDefault() {} });
  h.navigate('account');
  const focus = h.element('account-focus'); focus.focus();
  pending.resolve({ install_script: 'SYNTHETIC_SCRIPT' });
  await save;
  h.frames.forEach(frame => frame());
  assert.equal(h.element('modal-overlay').classList.contains('active'), false);
  assert.equal(h.document.body.classList.contains('modal-open'), false);
  assert.equal(h.document.activeElement, focus);
  assert.equal(refreshes, 0);
});

test('a late site delete failure cannot interrupt a newer site draft', async () => {
  const h = modalHarness(), pending = deferred();
  h.sandbox.API.deleteSite = () => pending.promise;
  h.sandbox.deleteSiteAction(1, 'First site');
  const deletion = h.element('delete-confirm').click();
  await h.sandbox.showSiteModal(h.site(2));
  const name = h.element('m-name'); name.value = 'Unsaved second site'; name.focus();
  pending.reject(new Error('synthetic old delete failure'));
  await deletion;
  assert.equal(h.element('modal-overlay').classList.contains('active'), true);
  assert.equal(h.element('m-name'), name);
  assert.equal(h.document.activeElement, name);
  assert.deepEqual(h.errors, []);
});

test('deleting a site restores focus without scrolling to an earlier offscreen control', async () => {
  const h = modalHarness();
  const search = h.element('sites-search');
  search.fixtureTop = 0;
  search.focus();
  h.sandbox.scrollTo({ top: 1600 });
  h.sandbox.API.deleteSite = async () => ({ status: 'deleted' });
  h.sandbox.deleteSiteAction(1, 'First site');
  h.frames.shift()();
  await h.element('delete-confirm').click();
  assert.equal(h.document.activeElement, search);
  assert.equal(h.sandbox.scrollY, 1600);
  assert.equal(h.element('modal-overlay').classList.contains('active'), false);
});
