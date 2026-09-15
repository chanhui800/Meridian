let nodesRefreshTimer = null;
let nodesSnapshot = { nodes: [], scheduler: { mode: 'auto', manual_node_id: 0, active_node_id: 0 } };
let siteSchedulesSnapshot = { sites: [] };
// Keep edits in the current page alive while the five-second status refresh
// replaces the server snapshot. Drafts are cleared after a successful save or
// when the page is recreated.
let siteScheduleDrafts = new Map();
const siteScheduleReactionDelay = 280;
const siteScheduleReactionTimers = new Map();
const siteScheduleConfigPendingWarningMS = 90 * 1000;
const siteScheduleConfigPendingMessage = 'Agent has not applied the site configuration';

function stopNodesRefresh() {
  if (nodesRefreshTimer) clearInterval(nodesRefreshTimer);
  nodesRefreshTimer = null;
  siteScheduleReactionTimers.forEach(timer => clearTimeout(timer));
  siteScheduleReactionTimers.clear();
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

// nodeAddressFamily mirrors the controller: only a literal IP has a family, and
// everything else is treated as "not set" rather than guessed.
function nodeAddressFamily(value) {
  const text = String(value || '').trim();
  if (!text) return '';
  if (/^\d{1,3}(\.\d{1,3}){3}$/.test(text)) {
    return text.split('.').every(part => Number(part) <= 255) ? 'v4' : '';
  }
  // An IPv6 literal. Brackets, a zone or a port all make it unusable as a DNS
  // record, so they are rejected here exactly like the controller rejects them
  // rather than being silently trimmed into a different value.
  if (text.includes(':')) {
    if (text.includes('[') || text.includes(']') || text.includes('%') || text.includes('/')) return '';
    return text.split(':').every(part => /^[0-9a-fA-F]{0,4}$/.test(part)) ? 'v6' : '';
  }
  return '';
}

// nodeFamilyAddress is the address the controller will publish for a family: the
// per-family slot, or the primary address when it belongs to that family.
function nodeFamilyAddress(node, family) {
  const slot = family === 'v4' ? node.address_v4 : node.address_v6;
  if (nodeAddressFamily(slot) === family) return String(slot).trim();
  if (nodeAddressFamily(node.address) === family) return String(node.address).trim();
  return '';
}

function nodeDNSPublishModeLabel(node) {
  switch (String(node.dns_publish || 'auto')) {
    case 'v4': return '仅 IPv4';
    case 'v6': return '仅 IPv6';
    default: return '自动';
  }
}

// nodeDNSPublishSummary states which records this node actually publishes, so a
// dual-stack host pinned to one family is visibly different from a single-stack
// host that cannot publish the other one at all.
function nodeDNSPublishSummary(node) {
  const hasV4 = nodeFamilyAddress(node, 'v4') !== '';
  const hasV6 = nodeFamilyAddress(node, 'v6') !== '';
  const mode = String(node.dns_publish || 'auto');
  const families = [];
  if (hasV4 && mode !== 'v6') families.push('A（IPv4）');
  if (hasV6 && mode !== 'v4') families.push('AAAA（IPv6）');
  if (!families.length) {
    return hasV4 || hasV6 ? '不发布 DNS 记录' : '待填写地址';
  }
  return `发布 ${families.join(' + ')}`;
}

function siteScheduleFeedback(site, now = Date.now()) {
  const enabled = site?.enabled === true;
  const lastError = String(site?.last_error || '').trim();
  const pendingSince = Number(site?.config_pending_since_ms || 0);
  if (!enabled) return { kind: 'disabled', className: '', text: '未启用节点调度，继续使用原面板入口' };

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
  if (!nodesSnapshot.nodes.length) {
    container.innerHTML = '<div class="node-empty">还没有节点。创建节点后会生成一次性安装脚本。</div>';
    return;
  }
  container.innerHTML = nodesSnapshot.nodes.map(node => {
    const usage = node.traffic_quota > 0 ? `${nodeBytes(node.traffic_used)} / ${nodeBytes(node.traffic_quota)}` : `${nodeBytes(node.traffic_used)} / 不限`;
    // reset_day 0 means the node's counters never reset, so the same number is a
    // lifetime total rather than a cycle one. Say so instead of letting a
    // depleted node look like a monthly quota that should recover on its own.
    const reset = node.reset_day === 0 ? '不自动重置（上限为累计值）' : `每月 ${node.reset_day} 日重置`;
    const entry = `HTTPS :${node.port}`;
    const applyError = String(node.agent_apply_error || '').trim();
    const listenerError = String(node.agent_listener_error || '').trim();
    const configState = applyError ? `应用失败：${applyError}` : (listenerError ? `监听异常：${listenerError}` : (node.desired_config_hash && node.desired_config_hash === node.applied_config_hash ? '配置已应用' : '等待 Agent 应用站点配置'));
    // An address the controller inferred (from enrollment) or verified from the
    // Agent's own report is marked so it gets reviewed: this value is published
    // as the site's DNS record, and a wrong guess points the site at the wrong
    // host.
    const addressText = node.address
      ? (node.address_source === 'enrollment' || node.address_source === 'detected'
        ? `${node.address}（自动探测，请核对）`
        : node.address)
      : '未填写地址';
    // Which records this node publishes. The family list is what the scheduler
    // actually resolved to, so a family with no address shows as unpublished
    // instead of silently looking configured.
    const published = nodeDNSPublishSummary(node);
    return `<article class="node-card ${node.active ? 'is-active' : ''}">
      <div class="node-card-head"><div><h3>${esc(node.name)}</h3><p>${esc(addressText)} · ${esc(node.interface_name || '等待识别网卡')}</p></div>
      <span class="node-status is-${esc(node.status)}">${esc(nodeStatusLabel(node))}</span></div>
      ${node.depleted ? '<div class="node-card-warning is-error">已超出流量上限，不再参与调度。提高上限、清除上限或改回每月重置日即可恢复。</div>' : ''}
      <div class="node-stats"><span><b>${usage}</b><small>${esc(node.billing_mode === 'bidirectional' ? '上下行计费' : '上行计费')}</small></span><span><b>${esc(reset)}</b><small>独立流量周期</small></span><span><b>${node.priority}</b><small>优先级${node.active ? ' · 当前选中' : ''}</small></span></div>
      <div class="node-entry-state"><div class="node-entry-details"><span>${esc(published)}</span><small class="node-agent-version">DNS 解析 · ${esc(nodeDNSPublishModeLabel(node))}</small></div><small class="node-agent-version">${esc(entry)} · Agent ${esc(node.agent_version || '未上报')}</small></div>
      <div class="node-entry-state"><div class="node-entry-details"><small class="${applyError || listenerError ? 'is-error' : ''}">${esc(configState)}</small></div></div>
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
    <div class="node-form-grid">
      <label>IPv4 地址<input class="form-input" id="node-address-v4" maxlength="255" value="${esc(node ? node.address_v4 || (nodeAddressFamily(node.address) === 'v4' ? node.address : '') : '')}" placeholder="例如 203.0.113.10"></label>
      <label>IPv6 地址<input class="form-input" id="node-address-v6" maxlength="255" value="${esc(node ? node.address_v6 || (nodeAddressFamily(node.address) === 'v6' ? node.address : '') : '')}" placeholder="例如 2001:db8::1"></label>
    </div>
    <div class="form-help">填入哪个族就发布哪条记录：只填 IPv4 发 A，只填 IPv6 发 AAAA，两个都填则同时发 A 和 AAAA（客户端按自身网络选择）。IPv6 只接受字面量，不要带方括号或端口。留空时节点首次注册会用主控观测到的来源 IP 兜底，Agent 上报的地址也会在主控探测通过后自动填入，并标记「自动探测」待你核对——该值会成为公开 DNS 记录。</div>
    <label>DNS 解析<select class="form-input" id="node-dns-publish"><option value="auto">自动（按已填地址发布）</option><option value="v4">仅 IPv4（只发 A）</option><option value="v6">仅 IPv6（只发 AAAA）</option></select></label>
    <div class="form-help" id="node-dns-publish-hint">双栈节点默认同时发布 A 和 AAAA；只发布其中一个时，另一个族的客户端会连不上，请确认你的落地机确实两组地址都能访问。</div>
    <label>端口<input class="form-input" id="node-port" type="number" min="1" max="65535" value="${node && node.port ? node.port : (location.port || 443)}"></label>
    <div class="form-help">Agent 仅提供 TLS/HTTPS。同一节点上的所有调度站点共用此端口并按域名区分；端口必须未被该 VPS 上的其他程序占用。默认采用当前主控端口，保存后独立管理。</div>
    <div class="node-form-grid"><label>流量上限（GiB，0 为不限）<input class="form-input" id="node-quota" type="number" min="0" step="0.01" value="${node ? (Number(node.traffic_quota) / 1073741824).toFixed(2) : '0'}"></label><label>重置日<input class="form-input" id="node-reset" type="number" min="0" max="31" value="${node ? node.reset_day : 1}"></label></div>
    <div class="form-help">重置日填 0 表示不自动重置，此时「流量上限」是累计上限：节点用量只增不减，一旦达到上限就不再参与调度，也不会自动恢复（可改回每月重置日，或清除流量上限）。</div>
    ${editing ? `<label>流量校正（GiB，可正负）<input class="form-input" id="node-offset" type="number" step="0.01" value="${(Number(node.traffic_manual_offset_bytes || 0) / 1073741824).toFixed(2)}"></label><div class="form-help">校正值叠加在网卡周期统计上，后续 Agent 上报不会覆盖。要把显示值调到目标值，可填写正数或负数。</div>` : ''}
    <div class="node-form-grid"><label>计费方式<select class="form-input" id="node-billing"><option value="outbound">上行</option><option value="bidirectional">上下行</option></select></label><label>优先级<input class="form-input" id="node-priority" type="number" min="0" max="1000" value="${node ? node.priority : 100}"></label></div>
    ${editing ? '<label class="node-check"><input id="node-enabled" type="checkbox" checked> 启用节点</label>' : '<label>控制器地址<input class="form-input" id="node-controller" type="url" required></label>'}
  </form>`;
  document.getElementById('node-billing').value = node ? node.billing_mode : 'outbound';
  document.getElementById('node-dns-publish').value = node ? String(node.dns_publish || 'auto') : 'auto';
  const dnsHint = document.getElementById('node-dns-publish-hint');
  const refreshDNSHint = () => {
    const addressV4 = document.getElementById('node-address-v4').value.trim();
    const addressV6 = document.getElementById('node-address-v6').value.trim();
    const mode = document.getElementById('node-dns-publish').value;
    dnsHint.textContent = nodeDNSPublishDraftSummary(addressV4, addressV6, mode);
  };
  ['node-address-v4', 'node-address-v6'].forEach(id => { document.getElementById(id).oninput = refreshDNSHint; });
  document.getElementById('node-dns-publish').onchange = refreshDNSHint;
  refreshDNSHint();
  if (editing) document.getElementById('node-enabled').checked = node.enabled;
  else document.getElementById('node-controller').value = location.origin;
  document.getElementById('modal-footer').innerHTML = '<button type="button" class="node-button" id="node-form-cancel">取消</button><button type="submit" form="node-form" class="node-button is-primary">保存</button>';
  openModal({ closeOnBackdrop: false });
  document.getElementById('node-form-cancel').onclick = closeModal;
  document.getElementById('node-form').onsubmit = async event => {
    event.preventDefault();
    const addressV4 = document.getElementById('node-address-v4').value.trim();
    const addressV6 = document.getElementById('node-address-v6').value.trim();
    const dnsPublish = document.getElementById('node-dns-publish').value;
    // A pinned family with no address for it would publish nothing at all, which
    // is a silent outage for that family. Refuse it here rather than after the
    // scheduler has already removed the record.
    const pinError = nodeDNSPublishDraftError(addressV4, addressV6, dnsPublish);
    if (pinError) { Toast.error(pinError); return; }
    // The primary/legacy address keeps naming one family so a cached panel build
    // and the Agent dial logic still see a usable address.
    const payload = {
      name: document.getElementById('node-name').value.trim(),
      address: addressV4 || addressV6,
      address_v4: addressV4,
      address_v6: addressV6,
      dns_publish: dnsPublish,
      port: Number(document.getElementById('node-port').value),
      traffic_quota: Math.round(Number(document.getElementById('node-quota').value || 0) * 1073741824),
      reset_day: Number(document.getElementById('node-reset').value),
      billing_mode: document.getElementById('node-billing').value,
      priority: Number(document.getElementById('node-priority').value),
      traffic_manual_offset_bytes: Math.round(Number(document.getElementById('node-offset')?.value || 0) * 1073741824),
    };
    try {
      if (editing) { payload.enabled = document.getElementById('node-enabled').checked; await API.updateNode(node.id, payload); closeModal(); Toast.success('节点已更新'); }
      else { payload.controller_url = document.getElementById('node-controller').value.trim(); const result = await API.createNode(payload); showNodeScript(result.install_script, result.install_command); }
      await loadNodes();
    } catch (error) { Toast.error(error.message || '保存失败'); }
  };
}

// nodeDNSPublishDraftSummary explains, in the form, exactly which records the
// current combination would publish. The operator is about to change what a
// public hostname resolves to, so the consequence is stated rather than implied.
function nodeDNSPublishDraftSummary(addressV4, addressV6, dnsPublish) {
  const hasV4 = nodeAddressFamily(addressV4) === 'v4';
  const hasV6 = nodeAddressFamily(addressV6) === 'v6';
  const families = [];
  if (hasV4 && dnsPublish !== 'v6') families.push('A（IPv4）');
  if (hasV6 && dnsPublish !== 'v4') families.push('AAAA（IPv6）');
  if (!families.length) {
    if (dnsPublish === 'v4') return '当前不会发布任何记录：已选择仅 IPv4，但未填写 IPv4 地址。';
    if (dnsPublish === 'v6') return '当前不会发布任何记录：已选择仅 IPv6，但未填写 IPv6 地址。';
    return '当前不会发布任何记录：至少填写一个地址。';
  }
  let text = `将发布 ${families.join(' + ')}。`;
  if (dnsPublish === 'auto' && hasV4 && hasV6) {
    text += '双栈：客户端会按自身网络选择，IPv6 优先。';
  } else if (dnsPublish !== 'auto' && hasV4 && hasV6) {
    text += `已手动限定为仅 ${dnsPublish === 'v4' ? 'IPv4' : 'IPv6'}，另一族的客户端将无法连接。`;
  } else if (hasV4 && !hasV6) {
    text += '仅 IPv4：纯 IPv6 客户端无法连接。';
  } else if (hasV6 && !hasV4) {
    text += '仅 IPv6：纯 IPv4 客户端无法连接。';
  }
  return text;
}

// nodeDNSPublishDraftError validates the form's address/publish combination
// before it reaches the controller, so the operator gets the reason immediately.
function nodeDNSPublishDraftError(addressV4, addressV6, dnsPublish) {
  if (addressV4 && nodeAddressFamily(addressV4) !== 'v4') return 'IPv4 地址格式不正确';
  if (addressV6 && nodeAddressFamily(addressV6) !== 'v6') return 'IPv6 地址请填字面量，不要带方括号或端口';
  if (dnsPublish === 'v4' && !addressV4) return '已选择仅 IPv4，但未填写 IPv4 地址';
  if (dnsPublish === 'v6' && !addressV6) return '已选择仅 IPv6，但未填写 IPv6 地址';
  return '';
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
    if (!confirm('这会生成新的注册脚本；当前 Agent 会继续运行，直到新 Agent 注册成功。确认继续？')) return;
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
    const formEnabled = view.enabled === true;
    const savedEnabled = site.enabled === true;
    const dirty = !!draft;
    const feedback = siteScheduleFeedback(site);
    const stateLabel = dirty
      ? `${savedEnabled ? '调度已启用' : '使用面板入口'} · 待保存`
      : (feedback.kind === 'pending' || feedback.kind === 'warning' || feedback.kind === 'error'
        ? feedback.text
        : (savedEnabled ? '调度已启用' : '使用面板入口'));
    const nodeOptions = nodesSnapshot.nodes.map(node => `<option value="${node.id}" ${Number(view.fixed_node_id) === Number(node.id) ? 'selected' : ''}>${esc(node.name)}</option>`).join('');
    const error = `<small class="${feedback.className}">${esc(feedback.text)}</small>`;
    const dirtyNote = dirty ? '<small class="node-site-dirty">当前修改尚未保存，保存后才会生效</small>' : '';
    return `<article class="node-site-row node-site-card" data-site-id="${site.site_id}">
      <header class="node-site-card-head"><div class="node-site-identity"><strong>${esc(site.site_name)}</strong><span>${esc(site.public_host || '未配置站点域名')}</span></div><span class="node-site-state ${savedEnabled ? 'is-enabled' : ''} ${dirty ? 'is-pending' : ''}" data-role="site-state" aria-live="polite">${stateLabel}</span></header>
      <div class="node-site-card-controls">
        <label class="node-check"><input type="checkbox" data-field="enabled" ${formEnabled ? 'checked' : ''}> 启用节点调度</label>
        <label class="node-site-field">调度方式<select class="form-input" data-field="mode" ${formEnabled ? '' : 'disabled'}><option value="global" ${view.mode !== 'fixed' ? 'selected' : ''}>跟随全局调度</option><option value="fixed" ${view.mode === 'fixed' ? 'selected' : ''}>固定节点</option></select></label>
        <label class="node-site-field">固定节点<select class="form-input" data-field="fixed-node" ${formEnabled && view.mode === 'fixed' ? '' : 'disabled'}><option value="">选择节点</option>${nodeOptions}</select></label>
      </div>
      <div class="node-site-status"><span>${savedEnabled ? `期望 ${esc(site.desired_node_name || nodeName(site.desired_node_id))} · 生效 ${esc(site.applied_node_name || nodeName(site.applied_node_id))}${site.applied_node_port ? ` :${esc(site.applied_node_port)}` : ''} · DNS ${esc(site.dns_status || 'disabled')}` : '原面板模式 · 节点调度未启用'}</span>${savedEnabled && site.agent_last_request_at_ms ? `<small>最近请求 ${meridianFormatDateTime(site.agent_last_request_at_ms)} · ${Number(site.agent_request_count || 0)} 次 · HTTP ${Number(site.agent_last_status || 0)}</small>` : ''}${error}${dirtyNote}</div>
      <footer class="node-site-card-actions"><button type="button" class="node-button is-primary" data-action="save-site">保存站点设置</button></footer>
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
    state.className = `node-site-state ${savedEnabled ? 'is-enabled' : ''} ${dirty ? 'is-pending' : ''}`;
    const feedback = siteScheduleFeedback(server);
    state.textContent = dirty
      ? `${savedEnabled ? '调度已启用' : '使用面板入口'} · 待保存`
      : (feedback.kind === 'pending' || feedback.kind === 'warning' || feedback.kind === 'error'
        ? feedback.text
        : (savedEnabled ? '调度已启用' : '使用面板入口'));
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
  document.getElementById('node-site-list').oninput = handleSiteScheduleAction;
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
