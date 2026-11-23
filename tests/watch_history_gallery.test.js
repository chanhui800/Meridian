'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '../web/static/js/pages/watch-history.js'), 'utf8');

function galleryHarness(kind = 'history') {
  let document;
  function matches(node, selector) {
    selector = selector.trim();
    if (selector.includes(' ')) {
      const split = selector.lastIndexOf(' ');
      return matches(node, selector.slice(split + 1)) && !!node.parentElement?.closest(selector.slice(0, split));
    }
    const not = [...selector.matchAll(/:not\(([^)]+)\)/g)].map(match => match[1]);
    if (not.some(value => matches(node, value))) return false;
    selector = selector.replace(/:not\([^)]+\)/g, '');
    const tag = selector.match(/^[a-z]+/i)?.[0];
    if (tag && node.tagName !== tag.toUpperCase()) return false;
    const id = selector.match(/#([\w-]+)/)?.[1];
    if (id && node.id !== id) return false;
    if ([...selector.matchAll(/\.([\w-]+)/g)].some(match => !node.classList.contains(match[1]))) return false;
    for (const [, name, , value] of selector.matchAll(/\[([\w-]+)(?:=(['"])(.*?)\2)?\]/g)) {
      if (value === undefined ? node.getAttribute(name) === null : node.getAttribute(name) !== value) return false;
    }
    return true;
  }
  class Element {
    constructor(tag = 'div', attributes = {}) {
      this.tagName = tag.toUpperCase();
      this.attributes = new Map(Object.entries(attributes));
      this.children = [];
      this.parentElement = null;
      this.hidden = false;
      this.disabled = false;
      this.events = new Map();
      this.textContent = '';
      this.scrollTop = 0;
      const classes = new Set(String(attributes.class || '').split(/\s+/).filter(Boolean));
      this.classList = {
        add: name => classes.add(name), remove: name => classes.delete(name), contains: name => classes.has(name),
        toggle(name, enabled = !classes.has(name)) { if (enabled) classes.add(name); else classes.delete(name); return enabled; },
      };
    }
    get id() { return this.getAttribute('id') || ''; }
    get src() { return this.getAttribute('src') || ''; }
    set src(value) { this.setAttribute('src', value); }
    get alt() { return this.getAttribute('alt') || ''; }
    set alt(value) { this.setAttribute('alt', value); }
    get isConnected() { return this === document?.body || !!this.parentElement?.isConnected; }
    get offsetParent() { return this.hidden || this.parentElement?.offsetParent === null ? null : document.body; }
    getAttribute(name) { return this.attributes.has(name) ? this.attributes.get(name) : null; }
    setAttribute(name, value) { this.attributes.set(name, String(value)); }
    removeAttribute(name) { this.attributes.delete(name); }
    appendChild(child) { child.remove(); this.children.push(child); child.parentElement = this; return child; }
    remove() { if (this.parentElement) this.parentElement.children = this.parentElement.children.filter(child => child !== this); this.parentElement = null; }
    contains(other) { return this === other || this.children.some(child => child.contains(other)); }
    closest(selector) { return selector.split(',').some(value => matches(this, value)) ? this : this.parentElement?.closest(selector) || null; }
    querySelectorAll(selector) {
      const result = [];
      for (const child of this.children) {
        if (selector.split(',').some(value => matches(child, value))) result.push(child);
        result.push(...child.querySelectorAll(selector));
      }
      return result;
    }
    querySelector(selector) { return this.querySelectorAll(selector)[0] || null; }
    addEventListener(name, listener) { const listeners = this.events.get(name) || []; listeners.push(listener); this.events.set(name, listeners); }
    focus() {
      if (['BUTTON', 'A', 'INPUT', 'SELECT', 'TEXTAREA'].includes(this.tagName) || this.getAttribute('tabindex') !== null) {
        document.activeElement = this;
      }
    }
    fire(name, overrides = {}) {
      const event = { target: this, prevented: 0, stopped: 0, preventDefault() { this.prevented++; }, stopPropagation() { this.stopped++; }, ...overrides };
      for (const listener of this.events.get(name) || []) listener(event);
      return event;
    }
  }
  const body = new Element('body');
  document = { body, activeElement: body, getElementById: id => body.querySelector('#' + id), querySelectorAll: selector => body.querySelectorAll(selector) };
  const make = (parent, tag, attributes) => parent.appendChild(new Element(tag, attributes));
  const page = make(body, 'div', { id: kind === 'history' ? 'page-watch-history' : 'site-page' });
  const detail = make(page, 'div', { id: kind === 'history' ? 'watch-history-detail-modal' : 'modal', class: 'modal site-live-modal', role: 'dialog' });
  const detailClose = make(detail, 'button', { class: 'modal-close', 'data-watch-history-close': '', 'aria-label': '关闭详情' });
  const root = make(detail, 'div', { id: kind === 'history' ? 'watch-history-detail-content' : 'modal-body' });
  const viewer = make(body, 'div', { id: 'watch-history-image-modal' });
  viewer.hidden = true;
  const backdrop = make(viewer, 'div', { 'data-watch-history-image-close': '' });
  const dialog = make(viewer, 'div', { class: 'watch-history-image-dialog', role: 'dialog' });
  const close = make(dialog, 'button', { class: 'watch-history-image-close', 'data-watch-history-image-close': '', 'aria-label': '关闭图片预览' });
  const stage = make(dialog, 'div', { class: 'watch-history-image-stage' });
  const previous = make(stage, 'button', { id: 'watch-history-image-prev', 'data-watch-history-image-prev': '', 'aria-label': '上一张剧照' });
  const image = make(stage, 'img', { id: 'watch-history-image-viewer-image', alt: '' });
  const next = make(stage, 'button', { id: 'watch-history-image-next', 'data-watch-history-image-next': '', 'aria-label': '下一张剧照' });
  const counter = make(stage, 'div', { id: 'watch-history-image-counter', role: 'status', 'aria-live': 'polite' });
  for (const control of [previous, next, counter]) control.hidden = true;
  const sandbox = { document, console, URL, esc: value => String(value).replace(/"/g, '&quot;'), Router: { current: kind === 'history' ? 'watch-history' : 'sites' } };
  vm.createContext(sandbox);
  vm.runInContext(source, sandbox, { filename: 'watch-history.js' });
  let controls = [], poster, markup = '';
  Object.defineProperty(root, 'innerHTML', {
    get: () => markup,
    set(value) {
      markup = String(value);
      for (const child of [...root.children]) child.remove();
      const opening = markup.match(/<div class="watch-history-detail-shell[^>]*>/)?.[0] || '';
      const attributes = Object.fromEntries([...opening.matchAll(/([\w-]+)="([^"]*)"/g)].map(match => [match[1], match[2]]));
      const shell = make(root, 'div', { ...attributes, class: 'watch-history-detail-shell history-detail-shell' });
      const artwork = markup.match(/<div class="watch-history-detail-background[^>]*data-watch-history-image="([^"]+)" data-watch-history-image-alt="([^"]+)"/);
      poster = artwork ? make(shell, 'div', { class: 'history-detail-art', role: 'button', tabindex: '0', 'data-watch-history-image': artwork[1], 'data-watch-history-image-alt': artwork[2] }) : null;
      const list = make(shell, 'div', { class: 'watch-history-detail-stills' });
      controls = [...markup.matchAll(/<button[^>]*class="watch-history-detail-still" data-watch-history-image="([^"]+)" data-watch-history-image-alt="([^"]+)"/g)].map(([, src, alt]) =>
        make(list, 'button', { class: 'watch-history-detail-still', 'data-watch-history-image': src, 'data-watch-history-image-alt': alt }));
    },
  });
  const item = { id: 11, media_item_id: 8, title: '画廊测试', poster_available: true, stills: ['/a.jpg', '/b.jpg', '/c.jpg', '/d.jpg'] };
  function render(value = item, replace = false) {
    const html = sandbox.watchHistoryDetailsHTML(value);
    if (replace) sandbox.watchHistoryReplaceDetailContent(root, html);
    else { root.innerHTML = html; sandbox.watchHistoryBindDetailImages(root); }
    return controls;
  }
  render();
  return { sandbox, document, body, root, detail, detailClose, viewer, backdrop, dialog, close, previous, image, next, counter, item,
    controls: () => controls, poster: () => poster, render,
    open(index) { const trigger = controls[index]; trigger.focus(); trigger.fire('click'); return trigger; } };
}

test('clicking a middle still opens its gallery at that position and buttons cycle without changing return focus', () => {
  const h = galleryHarness();
  const trigger = h.open(2);
  assert.equal(h.document.activeElement, h.close, 'opening must focus the close button, not the non-focusable backdrop');
  assert.equal(h.image.src, '/api/watch-history/stills/8/2');
  assert.equal(h.counter.textContent, '3 / 4');
  assert.equal(h.previous.hidden, false);
  assert.equal(h.next.hidden, false);
  h.next.focus(); h.next.fire('click');
  assert.equal(h.image.src, '/api/watch-history/stills/8/3');
  assert.equal(h.image.alt, '剧照 4');
  assert.equal(h.counter.textContent, '4 / 4');
  h.next.fire('click');
  assert.equal(h.counter.textContent, '1 / 4');
  h.previous.fire('click');
  assert.equal(h.counter.textContent, '4 / 4');
  h.close.fire('click');
  assert.equal(h.document.activeElement, trigger);
  assert.equal(h.viewer.hidden, true);
  assert.equal(h.image.src, '');
});

test('viewer arrow keys, Tab and Escape stay in the viewer and leave underlying details open', () => {
  const h = galleryHarness('sites');
  const trigger = h.open(0);
  assert.equal(h.document.activeElement, h.close);
  for (const [key, expected] of [['ArrowLeft', '4 / 4'], ['ArrowRight', '1 / 4']]) {
    const event = h.viewer.fire('keydown', { key });
    assert.equal(h.counter.textContent, expected);
    assert.equal(event.prevented, 1);
    assert.equal(event.stopped, 1);
  }
  h.next.focus();
  const tab = h.viewer.fire('keydown', { key: 'Tab', shiftKey: false });
  assert.equal(tab.stopped, 1);
  assert.equal(h.document.activeElement, h.close);
  h.close.focus();
  h.viewer.fire('keydown', { key: 'Tab', shiftKey: true });
  assert.equal(h.document.activeElement, h.next);
  const escape = h.viewer.fire('keydown', { key: 'Escape' });
  assert.equal(escape.stopped, 1);
  assert.equal(h.detail.hidden, false);
  assert.equal(h.document.activeElement, trigger);
});

test('a gallery excludes hidden failed thumbnails and the main poster, then resets for a single image', () => {
  const h = galleryHarness();
  h.controls()[1].hidden = true;
  h.open(2);
  assert.equal(h.counter.textContent, '2 / 3');
  h.previous.fire('click');
  assert.equal(h.image.src, '/api/watch-history/stills/8/0');
  h.previous.fire('click');
  assert.equal(h.image.src, '/api/watch-history/stills/8/3');
  h.close.fire('click');
  h.poster().focus(); h.poster().fire('click');
  assert.equal(h.image.src, '/api/watch-history/posters/8');
  assert.equal(h.previous.hidden, true);
  assert.equal(h.next.hidden, true);
  assert.equal(h.counter.hidden, true);
  h.viewer.fire('keydown', { key: 'ArrowRight' });
  assert.equal(h.image.src, '/api/watch-history/posters/8');
  h.close.fire('click');
  h.render({ ...h.item, id: 12, media_item_id: 9, stills: ['/only.jpg'] });
  h.open(0);
  assert.equal(h.image.src, '/api/watch-history/stills/9/0');
  assert.equal(h.previous.hidden, true);
  assert.equal(h.next.hidden, true);
});

test('closing a gallery returns to the detail close when the clicked thumbnail becomes hidden', () => {
  const h = galleryHarness();
  const trigger = h.open(1);
  trigger.hidden = true;
  h.next.fire('click');
  h.close.fire('click');
  assert.equal(h.document.activeElement, h.detailClose);
  assert.equal(h.viewer.hidden, true);
});

for (const kind of ['history', 'sites']) {
  test(`an open ${kind} gallery keeps its snapshot and restores an equivalent trigger after detail refresh`, () => {
    const h = galleryHarness(kind);
    const oldTrigger = h.open(1);
    h.render({ ...h.item, stills: ['/a.jpg', '/b.jpg', '/c.jpg', '/d.jpg', '/new.jpg'] }, true);
    assert.equal(oldTrigger.isConnected, false);
    h.next.fire('click');
    assert.equal(h.counter.textContent, '3 / 4');
    h.close.fire('click');
    assert.equal(h.document.activeElement, h.controls()[1]);
    h.open(1);
    h.render({ ...h.item, id: 99, media_item_id: 8 }, true);
    h.close.fire('click');
    assert.equal(h.document.activeElement, h.detailClose, 'a different record cannot receive the old gallery return focus');
    h.open(1);
    h.detailClose.focus();
    h.sandbox.watchHistoryCloseImageViewer({ restoreFocus: false });
    assert.equal(h.document.activeElement, h.detailClose);
  });
}

test('repeated gallery opening binds each viewer navigation handler once and handles the twelve image cap', () => {
  const h = galleryHarness();
  for (let cycle = 0; cycle < 3; cycle++) { h.open(1); h.close.fire('click'); }
  assert.equal(h.next.events.get('click').length, 1);
  assert.equal(h.previous.events.get('click').length, 1);
  assert.equal(h.viewer.events.get('keydown').length, 1);
  const urls = Array.from({ length: 20 }, (_, index) => ({ src: '/api/watch-history/stills/8/' + index, alt: '剧照 ' + (index + 1) }));
  h.sandbox.watchHistoryOpenImageViewer(urls[11].src, urls[11].alt, urls);
  assert.equal(h.counter.textContent, '12 / 12');
  urls[0].src = '/changed-after-opening';
  h.next.fire('click');
  assert.equal(h.counter.textContent, '1 / 12');
  assert.equal(h.image.src, '/api/watch-history/stills/8/0');
  h.close.fire('click');
});
