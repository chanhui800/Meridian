'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');

const css = fs.readFileSync(path.join(__dirname, '..', 'web', 'static', 'css', 'style.css'), 'utf8');

test('node scheduling keeps the final mobile layout within the viewport', () => {
  const desktopOverride = css.indexOf('grid-template-columns: minmax(220px, 1.25fr)');
  const mobileOverride = css.lastIndexOf('@media (max-width: 680px)');

  assert.ok(desktopOverride >= 0, 'desktop node scheduling grid is missing');
  assert.ok(mobileOverride > desktopOverride, 'mobile override must follow the final desktop grid');

  const mobile = css.slice(mobileOverride);
  assert.match(mobile, /\.node-site-row\s*\{[^}]*grid-template-columns:\s*minmax\(0, 1fr\)/s);
  assert.match(mobile, /\.node-site-row \.form-input\s*\{[^}]*width:\s*100%[^}]*max-width:\s*100%/s);
  assert.match(mobile, /\.node-site-row > \.node-button\s*\{[^}]*width:\s*100%[^}]*min-height:\s*44px/s);
});
