'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const vm = require('node:vm');

function loadSiteHelpers() {
  const source = fs.readFileSync(
    path.join(__dirname, '..', 'web', 'static', 'js', 'pages', 'sites.js'),
    'utf8',
  );
  const sandbox = { window: {} };
  vm.createContext(sandbox);
  vm.runInContext(source, sandbox, { filename: 'sites.js' });
  return sandbox;
}

test('custom UA form state exposes and hydrates all fields', () => {
  const { customUAFormState } = loadSiteHelpers();
  const state = customUAFormState('custom', {
    custom_user_agent: 'Meridian Custom/1.0',
    custom_client: 'Meridian Custom',
    custom_version: '1.0.0',
  });

  assert.equal(state.visible, true);
  assert.equal(state.required, true);
  assert.equal(state.customUserAgent, 'Meridian Custom/1.0');
  assert.equal(state.customClient, 'Meridian Custom');
  assert.equal(state.customVersion, '1.0.0');
});

test('preset form state hides and clears custom UA values', () => {
  const { customUAFormState } = loadSiteHelpers();
  const state = customUAFormState('web', {
    custom_user_agent: 'stale',
    custom_client: 'stale',
    custom_version: 'stale',
  });

  assert.equal(state.visible, false);
  assert.equal(state.required, false);
  assert.equal(state.customUserAgent, '');
  assert.equal(state.customClient, '');
  assert.equal(state.customVersion, '');
});

test('custom UA payload trims custom values and preset payload clears them', () => {
  const { buildCustomUAPayload } = loadSiteHelpers();
  const custom = buildCustomUAPayload('custom', ' UA ', ' Client ', ' 1.2.3 ');
  assert.equal(custom.custom_user_agent, 'UA');
  assert.equal(custom.custom_client, 'Client');
  assert.equal(custom.custom_version, '1.2.3');

  const preset = buildCustomUAPayload('infuse', 'stale', 'stale', 'stale');
  assert.equal(preset.custom_user_agent, '');
  assert.equal(preset.custom_client, '');
  assert.equal(preset.custom_version, '');
});
