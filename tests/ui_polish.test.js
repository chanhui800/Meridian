'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');

const ROOT = path.join(__dirname, '..');
const indexSource = fs.readFileSync(path.join(ROOT, 'web/static/index.html'), 'utf8');
const appSource = fs.readFileSync(path.join(ROOT, 'web/static/js/app.js'), 'utf8');
const dashboardSource = fs.readFileSync(path.join(ROOT, 'web/static/js/pages/dashboard.js'), 'utf8');
const cssSource = fs.readFileSync(path.join(ROOT, 'web/static/css/style.css'), 'utf8');

test('GitHub project link points to the official repository', () => {
  assert.match(indexSource, /class="github-project-link" href="https:\/\/github\.com\/snnabb\/Meridian"/);
  assert.doesNotMatch(indexSource, /github\.com\/chanhui800\/Meridian/);
});

test('site modal resets overlay and content scroll on every open', () => {
  assert.match(appSource, /function resetModalScroll\(\)/);
  assert.match(appSource, /overlay\.scrollTop = 0/);
  assert.match(appSource, /body\.scrollTop = 0/);
  assert.match(appSource, /requestAnimationFrame\(resetModalScroll\)/);
});

test('dashboard trends trace smooth curves instead of only straight segments', () => {
  assert.match(dashboardSource, /function dashboardTraceSmoothLine\(ctx, points\)/);
  assert.match(dashboardSource, /ctx\.bezierCurveTo\(/);
  assert.match(dashboardSource, /dashboardTraceSmoothLine\(ctx, pointsOnCanvas\)/);
});

test('mobile navigation keeps the header and drawer available', () => {
  assert.match(cssSource, /@media \(max-width: 768px\)[\s\S]*?\.app-header \{[\s\S]*?display: flex;[\s\S]*?\.sidebar \{[\s\S]*?display: flex;/);
  assert.match(cssSource, /#app-shell\.sidebar-expanded \.sidebar \{ transform: translateX\(0\); \}/);
});
test('upstream header rows do not inherit the browser fieldset frame', () => {
  assert.match(cssSource, /\.form-list-row\.upstream-header-row[\s\S]*?border: 0;/);
  assert.match(cssSource, /\.site-config-modal \.upstream-line-labels,[\s\S]*?grid-template-columns: 76px minmax\(150px, \.8fr\)/);
});
