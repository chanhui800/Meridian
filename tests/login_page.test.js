'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');

const root = path.join(__dirname, '..');
const html = fs.readFileSync(path.join(root, 'web', 'static', 'index.html'), 'utf8');
const app = fs.readFileSync(path.join(root, 'web', 'static', 'js', 'app.js'), 'utf8');
const css = fs.readFileSync(path.join(root, 'web', 'static', 'css', 'style.css'), 'utf8');

test('login errors remain visible while the application shell is hidden', () => {
  assert.match(html, /id="login-message"[^>]*role="alert"[^>]*aria-live="assertive"/);
  assert.match(html, /密码连续输错 5 次将锁定登录 15 分钟；登录成功后会自动清零错误次数/);
  assert.match(
    html,
    /<\/div>\s*<!-- Toast must remain outside the hidden application shell so login errors are visible\. -->\s*<div id="toast-container"/,
  );
  assert.match(css, /\.login-message\s*\{[\s\S]*?background:\s*var\(--red-dim\)/);
});

test('login failures distinguish credentials from a rate-limit countdown', () => {
  assert.match(app, /用户名或密码错误。连续输错 5 次将锁定登录 15 分钟。/);
  assert.match(app, /登录尝试次数过多，请在 \$\{loginRetryText\(remaining\)\}后重试/);
  assert.match(app, /loginButtonEl\.textContent = `请等待 \$\{loginRetryText\(remaining\)\}`/);
  assert.match(app, /startLoginRetryCountdown\(failure\.retryAfter\)/);
});
