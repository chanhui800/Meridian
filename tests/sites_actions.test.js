'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const vm = require('node:vm');

function siteActionsHarness() {
  const elements = new Map(), calls = [], copied = [];
  let scrollY = 0;
  const maxScrollY = () => Math.max(0, grid.children.filter(card => !card.hidden).length * 100 + 300 - 600);
  const decode = value => String(value || '').replaceAll('&quot;', '"').replaceAll('&amp;', '&');
  const attributes = text => Object.fromEntries([...text.matchAll(/([\w-]+)(?:="([^"]*)")?/g)]
    .map(([, name, value]) => [name, value === undefined ? '' : decode(value)]));
  const element = (tagName, attrs = {}) => {
    const classes = new Set(String(attrs.class || '').split(/\s+/).filter(Boolean));
    const listeners = new Map();
    const node = {
      tagName: tagName.toUpperCase(), children: [], parentElement: null, attributes: { ...attrs },
      dataset: Object.fromEntries(Object.entries(attrs).filter(([name]) => name.startsWith('data-'))
        .map(([name, value]) => [name.slice(5).replace(/-([a-z])/g, (_, letter) => letter.toUpperCase()), value])),
      textContent: '', hidden: Object.hasOwn(attrs, 'hidden'), isConnected: true, value: '',
      classList: {
        contains: value => classes.has(value),
        remove: value => classes.delete(value),
        toggle(value, enabled = !classes.has(value)) {
          if (enabled) classes.add(value); else classes.delete(value);
          return enabled;
        },
      },
      setAttribute(name, value) { this.attributes[name] = String(value); },
      addEventListener(type, listener) {
        if (!listeners.has(type)) listeners.set(type, []);
        listeners.get(type).push(listener);
      },
      append(child) { child.parentElement = this; this.children.push(child); return child; },
      matches(selector) {
        return selector.split(',').some(part => {
          const value = part.trim();
          if (/^[a-z]+$/.test(value)) return value.toUpperCase() === this.tagName;
          const className = value.match(/^\.([\w-]+)(.*)$/);
          if (className) return classes.has(className[1]) && (!className[2] || this.matches(className[2]));
          const attribute = value.match(/^\[([^=\]]+)(?:="([^"]*)")?\]$/);
          return !!attribute && Object.hasOwn(this.attributes, attribute[1])
            && (attribute[2] === undefined || this.attributes[attribute[1]] === attribute[2]);
        });
      },
      closest(selector) {
        for (let candidate = this; candidate; candidate = candidate.parentElement) {
          if (candidate.matches(selector)) return candidate;
        }
        return null;
      },
      querySelectorAll(selector) {
        return this.children.flatMap(child => [...(child.matches(selector) ? [child] : []), ...child.querySelectorAll(selector)]);
      },
      querySelector(selector) { return this.querySelectorAll(selector)[0] || null; },
      getBoundingClientRect() {
        const visible = grid.children.filter(card => !card.hidden);
        const top = 300 + visible.indexOf(this) * 100 - scrollY;
        return { top, bottom: top + (this.hidden ? 0 : 100) };
      },
      click() {
        // DOM dispatch fixes the propagation path before listeners run. Replacing
        // the eye SVG detaches the target but does not change that event path.
        const propagationPath = [];
        for (let candidate = this; candidate; candidate = candidate.parentElement) propagationPath.push(candidate);
        const event = {
          target: this, currentTarget: null, defaultPrevented: false, stopped: false,
          composedPath: () => [...propagationPath],
          preventDefault() { this.defaultPrevented = true; },
          stopPropagation() { this.stopped = true; },
        };
        for (const current of propagationPath) {
          event.currentTarget = current;
          for (const listener of listenersFor(current)) listener(event);
          if (event.stopped) break;
        }
        return event;
      },
      get clickListeners() { return listeners.get('click') || []; },
    };
    let markup = '';
    Object.defineProperty(node, 'innerHTML', {
      configurable: true,
      get: () => markup,
      set(value) {
        markup = String(value);
        node.children.forEach(child => { child.parentElement = null; });
        node.children = [];
        if (node.dataset.siteAction === 'access' || node.dataset.siteAction === 'copy') {
          node.append(element('svg')).append(element('path'));
        }
      },
    });
    if (attrs.id) elements.set(attrs.id, node);
    return node;
  };
  const listenersFor = node => node.clickListeners;
  const grid = element('div', { id: 'sites-grid' });
  const renderRows = markup => {
    grid.children.forEach(child => { child.parentElement = null; });
    grid.children = [];
    for (const [, articleAttrs, contents] of markup.matchAll(/<article\b([^>]*)>([\s\S]*?)<\/article>/g)) {
      const card = grid.append(element('article', attributes(articleAttrs)));
      const detailsAttrs = contents.match(/<div class="management-site-details"([^>]*)>/)[1];
      const details = card.append(element('div', attributes(detailsAttrs)));
      const name = card.append(element('h2', { class: 'site-name' }));
      name.textContent = contents.match(/<h2 class="site-name">([^<]*)<\/h2>/)[1];
      const accessRow = details.append(element('div', { class: 'site-access-row' }));
      const address = contents.match(/<span class="console-mono site-access-address is-hidden"([^>]*)>/)[1];
      accessRow.append(element('span', { ...attributes(address), class: 'is-hidden' })).textContent = '********';
      for (const [, buttonAttrs, buttonContents] of contents.matchAll(/<button\b([^>]*)>([\s\S]*?)<\/button>/g)) {
        const attrs = attributes(buttonAttrs);
        const parent = ['access', 'copy'].includes(attrs['data-site-action']) ? accessRow : card;
        const button = parent.append(element('button', attrs));
        button.innerHTML = buttonContents;
      }
    }
    scrollY = Math.min(scrollY, maxScrollY());
  };
  Object.defineProperty(grid, 'innerHTML', { set: renderRows });
  for (const id of ['page-sites', 'sites-search', 'sites-count', 'sites-load-state']) element('div', { id });
  const site = { id: 3, name: '测试站点', upstream_mode: 'fixed', ingress_mode: 'port', listen_port: 8096,
    target_url: 'http://origin.example.test:8096', ua_mode: 'passthrough', enabled: true, running: true };
  let sites = [site];
  const sandbox = {
    URL, console, Router: { current: 'sites' }, esc: value => String(value ?? ''),
    window: {
      getSelection: () => '', setTimeout: () => 1, clearTimeout() {},
      scrollX: 0, get scrollY() { return scrollY; }, innerHeight: 600,
      scrollTo({ top }) { scrollY = Math.max(0, Math.min(top, maxScrollY())); },
    },
    document: { getElementById: id => elements.get(id) || null },
    navigator: { clipboard: { async writeText(value) { copied.push(value); } } },
    formatBytes: value => `${value} B`, uaNameMap: { passthrough: '透传' },
    API: {
      async ingressCapabilities() { return { panel_tls_enabled: false }; },
      async getActiveWatchHistory() { return { items: [{ id: 'active-session', site_id: 3 }] }; },
      async listSites() { return sites.map(site => ({ ...site })); },
    },
    Toast: { success() {}, error(message) { throw new Error(message); } },
  };
  vm.createContext(sandbox);
  vm.runInContext(fs.readFileSync(path.join(__dirname, '../web/static/js/pages/sites.js'), 'utf8'), sandbox);
  // Sorting has its own interaction tests; exercise the real loadSites action
  // and disclosure bindings here without pointer geometry or network requests.
  sandbox.setupSiteSorting = () => {};
  for (const [name, action] of [['testSiteLatency', 'latency'], ['showSiteLiveDetails', 'live'],
    ['toggleSiteAction', 'toggle'], ['showSiteModal', 'edit'], ['deleteSiteAction', 'delete'], ['showSiteIconManager', 'icon']]) {
    sandbox[name] = (...args) => { calls.push({ action, id: typeof args[0] === 'object' ? args[0].id : args[0] }); };
  }
  return { sandbox, calls, copied, elements, grid,
    site, setSites(values) { sites = values; },
    scrollTo(top) { sandbox.window.scrollTo({ top }); }, scrollTop: () => scrollY,
    button: action => grid.querySelector(`[data-site-action="${action}"]`),
    trigger: () => grid.querySelector('[data-site-row-toggle]'),
    details: () => elements.get('site-details-3'),
    address: () => grid.querySelector('[data-access-address]'),
  };
}

test('clicking the replaced eye SVG shows and hides the address without collapsing site details', async () => {
  const h = siteActionsHarness();
  await h.sandbox.loadSites();
  h.trigger().click();
  for (const shown of [true, false, true]) {
    const originalPath = h.button('access').querySelector('path');
    originalPath.click();
    assert.equal(originalPath.closest('button'), null, 'the clicked SVG subtree was replaced');
    assert.equal(h.details().hidden, false, 'eye clicks must keep the site row expanded');
    assert.equal(h.trigger().attributes['aria-expanded'], 'true');
    assert.equal(h.address().textContent, shown ? 'http://127.0.0.1:8096' : '********');
    assert.equal(h.button('access').attributes['aria-pressed'], String(shown));
  }
  h.trigger().click();
  assert.equal(h.details().hidden, true, 'the row disclosure button must still collapse the row');
});

test('site action bindings survive unchanged refreshes and preserve disclosure and drag controls', async () => {
  const h = siteActionsHarness();
  await h.sandbox.loadSites();
  await h.sandbox.loadSites({ background: true });
  h.trigger().click();
  h.button('copy').querySelector('path').click();
  await Promise.resolve();
  assert.deepEqual(h.copied, ['http://127.0.0.1:8096']);
  for (const action of ['latency', 'live', 'toggle', 'edit', 'delete']) {
    h.button(action).click();
    assert.equal(h.details().hidden, false, `${action} must keep the row expanded`);
  }
  h.grid.querySelector('[data-site-icon-id]').click();
  assert.equal(h.details().hidden, false, 'icon selection must keep the row expanded');
  assert.deepEqual(h.calls, ['latency', 'live', 'toggle', 'edit', 'delete', 'icon'].map(action => ({ action, id: 3 })));
  h.grid.querySelector('[data-site-drag-handle]').click();
  assert.equal(h.details().hidden, false, 'drag-handle clicks must not toggle disclosure');
  h.grid.querySelector('.site-card').click();
  assert.equal(h.details().hidden, true, 'a noninteractive row area still toggles disclosure');
});

for (const removed of ['above', 'first-visible']) {
  test(`refresh keeps the visible site in place after deleting a site ${removed}`, async () => {
    const h = siteActionsHarness();
    const sites = Array.from({ length: 30 }, (_, index) => ({ ...h.site, id: index + 1 }));
    h.setSites(sites);
    await h.sandbox.loadSites();
    h.scrollTo(1250);
    const anchorID = removed === 'above' ? 11 : 12;
    const anchor = () => h.grid.querySelector(`.site-card[data-site-id="${anchorID}"]`);
    const top = anchor().getBoundingClientRect().top;
    h.setSites(sites.filter(site => site.id !== (removed === 'above' ? 1 : 11)));
    await h.sandbox.loadSites({ background: true });
    assert.equal(anchor().getBoundingClientRect().top, top);
    assert.equal(h.scrollTop(), 1150);
  });
}

test('a delayed site refresh preserves scrolling performed while the request was pending', async () => {
  const h = siteActionsHarness();
  const sites = Array.from({ length: 30 }, (_, index) => ({ ...h.site, id: index + 1 }));
  h.setSites(sites);
  await h.sandbox.loadSites();
  h.scrollTo(1250);
  let finishRead, started;
  const reading = new Promise(resolve => { started = resolve; });
  h.sandbox.API.listSites = () => new Promise(resolve => { finishRead = resolve; started(); });
  const refresh = h.sandbox.loadSites({ background: true });
  await reading;
  h.scrollTo(1800);
  finishRead(sites.map(site => ({ ...site, traffic_used: 100 })));
  await refresh;
  assert.equal(h.scrollTop(), 1800);
  assert.equal(h.grid.querySelector('.site-card[data-site-id="20"]').getBoundingClientRect().top, 400);
});

test('deleting the last site clamps to the new bottom and deleting all sites leaves a usable empty page', async () => {
  const h = siteActionsHarness();
  const sites = Array.from({ length: 10 }, (_, index) => ({ ...h.site, id: index + 1 }));
  h.setSites(sites);
  await h.sandbox.loadSites();
  h.scrollTo(10000);
  assert.equal(h.scrollTop(), 700);
  h.setSites(sites.slice(0, -1));
  await h.sandbox.loadSites();
  assert.equal(h.scrollTop(), 600);
  h.setSites([]);
  await h.sandbox.loadSites();
  assert.equal(h.scrollTop(), 0);
  assert.equal(h.grid.children.length, 0);
});
