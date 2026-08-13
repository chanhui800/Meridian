'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

function loadSource(file) {
  return fs.readFileSync(path.join(__dirname, '..', file), 'utf8');
}

test('backup and restore API never put the backup password in a URL', () => {
  const source = loadSource('web/static/js/api.js');
  assert.match(source, /fetch\('\/api\/backup\/export'/);
  assert.match(source, /body:\s*JSON\.stringify\(\{ password, include_tls: includeTLS === true \}\)/);
  assert.match(source, /body\.append\('password', password\)/);
  assert.doesNotMatch(source, /backup\/export[^'"\n]*password=/);
});

test('global settings navigation and router include backup and restore', () => {
  const page = loadSource('web/static/js/pages/global-settings.js');
  const router = loadSource('web/static/js/router.js');
  const index = loadSource('web/static/index.html');
  assert.match(page, /href="#backup-restore"/);
  assert.match(page, /function renderBackupRestore\(/);
  assert.match(page, /settingsCheck\('backup-include-tls',[\s\S]*?false,/);
  assert.match(page, /confirmation !== '恢复'/);
  assert.match(router, /'backup-restore'/);
  assert.match(router, /parentRoutes: new Set\(\[[^\]]*'backup-restore'/s);
  assert.match(index, /id="page-backup-restore"/);
});

test('backup filename parser only returns the attachment filename', () => {
  const source = loadSource('web/static/js/pages/global-settings.js');
  const context = { console, Date, URL, setTimeout, document: {}, API: {}, Toast: {}, Router: {} };
  vm.createContext(context);
  vm.runInContext(source, context);
  assert.equal(context.backupFilename('attachment; filename="meridian-backup-test.mrbak"'), 'meridian-backup-test.mrbak');
  assert.match(context.backupFilename(''), /^meridian-backup-\d{4}-\d{2}-\d{2}\.mrbak$/);
});
