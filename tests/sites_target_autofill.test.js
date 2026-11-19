'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const vm = require('node:vm');

function loadHelpers() {
  const source = fs.readFileSync(path.join(__dirname, '..', 'web', 'static', 'js', 'pages', 'sites.js'), 'utf8');
  const sandbox = { window: {}, URL, esc: value => String(value) };
  vm.createContext(sandbox);
  vm.runInContext(source, sandbox, { filename: 'sites.js' });
  return sandbox;
}

test('site address autofill completes scheme and splits the port', () => {
  const { autoFillTargetAddress } = loadHelpers();
  const fill = raw => {
    const address = { value: raw };
    const port = { value: '' };
    autoFillTargetAddress(address, port);
    return { address: address.value, port: port.value };
  };
  assert.deepEqual(fill('emby.example.com'), { address: 'https://emby.example.com', port: '443' });
  assert.deepEqual(fill('emby.example.com/emby'), { address: 'https://emby.example.com/emby', port: '443' });
  assert.deepEqual(fill('media.example.com:8096'), { address: 'http://media.example.com', port: '8096' });
  assert.deepEqual(fill('media.example.com:443'), { address: 'https://media.example.com', port: '443' });
  assert.deepEqual(fill('lingj.k:80'), { address: 'http://lingj.k', port: '80' });
  assert.deepEqual(fill('https://cdn.example.com:8443/base?x=1'), { address: 'https://cdn.example.com/base?x=1', port: '8443' });
  assert.deepEqual(fill('https://cdn.example.com:443/base'), { address: 'https://cdn.example.com/base', port: '443' });
  assert.deepEqual(fill('http://plain.example.com'), { address: 'http://plain.example.com', port: '80' });
  assert.deepEqual(fill('http://plain.example.com:80'), { address: 'http://plain.example.com', port: '80' });
  assert.deepEqual(fill('[::1]:8096'), { address: 'http://[::1]', port: '8096' });
  assert.deepEqual(fill('media.example.com：8096'), { address: 'http://media.example.com', port: '8096' });
  assert.deepEqual(fill('not a url with spaces'), { address: 'not a url with spaces', port: '' });
  assert.deepEqual(fill('   '), { address: '   ', port: '' });
});
