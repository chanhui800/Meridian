'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const vm = require('node:vm');

const STATIC_JS = path.join(__dirname, '..', 'web', 'static', 'js');

// Loads api.js into a sandbox where fetch/window are injected per test, the
// same way a browser page provides them.
function loadAPIClient() {
  const sandbox = { window: {} };
  vm.createContext(sandbox);
  vm.runInContext(
    fs.readFileSync(path.join(STATIC_JS, 'api.js'), 'utf8'),
    sandbox,
    { filename: 'api.js' },
  );
  return sandbox;
}

test('401 on a protected call logs out, reloads, and stops the request flow', async () => {
  const sandbox = loadAPIClient();
  let reloads = 0;
  let logoutCalled = false;
  let bodyParsed = false;
  sandbox.window.location = { reload() { reloads++; } };
  sandbox.fetch = async (url) => {
    if (String(url).endsWith('/api/auth/logout')) {
      logoutCalled = true;
      return { status: 200, ok: true, json: async () => ({}) };
    }
    return {
      status: 401,
      ok: false,
      statusText: 'Unauthorized',
      json: async () => {
        bodyParsed = true;
        return { error: 'session expired' };
      },
    };
  };

  const result = await vm.runInContext('API.request("GET", "/api/dashboard")', sandbox);

  assert.equal(logoutCalled, true, 'logout must be triggered on 401');
  assert.equal(reloads, 1, 'page must reload on 401');
  assert.equal(bodyParsed, false, 'the stale 401 body must not be parsed after logout/reload');
  assert.equal(result, undefined, 'the request must terminate instead of resolving with a value');
});

test('401 on the login endpoint is an ordinary failure the caller can report', async () => {
  const sandbox = loadAPIClient();
  let reloads = 0;
  sandbox.window.location = { reload() { reloads++; } };
  sandbox.fetch = async () => ({
    status: 401,
    ok: false,
    statusText: 'Unauthorized',
    json: async () => ({ error: '用户名或密码错误' }),
  });

  await assert.rejects(
    vm.runInContext('API.request("POST", "/api/auth/login", { username: "a", password: "b" })', sandbox),
    /用户名或密码错误/,
  );
  assert.equal(reloads, 0, 'a failed login must not reload the page');
});

test('dynamic discovery API calls use the exact authenticated paths and verbs', async () => {
  const sandbox = loadAPIClient();
  const requests = [];
  sandbox.fetch = async (url, options) => {
    requests.push({ url, options });
    return {
      status: 200,
      ok: true,
      json: async () => ({ observations: [], dropped_observations: 0 }),
    };
  };

  await vm.runInContext('API.getDynamicProfiles()', sandbox);
  await vm.runInContext('API.getDynamicObservations("site/42 ?")', sandbox);
  await vm.runInContext('API.deleteDynamicObservations("site/42 ?")', sandbox);

  assert.deepEqual(requests.map(request => [request.options.method, request.url]), [
    ['GET', '/api/dynamic-profiles'],
    ['GET', '/api/sites/site%2F42%20%3F/dynamic-observations'],
    ['DELETE', '/api/sites/site%2F42%20%3F/dynamic-observations'],
  ]);
  for (const request of requests) {
    assert.equal(request.options.credentials, 'same-origin');
    assert.equal(request.options.body, undefined, 'read/clear calls must not send a request body');
    assert.equal(Object.keys(request.options.headers).length, 0);
  }
});

test('mobile credential inputs disable keyboard text transformations', () => {
  const html = fs.readFileSync(path.join(__dirname, '..', 'web', 'static', 'index.html'), 'utf8');

  for (const id of ['inp-username', 'inp-password', 'inp-setup-token']) {
    const input = html.match(new RegExp(`<input\\b(?=[^>]*\\bid="${id}")[^>]*>`));
    assert.ok(input, `missing ${id} input`);
    assert.match(input[0], /\bautocapitalize="none"/);
    assert.match(input[0], /\bautocorrect="off"/);
    assert.match(input[0], /\bspellcheck="false"/);
  }
});
