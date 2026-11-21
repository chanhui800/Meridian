// Diagnostics page
let diagnosticsGeneration = 0;

function renderDiag() {
  diagnosticsGeneration++;
  const page = document.getElementById('page-diagnostics');
  page.innerHTML = settingsPageFrame('diagnostics', '故障诊断', '检查站点入口、上游连接与证书状态。', `
    <section class="console-panel settings-panel diag-selection"><label class="settings-field"><span>选择站点</span><div><select class="form-select" id="diag-select" aria-label="选择诊断站点"><option value="">正在读取…</option></select></div></label><button type="button" class="console-button is-primary" id="btn-scan" disabled>开始诊断</button></section>
    <div class="diag-grid" id="diag-grid" aria-busy="false"><div class="console-panel console-empty diag-report-wide">选择一个站点，查看连接与代理诊断结果。</div></div>`);
  bindGlobalSettingsNav(page);
  loadDiagSites();
  document.getElementById('btn-scan').onclick = runDiag;
}
async function loadDiagSites() {
  const generation = diagnosticsGeneration;
  try {
    const sites = await API.listSites();
    if (generation !== diagnosticsGeneration || Router.current !== 'diagnostics') return;
    const sel = document.getElementById('diag-select');
    if (!sites || sites.length === 0) {
      sel.innerHTML = '<option value="">暂无站点</option>';
      return;
    }
    sel.innerHTML = sites.map(s => `<option value="${esc(String(s.id))}">${esc(s.name)}</option>`).join('');
    sel.onchange = () => {
      document.getElementById('diag-grid').innerHTML = '<div class="console-panel console-empty diag-report-wide">站点已切换，点击开始诊断查看当前站点的结果。</div>';
    };
    document.getElementById('btn-scan').disabled = false;
  } catch (e) {
    if (generation !== diagnosticsGeneration || Router.current !== 'diagnostics') return;
    document.getElementById('diag-grid').innerHTML = '<section class="console-panel settings-panel diag-report-wide"><p class="settings-load-error" role="alert">加载站点失败，请重新读取。</p><button type="button" class="console-button" id="diag-retry">重新读取</button></section>';
    document.getElementById('diag-retry').onclick = loadDiagSites;
  }
}

async function runDiag() {
  const select = document.getElementById('diag-select');
  const siteId = select.value;
  if (!siteId) {
    Toast.error('请选择一个站点');
    return;
  }

  const btn = document.getElementById('btn-scan');
  if (btn.disabled) return;
  const generation = diagnosticsGeneration;
  const grid = document.getElementById('diag-grid');
  btn.disabled = true;
  select.disabled = true;
  btn.textContent = '诊断中…';
  btn.classList.add('running');
  grid.setAttribute('aria-busy', 'true');
  grid.innerHTML = '<div class="console-panel settings-loading diag-report-wide" role="status">正在检查站点入口、上游连接与证书状态…</div>';

  try {
    const result = await API.diagSite(siteId);
    if (!result || generation !== diagnosticsGeneration || Router.current !== 'diagnostics') return;
    grid.innerHTML = renderDiagResult(result);
  } catch (e) {
    if (generation === diagnosticsGeneration && Router.current === 'diagnostics') {
      grid.innerHTML = `<section class="console-panel settings-panel diag-report-wide"><p class="settings-load-error" role="alert">诊断失败：${esc(e.message)}</p></section>`;
      Toast.error('诊断失败: ' + e.message);
    }
  } finally {
    if (generation === diagnosticsGeneration && Router.current === 'diagnostics') {
      btn.classList.remove('running');
      btn.textContent = '开始诊断';
      btn.disabled = false;
      select.disabled = false;
      grid.setAttribute('aria-busy', 'false');
    }
  }
}

function renderDiagResult(result) {
  const upstreams = result.upstreams || {};
  const primary = upstreams.primary || {};
  const playback = upstreams.playback || {};
  const failovers = Array.isArray(upstreams.failovers) ? upstreams.failovers : [];
  const relation = playback.using_fallback
    ? '播放回源未单独配置，播放请求当前复用主回源。'
    : playback.same_as_primary ? '播放回源与主回源相同，当前复用本组诊断结果。' : '';
  const reports = [renderUpstreamReport('primary', '主回源', 'API 与媒体资料请求的上游连接', primary, true, true, relation)];

  if (playback.show_health || playback.show_tls) {
    reports.push(renderUpstreamReport('playback', '独立播放回源', '播放、转码与直链请求的上游连接', playback, playback.show_health, playback.show_tls));
  }
  if (failovers.length) reports.push(renderFailoverCard(failovers));

  reports.push(`<section class="console-panel settings-panel diag-report-panel"><header class="diag-report-header"><div><h2>代理与请求头</h2><p class="diag-report-description">站点入口与发往上游的身份配置</p></div></header><div class="diag-support-checks">${renderProxyCard(result.proxy || {})}${renderHeadersCard(result.headers || {})}</div></section>`);
  reports.push(renderDiagNotes([
    playbackNote(playback, primary),
    primaryProbeNote(primary),
    '健康表示上游可达性与探针结果，不是完整业务可用性证明。',
    'TLS 展示的是上游站点证书，不是 Meridian 自己监听端口的证书。',
  ].filter(Boolean)));
  return reports.join('');
}

function renderUpstreamReport(key, title, description, upstream, showHealth, showTLS, relation = '') {
  const checks = [
    showHealth ? renderHealthCard('健康与延迟', '上游可达性与探针结果', upstream) : '',
    showTLS ? renderTLSCard('TLS 证书', '上游站点证书信息', upstream) : '',
  ].filter(Boolean);
  return `<section class="console-panel settings-panel diag-report-panel diag-upstream-report" data-diag-upstream="${esc(key)}"><header class="diag-report-header"><div><h2>${esc(title)}</h2><p class="diag-report-description">${esc(description)}</p></div></header>
    <dl class="diag-target"><dt class="diag-key">实际目标</dt><dd class="console-mono diag-wrap">${diagText(upstream.effective_url, '未配置')}</dd></dl>
    ${relation ? `<p class="diag-route-relation">${esc(relation)}</p>` : ''}
    <div class="diag-upstream-checks${checks.length === 1 ? ' is-single' : ''}">${checks.join('')}</div>
  </section>`;
}

function diagReportRow(label, value, className = '') {
  return `<div class="diag-row"><dt class="diag-key">${esc(label)}</dt><dd class="diag-val ${className}">${value}</dd></div>`;
}

function renderFailoverCard(failovers) {
  return `<section class="console-panel settings-panel diag-report-panel diag-backup-report"><header class="diag-report-header"><div><h2>备用线路</h2><p class="diag-report-description">按配置顺序显示；实际请求失败后，会临时优先使用可用线路。</p></div><span>${failovers.length} 条线路</span></header><ol class="diag-backup-list">${failovers.map((upstream, index) => {
    const health = upstream.health || {};
    const tls = upstream.tls || {};
    const latency = typeof health.latency_ms === 'number' && Number.isFinite(health.latency_ms) ? `${health.latency_ms} ms` : '--';
    return `<li><header class="diag-backup-header"><h3>备用 ${index + 1}</h3><div class="diag-backup-signals"><span class="diag-result-status ${statusClass(health.status)}">${statusText(health.status)}</span><span class="diag-backup-latency">${latency}</span><span class="diag-result-status ${tls.enabled ? tls.valid ? 'good' : 'bad' : ''}">${tls.enabled ? tls.valid ? '证书有效' : '证书异常' : '未使用 HTTPS'}</span></div></header><p class="console-mono diag-wrap diag-backup-target">${diagText(upstream.effective_url, '未配置')}</p>${health.error ? `<p class="settings-load-error">${esc(health.error)}</p>` : ''}${tls.error ? `<p class="settings-load-error">TLS：${esc(tls.error)}</p>` : ''}</li>`;
  }).join('')}</ol></section>`;
}

function renderDiagNotes(notes) {
  return `<details class="console-panel settings-panel settings-disclosure diag-report-notes"><summary><strong>诊断结果说明</strong><span>探测范围与回源关系</span></summary><ul class="diag-note-list">${notes.map(note => `<li>${esc(note)}</li>`).join('')}</ul></details>`;
}

function renderHealthCard(title, subtitle, upstream) {
  const health = upstream.health || {};
  const probe = health.probe || {};
  const latency = typeof health.latency_ms === 'number' && Number.isFinite(health.latency_ms) ? health.latency_ms : null;
  const latencyText = latency === null ? '--' : `${latency} ms`;
  return `<section class="diag-check diag-health-check"><header class="diag-check-header"><h3>${esc(title)}</h3><span class="diag-result-status ${statusClass(health.status)}">${statusText(health.status)}</span></header>
    <div class="diag-health-summary"><strong class="${latencyClass(latency)}">${latencyText}</strong><span>响应延迟</span></div><p class="settings-panel-help">${esc(subtitle)}</p>
    <dl class="diag-rows">${diagReportRow('Emby 版本', diagText(health.emby_version))}</dl>
    ${health.error ? `<p class="settings-load-error">${esc(health.error)}</p>` : ''}
    <details class="settings-disclosure"><summary><strong>探针详情</strong><span>类型、请求与响应</span></summary><dl class="diag-rows">${diagReportRow('探针类型', esc(probeLabel(probe)))}${diagReportRow('探针请求', diagText(probeRequestText(probe)), 'diag-wrap')}${typeof probe.http_status === 'number' && probe.http_status > 0 ? diagReportRow('探针响应', String(probe.http_status)) : ''}</dl></details>
  </section>`;
}

function renderTLSCard(title, subtitle, upstream) {
  const tls = upstream.tls || {};
  const daysLeft = typeof tls.days_left === 'number' && Number.isFinite(tls.days_left) ? tls.days_left : null;
  const expiresText = tls.expires_at ? `${tls.expires_at}${daysLeft !== null ? ` (${daysLeft} 天)` : ''}` : '未获取';
  const expiryClass = !tls.valid ? 'bad' : !tls.expires_at ? '' : daysLeft !== null && daysLeft < 30 ? 'warn' : 'good';
  return `<section class="diag-check diag-tls-check"><header class="diag-check-header"><h3>${esc(title)}</h3><span class="diag-result-status ${tls.enabled ? tls.valid ? 'good' : 'bad' : ''}">${tls.enabled ? tls.valid ? '证书有效' : '无效或已过期' : '未使用 HTTPS'}</span></header><p class="settings-panel-help">${esc(subtitle)}</p>
    <dl class="diag-rows">${tls.enabled ? diagReportRow('颁发机构', diagText(tls.issuer), 'diag-wrap') + diagReportRow('到期时间', diagText(expiresText), expiryClass) : diagReportRow('TLS', '该上游未使用 HTTPS')}${tls.error ? diagReportRow('TLS 结果', esc(tls.error), 'bad diag-wrap') : ''}</dl>
  </section>`;
}

function renderHeadersCard(headers) {
  let rows;
  if (headers.passthrough) {
    rows = diagReportRow('UA 透传', '保留客户端原值', 'good');
  } else {
    rows = diagReportRow('UA 改写', headers.ua_applied ? '已启用' : '未启用', headers.ua_applied ? 'good' : 'bad') + diagReportRow('当前 UA', diagText(headers.current_ua), 'diag-wrap') + diagReportRow('Client 字段', diagText(headers.client_field)) + diagReportRow('Version 字段', diagText(headers.version_field));
  }
  if (headers.profile_error) rows += diagReportRow('身份配置', esc(headers.profile_error), 'bad diag-wrap');
  return `<section class="diag-check diag-headers-check"><header class="diag-check-header"><h3>请求头配置</h3></header><dl class="diag-rows">${rows}</dl></section>`;
}

function renderProxyCard(proxy) {
  const mode = String(proxy.ingress_mode || (proxy.public_host ? 'host' : 'port')).toLowerCase();
  const modeLabel = { port: '仅独立端口', path: '共享路径', host: '仅共享域名', both: '共享域名 + 独立端口' }[mode] || mode;
  const portValue = proxy.port_listening ? `:${esc(proxy.listen_port || '--')}（监听中）` : mode === 'host' ? `:${esc(proxy.listen_port || '--')}（仅保留，未监听）` : `:${esc(proxy.listen_port || '--')}（未监听）`;
  return `<section class="diag-check diag-proxy-check"><header class="diag-check-header"><h3>代理状态</h3><span class="diag-result-status ${proxy.running ? 'good' : 'bad'}">${proxy.running ? '运行中' : '已停止'}</span></header><dl class="diag-rows">${diagReportRow('入口模式', esc(modeLabel))}${proxy.public_host ? diagReportRow('共享域名', esc(proxy.public_host), 'diag-wrap') : ''}${proxy.path_prefix ? diagReportRow('路径前缀', esc(proxy.path_prefix)) : ''}${diagReportRow('端口状态', portValue)}${diagReportRow('总请求数', typeof proxy.total_requests === 'number' ? String(proxy.total_requests) : '--')}</dl></section>`;
}
function primaryProbeNote(primary) {
  const probe = primary && primary.health ? primary.health.probe || {} : {};
  if (probe.kind !== 'reachability_fallback') return '';
  return '主回源元数据接口未命中，当前已退回到目标根路径可达性探针。';
}

function playbackNote(playback, primary) {
  if (playback.using_fallback) {
    return `播放回源未单独配置，当前回退到主回源 ${primary.effective_url || '--'}，因此不重复展示播放健康或播放 TLS。`;
  }
  if (playback.same_as_primary) {
    return '播放回源已配置，但与主回源相同，当前复用主回源诊断结果，不重复展示完全相同的诊断块。';
  }

  const probe = playback.health && playback.health.probe ? playback.health.probe : {};
  if (probe.kind === 'metadata_api' || probe.kind === 'reachability_fallback') {
    const probeText = probeRequestText(probe) || 'GET Metadata / API 探针';
    if (playback.show_tls) {
      return `播放回源是独立 HTTPS 上游：${playback.effective_url || '--'}。当前健康块使用与主回源一致的 ${probeText} 做可达性探针，不代表完整播放一定成功。`;
    }
    return `播放回源是独立上游：${playback.effective_url || '--'}。当前健康块使用与主回源一致的 ${probeText} 做可达性探针，不代表完整播放一定成功。`;
  }

  if (playback.show_tls) {
    return `播放回源是独立 HTTPS 上游：${playback.effective_url || '--'}，因此会单独展示播放健康和播放 TLS。`;
  }
  return `播放回源是独立上游：${playback.effective_url || '--'}。该上游未使用 HTTPS，因此只展示播放健康。`;
}

function probeLabel(probe) {
  if (!probe || !probe.kind) return '--';
  if (probe.kind === 'reachability_fallback') return '可达性回退探针';
  if (probe.kind === 'metadata_api') return 'Metadata / API 探针';
  return probe.kind;
}

function probeRequestText(probe) {
  if (!probe) return '';
  const method = probe.method || '';
  const url = probe.url || '';
  if (!method && !url) return '';
  return `${method} ${url}`.trim();
}

function statusClass(value) {
  if (value === 'online' || value === true) return 'good';
  if (value === 'error') return 'warn';
  return 'bad';
}

function statusText(value) {
  if (value === 'online') return '在线';
  if (value === 'error') return '探针异常';
  return '离线';
}

function latencyClass(value) {
  if (typeof value !== 'number') return '';
  if (value < 100) return 'good';
  if (value < 300) return 'warn';
  return 'bad';
}

function diagText(value, fallback = '--') {
  if (value === undefined || value === null || value === '') {
    return esc(String(fallback));
  }
  return esc(String(value));
}
