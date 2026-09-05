let nodesRefreshTimer = null;
let nodesSnapshot = { nodes: [], scheduler: { mode: 'auto', manual_node_id: 0, active_node_id: 0 } };
let siteSchedulesSnapshot = { sites: [] };
// Keep edits in the current page alive while the five-second status refresh
// replaces the server snapshot. Drafts are cleared after a successful save or
// when the page is recreated.
let siteScheduleDrafts = new Map();

function stopNodesRefresh() {
  if (nodesRefreshTimer) clearInterval(nodesRefreshTimer);
  nodesRefreshTimer = null;
}

function nodeBytes(value) {
  const bytes = Math.max(0, Number(value) || 0);
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
  let index = 0;
  let current = bytes;
  while (current >= 1024 && index < units.length - 1) { current /= 1024; index++; }
  return `${current.toFixed(index ? 2 : 0)} ${units[index]}`;
}

function nodeStatusLabel(node) {
  return { online: '在线', offline: '离线', pending: '待安装' }[node.status] || node.status;
}

function renderNodeCards() {
  const container = document.getElementById('node-list');
  if (!container) return;
  if (!nodesSnapshot.nodes.length) {
    container.innerHTML = '<div class="node-empty">还没有节点。创建节点后会生成一次性安装脚本。</div>';
    return;
  }
  container.innerHTML = nodesSnapshot.nodes.map(node => {
    const usage = node.traffic_quota > 0 ? `${nodeBytes(node.traffic_used)} / ${nodeBytes(node.traffic_quota)}` : `${nodeBytes(node.traffic_used)} / 不限`;
    const reset = node.reset_day === 0 ? '不自动重置' : `每月 ${node.reset_day} 日重置`;
    const entry = `HTTPS :${node.port}`;
    const configState = node.agent_listener_error ? `监听异常：${node.agent_listener_error}` : (node.desired_config_hash && node.desired_config_hash === node.applied_config_hash ? '配置已应用' : '等待 Agent 应用配置');
    return `<article class="node-card ${node.active ? 'is-active' : ''}">
      <div class="node-card-head"><div><h3>${esc(node.name)}</h3><p>${esc(node.address || '未填写地址')} · ${esc(node.interface_name || '等待识别网卡')}</p></div>
      <span class="node-status is-${esc(node.status)}">${esc(nodeStatusLabel(node))}</span></div>
      <div class="node-stats"><span><b>${usage}</b><small>${esc(node.billing_mode === 'bidirectional' ? '上下行计费' : '上行计费')}</small></span><span><b>${esc(reset)}</b><small>独立流量周期</small></span><span><b>${node.priority}</b><small>优先级${node.active ? ' · 当前选中' : ''}</small></span></div>
      <div class="node-entry-state"><span>${esc(entry)}</span><small class="${node.agent_listener_error ? 'is-error' : ''}">${esc(configState)}</small></div>
      <div class="node-actions"><button type="button" data-action="edit" data-id="${node.id}">编辑</button><button type="button" data-action="enroll" data-id="${node.id}">重新生成脚本</button><button type="button" class="is-danger" data-action="delete" data-id="${node.id}">删除</button></div>
    </article>`;
  }).join('');
}

function renderScheduler() {
  const scheduler = nodesSnapshot.scheduler || {};
  const auto = document.getElementById('node-mode-auto');
  const manual = document.getElementById('node-mode-manual');
  if (!auto || !manual) return;
  auto.checked = scheduler.mode !== 'manual';
  manual.checked = scheduler.mode === 'manual';
  const choices = document.getElementById('node-manual-choices');
  choices.innerHTML = nodesSnapshot.nodes.length ? nodesSnapshot.nodes.map(node => `<label class="node-choice"><input type="radio" name="manual-node" value="${node.id}" ${Number(scheduler.manual_node_id) === Number(node.id) ? 'checked' : ''}><span>${esc(node.name)}</span><small>${esc(nodeStatusLabel(node))}</small></label>`).join('') : '<span class="node-choice-empty">请先创建节点</span>';
  choices.hidden = !manual.checked;
}

async function loadNodes() {
  captureSiteScheduleDrafts();
  try {
    const [snapshot, siteSchedules] = await Promise.all([API.getNodes(), API.getSiteNodeSchedules()]);
    if (!snapshot || !siteSchedules || Router.current !== 'nodes') return;
    nodesSnapshot = snapshot;
    siteSchedulesSnapshot = siteSchedules;
    renderScheduler();
    renderNodeCards();
    renderSiteSchedules();
  } catch (error) {
    if (Router.current === 'nodes') Toast.error(error.message || '节点数据加载失败');
  }
}

function showNodeScript(script, command) {
  document.getElementById('modal-title').textContent = 'Agent 一键安装脚本';
  document.getElementById('modal-body').innerHTML = `${command ? '<p class="node-script-note">推荐直接执行以下命令（不会进入分页器）：</p><textarea class="node-script" id="node-install-command" readonly></textarea>' : ''}<p class="node-script-note">脚本含一次性令牌，24 小时内有效。请在目标 Linux amd64 或 arm64 VPS 上以 root 执行。</p><textarea class="node-script" id="node-install-script" readonly></textarea>`;
  document.getElementById('node-install-script').value = script;
  if (command) document.getElementById('node-install-command').value = command;
  document.getElementById('modal-footer').innerHTML = `${command ? '<button type="button" class="node-button" id="node-copy-command">复制一键命令</button>' : ''}<button type="button" class="node-button" id="node-copy-script">复制脚本</button><button type="button" class="node-button is-primary" id="node-close-script">完成</button>`;
  openModal({ closeOnBackdrop: false });
  if (command) document.getElementById('node-copy-command').onclick = async () => { await navigator.clipboard.writeText(command); Toast.success('一键命令已复制'); };
  document.getElementById('node-copy-script').onclick = async () => { await navigator.clipboard.writeText(script); Toast.success('脚本已复制'); };
  document.getElementById('node-close-script').onclick = closeModal;
}

function openNodeForm(node) {
  const editing = !!node;
  document.getElementById('modal-title').textContent = editing ? '编辑节点' : '创建节点';
  document.getElementById('modal-body').innerHTML = `<form id="node-form" class="node-form">
    <label>节点名称<input class="form-input" id="node-name" maxlength="64" required value="${esc(node ? node.name : '')}"></label>
    <label>显示地址<input class="form-input" id="node-address" maxlength="255" value="${esc(node ? node.address : '')}" placeholder="例如 203.0.113.10"></label>
    <label>端口<input class="form-input" id="node-port" type="number" min="1" max="65535" value="${node && node.port ? node.port : (location.port || 443)}"></label>
    <div class="form-help">Agent 仅提供 TLS/HTTPS。同一节点上的所有调度站点共用此端口并按域名区分；端口必须未被该 VPS 上的其他程序占用。默认采用当前主控端口，保存后独立管理。</div>
    <div class="node-form-grid"><label>流量上限（GiB，0 为不限）<input class="form-input" id="node-quota" type="number" min="0" step="0.01" value="${node ? (Number(node.traffic_quota) / 1073741824).toFixed(2) : '0'}"></label><label>重置日<input class="form-input" id="node-reset" type="number" min="0" max="31" value="${node ? node.reset_day : 1}"></label></div>
    ${editing ? `<label>流量校正（GiB，可正负）<input class="form-input" id="node-offset" type="number" step="0.01" value="${(Number(node.traffic_manual_offset_bytes || 0) / 1073741824).toFixed(2)}"></label><div class="form-help">校正值叠加在网卡周期统计上，后续 Agent 上报不会覆盖。要把显示值调到目标值，可填写正数或负数。</div>` : ''}
    <div class="node-form-grid"><label>计费方式<select class="form-input" id="node-billing"><option value="outbound">上行</option><option value="bidirectional">上下行</option></select></label><label>优先级<input class="form-input" id="node-priority" type="number" min="0" max="1000" value="${node ? node.priority : 100}"></label></div>
    ${editing ? '<label class="node-check"><input id="node-enabled" type="checkbox" checked> 启用节点</label>' : '<label>控制器地址<input class="form-input" id="node-controller" type="url" required></label>'}
  </form>`;
  document.getElementById('node-billing').value = node ? node.billing_mode : 'outbound';
  if (editing) document.getElementById('node-enabled').checked = node.enabled;
  else document.getElementById('node-controller').value = location.origin;
  document.getElementById('modal-footer').innerHTML = '<button type="button" class="node-button" id="node-form-cancel">取消</button><button type="submit" form="node-form" class="node-button is-primary">保存</button>';
  openModal({ closeOnBackdrop: false });
  document.getElementById('node-form-cancel').onclick = closeModal;
  document.getElementById('node-form').onsubmit = async event => {
    event.preventDefault();
    const payload = { name: document.getElementById('node-name').value.trim(), address: document.getElementById('node-address').value.trim(), port: Number(document.getElementById('node-port').value), traffic_quota: Math.round(Number(document.getElementById('node-quota').value || 0) * 1073741824), reset_day: Number(document.getElementById('node-reset').value), billing_mode: document.getElementById('node-billing').value, priority: Number(document.getElementById('node-priority').value), traffic_manual_offset_bytes: Math.round(Number(document.getElementById('node-offset')?.value || 0) * 1073741824) };
    try {
      if (editing) { payload.enabled = document.getElementById('node-enabled').checked; await API.updateNode(node.id, payload); closeModal(); Toast.success('节点已更新'); }
      else { payload.controller_url = document.getElementById('node-controller').value.trim(); const result = await API.createNode(payload); showNodeScript(result.install_script, result.install_command); }
      await loadNodes();
    } catch (error) { Toast.error(error.message || '保存失败'); }
  };
}

async function handleNodeAction(event) {
  const button = event.target.closest('button[data-action]');
  if (!button) return;
  const node = nodesSnapshot.nodes.find(item => Number(item.id) === Number(button.dataset.id));
  if (!node) return;
  if (button.dataset.action === 'edit') return openNodeForm(node);
  if (button.dataset.action === 'delete') {
    if (!confirm(`确认删除节点“${node.name}”？已安装的 Agent 也将失去授权。`)) return;
    try { await API.deleteNode(node.id); Toast.success('节点已删除'); await loadNodes(); } catch (error) { Toast.error(error.message); }
    return;
  }
  if (button.dataset.action === 'enroll') {
    if (!confirm('这会立即撤销该节点现有 Agent 令牌，确认继续？')) return;
    try { const result = await API.refreshNodeEnrollment(node.id, location.origin); showNodeScript(result.install_script, result.install_command); await loadNodes(); } catch (error) { Toast.error(error.message); }
  }
}

function nodeName(id) {
  const node = nodesSnapshot.nodes.find(item => Number(item.id) === Number(id));
  return node ? node.name : '—';
}

function captureSiteScheduleDrafts() {
  const rows = document.querySelectorAll('#node-site-list .node-site-row');
  rows.forEach(row => {
    const siteID = Number(row.dataset.siteId);
    if (!Number.isFinite(siteID) || siteID <= 0) return;
    const enabled = row.querySelector('[data-field="enabled"]')?.checked === true;
    const mode = row.querySelector('[data-field="mode"]')?.value || 'global';
    const fixedNodeID = Number(row.querySelector('[data-field="fixed-node"]')?.value || 0);
    const server = (siteSchedulesSnapshot.sites || []).find(site => Number(site.site_id) === siteID);
    if (!server || (enabled === (server.enabled === true) && mode === (server.mode || 'global') && fixedNodeID === Number(server.fixed_node_id || 0))) {
      siteScheduleDrafts.delete(siteID);
      return;
    }
    siteScheduleDrafts.set(siteID, { enabled, mode, fixed_node_id: fixedNodeID });
  });
}

function renderSiteSchedules() {
  const container = document.getElementById('node-site-list');
  if (!container) return;
  const sites = siteSchedulesSnapshot.sites || [];
  if (!sites.length) { container.innerHTML = '<div class="node-empty">还没有可调度的站点。</div>'; return; }
  container.innerHTML = sites.map(site => {
    const draft = siteScheduleDrafts.get(Number(site.site_id));
    const view = draft ? { ...site, ...draft } : site;
    const scheduleEnabled = view.enabled === true;
    const nodeOptions = nodesSnapshot.nodes.map(node => `<option value="${node.id}" ${Number(view.fixed_node_id) === Number(node.id) ? 'selected' : ''}>${esc(node.name)}</option>`).join('');
    const error = site.last_error ? `<small class="is-error">${esc(site.last_error)}</small>` : `<small>${scheduleEnabled ? 'DNS 只会在 Agent 配置与入口健康检查通过后生效' : '未启用节点调度，继续使用原面板入口'}</small>`;
    return `<article class="node-site-row node-site-card" data-site-id="${site.site_id}">
      <header class="node-site-card-head"><div class="node-site-identity"><strong>${esc(site.site_name)}</strong><span>${esc(site.public_host || '未配置站点域名')}</span></div><span class="node-site-state ${scheduleEnabled ? 'is-enabled' : ''}">${scheduleEnabled ? '调度已启用' : '使用面板入口'}</span></header>
      <div class="node-site-card-controls">
        <label class="node-check"><input type="checkbox" data-field="enabled" ${scheduleEnabled ? 'checked' : ''}> 启用节点调度</label>
        <label class="node-site-field">调度方式<select class="form-input" data-field="mode" ${scheduleEnabled ? '' : 'disabled'}><option value="global" ${view.mode !== 'fixed' ? 'selected' : ''}>跟随全局调度</option><option value="fixed" ${view.mode === 'fixed' ? 'selected' : ''}>固定节点</option></select></label>
        <label class="node-site-field">固定节点<select class="form-input" data-field="fixed-node" ${scheduleEnabled && view.mode === 'fixed' ? '' : 'disabled'}><option value="">选择节点</option>${nodeOptions}</select></label>
      </div>
      <div class="node-site-status"><span>${scheduleEnabled ? `期望 ${esc(site.desired_node_name || nodeName(site.desired_node_id))} · 生效 ${esc(site.applied_node_name || nodeName(site.applied_node_id))}${site.applied_node_port ? ` :${esc(site.applied_node_port)}` : ''} · DNS ${esc(site.dns_status || 'disabled')}` : '原面板模式 · 节点调度未启用'}</span>${scheduleEnabled && site.agent_last_request_at_ms ? `<small>最近请求 ${meridianFormatDateTime(site.agent_last_request_at_ms)} · ${Number(site.agent_request_count || 0)} 次 · HTTP ${Number(site.agent_last_status || 0)}</small>` : ''}${error}</div>
      <footer class="node-site-card-actions"><button type="button" class="node-button is-primary" data-action="save-site">保存站点设置</button></footer>
    </article>`;
  }).join('');
}

function syncSiteScheduleRow(row) {
  const enabled = row.querySelector('[data-field="enabled"]')?.checked === true;
  const mode = row.querySelector('[data-field="mode"]');
  const fixedNode = row.querySelector('[data-field="fixed-node"]');
  if (mode) mode.disabled = !enabled;
  if (fixedNode) fixedNode.disabled = !enabled || mode?.value !== 'fixed';
}

async function handleSiteScheduleAction(event) {
  const row = event.target.closest('.node-site-row');
  if (!row) return;
  if (event.target.matches('[data-field="enabled"]')) {
    syncSiteScheduleRow(row);
    captureSiteScheduleDrafts();
    return;
  }
  if (event.target.matches('[data-field="mode"]')) {
    syncSiteScheduleRow(row);
    captureSiteScheduleDrafts();
    return;
  }
  if (event.target.matches('[data-field="fixed-node"]')) {
    captureSiteScheduleDrafts();
    return;
  }
  const button = event.target.closest('[data-action="save-site"]');
  if (!button) return;
  const enabled = row.querySelector('[data-field="enabled"]').checked;
  const mode = row.querySelector('[data-field="mode"]').value;
  const fixedNodeID = Number(row.querySelector('[data-field="fixed-node"]').value || 0);
  if (enabled && mode === 'fixed' && !fixedNodeID) return Toast.error('请选择固定节点');
  if (enabled && !confirm('启用后，健康检查通过时 Meridian 会为该站点创建或更新精确 DNS 记录。确认继续？')) return;
  button.disabled = true;
  try {
    await API.saveSiteNodeSchedule(Number(row.dataset.siteId), { enabled, mode, fixed_node_id: fixedNodeID });
    siteScheduleDrafts.delete(Number(row.dataset.siteId));
    Toast.success('站点调度已保存');
    await loadNodes();
  } catch (error) { Toast.error(error.message || '站点调度保存失败'); }
  finally { button.disabled = false; }
}

function renderNodes() {
  stopNodesRefresh();
  siteScheduleDrafts = new Map();
  const page = document.getElementById('page-nodes');
  page.innerHTML = `<div class="nodes-page fade-up">
    <div class="node-test-banner"><strong>切换保护</strong><span>现有站点默认不参与调度。DNS 仅在 Agent 应用配置并通过域名证书与入口健康检查后切换；已有连接不会被强制迁移。</span></div>
    <section class="node-scheduler-card"><div class="node-section-head"><div><h2>调度模式</h2><p>自动模式按可用状态、流量额度和优先级选择；手动模式固定指定节点。</p></div><button class="node-button is-primary" id="node-add">添加节点</button></div>
      <div class="node-mode"><label><input type="radio" name="node-mode" id="node-mode-auto" value="auto"> 自动</label><label><input type="radio" name="node-mode" id="node-mode-manual" value="manual"> 手动</label></div>
      <div id="node-manual-choices" class="node-choices"></div><button class="node-button is-primary" id="node-save-scheduler">保存调度</button>
    </section><div id="node-list" class="node-list"><div class="node-empty">正在加载…</div></div>
    <section class="node-site-section"><div class="node-section-head"><div><h2>站点调度</h2><p>在此分组逐个选择是否接入节点调度；关闭时保持原面板模式，可跟随全局节点或固定到指定节点。</p></div></div><div id="node-site-list" class="node-site-list"><div class="node-empty">正在加载…</div></div></section></div>`;
  document.getElementById('node-add').onclick = () => openNodeForm(null);
  document.getElementById('node-list').onclick = handleNodeAction;
  document.getElementById('node-site-list').onclick = handleSiteScheduleAction;
  document.getElementById('node-site-list').onchange = handleSiteScheduleAction;
  document.querySelectorAll('input[name="node-mode"]').forEach(input => input.onchange = () => {
    const manual = document.getElementById('node-mode-manual').checked;
    const choices = document.getElementById('node-manual-choices');
    choices.hidden = !manual;
    if (!manual) choices.querySelectorAll('input[name="manual-node"]').forEach(node => { node.checked = false; });
  });
  document.getElementById('node-save-scheduler').onclick = async () => {
    const mode = document.getElementById('node-mode-manual').checked ? 'manual' : 'auto';
    const selected = document.querySelector('input[name="manual-node"]:checked');
    if (mode === 'manual' && !selected) return Toast.error('请选择一个节点');
    try { nodesSnapshot = await API.saveNodeScheduler({ mode, manual_node_id: mode === 'manual' && selected ? Number(selected.value) : 0 }); renderScheduler(); renderNodeCards(); Toast.success('调度设置已保存'); } catch (error) { Toast.error(error.message); }
  };
  loadNodes();
  nodesRefreshTimer = setInterval(loadNodes, 5000);
}
