'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');

const css = fs.readFileSync(path.join(__dirname, '..', 'web', 'static', 'css', 'nodes-console.css'), 'utf8');

test('node scheduling keeps the final mobile layout within the viewport', () => {
  const desktopOverride = css.indexOf('.route-list-heading, .route-summary');
  const mobileOverride = css.lastIndexOf('@media (max-width: 560px)');

  assert.ok(desktopOverride >= 0, 'desktop node scheduling grid is missing');
  assert.ok(mobileOverride > desktopOverride, 'mobile override must follow the final desktop grid');

  const mobile = css.slice(mobileOverride);
  assert.match(mobile, /\.route-editor-fields\s*\{[^}]*grid-template-columns:\s*minmax\(0, 1fr\)/s);
  assert.match(mobile, /\.fleet-console \.route-field \.form-input\s*\{[^}]*width:\s*100%[^}]*max-width:\s*100%/s);
  assert.match(mobile, /\.route-editor-actions \.console-button\s*\{[^}]*min-height:\s*44px/s);
});

test('new node surface has no decorative blur and respects reduced motion', () => {
  assert.doesNotMatch(css, /backdrop-filter|linear-gradient/);
  assert.match(css, /@media \(prefers-reduced-motion: reduce\)/);
  assert.match(css, /:focus-visible/);
});
