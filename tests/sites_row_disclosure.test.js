'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const vm = require('node:vm');

function disclosureHarness() {
  const classes = new Set();
  const triggerListeners = {}, rowListeners = {}, attributes = {};
  const details = { hidden: true };
  let selection = '';
  const trigger = {
    dataset: { siteId: '3' },
    addEventListener(type, listener) { triggerListeners[type] = listener; },
    setAttribute(name, value) { attributes[name] = value; },
    closest() { return card; },
  };
  const card = {
    querySelector(selector) {
      return selector === '[data-site-row-toggle]' ? trigger : selector === '.site-name' ? { textContent: '测试站点' } : null;
    },
    classList: { toggle(name, enabled) { if (enabled) classes.add(name); else classes.delete(name); } },
    addEventListener(type, listener) { rowListeners[type] = listener; },
  };
  const sandbox = {
    window: { getSelection: () => selection }, URL, esc: value => String(value),
    document: { getElementById(id) { return id === 'site-details-3' ? details : null; } },
  };
  vm.createContext(sandbox);
  vm.runInContext(fs.readFileSync(path.join(__dirname, '../web/static/js/pages/sites.js'), 'utf8'), sandbox);
  sandbox.bindSiteRowDisclosure(card);
  return {
    attributes, details, classes,
    trigger() { let stopped = false; triggerListeners.click({ stopPropagation() { stopped = true; } }); return stopped; },
    click(interactive = false, prevented = false) {
      rowListeners.click({ defaultPrevented: prevented, target: { closest: () => interactive ? {} : null } });
    },
    select(value) { selection = value; },
  };
}

test('row disclosure opens and closes through its native button without replacing row controls', () => {
  const h = disclosureHarness();
  assert.equal(h.trigger(), true);
  assert.equal(h.details.hidden, false);
  assert.equal(h.attributes['aria-expanded'], 'true');
  assert.match(h.attributes['aria-label'], /收起 测试站点/);
  assert.equal(h.classes.has('is-expanded'), true);
  h.trigger();
  assert.equal(h.details.hidden, true);
  assert.equal(h.attributes['aria-expanded'], 'false');
  assert.match(h.attributes['aria-label'], /展开 测试站点/);
});

test('a blank details area toggles the row but actions, text selection and prevented clicks do not', () => {
  const h = disclosureHarness();
  h.click();
  assert.equal(h.details.hidden, false);
  h.click(true);
  assert.equal(h.details.hidden, false);
  h.click(false, true);
  assert.equal(h.details.hidden, false);
  h.select('selected upstream address');
  h.click();
  assert.equal(h.details.hidden, false);
  h.select('');
  h.click();
  assert.equal(h.details.hidden, true);
});
