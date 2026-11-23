let nodesRefreshTimer = null;
let nodesSnapshot = { nodes: [], scheduler: { mode: 'auto', manual_node_id: 0, active_node_id: 0 } };
let siteSchedulesSnapshot = { sites: [] };
let nodesActiveTab = 'fleet';
let nodesPageGeneration = 0;
let nodesLoadPromise = null;
let nodesMutationVersion = 0;
let nodeSchedulerDraft = null;
let nodeSchedulerSaving = false;
let nodeDetailOpen = new Set();
let siteScheduleOpen = new Set();
let siteScheduleSaving = new Set();
// Keep edits in the current page alive while the five-second status refresh
// replaces the server snapshot. Drafts are cleared after a successful save or
// when the page is recreated.
let siteScheduleDrafts = new Map();
const siteScheduleReactionDelay = 280;
const siteScheduleReactionTimers = new Map();
const siteScheduleConfigPendingWarningMS = 90 * 1000;
const siteScheduleConfigPendingMessage = 'Agent has not applied the site configuration';

function stopNodesRefresh() {
  nodesPageGeneration++;
  nodesLoadPromise = null;
  if (nodesRefreshTimer) clearInterval(nodesRefreshTimer);
  nodesRefreshTimer = null;
  siteScheduleReactionTimers.forEach(timer => clearTimeout(timer));
  siteScheduleReactionTimers.clear();
  if (typeof invalidateModalIntent === 'function' && invalidateModalIntent('nodes')) {
    const overlay = document.getElementById('modal-overlay');
    const modal = document.getElementById('modal');
    if (overlay?.classList.contains('active') && modal?.classList.contains('fleet-modal')) closeModal({ restoreFocus: false });
  }
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

function nodeAvailability(node) {
  if (node.enabled === false) return { label: '已停用', kind: 'muted' };
  if (node.depleted) return { label: '额度耗尽', kind: 'warning' };
  if (node.status === 'offline') return { label: '离线', kind: 'error' };
  if (node.status === 'pending') return { label: '待安装', kind: 'muted' };
  if (node.agent_apply_error || node.agent_listener_error) return { label: '需处理', kind: 'warning' };
  return { label: nodeStatusLabel(node) || '未上报', kind: node.status === 'online' ? 'online' : 'muted' };
}

function nodeAddressLabel(node) {
  return node.address_v4 || node.address_v6 || node.address || '等待地址上报';
}

function renderNodesOverview() {
  const nodes = nodesSnapshot.nodes || [];
  const active = nodes.find(node => Number(node.id) === Number(nodesSnapshot.scheduler?.active_node_id));
  const setText = (id, value) => {
    const element = document.getElementById(id);
    if (element) element.textContent = value;
  };
  setText('nodes-current-exit', active?.name || '主控入口');
  setText('nodes-current-exit-note', active ? `${nodeAddressLabel(active)} · HTTPS :${active.port}` : '暂无生效节点');
  setText('nodes-online-summary', `${nodes.filter(node => node.status === 'online').length} / ${nodes.length}`);
  setText('nodes-routes-summary', `${(siteSchedulesSnapshot.sites || []).filter(site => site.enabled === true).length}`);
  setText('nodes-fleet-count', nodes.length);
  setText('nodes-route-count', (siteSchedulesSnapshot.sites || []).length);
}

function captureNodesFocus() {
  const element = document.activeElement;
  if (!element || typeof element.closest !== 'function') return null;
  const route = element.closest('.node-site-row');
  const node = element.closest('.fleet-row');
  if (!route && !node) {
    if (element.name === 'node-mode' || element.name === 'manual-node') return { inputName: element.name, inputValue: element.value };
    return null;
  }
  return {
    siteID: route?.dataset.siteId,
    nodeID: node?.dataset.nodeId,
    field: element.dataset?.field,
    action: element.dataset?.action,
  };
}

function restoreNodesFocus(identity) {
  if (!identity || typeof document.querySelector !== 'function') return;
  if (identity.inputName) {
    const input = document.querySelector(`input[name="${identity.inputName}"][value="${identity.inputValue === 'manual' || identity.inputValue === 'auto' ? identity.inputValue : Number(identity.inputValue)}"]`);
    if (input && typeof input.focus === 'function') input.focus({ preventScroll: true });
    return;
  }
  const owner = identity.siteID
    ? document.querySelector(`.node-site-row[data-site-id="${Number(identity.siteID)}"]`)
    : document.querySelector(`.fleet-row[data-node-id="${Number(identity.nodeID)}"]`);
  const element = identity.field
    ? owner?.querySelector(`[data-field="${identity.field}"]`)
    : owner?.querySelector(`[data-action="${identity.action}"]`);
  if (element && !element.disabled && typeof element.focus === 'function') element.focus({ preventScroll: true });
}

function siteScheduleFeedback(site, now = Date.now()) {
  const enabled = site?.enabled === true;
  const lastError = String(site?.last_error || '').trim();
  const pendingSince = Number(site?.config_pending_since_ms || 0);
  if (!enabled) return { kind: 'disabled', className: '', text: '未启用节点调度，继续使用主控入口' };

  // The scheduler uses this readiness error while an Agent is still fetching
  // the first snapshot. It is expected progress, not a failed operation.
  const waitingForConfig = lastError === siteScheduleConfigPendingMessage || pendingSince > 0 && !lastError;
  if (waitingForConfig) {
    const overdue = pendingSince > 0 && Math.max(0, now - pendingSince) >= siteScheduleConfigPendingWarningMS;
    return overdue
      ? { kind: 'warning', className: 'is-warning', text: 'Agent 长时间未应用配置' }
      : { kind: 'pending', className: 'is-pending', text: '等待 Agent 应用站点配置' };
  }
  if (lastError) {
    const applyPrefix = 'Agent configuration apply failed:';
    if (lastError.startsWith(applyPrefix)) {
      return { kind: 'error', className: 'is-error', text: `Agent 应用配置失败：${lastError.slice(applyPrefix.length).trim()}` };
    }
    return { kind: 'error', className: 'is-error', text: `调度异常：${lastError}` };
  }
  return { kind: 'ready', className: '', text: 'DNS 只会在 Agent 配置与入口健康检查通过后生效' };
}

function renderNodeCards() {
  const container = document.getElementById('node-list');
  if (!container) return;
  const nodes = nodesSnapshot.nodes || [];
  if (!nodes.length) {
    container.innerHTML = '<div class="console-empty fleet-empty"><h3>还没有边缘节点</h3><p>添加第一台节点，为站点准备独立的访问出口。</p><button class="console-button" type="button" data-action="create">添加节点</button></div>';
    return;
  }
  container.innerHTML = '<div class="fleet-list-heading" aria-hidden="true"><span>节点</span><span>状态</span><span>流量用量</span><span></span></div>' + nodes.map(node => {
    const id = Number(node.id);
    const state = nodeAvailability(node);
    const quota = Number(node.traffic_quota || 0);
    const ratio = quota > 0 ? Math.min(100, Math.max(0, Number(node.traffic_used || 0) / quota * 100)) : 0;
    const reset = node.reset_day === 0 ? '不自动重置（上限为累计值）' : `每月 ${node.reset_day} 日重置`;
    const applyError = String(node.agent_apply_error || '').trim();
    const listenerError = String(node.agent_listener_error || '').trim();
    const configState = applyError ? `应用失败：${applyError}` : (listenerError ? `监听异常：${listenerError}` : (node.desired_config_hash && node.desired_config_hash === node.applied_config_hash ? '配置已应用' : '等待 Agent 应用站点配置'));
    const open = nodeDetailOpen.has(id);
    return `<article class="fleet-row ${node.active ? 'is-active' : ''}" data-node-id="${id}">
      <div class="fleet-row-summary">
        <div class="fleet-identity"><span class="fleet-symbol" aria-hidden="true"><svg viewBox="0 0 24 24"><rect x="4" y="3" width="16" height="7" rx="2"/><rect x="4" y="14" width="16" height="7" rx="2"/><path d="M8 6.5h.01M8 17.5h.01M12 6.5h4M12 17.5h4"/></svg></span><div><h3>${esc(node.name)}${node.active ? '<span class="fleet-active-label">当前出口</span>' : ''}</h3><p class="console-mono">${esc(nodeAddressLabel(node))}</p></div></div>
        <span class="console-status is-${state.kind}">${esc(state.label)}</span>
        <div class="fleet-usage"><div><strong>${nodeBytes(node.traffic_used)}</strong><span>${quota > 0 ? ` / ${nodeBytes(quota)}` : ' / 不限'}</span></div>${quota > 0 ? `<div class="fleet-usage-track ${node.depleted ? 'is-depleted' : ''}" role="meter" aria-label="${esc(node.name)}流量额度" aria-valuemin="0" aria-valuemax="100" aria-valuenow="${Math.round(ratio)}"><i style="width:${ratio.toFixed(2)}%"></i></div>` : '<small>未设流量上限</small>'}</div>
        <button type="button" class="console-button fleet-detail-button" data-action="details" data-id="${id}" aria-expanded="${open}" aria-controls="node-detail-${id}">详情<span aria-hidden="true">${open ? '−' : '+'}</span></button>
      </div>
      <div class="fleet-row-details" id="node-detail-${id}" ${open ? '' : 'hidden'}>
        ${node.depleted ? '<p class="fleet-warning">已超出流量上限，不再参与调度。提高上限、清除上限或改回每月重置日即可恢复。</p>' : ''}
        <dl class="fleet-detail-grid"><div><dt>IPv4 / IPv6</dt><dd class="console-mono">${esc(node.address_v4 || '—')}<br>${esc(node.address_v6 || '—')}</dd></div><div><dt>入口与版本</dt><dd>HTTPS :${Number(node.port)}<small class="node-agent-version">Agent ${esc(node.agent_version || '未上报')}</small></dd></div><div><dt>流量周期</dt><dd>${esc(reset)}<small>${node.billing_mode === 'bidirectional' ? '上下行计费' : '上行计费'} · 优先级 ${Number(node.priority || 0)}</small></dd></div><div><dt>最近心跳</dt><dd>${node.last_seen_at_ms ? esc(meridianFormatDateTime(node.last_seen_at_ms)) : '等待 Agent 上报'}<small>${esc(node.interface_name || '等待识别网卡')}</small></dd></div></dl>
        ${node.address_source === 'enrollment' || node.address_v6_source === 'enrollment' ? '<p class="fleet-warning">地址来自注册请求，请核对后再用于 DNS 发布。</p>' : ''}
        <p class="fleet-config-state ${applyError || listenerError ? 'is-error' : ''}">${esc(configState)}</p>
        <div class="fleet-row-actions"><button type="button" class="console-button" data-action="edit" data-id="${id}">编辑节点</button><button type="button" class="console-button" data-action="enroll" data-id="${id}">重新生成脚本</button><button type="button" class="console-button is-danger" data-action="delete" data-id="${id}">删除节点</button></div>
      </div>
    </article>`;
  }).join('');
}

function renderScheduler() {
  const scheduler = nodesSnapshot.scheduler || {};
  const view = nodeSchedulerDraft || scheduler;
  const auto = document.getElementById('node-mode-auto');
  const manual = document.getElementById('node-mode-manual');
  if (!auto || !manual) return;
  auto.checked = view.mode !== 'manual';
  manual.checked = view.mode === 'manual';
  auto.disabled = nodeSchedulerSaving;
  manual.disabled = nodeSchedulerSaving;
  const choices = document.getElementById('node-manual-choices');
  choices.innerHTML = (nodesSnapshot.nodes || []).length ? nodesSnapshot.nodes.map(node => `<label class="fleet-node-choice"><input type="radio" name="manual-node" value="${Number(node.id)}" ${Number(view.manual_node_id) === Number(node.id) ? 'checked' : ''} ${nodeSchedulerSaving ? 'disabled' : ''}><span>${esc(node.name)}</span><small>${esc(nodeAvailability(node).label)}</small></label>`).join('') : '<span class="console-muted">请先添加节点</span>';
  choices.hidden = !manual.checked;
  const save = document.getElementById('node-save-scheduler');
  if (save) {
    save.disabled = nodeSchedulerSaving || !nodeSchedulerDraft;
    save.textContent = nodeSchedulerSaving ? '保存中…' : '保存策略';
  }
  const note = document.getElementById('node-scheduler-state');
  if (note) note.textContent = nodeSchedulerDraft ? '策略有未保存的修改' : (scheduler.mode === 'manual' ? '固定使用选定节点' : '按健康状态、额度与优先级自动选择');
}

function captureNodeSchedulerDraft() {
  const manual = document.getElementById('node-mode-manual');
  if (!manual) return;
  const selected = typeof document.querySelector === 'function' ? document.querySelector('input[name="manual-node"]:checked') : null;
  const draft = { mode: manual.checked ? 'manual' : 'auto', manual_node_id: manual.checked ? Number(selected?.value || 0) : 0 };
  const saved = nodesSnapshot.scheduler || {};
  nodeSchedulerDraft = draft.mode === (saved.mode || 'auto') && draft.manual_node_id === Number(saved.manual_node_id || 0) ? null : draft;
}

async function loadNodes(force = false) {
  if (nodesLoadPromise) {
    await nodesLoadPromise;
    if (force && Router.current === 'nodes') return loadNodes();
    return;
  }
  captureSiteScheduleDrafts();
  const generation = nodesPageGeneration;
  const mutation = nodesMutationVersion;
  const load = (async () => {
   try {
    const [snapshot, siteSchedules] = await Promise.all([API.getNodes(), API.getSiteNodeSchedules()]);
    if (!snapshot || !siteSchedules || Router.current !== 'nodes' || generation !== nodesPageGeneration || mutation !== nodesMutationVersion) return;
    const focus = captureNodesFocus();
    nodesSnapshot = snapshot;
    siteSchedulesSnapshot = siteSchedules;
    renderScheduler();
    renderNodeCards();
    renderSiteSchedules();
    renderNodesOverview();
    restoreNodesFocus(focus);
    const state = document.getElementById('nodes-load-state');
    if (state) { state.textContent = '每 5 秒更新'; state.classList.remove('is-error'); }
  } catch (error) {
    if (Router.current === 'nodes' && generation === nodesPageGeneration) {
      const state = document.getElementById('nodes-load-state');
      if (state) { state.textContent = error.message || '更新失败，请重试'; state.classList.add('is-error'); }
    }
   }
  })();
  nodesLoadPromise = load;
  try { return await load; }
  finally {
    if (nodesLoadPromise === load) nodesLoadPromise = null;
  }
}

function focusFleetModal(preferredID) {
  const modal = document.getElementById('modal');
  if (!modal) return;
  const focusable = () => Array.from(modal.querySelectorAll('button, a[href], input, select, textarea, [tabindex]'))
    .filter(element => !element.disabled && element.tabIndex >= 0 && element.getClientRects().length > 0 && getComputedStyle(element).visibility !== 'hidden');
  // Assign rather than accumulating listeners as the form becomes an install
  // dialog. Other page dialogs retain their own keyboard behavior.
  modal.onkeydown = event => {
    if (event.key !== 'Tab' || !modal.classList.contains('fleet-modal')) return;
    const elements = focusable();
    const first = elements[0];
    const last = elements[elements.length - 1];
    if (!first) {
      event.preventDefault();
      modal.tabIndex = -1;
      modal.focus();
    } else if (!modal.contains(document.activeElement) || event.shiftKey && document.activeElement === first || !event.shiftKey && document.activeElement === last) {
      event.preventDefault();
      (event.shiftKey ? last : first).focus();
    }
  };
  const preferred = document.getElementById(preferredID);
  const target = preferred && modal.contains(preferred) ? preferred : focusable()[0];
  if (target) target.focus({ preventScroll: true });
}

function showNodeScript(script, command, intent) {
  const modalIntent = intent || beginModalIntent('nodes');
  if (!isModalIntentCurrent(modalIntent)) return;
  document.getElementById('modal-title').textContent = 'Agent 一键安装脚本';
  document.getElementById('modal-body').innerHTML = `${command ? '<p class="node-script-note">推荐直接执行以下命令（不会进入分页器）：</p><textarea class="node-script" id="node-install-command" readonly></textarea>' : ''}<p class="node-script-note">脚本含一次性令牌，24 小时内有效。请在目标 Linux amd64 或 arm64 VPS 上以 root 执行。</p><textarea class="node-script" id="node-install-script" readonly></textarea>`;
  document.getElementById('node-install-script').value = script;
  if (command) document.getElementById('node-install-command').value = command;
  document.getElementById('modal-footer').innerHTML = `${command ? '<button type="button" class="console-button" id="node-copy-command">复制一键命令</button>' : ''}<button type="button" class="console-button" id="node-copy-script">复制脚本</button><button type="button" class="console-button is-primary" id="node-close-script">完成</button>`;
  openModal({ closeOnBackdrop: false, modalClass: 'fleet-modal', intent: modalIntent });
  focusFleetModal(command ? 'node-install-command' : 'node-install-script');
  const copy = async (value, message) => {
    try { await navigator.clipboard.writeText(value); Toast.success(message); }
    catch (_) { Toast.error('复制失败，请从文本框手动复制'); }
  };
  if (command) document.getElementById('node-copy-command').onclick = () => copy(command, '一键命令已复制');
  document.getElementById('node-copy-script').onclick = () => copy(script, '脚本已复制');
  document.getElementById('node-close-script').onclick = closeModal;
}

function openNodeForm(node) {
  const modalIntent = beginModalIntent('nodes');
  const editing = !!node;
  document.getElementById('modal-title').textContent = editing ? '编辑节点' : '创建节点';
  document.getElementById('modal-body').innerHTML = `<form id="node-form" class="fleet-form">
    <fieldset><legend>节点信息</legend><div class="fleet-form-grid"><label>节点名称<input class="form-input" id="node-name" maxlength="64" required value="${esc(node ? node.name : '')}"></label><label>端口<input class="form-input" id="node-port" type="number" min="1" max="65535" value="${node && node.port ? node.port : (location.port || 443)}"></label></div>
    <div class="fleet-form-grid"><label>IPv4 地址<input class="form-input" id="node-address-v4" value="${esc(node ? (node.address_v4 || '') : '')}" placeholder="203.0.113.10"></label><label>IPv6 地址<input class="form-input" id="node-address-v6" value="${esc(node ? (node.address_v6 || '') : '')}" placeholder="2001:db8::10"></label></div>
    <label>DNS 发布<select class="form-input" id="node-dns-publish"><option value="auto">自动（双栈同时发布）</option><option value="v4">仅 IPv4</option><option value="v6">仅 IPv6</option></select></label>
    <div class="form-help" id="node-dns-preview" role="status" aria-live="polite"></div>
    <div class="form-help">Agent 仅提供 TLS/HTTPS。所有调度站点共用此端口并按域名区分，请确保端口可用。</div>
    ${editing ? '<label class="fleet-form-check"><input id="node-enabled" type="checkbox" checked> 启用节点</label>' : '<label>控制器地址<input class="form-input" id="node-controller" type="url" required></label>'}
    </fieldset>
    <fieldset><legend>流量额度</legend><div class="fleet-form-grid"><label>流量上限（GiB，0 为不限）<input class="form-input" id="node-quota" type="number" min="0" step="0.01" value="${node ? (Number(node.traffic_quota) / 1073741824).toFixed(2) : '0'}"></label><label>计费方式<select class="form-input" id="node-billing"><option value="outbound">上行</option><option value="bidirectional">上下行</option></select></label></div></fieldset>
    <details><summary>重置周期与高级设置</summary><div>
      <div class="fleet-form-grid"><label>重置日<input class="form-input" id="node-reset" type="number" min="0" max="31" value="${node ? node.reset_day : 1}"></label><label>优先级<input class="form-input" id="node-priority" type="number" min="0" max="1000" value="${node ? node.priority : 100}"></label></div>
      <div class="form-help">重置日 0 表示不自动重置，上限为累计值。额度耗尽后不会自动恢复；可调整上限或改回每月重置。</div>
      ${editing ? `<label>流量校正（GiB，可正负）<input class="form-input" id="node-offset" type="number" step="0.01" value="${(Number(node.traffic_manual_offset_bytes || 0) / 1073741824).toFixed(2)}"></label><div class="form-help">校正值叠加在周期用量上，后续 Agent 上报不会覆盖。</div>` : ''}
      <label>兼容地址<input class="form-input" id="node-address" maxlength="255" value="${esc(node ? node.address : '')}" placeholder="可留空"></label>
      <div class="form-help">Agent 地址上报仅填充空白地址，人工填写的地址不会被覆盖。</div>
    </div></details>
  </form>`;
  document.getElementById('node-billing').value = node ? node.billing_mode : 'outbound';
  document.getElementById('node-dns-publish').value = node ? (node.dns_publish || 'auto') : 'auto';
  const refreshDNSPreview = () => {
    const v4 = document.getElementById('node-address-v4').value.trim();
    const v6 = document.getElementById('node-address-v6').value.trim();
    const mode = document.getElementById('node-dns-publish').value;
    const records = [];
    if (v4 && mode !== 'v6') records.push(`A → ${v4}`);
    if (v6 && mode !== 'v4') records.push(`AAAA → ${v6}`);
    const missing = mode === 'v4' && !v4 || mode === 'v6' && !v6;
    const preview = document.getElementById('node-dns-preview');
    preview.textContent = missing ? '所选地址族尚未填写，无法保存。' : (records.length ? `将发布：${records.join('；')}` : '地址上报后将按可用地址族发布。');
    preview.classList.toggle('is-error', missing);
    return !missing;
  };
  ['node-address-v4', 'node-address-v6', 'node-dns-publish'].forEach(id => document.getElementById(id).addEventListener('input', refreshDNSPreview));
  refreshDNSPreview();
  if (editing) document.getElementById('node-enabled').checked = node.enabled;
  else document.getElementById('node-controller').value = location.origin;
  document.getElementById('modal-footer').innerHTML = '<button type="button" class="console-button" id="node-form-cancel">取消</button><button type="submit" form="node-form" class="console-button is-primary" id="node-form-save">保存节点</button>';
  openModal({ closeOnBackdrop: false, modalClass: 'fleet-modal', intent: modalIntent });
  focusFleetModal('node-name');
  document.getElementById('node-form-cancel').onclick = closeModal;
  let saving = false;
  document.getElementById('node-form').onsubmit = async event => {
    event.preventDefault();
    if (saving) return;
    if (!refreshDNSPreview()) return Toast.error('请填写所选 DNS 地址族');
    const payload = { name: document.getElementById('node-name').value.trim(), address: document.getElementById('node-address').value.trim(), address_v4: document.getElementById('node-address-v4').value.trim(), address_v6: document.getElementById('node-address-v6').value.trim(), dns_publish: document.getElementById('node-dns-publish').value, port: Number(document.getElementById('node-port').value), traffic_quota: Math.round(Number(document.getElementById('node-quota').value || 0) * 1073741824), reset_day: Number(document.getElementById('node-reset').value), billing_mode: document.getElementById('node-billing').value, priority: Number(document.getElementById('node-priority').value), traffic_manual_offset_bytes: Math.round(Number(document.getElementById('node-offset')?.value || 0) * 1073741824) };
    const generation = nodesPageGeneration;
    const button = document.getElementById('node-form-save');
    saving = true;
    nodesMutationVersion++;
    button.disabled = true;
    button.textContent = '保存中…';
    try {
      if (editing) {
        payload.enabled = document.getElementById('node-enabled').checked;
        const result = await API.updateNode(node.id, payload);
        if (!result) return;
        nodesMutationVersion++;
        if (generation !== nodesPageGeneration || Router.current !== 'nodes') return;
        if (isModalIntentCurrent(modalIntent)) {
          closeModal({ intent: modalIntent });
          Toast.success('节点已更新');
        }
      } else {
        payload.controller_url = document.getElementById('node-controller').value.trim();
        const result = await API.createNode(payload);
        if (!result) return;
        nodesMutationVersion++;
        if (generation !== nodesPageGeneration || Router.current !== 'nodes') return;
        showNodeScript(result.install_script, result.install_command, modalIntent);
      }
      await loadNodes(true);
    } catch (error) { if (isModalIntentCurrent(modalIntent)) Toast.error(error.message || '保存失败'); }
    finally { saving = false; if (button.isConnected) { button.disabled = false; button.textContent = '保存节点'; } }
  };
}

async function handleNodeAction(event) {
  const button = event.target.closest('button[data-action]');
  if (!button) return;
  if (button.dataset.action === 'create') return openNodeForm(null);
  const node = nodesSnapshot.nodes.find(item => Number(item.id) === Number(button.dataset.id));
  if (!node) return;
  if (button.dataset.action === 'details') {
    const id = Number(node.id);
    if (nodeDetailOpen.has(id)) nodeDetailOpen.delete(id);
    else nodeDetailOpen.add(id);
    const details = document.getElementById(`node-detail-${id}`);
    if (details) details.hidden = !nodeDetailOpen.has(id);
    button.setAttribute('aria-expanded', String(nodeDetailOpen.has(id)));
    button.innerHTML = `详情<span aria-hidden="true">${nodeDetailOpen.has(id) ? '−' : '+'}</span>`;
    return;
  }
  if (button.dataset.action === 'edit') return openNodeForm(node);
  if (button.dataset.action === 'delete') {
    if (!confirm(`确认删除节点“${node.name}”？已安装的 Agent 也将失去授权。`)) return;
    const generation = nodesPageGeneration;
    nodesMutationVersion++;
    button.disabled = true;
    try {
      const result = await API.deleteNode(node.id);
      if (!result) return;
      nodesMutationVersion++;
      if (generation !== nodesPageGeneration || Router.current !== 'nodes') return;
      nodeDetailOpen.delete(Number(node.id));
      Toast.success('节点已删除'); await loadNodes(true);
    } catch (error) { Toast.error(error.message); }
    finally { button.disabled = false; }
    return;
  }
  if (button.dataset.action === 'enroll') {
    if (!confirm('这会生成新的注册脚本；当前 Agent 会继续运行，直到新 Agent 注册成功。确认继续？')) return;
    const generation = nodesPageGeneration;
    const modalIntent = beginModalIntent('nodes');
    nodesMutationVersion++;
    button.disabled = true;
    try {
      const result = await API.refreshNodeEnrollment(node.id, location.origin);
      if (!result) return;
      nodesMutationVersion++;
      if (generation !== nodesPageGeneration || Router.current !== 'nodes') return;
      showNodeScript(result.install_script, result.install_command, modalIntent); await loadNodes(true);
    } catch (error) { if (isModalIntentCurrent(modalIntent)) Toast.error(error.message); }
    finally { button.disabled = false; }
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
  if (!sites.length) { container.innerHTML = '<div class="console-empty fleet-empty"><h3>还没有站点路由</h3><p>先添加站点，再为需要的站点选择访问出口。</p><a class="console-button" href="#sites">管理站点</a></div>'; return; }
  container.innerHTML = '<div class="route-list-heading" aria-hidden="true"><span>站点</span><span>访问出口</span><span>调度策略</span><span>状态</span><span></span></div>' + sites.map(site => {
    const id = Number(site.site_id);
    const draft = siteScheduleDrafts.get(id);
    const view = draft ? { ...site, ...draft } : site;
    const formEnabled = view.enabled === true;
    const savedEnabled = site.enabled === true;
    const dirty = !!draft;
    const feedback = siteScheduleFeedback(site);
    const stateLabel = dirty
      ? `${savedEnabled ? '调度已启用' : '使用主控入口'} · 待保存`
      : (feedback.kind === 'pending' || feedback.kind === 'warning' || feedback.kind === 'error'
        ? feedback.text
        : (savedEnabled ? '调度已启用' : '使用主控入口'));
    const nodeOptions = (nodesSnapshot.nodes || []).map(node => `<option value="${Number(node.id)}" ${Number(view.fixed_node_id) === Number(node.id) ? 'selected' : ''}>${esc(node.name)}</option>`).join('');
    const error = `<small class="${feedback.className}">${esc(feedback.text)}</small>`;
    const dirtyNote = dirty ? '<small class="node-site-dirty">当前修改尚未保存，保存后才会生效</small>' : '';
    const open = siteScheduleOpen.has(id);
    const saving = siteScheduleSaving.has(id);
    const compactStatus = dirty ? '待保存' : ({ pending: '配置应用中', warning: '等待时间较长', error: '调度异常', ready: '已启用', disabled: '主控模式' }[feedback.kind] || '—');
    return `<article class="node-site-row route-row" data-site-id="${id}">
      <div class="route-summary"><div class="route-identity"><strong>${esc(site.site_name)}</strong><span class="console-mono">${esc(site.public_host || '未配置域名')}</span></div><div class="route-destination"><strong>${savedEnabled ? esc(site.applied_node_name || nodeName(site.applied_node_id)) : '主控入口'}</strong>${savedEnabled && Number(site.desired_node_id) !== Number(site.applied_node_id) ? `<small>正在切换至 ${esc(site.desired_node_name || nodeName(site.desired_node_id))}</small>` : ''}</div><span class="route-policy">${savedEnabled ? (site.mode === 'fixed' ? '固定节点' : '跟随全局') : '—'}</span><span class="console-status route-state is-${dirty || feedback.kind === 'pending' || feedback.kind === 'warning' ? 'warning' : feedback.kind === 'error' ? 'error' : savedEnabled ? 'online' : 'muted'}" data-role="site-state" aria-label="${esc(stateLabel)}">${compactStatus}</span><button type="button" class="console-button route-edit-button" data-action="edit-site" aria-expanded="${open}" aria-controls="site-editor-${id}">${open ? '收起' : '调整'}</button></div>
      <div class="route-editor" id="site-editor-${id}" ${open ? '' : 'hidden'}>
        <div class="route-editor-heading"><strong>调整 ${esc(site.site_name)} 的访问出口</strong><span class="console-muted">保存后生效</span></div>
        <div class="route-editor-fields">
          <label class="route-enable"><input type="checkbox" data-field="enabled" ${formEnabled ? 'checked' : ''} ${saving ? 'disabled' : ''}> 启用节点调度</label>
          <label class="route-field">调度方式<select class="form-input" data-field="mode" ${formEnabled && !saving ? '' : 'disabled'}><option value="global" ${view.mode !== 'fixed' ? 'selected' : ''}>跟随全局调度</option><option value="fixed" ${view.mode === 'fixed' ? 'selected' : ''}>固定节点</option></select></label>
          <label class="route-field">固定节点<select class="form-input" data-field="fixed-node" ${formEnabled && view.mode === 'fixed' && !saving ? '' : 'disabled'}><option value="">选择节点</option>${nodeOptions}</select></label>
        </div>
        <div class="node-site-status"><span>${savedEnabled ? `期望 ${esc(site.desired_node_name || nodeName(site.desired_node_id))} · 生效 ${esc(site.applied_node_name || nodeName(site.applied_node_id))}${site.applied_node_port ? ` :${esc(site.applied_node_port)}` : ''} · DNS ${esc(site.dns_status || 'disabled')}` : '主控模式 · 节点调度未启用'}</span>${savedEnabled && site.agent_last_request_at_ms ? `<small>最近请求 ${esc(meridianFormatDateTime(site.agent_last_request_at_ms))} · ${Number(site.agent_request_count || 0)} 次 · HTTP ${Number(site.agent_last_status || 0)}</small>` : ''}${error}${dirtyNote}</div>
        <div class="route-editor-actions"><button type="button" class="console-button" data-action="cancel-site" ${saving ? 'disabled' : ''}>取消修改</button><button type="button" class="console-button is-primary" data-action="save-site" ${saving ? 'disabled' : ''}>${saving ? '保存中…' : '保存站点设置'}</button></div>
      </div>
    </article>`;
  }).join('');
}

function refreshSiteScheduleRowState(row) {
  const siteID = Number(row?.dataset?.siteId);
  if (!Number.isFinite(siteID) || siteID <= 0) return;
  const server = (siteSchedulesSnapshot.sites || []).find(site => Number(site.site_id) === siteID);
  if (!server) return;
  const draft = siteScheduleDrafts.get(siteID);
  const savedEnabled = server.enabled === true;
  const dirty = !!draft;
  const state = row.querySelector('[data-role="site-state"]');
  if (state) {
    const feedback = siteScheduleFeedback(server);
    state.className = `console-status route-state is-${dirty || feedback.kind === 'pending' || feedback.kind === 'warning' ? 'warning' : feedback.kind === 'error' ? 'error' : savedEnabled ? 'online' : 'muted'}`;
    state.textContent = dirty ? '待保存' : ({ pending: '配置应用中', warning: '等待时间较长', error: '调度异常', ready: '已启用', disabled: '主控模式' }[feedback.kind] || '—');
    state.setAttribute('aria-label', dirty
      ? `${savedEnabled ? '调度已启用' : '使用主控入口'} · 待保存`
      : (feedback.kind === 'pending' || feedback.kind === 'warning' || feedback.kind === 'error'
        ? feedback.text
        : (savedEnabled ? '调度已启用' : '使用主控入口')));
  }
  const status = row.querySelector('.node-site-status');
  if (!status) return;
  const dirtyNote = status.querySelector('.node-site-dirty');
  if (dirty && !dirtyNote) {
    status.insertAdjacentHTML('beforeend', '<small class="node-site-dirty">当前修改尚未保存，保存后才会生效</small>');
  } else if (!dirty && dirtyNote) {
    dirtyNote.remove();
  }
}

function syncSiteScheduleRow(row) {
  const enabled = row.querySelector('[data-field="enabled"]')?.checked === true;
  const mode = row.querySelector('[data-field="mode"]');
  const fixedNode = row.querySelector('[data-field="fixed-node"]');
  if (mode) mode.disabled = !enabled;
  if (fixedNode) fixedNode.disabled = !enabled || mode?.value !== 'fixed';
  captureSiteScheduleDrafts();
  refreshSiteScheduleRowState(row);
}

// Keep checkbox/select state changes just behind the pointer event. The small
// debounce prevents a fast five-second refresh or double tap from making the
// controls jump while the unsaved draft is being captured.
function queueSiteScheduleRowSync(row) {
  const siteID = Number(row?.dataset?.siteId);
  if (!Number.isFinite(siteID) || siteID <= 0) return;
  const previous = siteScheduleReactionTimers.get(siteID);
  if (previous) clearTimeout(previous);
  // Capture the edit before deferring dependent-control reactions: an in-flight
  // status response can arrive inside this debounce window.
  captureSiteScheduleDrafts();
  row.classList.add('is-schedule-pending');
  const timer = setTimeout(() => {
    siteScheduleReactionTimers.delete(siteID);
    row.classList.remove('is-schedule-pending');
    syncSiteScheduleRow(row);
  }, siteScheduleReactionDelay);
  siteScheduleReactionTimers.set(siteID, timer);
}

async function handleSiteScheduleAction(event) {
  const row = event.target.closest('.node-site-row');
  if (!row) return;
  if (event.target.matches('[data-field="enabled"]')) {
    queueSiteScheduleRowSync(row);
    return;
  }
  if (event.target.matches('[data-field="mode"]')) {
    queueSiteScheduleRowSync(row);
    return;
  }
  if (event.target.matches('[data-field="fixed-node"]')) {
    captureSiteScheduleDrafts();
    refreshSiteScheduleRowState(row);
    return;
  }
  const action = event.target.closest('button[data-action]');
  const id = Number(row.dataset.siteId);
  if (siteScheduleSaving.has(id)) return;
  if (action?.dataset.action === 'edit-site') {
    captureSiteScheduleDrafts();
    if (siteScheduleOpen.has(id)) siteScheduleOpen.delete(id); else siteScheduleOpen.add(id);
    const editor = document.getElementById(`site-editor-${id}`);
    if (editor) editor.hidden = !siteScheduleOpen.has(id);
    action.setAttribute('aria-expanded', String(siteScheduleOpen.has(id)));
    action.textContent = siteScheduleOpen.has(id) ? '收起' : '调整';
    return;
  }
  if (action?.dataset.action === 'cancel-site') {
    const reaction = siteScheduleReactionTimers.get(id);
    if (reaction) clearTimeout(reaction);
    siteScheduleReactionTimers.delete(id);
    siteScheduleDrafts.delete(id);
    siteScheduleOpen.delete(id);
    renderSiteSchedules();
    return;
  }
  const button = event.target.closest('[data-action="save-site"]');
  if (!button) return;
  if (siteScheduleSaving.has(id)) return;
  const enabled = row.querySelector('[data-field="enabled"]').checked;
  const mode = row.querySelector('[data-field="mode"]').value;
  const fixedNodeID = Number(row.querySelector('[data-field="fixed-node"]').value || 0);
  if (enabled && mode === 'fixed' && !fixedNodeID) return Toast.error('请选择固定节点');
  if (enabled && !confirm('启用后，健康检查通过时 Meridian 会为该站点创建或更新精确 DNS 记录。确认继续？')) return;
  captureSiteScheduleDrafts();
  button.disabled = true;
  const reaction = siteScheduleReactionTimers.get(id);
  if (reaction) clearTimeout(reaction);
  siteScheduleReactionTimers.delete(id);
  siteScheduleSaving.add(id);
  const savingSites = siteScheduleSaving;
  nodesMutationVersion++;
  row.querySelectorAll('[data-field]').forEach(field => { field.disabled = true; });
  const generation = nodesPageGeneration;
  try {
    await API.saveSiteNodeSchedule(id, { enabled, mode, fixed_node_id: fixedNodeID });
    nodesMutationVersion++;
    if (generation !== nodesPageGeneration || Router.current !== 'nodes') return;
    siteScheduleDrafts.delete(id);
    siteScheduleOpen.delete(id);
    Toast.success('站点调度已保存');
    // Do not re-capture the now-saved controls against an old server snapshot.
    const saved = (siteSchedulesSnapshot.sites || []).find(site => Number(site.site_id) === id);
    if (saved) Object.assign(saved, { enabled, mode, fixed_node_id: fixedNodeID });
    await loadNodes(true);
  } catch (error) {
    if (generation === nodesPageGeneration && Router.current === 'nodes') Toast.error(error.message || '站点调度保存失败');
  }
  finally {
    savingSites.delete(id);
    button.disabled = false;
    if (generation === nodesPageGeneration && Router.current === 'nodes') {
      const focus = captureNodesFocus();
      renderSiteSchedules();
      restoreNodesFocus(focus);
    }
  }
}

function selectNodesTab(tab, moveFocus = false) {
  nodesActiveTab = tab === 'routes' ? 'routes' : 'fleet';
  ['fleet', 'routes'].forEach(key => {
    const button = document.getElementById(`nodes-tab-${key}`);
    const panel = document.getElementById(`nodes-panel-${key}`);
    const selected = key === nodesActiveTab;
    if (button) { button.setAttribute('aria-selected', String(selected)); button.tabIndex = selected ? 0 : -1; }
    if (panel) panel.hidden = !selected;
  });
  if (moveFocus) document.getElementById(`nodes-tab-${nodesActiveTab}`)?.focus();
}

function renderNodes() {
  stopNodesRefresh();
  siteScheduleDrafts = new Map();
  siteScheduleOpen = new Set();
  siteScheduleSaving = new Set();
  nodeDetailOpen = new Set();
  nodeSchedulerDraft = null;
  nodeSchedulerSaving = false;
  nodesActiveTab = 'fleet';
  const page = document.getElementById('page-nodes');
  page.innerHTML = `<div class="console-page fleet-console">
    <header class="console-heading"><div><p class="console-eyebrow">流量出口</p><h1>节点调度</h1><p class="console-lead">管理边缘节点，为每个站点选择合适的访问出口。</p></div><button type="button" class="console-button is-primary" id="node-add"><span aria-hidden="true">＋</span>添加节点</button></header>
    <section class="fleet-overview" aria-label="调度概览"><div><span>全局出口</span><strong id="nodes-current-exit">—</strong><small class="console-mono" id="nodes-current-exit-note">正在读取…</small></div><div><span>在线节点</span><strong id="nodes-online-summary">—</strong><small>实时 Agent 心跳</small></div><div><span>启用调度的站点</span><strong id="nodes-routes-summary">—</strong><small>其余站点使用主控入口</small></div></section>
    <section class="fleet-policy-shell console-panel" aria-labelledby="fleet-policy-title"><div class="fleet-policy-line"><div><h2 id="fleet-policy-title">全局策略</h2><p id="node-scheduler-state">按健康状态、额度与优先级自动选择</p></div><fieldset class="fleet-policy-options"><legend class="sr-only">选择全局调度策略</legend><label><input type="radio" name="node-mode" id="node-mode-auto" value="auto" checked><span>自动择优</span></label><label><input type="radio" name="node-mode" id="node-mode-manual" value="manual"><span>固定节点</span></label></fieldset><button type="button" class="console-button" id="node-save-scheduler" disabled>保存策略</button></div><div id="node-manual-choices" class="fleet-node-choices" hidden></div></section>
    <section class="console-panel fleet-management"><div class="fleet-management-heading"><div class="fleet-tabs" role="tablist" aria-label="节点与站点管理"><button type="button" id="nodes-tab-fleet" role="tab" aria-controls="nodes-panel-fleet" aria-selected="true" tabindex="0">边缘节点<span id="nodes-fleet-count">0</span></button><button type="button" id="nodes-tab-routes" role="tab" aria-controls="nodes-panel-routes" aria-selected="false" tabindex="-1">站点路由<span id="nodes-route-count">0</span></button></div><span class="fleet-update-state" id="nodes-load-state" role="status">正在读取…</span><button type="button" class="console-button fleet-refresh" id="nodes-refresh" aria-label="刷新节点和站点路由"><svg viewBox="0 0 24 24" aria-hidden="true"><path d="M20 7v5h-5M4 17v-5h5M6.2 6.2A8 8 0 0 1 19.7 12M4.3 12A8 8 0 0 0 17.8 17.8"/></svg></button></div>
      <div id="nodes-panel-fleet" role="tabpanel" aria-labelledby="nodes-tab-fleet"><div id="node-list" class="fleet-list"><div class="console-empty">正在读取节点…</div></div></div>
      <div id="nodes-panel-routes" role="tabpanel" aria-labelledby="nodes-tab-routes" hidden><p class="route-intro">逐个设置站点出口；未启用的站点继续使用主控。</p><div id="node-site-list" class="route-list"><div class="console-empty">正在读取站点…</div></div></div>
    </section>
    <details class="fleet-switch-note"><summary>关于出口切换</summary><p>DNS 仅在 Agent 应用配置并通过域名证书与入口健康检查后切换；已有连接不会被强制迁移。现有站点默认不参与调度。</p></details>
  </div>`;
  document.getElementById('node-add').onclick = () => openNodeForm(null);
  document.getElementById('node-list').onclick = handleNodeAction;
  document.getElementById('node-site-list').onclick = handleSiteScheduleAction;
  document.getElementById('node-site-list').onchange = handleSiteScheduleAction;
  document.getElementById('node-site-list').oninput = handleSiteScheduleAction;
  document.getElementById('nodes-refresh').onclick = () => loadNodes();
  ['fleet', 'routes'].forEach(tab => {
    const button = document.getElementById(`nodes-tab-${tab}`);
    button.onclick = () => selectNodesTab(tab);
    button.onkeydown = event => {
      if (!['ArrowLeft', 'ArrowRight', 'Home', 'End'].includes(event.key)) return;
      event.preventDefault();
      selectNodesTab(event.key === 'Home' ? 'fleet' : event.key === 'End' ? 'routes' : nodesActiveTab === 'fleet' ? 'routes' : 'fleet', true);
    };
  });
  document.querySelectorAll('input[name="node-mode"]').forEach(input => input.onchange = () => {
    captureNodeSchedulerDraft();
    const focus = captureNodesFocus();
    renderScheduler();
    restoreNodesFocus(focus);
  });
  document.getElementById('node-manual-choices').onchange = () => {
    captureNodeSchedulerDraft();
    const focus = captureNodesFocus();
    renderScheduler();
    restoreNodesFocus(focus);
  };
  document.getElementById('node-save-scheduler').onclick = async () => {
    if (nodeSchedulerSaving) return;
    const mode = document.getElementById('node-mode-manual').checked ? 'manual' : 'auto';
    const selected = document.querySelector('input[name="manual-node"]:checked');
    if (mode === 'manual' && !selected) return Toast.error('请选择一个节点');
    const generation = nodesPageGeneration;
    nodeSchedulerSaving = true;
    nodesMutationVersion++;
    renderScheduler();
    try {
      const snapshot = await API.saveNodeScheduler({ mode, manual_node_id: mode === 'manual' && selected ? Number(selected.value) : 0 });
      nodesMutationVersion++;
      if (Router.current !== 'nodes' || generation !== nodesPageGeneration) return;
      nodesSnapshot = snapshot;
      nodeSchedulerDraft = null;
      renderNodeCards();
      renderNodesOverview();
      Toast.success('调度设置已保存');
    } catch (error) {
      if (generation === nodesPageGeneration && Router.current === 'nodes') Toast.error(error.message || '调度设置保存失败');
    }
    finally {
      if (generation === nodesPageGeneration && Router.current === 'nodes') {
        nodeSchedulerSaving = false;
        renderScheduler();
      }
    }
  };
  loadNodes();
  nodesRefreshTimer = setInterval(loadNodes, 5000);
}
