const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const root = path.resolve(__dirname, '..');
const html = fs.readFileSync(path.join(root, 'web/static/index.html'), 'utf8');
const api = fs.readFileSync(path.join(root, 'web/static/js/api.js'), 'utf8');
const page = fs.readFileSync(path.join(root, 'web/static/js/pages/nodes.js'), 'utf8');
const style = fs.readFileSync(path.join(root, 'web/static/css/nodes-console.css'), 'utf8');
const router = fs.readFileSync(path.join(root, 'web/static/js/router.js'), 'utf8');

test('node scheduling is a first-level page with refresh cleanup', () => {
  assert.match(html, /data-page="nodes"/);
  assert.match(html, /id="page-nodes"/);
  assert.match(html, /pages\/nodes\.js/);
  assert.match(router, /previous === 'nodes'.*stopNodesRefresh/s);
});

test('node page surfaces Agent configuration apply errors before generic waiting', () => {
  assert.match(page, /agent_apply_error/);
  assert.match(page, /应用失败：\$\{applyError\}/);
  assert.match(page, /const listenerError = String\(node\.agent_listener_error/);
  assert.match(page, /等待 Agent 应用站点配置/);
});

test('node details retain the reported Agent version', () => {
  assert.match(page, /node\.agent_version \|\| '未上报'/);
  assert.match(page, /class="node-agent-version"/);
  assert.match(style, /\.fleet-detail-grid small\s*\{/);
});

test('site scheduling distinguishes normal config wait from errors and overdue waits', () => {
  const sandbox = { Date, Number, String, Math, Map, console };
  vm.createContext(sandbox);
  vm.runInContext(page, sandbox);
  const pending = sandbox.siteScheduleFeedback({ enabled: true, config_pending_since_ms: 1000, last_error: 'Agent has not applied the site configuration' }, 5000);
  assert.deepEqual({ kind: pending.kind, text: pending.text }, { kind: 'pending', text: '等待 Agent 应用站点配置' });
  const overdue = sandbox.siteScheduleFeedback({ enabled: true, config_pending_since_ms: 1000, last_error: 'Agent has not applied the site configuration' }, 91001);
  assert.deepEqual({ kind: overdue.kind, text: overdue.text }, { kind: 'warning', text: 'Agent 长时间未应用配置' });
  const failed = sandbox.siteScheduleFeedback({ enabled: true, config_pending_since_ms: 1000, last_error: 'Agent configuration apply failed: runtime key is invalid' }, 5000);
  assert.deepEqual({ kind: failed.kind, text: failed.text }, { kind: 'error', text: 'Agent 应用配置失败：runtime key is invalid' });
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
  assert.match(page, /当前 Agent 会继续运行，直到新 Agent 注册成功/);
  assert.doesNotMatch(page, /立即撤销该节点现有 Agent 令牌/);
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
  assert.match(page, /未启用节点调度，继续使用主控入口/);
  assert.match(page, /主控模式 · 节点调度未启用/);
  assert.match(page, /syncSiteScheduleRow/);
  assert.match(page, /refreshSiteScheduleRowState/);
  assert.match(page, /getElementById\('node-site-list'\)\.oninput = handleSiteScheduleAction/);
  assert.match(page, /enabled && mode === 'fixed'/);
  assert.match(page, /class="node-site-row route-row"/);
  assert.match(page, /保存站点设置/);
  assert.match(page, /id="site-editor-\$\{id\}" \$\{open \? '' : 'hidden'\}/);
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
  assert.match(container.innerHTML, /调度已启用 · 待保存/);
  assert.match(container.innerHTML, /期望 node · 生效 node :9090 · DNS active/);

  fields['[data-field="enabled"]'].checked = true;
  fields['[data-field="mode"]'].value = 'fixed';
  fields['[data-field="fixed-node"]'].value = '42';
  sandbox.captureSiteScheduleDrafts();
  sandbox.renderSiteSchedules();
  assert.match(container.innerHTML, /data-field="enabled" checked/);
  assert.match(container.innerHTML, /<option value="fixed" selected>固定节点<\/option>/);
  assert.match(container.innerHTML, /value="42" selected/);
  assert.match(container.innerHTML, /调度已启用 · 待保存/);
  assert.match(container.innerHTML, /期望 node · 生效 node :9090 · DNS active/);
  assert.match(container.innerHTML, /当前修改尚未保存，保存后才会生效/);

  // A saved enabled state remains authoritative while an unsaved disable is
  // being edited: only the form controls reflect the draft.
  siteSchedulesSnapshot = { sites: [{ site_id: 1, site_name: 'site', public_host: 'site.example', enabled: true, mode: 'global', fixed_node_id: 0, desired_node_id: 42, desired_node_name: 'node', applied_node_id: 42, applied_node_name: 'node', applied_node_port: 9090, dns_status: 'active', last_error: '' }] };
  vm.runInContext('siteScheduleDrafts.clear(); siteScheduleDrafts.set(1, { enabled: false, mode: "global", fixed_node_id: 0 });', sandbox);
  sandbox.renderSiteSchedules();
  assert.doesNotMatch(container.innerHTML, /data-field="enabled" checked/);
  assert.match(container.innerHTML, /调度已启用 · 待保存/);
  assert.match(container.innerHTML, /期望 node · 生效 node :9090 · DNS active/);
  assert.match(container.innerHTML, /当前修改尚未保存，保存后才会生效/);
});

test('unsaved schedule draft does not change persisted status label', () => {
  const container = { innerHTML: '' };
  const sandbox = {
    console,
    Map,
    Number,
    String,
    Math,
    document: {
      getElementById(id) { return id === 'node-site-list' ? container : null; },
      querySelectorAll() { return []; },
    },
    esc(value) { return String(value); },
    meridianFormatDateTime(value) { return String(value); },
  };
  vm.createContext(sandbox);
  vm.runInContext(page, sandbox);
  vm.runInContext(`nodesSnapshot = { nodes: [] }; siteSchedulesSnapshot = { sites: [{ site_id: 7, site_name: 'site', public_host: 'site.example', enabled: false, mode: 'global', fixed_node_id: 0, desired_node_id: 0, applied_node_id: 0, dns_status: 'disabled', last_error: '' }] }; siteScheduleDrafts = new Map([[7, { enabled: true, mode: 'global', fixed_node_id: 0 }]]);`, sandbox);
  sandbox.renderSiteSchedules();
  assert.match(container.innerHTML, /使用主控入口 · 待保存/);
  assert.doesNotMatch(container.innerHTML, /调度已启用 · 待保存/);
  assert.match(container.innerHTML, /主控模式 · 节点调度未启用/);
});

test('offline, disabled, exhausted and failed nodes do not look ready', () => {
  const sandbox = { console };
  vm.createContext(sandbox);
  vm.runInContext(page, sandbox);
  const cases = [
    [{ status: 'online', enabled: false, active: true }, '已停用', 'muted'],
    [{ status: 'online', enabled: true, depleted: true }, '额度耗尽', 'warning'],
    [{ status: 'offline', enabled: true, active: true }, '离线', 'error'],
    [{ status: 'online', enabled: true, agent_apply_error: 'apply failed' }, '需处理', 'warning'],
    [{ status: 'online', enabled: true, agent_listener_error: 'port in use' }, '需处理', 'warning'],
  ];
  for (const [node, label, kind] of cases) {
    const state = sandbox.nodeAvailability(node);
    assert.equal(state.label, label);
    assert.equal(state.kind, kind);
  }
});

test('global policy refresh retains the selected unsaved node', () => {
  const elements = {
    'node-mode-auto': { checked: true },
    'node-mode-manual': { checked: false },
    'node-manual-choices': { innerHTML: '', hidden: true },
    'node-save-scheduler': {},
    'node-scheduler-state': {},
  };
  const sandbox = {
    document: { getElementById(id) { return elements[id] || null; } },
    esc: String,
  };
  vm.createContext(sandbox);
  vm.runInContext(page, sandbox);
  vm.runInContext(`nodesSnapshot = { nodes: [{ id: 1, name: 'first', status: 'online' }, { id: 2, name: 'second', status: 'online' }], scheduler: { mode: 'auto', manual_node_id: 0 } }; nodeSchedulerDraft = { mode: 'manual', manual_node_id: 2 };`, sandbox);
  sandbox.renderScheduler();
  assert.equal(elements['node-mode-auto'].checked, false);
  assert.equal(elements['node-mode-manual'].checked, true);
  assert.equal(elements['node-manual-choices'].hidden, false);
  assert.match(elements['node-manual-choices'].innerHTML, /value="2" checked/);
  assert.equal(elements['node-save-scheduler'].disabled, false);
  assert.equal(elements['node-scheduler-state'].textContent, '策略有未保存的修改');

  vm.runInContext(`nodesSnapshot.scheduler = { mode: 'manual', manual_node_id: 1, active_node_id: 1 };`, sandbox);
  sandbox.renderScheduler();
  assert.match(elements['node-manual-choices'].innerHTML, /value="2" checked/);
});

test('collapsed route settings stay collapsed after status refresh', () => {
  const container = { innerHTML: '' };
  const sandbox = {
    document: { getElementById(id) { return id === 'node-site-list' ? container : null; } },
    esc: String,
    meridianFormatDateTime: String,
  };
  vm.createContext(sandbox);
  vm.runInContext(page, sandbox);
  vm.runInContext(`siteSchedulesSnapshot = { sites: [{ site_id: 7, site_name: 'example', enabled: false, mode: 'global' }] }; siteScheduleOpen.add(7);`, sandbox);
  sandbox.renderSiteSchedules();
  assert.doesNotMatch(container.innerHTML, /id="site-editor-7" hidden/);
  assert.match(container.innerHTML, /aria-expanded="true" aria-controls="site-editor-7"/);
  vm.runInContext('siteScheduleOpen.clear()', sandbox);
  sandbox.renderSiteSchedules();
  assert.match(container.innerHTML, /id="site-editor-7" hidden/);
});

test('node refresh is single flight and drops a response after teardown', async () => {
  const list = { innerHTML: 'loading' };
  let resolveNodes;
  let resolveSites;
  let calls = 0;
  const sandbox = {
    console,
    Router: { current: 'nodes' },
    API: {
      getNodes() { calls++; return new Promise(resolve => { resolveNodes = resolve; }); },
      getSiteNodeSchedules() { return new Promise(resolve => { resolveSites = resolve; }); },
    },
    document: {
      getElementById(id) { return id === 'node-list' ? list : null; },
      querySelectorAll() { return []; },
    },
    esc: String,
  };
  vm.createContext(sandbox);
  vm.runInContext(page, sandbox);
  const first = sandbox.loadNodes();
  const second = sandbox.loadNodes();
  assert.equal(calls, 1);
  sandbox.stopNodesRefresh();
  sandbox.Router.current = 'dashboard';
  resolveNodes({ nodes: [{ id: 1, name: 'late node' }], scheduler: {} });
  resolveSites({ sites: [] });
  await Promise.all([first, second]);
  assert.equal(list.innerHTML, 'loading');
});

test('failed route save retains immediate edits and cannot submit twice', async () => {
  const fields = {
    '[data-field="enabled"]': { checked: true, disabled: false },
    '[data-field="mode"]': { value: 'fixed', disabled: false },
    '[data-field="fixed-node"]': { value: '2', disabled: false },
  };
  const row = {
    dataset: { siteId: '7' },
    querySelector(selector) { return fields[selector] || null; },
    querySelectorAll() { return Object.values(fields); },
  };
  const button = { dataset: { action: 'save-site' }, disabled: false };
  const container = { innerHTML: '' };
  const failures = [];
  let rejectSave;
  let calls = 0;
  let submitted;
  const sandbox = {
    Router: { current: 'nodes' },
    API: { saveSiteNodeSchedule(id, payload) {
      calls++;
      submitted = JSON.parse(JSON.stringify({ id, payload }));
      return new Promise((resolve, reject) => { rejectSave = reject; });
    } },
    Toast: { error(message) { failures.push(message); } },
    confirm() { return true; },
    document: {
      getElementById(id) { return id === 'node-site-list' ? container : null; },
      querySelectorAll(selector) { return selector === '#node-site-list .node-site-row' ? [row] : []; },
      querySelector() { return null; },
    },
    esc: String,
    meridianFormatDateTime: String,
  };
  vm.createContext(sandbox);
  vm.runInContext(page, sandbox);
  vm.runInContext(`nodesSnapshot = { nodes: [{ id: 2, name: 'second' }], scheduler: {} }; siteSchedulesSnapshot = { sites: [{ site_id: 7, site_name: 'example', enabled: false, mode: 'global', fixed_node_id: 0 }] }; siteScheduleOpen.add(7);`, sandbox);
  const event = { target: {
    matches() { return false; },
    closest(selector) { return selector === '.node-site-row' ? row : button; },
  } };
  const saving = sandbox.handleSiteScheduleAction(event);
  await sandbox.handleSiteScheduleAction(event);
  assert.equal(calls, 1);
  assert.equal(button.disabled, true);
  assert.ok(Object.values(fields).every(field => field.disabled));
  assert.deepEqual(submitted, { id: 7, payload: { enabled: true, mode: 'fixed', fixed_node_id: 2 } });
  rejectSave(new Error('synthetic save failure'));
  await saving;
  assert.deepEqual(failures, ['synthetic save failure']);
  assert.match(container.innerHTML, /data-field="enabled" checked/);
  assert.match(container.innerHTML, /<option value="fixed" selected>固定节点/);
  assert.match(container.innerHTML, /value="2" selected/);
  assert.match(container.innerHTML, /使用主控入口 · 待保存/);
  assert.doesNotMatch(container.innerHTML, /id="site-editor-7" hidden/);
});

test('a checkbox changed during refresh survives before its debounce', async () => {
  let resolveNodes, resolveSites;
  const timers = [];
  const fields = {
    '[data-field="enabled"]': { checked: true },
    '[data-field="mode"]': { value: 'global' },
    '[data-field="fixed-node"]': { value: '' },
  };
  const row = {
    dataset: { siteId: '7' },
    classList: { add() {}, remove() {} },
    querySelector(selector) { return fields[selector] || null; },
  };
  const container = { innerHTML: '' };
  const sandbox = {
    Router: { current: 'nodes' }, esc: String, meridianFormatDateTime: String,
    setTimeout(fn) { timers.push(fn); return timers.length; }, clearTimeout() {},
    API: {
      getNodes() { return new Promise(resolve => { resolveNodes = resolve; }); },
      getSiteNodeSchedules() { return new Promise(resolve => { resolveSites = resolve; }); },
    },
    document: {
      getElementById(id) { return id === 'node-site-list' ? container : null; },
      querySelectorAll(selector) { return selector === '#node-site-list .node-site-row' ? [row] : []; },
      querySelector() { return null; },
    },
  };
  vm.createContext(sandbox);
  vm.runInContext(page, sandbox);
  const savedSite = { site_id: 7, site_name: 'example', enabled: true, mode: 'global', fixed_node_id: 0, desired_node_id: 1, applied_node_id: 1 };
  sandbox.savedSite = savedSite;
  vm.runInContext('siteSchedulesSnapshot = { sites: [savedSite] };', sandbox);
  const refreshing = sandbox.loadNodes();
  fields['[data-field="enabled"]'].checked = false;
  sandbox.queueSiteScheduleRowSync(row);
  assert.equal(timers.length, 1);
  resolveNodes({ nodes: [], scheduler: { mode: 'auto', manual_node_id: 0 } });
  resolveSites({ sites: [savedSite] });
  await refreshing;
  assert.doesNotMatch(container.innerHTML, /data-field="enabled" checked/);
  assert.match(container.innerHTML, /调度已启用 · 待保存/);
});

test('a refresh started while saving cannot roll back the saved global policy', async () => {
  const elements = new Map();
  const element = id => {
    if (!elements.has(id)) elements.set(id, {
      innerHTML: '', textContent: '', checked: false, disabled: false,
      classList: { add() {}, remove() {} },
      setAttribute() {}, focus() {},
    });
    return elements.get(id);
  };
  Object.assign(element('node-mode-auto'), { name: 'node-mode', value: 'auto', checked: true });
  Object.assign(element('node-mode-manual'), { name: 'node-mode', value: 'manual' });
  const original = { nodes: [{ id: 1, name: 'example', status: 'online', enabled: true }], scheduler: { mode: 'auto', manual_node_id: 0 } };
  let initial = true;
  let resolveNodes, resolveSites, resolveSave;
  const sandbox = {
    Router: { current: 'nodes' }, esc: String, meridianFormatDateTime: String,
    Toast: { success() {}, error(message) { throw new Error(message); } },
    setInterval() { return 1; }, clearInterval() {},
    API: {
      getNodes() { return initial ? Promise.resolve(original) : new Promise(resolve => { resolveNodes = resolve; }); },
      getSiteNodeSchedules() { return initial ? Promise.resolve({ sites: [] }) : new Promise(resolve => { resolveSites = resolve; }); },
      saveNodeScheduler() { return new Promise(resolve => { resolveSave = resolve; }); },
    },
    document: {
      getElementById: element,
      querySelectorAll(selector) { return selector === 'input[name="node-mode"]' ? [element('node-mode-auto'), element('node-mode-manual')] : []; },
      querySelector(selector) { return selector === 'input[name="manual-node"]:checked' ? { value: '1' } : null; },
    },
  };
  vm.createContext(sandbox);
  vm.runInContext(page, sandbox);
  sandbox.renderNodes();
  await sandbox.loadNodes();
  initial = false;
  vm.runInContext('nodeSchedulerDraft = { mode: "manual", manual_node_id: 1 };', sandbox);
  sandbox.renderScheduler();
  const saving = element('node-save-scheduler').onclick();
  const refreshing = sandbox.loadNodes();
  resolveSave({ ...original, scheduler: { mode: 'manual', manual_node_id: 1, active_node_id: 1 } });
  await saving;
  assert.equal(element('node-mode-manual').checked, true);
  resolveNodes(original);
  resolveSites({ sites: [] });
  await refreshing;
  assert.equal(element('node-mode-manual').checked, true);
  assert.equal(element('node-mode-auto').checked, false);
});
