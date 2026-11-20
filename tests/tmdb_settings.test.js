'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const vm = require('node:vm');

function tmdbPageHarness(overrides = {}) {
  const fields = new Map();
  const submissions = [];
  const errors = [];
  const stored = {
    enabled: true, configured: true, secret_stable: true, language: 'zh-CN',
    history_retention_days: 365, credential_state: 'ready', last_error_code: '',
    last_tested_at_ms: 1795132800000, cache_entries: 31, cache_size_bytes: 4096,
    ...overrides,
  };
  function attributes(source) {
    return Object.fromEntries([...source.matchAll(/([\w-]+)(?:="([^"]*)")?/g)].map(match => [match[1], match[2] ?? true]));
  }
  const content = {
    html: '',
    set innerHTML(html) {
      this.html = html;
      fields.forEach(field => { field.isConnected = false; });
      fields.clear();
      for (const [, , rawAttributes] of html.matchAll(/<(input|select|button|div|b)\b([^>]*\bid="[^"]+"[^>]*)>/g)) {
        const attrs = attributes(rawAttributes);
        fields.set(attrs.id, { value: attrs.value ?? '', disabled: attrs.disabled === true, isConnected: true, textContent: '' });
      }
      for (const [, rawAttributes, optionsHTML] of html.matchAll(/<select\b([^>]*)>([\s\S]*?)<\/select>/g)) {
        const attrs = attributes(rawAttributes);
        const options = [...optionsHTML.matchAll(/<option\b([^>]*)>/g)].map(match => attributes(match[1]));
        fields.get(attrs.id).value = (options.find(option => option.selected === true) || options[0]).value;
      }
    },
  };
  const page = {
    isConnected: true,
    querySelector(selector) { return selector === '.settings-content' ? content : fields.get(selector.slice(1)); },
  };
  const sandbox = {
    document: { getElementById(id) { return id === 'page-settings-tmdb' ? page : fields.get(id); } },
    Router: { current: 'settings-tmdb' },
    API: {
      getTMDBSettings() { return Promise.resolve({ ...stored }); },
      saveTMDBSettings(payload) {
        submissions.push({ ...payload });
        Object.assign(stored, payload);
        return Promise.resolve({ ...stored });
      },
    },
    Toast: { error(message) { errors.push(message); }, success() {} },
    esc: String,
    setInterval() { return 1; }, clearInterval() {},
  };
  vm.createContext(sandbox);
  vm.runInContext(fs.readFileSync(path.join(__dirname, '../web/static/js/pages/tmdb-settings.js'), 'utf8'), sandbox);
  sandbox.paintTMDBSettings({ ...stored });
  return { sandbox, submissions, errors, stored, content, field: id => fields.get(id) };
}

test('changing TMDB language through the rendered form preserves the saved history retention', async () => {
  const h = tmdbPageHarness();
  h.field('tmdb-language').value = 'en-US';
  await h.field('tmdb-save').onclick();
  assert.deepEqual(h.submissions, [{ enabled: true, language: 'en-US', history_retention_days: 365 }]);
  assert.equal(h.field('tmdb-history-retention').value, '365');
  assert.equal(h.stored.history_retention_days, 365);
});

test('TMDB settings retain credentials, secret stability and test metadata when normalized again', () => {
  const h = tmdbPageHarness();
  const parsed = h.sandbox.normalizeTMDBSettings({ settings: {
    enabled: true, configured: true, secret_stable: false, language: 'en-US',
    history_retention_days: 365, credential_state: 'invalid', last_error_code: 'invalid_credentials',
    last_tested_at_ms: 1795132800000, cache_entries: 31, cache_size_bytes: 4096,
  } });
  const reparsed = h.sandbox.normalizeTMDBSettings(parsed);
  assert.deepEqual(JSON.parse(JSON.stringify(reparsed)), {
    enabled: true, configured: true, secretStable: false, language: 'en-US',
    historyRetentionDays: 365, cacheEntries: 31, cacheSizeBytes: 4096,
    credentialState: 'invalid', lastErrorCode: 'invalid_credentials', lastTestedAtMS: 1795132800000,
  });
});

test('the rendered TMDB page keeps ready and invalid credential status visible', () => {
  const ready = tmdbPageHarness({ credential_state: 'ready' });
  assert.match(ready.content.html, /TMDB 资料补全正常/);
  const invalid = tmdbPageHarness({ credential_state: 'invalid', last_error_code: 'invalid_credentials' });
  assert.match(invalid.content.html, /TMDB Token 无效/);
});

test('a temporary secret remains visible and prevents submitting a replacement TMDB token', async () => {
  const h = tmdbPageHarness({ secret_stable: false });
  assert.match(h.content.html, /当前密钥为临时值/);
  h.field('tmdb-api-token').value = 'synthetic-replacement-token';
  await h.field('tmdb-save').onclick();
  assert.equal(h.submissions.length, 0);
  assert.deepEqual(h.errors, ['当前 JWT_SECRET 为临时值，无法安全保存 TMDB Token']);
});
