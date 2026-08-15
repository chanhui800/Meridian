'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const vm = require('node:vm');

const root = path.join(__dirname, '..');

function loadSiteOrderingSandbox(overrides = {}) {
  const source = fs.readFileSync(path.join(root, 'web', 'static', 'js', 'pages', 'sites.js'), 'utf8');
  const sandbox = { window: {}, URL, esc: value => String(value), ...overrides };
  vm.createContext(sandbox);
  vm.runInContext(source, sandbox, { filename: 'sites.js' });
  return sandbox;
}

test('site page exposes a drag handle and persists the complete card order', () => {
  const source = fs.readFileSync(path.join(root, 'web', 'static', 'js', 'pages', 'sites.js'), 'utf8');
  assert.match(source, /data-site-id="\$\{s\.id\}"/);
  assert.match(source, /data-site-drag-handle/);
  assert.doesNotMatch(source, /class="site-card fade-up/);
  assert.match(source, /siteOrderFromGrid/);
  assert.match(source, /API\.reorderSites\(siteIds\)/);
  assert.match(source, /仪表盘已同步/);
  assert.match(source, /pointerdown/);
  assert.match(source, /pointermove/);
  assert.match(source, /site-card-placeholder/);
  assert.match(source, /translate3d/);
});

test('API client sends site ordering to the dedicated reorder endpoint', () => {
  const source = fs.readFileSync(path.join(root, 'web', 'static', 'js', 'api.js'), 'utf8');
  assert.match(source, /reorderSites\(siteIds\).*'PUT', '\/api\/sites\/reorder', \{ site_ids: siteIds \}/s);
});

test('site ordering styles keep the handle touch-capable and show drag state', () => {
  const css = fs.readFileSync(path.join(root, 'web', 'static', 'css', 'style.css'), 'utf8');
  assert.match(css, /\.site-drag-handle[\s\S]*?touch-action:\s*none/);
  assert.match(css, /\.site-card\.is-dragging/);
  assert.doesNotMatch(css, /\.site-card\.is-dragging \.site-rows[\s\S]*?display:\s*none/);
  assert.match(css, /\.site-card-placeholder::after/);
  assert.match(css, /\.sites-grid\.is-saving-order/);
});

test('persistSiteOrder sends the current DOM order and records it after success', async () => {
  let saved = null;
  let message = '';
  const sandbox = loadSiteOrderingSandbox({
    API: { reorderSites: async ids => { saved = ids; } },
    Toast: { success: value => { message = value; }, error() {} },
  });
  const cards = [2, 1, 3].map(id => ({ dataset: { siteId: String(id) } }));
  const classes = new Set();
  const grid = {
    dataset: { siteOrder: '1,2,3' },
    classList: { add: value => classes.add(value), remove: value => classes.delete(value) },
    querySelectorAll: () => cards,
  };

  await sandbox.persistSiteOrder(grid);

  assert.deepEqual(Array.from(saved), [2, 1, 3]);
  assert.equal(grid.dataset.siteOrder, '2,1,3');
  assert.match(message, /仪表盘已同步/);
  assert.equal(classes.has('is-saving-order'), false);
});

test('moveSitePlaceholderAtPoint can jump across fixed desktop slots without oscillating', () => {
  const card1 = { dataset: { siteId: '1' } };
  const card2 = {
    dataset: { siteId: '2' },
    getBoundingClientRect: () => ({ left: 320, right: 620, top: 100, bottom: 400, width: 300, height: 300 }),
  };
  const card3 = {
    dataset: { siteId: '3' },
    getBoundingClientRect: () => ({ left: 640, right: 940, top: 100, bottom: 400, width: 300, height: 300 }),
  };
  const placeholder = { marker: true };
  const cards = [placeholder, card1, card2, card3];
  const grid = {
    clientWidth: 940,
    querySelectorAll: () => [card1, card2, card3],
    insertBefore(item, reference) {
      cards.splice(cards.indexOf(item), 1);
      const index = reference ? cards.indexOf(reference) : cards.length;
      cards.splice(index, 0, item);
    },
  };
  for (const card of cards) card.parentElement = grid;
  card2.nextSibling = card3;
  card3.nextSibling = null;
  const sandbox = loadSiteOrderingSandbox();
  const slots = [
    { x: 150, y: 250, width: 300, height: 300 },
    { x: 470, y: 250, width: 300, height: 300 },
    { x: 790, y: 250, width: 300, height: 300 },
  ];

  sandbox.moveSitePlaceholderAtPoint(grid, card1, placeholder, slots, 900, 250);

  assert.deepEqual(cards.map(card => card.dataset ? card.dataset.siteId : 'placeholder'), ['1', '2', '3', 'placeholder']);

  sandbox.moveSitePlaceholderAtPoint(grid, card1, placeholder, slots, 160, 250);
  assert.deepEqual(cards.map(card => card.dataset ? card.dataset.siteId : 'placeholder'), ['1', 'placeholder', '2', '3']);
});

test('positionDraggedSiteCard makes the floating card follow the pointer', () => {
  const sandbox = loadSiteOrderingSandbox();
  const card = { style: { left: '100px', top: '200px', transform: '' } };

  sandbox.positionDraggedSiteCard(card, 480, 390, 30, 40);

  assert.equal(card.style.left, '450px');
  assert.equal(card.style.top, '350px');
  assert.equal(card.style.transform, 'translate3d(0, 0, 0)');
});
