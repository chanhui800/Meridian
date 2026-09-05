const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const root = path.resolve(__dirname, '..');
const html = fs.readFileSync(path.join(root, 'web/static/index.html'), 'utf8');
const api = fs.readFileSync(path.join(root, 'web/static/js/api.js'), 'utf8');
const page = fs.readFileSync(path.join(root, 'web/static/js/pages/nodes.js'), 'utf8');
const style = fs.readFileSync(path.join(root, 'web/static/css/style.css'), 'utf8');
const router = fs.readFileSync(path.join(root, 'web/static/js/router.js'), 'utf8');

test('node scheduling is a first-level page with refresh cleanup', () => {
  assert.match(html, /data-page="nodes"/);
  assert.match(html, /id="page-nodes"/);
  assert.match(html, /pages\/nodes\.js/);
  assert.match(router, /previous === 'nodes'.*stopNodesRefresh/s);
});

test('node API exposes CRUD, enrollment refresh, and scheduler verbs', () => {
  assert.match(api, /getNodes\(\).*'GET', '\/api\/nodes'/s);
  assert.match(api, /createNode\(data\).*'POST', '\/api\/nodes'/s);
  assert.match(api, /updateNode\(id, data\).*'PUT'/s);
  assert.match(api, /deleteNode\(id\).*'DELETE'/s);
  assert.match(api, /refreshNodeEnrollment.*'POST'.*\/enrollment/s);
  assert.match(api, /saveNodeScheduler\(data\).*'PUT', '\/api\/node-scheduler'/s);
});

test('node page wires selectable manual mode, deletion, and DNS safety', () => {
  assert.match(page, /name="manual-node"/);
  assert.match(page, /请选择一个节点/);
  assert.match(page, /API\.deleteNode/);
  assert.match(page, /DNS 仅在 Agent 应用配置并通过域名证书与入口健康检查后切换/);
  assert.match(page, /已有连接不会被强制迁移/);
});

test('node form exposes one HTTPS port without entry or gateway modes', () => {
  assert.doesNotMatch(page, /入口模式|Agent 本地端口|共享现有 Nginx\/Caddy|gateway_snippets|网关配置/);
  assert.match(page, /<label>端口<input[^>]+id="node-port"/);
  assert.match(page, /location\.port \|\| 443/);
  assert.match(page, /Agent 仅提供 TLS\/HTTPS/);
  assert.match(page, /所有调度站点共用此端口并按域名区分/);
  assert.match(page, /port: Number\(document\.getElementById\('node-port'\)\.value\)/);
  assert.doesNotMatch(page, /entry_mode:|http_port:|https_port:/);
});

test('site scheduling is opt-in and uses authenticated scheduler APIs', () => {
  assert.match(api, /getSiteNodeSchedules\(\).*'GET', '\/api\/node-scheduler\/sites'/s);
  assert.match(api, /saveSiteNodeSchedule\(id, data\).*'PUT'/s);
  assert.match(page, /data-field="enabled"/);
  assert.match(page, /跟随全局调度/);
  assert.match(page, /固定节点/);
  assert.match(page, /创建或更新精确 DNS 记录/);
  assert.match(page, /未启用节点调度，继续使用原面板入口/);
  assert.match(page, /原面板模式 · 节点调度未启用/);
  assert.match(page, /syncSiteScheduleRow/);
  assert.match(page, /enabled && mode === 'fixed'/);
  assert.match(page, /node-site-card/);
  assert.match(page, /保存站点设置/);
  assert.match(style, /\.node-site-card\s*\{[\s\S]*grid-template-columns:\s*minmax\(0, 1fr\)/);
});

test('node schedule refresh preserves unsaved checkbox and selector edits', () => {
  const fields = {
    '[data-field="enabled"]': { checked: false },
    '[data-field="mode"]': { value: 'global' },
    '[data-field="fixed-node"]': { value: '' },
  };
  const row = {
    dataset: { siteId: '1' },
    querySelector(selector) { return fields[selector] || null; },
  };
  const container = { innerHTML: '' };
  const sandbox = {
    console,
    Map,
    Number,
    String,
    Math,
    document: {
      getElementById(id) { return id === 'node-site-list' ? container : null; },
      querySelectorAll(selector) { return selector === '#node-site-list .node-site-row' ? [row] : []; },
    },
    esc(value) { return String(value); },
    meridianFormatDateTime(value) { return String(value); },
  };
  vm.createContext(sandbox);
  vm.runInContext(page, sandbox);
  vm.runInContext(`siteSchedulesSnapshot = { sites: [{ site_id: 1, site_name: 'site', public_host: 'site.example', enabled: true, mode: 'global', fixed_node_id: 0, desired_node_id: 42, desired_node_name: 'node', applied_node_id: 42, applied_node_name: 'node', applied_node_port: 9090, dns_status: 'active', last_error: '' }] }; nodesSnapshot = { nodes: [{ id: 42, name: 'node' }] };`, sandbox);

  sandbox.captureSiteScheduleDrafts();
  sandbox.renderSiteSchedules();
  assert.doesNotMatch(container.innerHTML, /data-field="enabled" checked/);
  assert.match(container.innerHTML, /原面板模式 · 节点调度未启用/);

  fields['[data-field="enabled"]'].checked = true;
  fields['[data-field="mode"]'].value = 'fixed';
  fields['[data-field="fixed-node"]'].value = '42';
  sandbox.captureSiteScheduleDrafts();
  sandbox.renderSiteSchedules();
  assert.match(container.innerHTML, /data-field="enabled" checked/);
  assert.match(container.innerHTML, /<option value="fixed" selected>固定节点<\/option>/);
  assert.match(container.innerHTML, /value="42" selected/);
});
