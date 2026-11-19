// Diagnostics page
let diagnosticsGeneration = 0;

function renderDiag() {
  diagnosticsGeneration++;
  const page = document.getElementById('page-diagnostics');
  page.innerHTML = settingsPageFrame('diagnostics', '故障诊断', '检查站点入口、上游连接与证书状态。', `
    <section class="console-panel settings-panel diag-selection"><label class="settings-field"><span>选择站点</span><div><select class="form-select" id="diag-select" aria-label="选择诊断站点"><option value="">正在读取…</option></select></div></label><button type="button" class="console-button is-primary" id="btn-scan" disabled>开始诊断</button></section>
    <div class="diag-grid" id="diag-grid"><div class="console-panel console-empty diag-report-wide">选择一个站点，查看连接与代理诊断结果。</div></div>`);
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
    sel.innerHTML = sites.map(s => `<option value="${s.id}">${esc(s.name)}</option>`).join('');
    document.getElementById('btn-scan').disabled = false;
  } catch (e) {
    if (generation !== diagnosticsGeneration || Router.current !== 'diagnostics') return;
    document.getElementById('diag-grid').innerHTML = '<section class="console-panel settings-panel diag-report-wide"><p class="settings-load-error" role="alert">加载站点失败，请重新读取。</p><button type="button" class="console-button" id="diag-retry">重新读取</button></section>';
    document.getElementById('diag-retry').onclick = loadDiagSites;
  }
}

async function runDiag() {
  const siteId = document.getElementById('diag-select').value;
  if (!siteId) {
    Toast.error('请选择一个站点');
    return;
  }

  const btn = document.getElementById('btn-scan');
  if (btn.disabled) return;
  const generation = diagnosticsGeneration;
  btn.disabled = true;
  btn.textContent = '诊断中...';
  btn.classList.add('running');

  try {
    const result = await API.diagSite(siteId);
    if (!result || generation !== diagnosticsGeneration || Router.current !== 'diagnostics') return;
    const upstreams = result.upstreams || {};
    const primary = upstreams.primary || {};
    const playback = upstreams.playback || {};
		const failovers = Array.isArray(upstreams.failovers) ? upstreams.failovers : [];
    const headers = result.headers || {};
    const proxy = result.proxy || {};

    const notes = [
      playbackNote(playback, primary),
      primaryProbeNote(primary),
      '健康表示上游可达性与探针结果，不是完整业务可用性证明。',
      'TLS 展示的是上游站点证书，不是 Meridian 自己监听端口的证书。',
    ].filter(Boolean);

    const cards = [
      renderDiagNotes(notes),
      renderHealthCard('主回源健康', '主回源可达性与探针结果', primary, 'stagger-2'),
      renderTLSCard('主回源 TLS', '主回源上游站点证书信息', primary, 'stagger-3'),
    ];

    if (playback.show_health) {
      cards.push(renderHealthCard('播放回源健康', '播放、转码、直链上游的可达性与探针结果', playback, 'stagger-4'));
    }
    if (playback.show_tls) {
      cards.push(renderTLSCard('播放回源 TLS', '播放回源上游站点证书信息', playback, 'stagger-5'));
    }
		if (failovers.length) {
			cards.push(renderFailoverCard(failovers, 'stagger-5'));
		}

    cards.push(renderHeadersCard(headers, 'stagger-5'));
    cards.push(renderProxyCard(proxy, 'stagger-6'));

    document.getElementById('diag-grid').innerHTML = cards.filter(Boolean).join('');
  } catch (e) {
    if (generation === diagnosticsGeneration && Router.current === 'diagnostics') {
      document.getElementById('diag-grid').innerHTML = `<section class="console-panel settings-panel diag-report-wide"><p class="settings-load-error" role="alert">诊断失败：${esc(e.message)}</p></section>`;
      Toast.error('诊断失败: ' + e.message);
    }
  } finally {
    btn.classList.remove('running');
    btn.textContent = '开始诊断';
    btn.disabled = false;
  }
}

function diagReportRow(label, value, className = '') {
  return `<div class="diag-row"><dt class="diag-key">${esc(label)}</dt><dd class="diag-val ${className}">${value}</dd></div>`;
}

function renderFailoverCard(failovers) {
  return `<section class="console-panel settings-panel diag-report-wide"><header><h2>备用线路</h2><span>按配置顺序探测</span></header><dl class="diag-rows">${failovers.map((upstream, index) => {
    const health = upstream.health || {};
    const latency = typeof health.latency_ms === 'number' ? `${health.latency_ms}ms` : '--';
    return diagReportRow(`备用 ${index + 1}`, `<span class="diag-wrap">${diagText(upstream.effective_url)}</span><span class="diag-inline-status ${statusClass(health.status)}">${statusText(health.status)} · ${latency}</span>`);
  }).join('')}</dl><p class="settings-panel-help">实际请求失败后，会临时优先使用可用线路。</p></section>`;
}

function renderDiagNotes(notes) {
  return `<details class="console-panel settings-panel settings-disclosure diag-report-wide"><summary><strong>诊断结果说明</strong><span>探测范围与回源关系</span></summary><ul class="diag-note-list">${notes.map(note => `<li>${esc(note)}</li>`).join('')}</ul></details>`;
}

function renderHealthCard(title, subtitle, upstream) {
  const health = upstream.health || {};
  const probe = health.probe || {};
  const latency = typeof health.latency_ms === 'number' ? health.latency_ms : null;
  const latencyText = latency === null ? '--' : `${latency}ms`;
  return `<section class="console-panel settings-panel diag-report-panel"><header><h2>${esc(title)}</h2><span class="diag-result-status ${statusClass(health.status)}">${statusText(health.status)}</span></header>
    <div class="diag-health-summary"><strong class="${latencyClass(latency)}">${latencyText}</strong><span>响应延迟</span></div><p class="settings-panel-help">${esc(subtitle)}</p>
    <dl class="diag-rows">${diagReportRow('实际目标', diagText(upstream.effective_url, '未配置'), 'diag-wrap')}${diagReportRow('Emby 版本', diagText(health.emby_version))}</dl>
    ${health.error ? `<p class="settings-load-error">${esc(health.error)}</p>` : ''}
    <details class="settings-disclosure"><summary><strong>探针详情</strong><span>类型、请求与响应</span></summary><dl class="diag-rows">${diagReportRow('探针类型', esc(probeLabel(probe)))}${diagReportRow('探针请求', diagText(probeRequestText(probe)), 'diag-wrap')}${typeof probe.http_status === 'number' && probe.http_status > 0 ? diagReportRow('探针响应', String(probe.http_status)) : ''}</dl></details>
  </section>`;
}

function renderTLSCard(title, subtitle, upstream) {
  const tls = upstream.tls || {};
  const daysLeft = typeof tls.days_left === 'number' ? tls.days_left : null;
  const expiresText = tls.expires_at ? `${tls.expires_at}${daysLeft !== null ? ` (${daysLeft} 天)` : ''}` : '未获取';
  return `<section class="console-panel settings-panel diag-report-panel"><header><h2>${esc(title)}</h2><span class="diag-result-status ${tls.enabled ? tls.valid ? 'good' : 'bad' : ''}">${tls.enabled ? tls.valid ? '证书有效' : '无效或已过期' : '未使用 HTTPS'}</span></header><p class="settings-panel-help">${esc(subtitle)}</p>
    <dl class="diag-rows">${diagReportRow('实际目标', diagText(upstream.effective_url, '未配置'), 'diag-wrap')}${tls.enabled ? diagReportRow('颁发机构', diagText(tls.issuer), 'diag-wrap') + diagReportRow('到期时间', diagText(expiresText), daysLeft !== null && daysLeft < 30 ? 'warn' : 'good') : diagReportRow('TLS', '该上游未使用 HTTPS')}${tls.error ? diagReportRow('TLS 结果', esc(tls.error), 'bad diag-wrap') : ''}</dl>
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
  return `<details class="console-panel settings-panel settings-disclosure diag-report-panel"><summary><strong>请求头配置</strong><span>发往上游的身份字段</span></summary><dl class="diag-rows">${rows}</dl></details>`;
}

function renderProxyCard(proxy) {
  const mode = String(proxy.ingress_mode || (proxy.public_host ? 'host' : 'port')).toLowerCase();
  const modeLabel = { port: '仅独立端口', path: '共享路径', host: '仅共享域名', both: '共享域名 + 独立端口' }[mode] || mode;
  const portValue = proxy.port_listening ? `:${esc(proxy.listen_port || '--')}（监听中）` : mode === 'host' ? `:${esc(proxy.listen_port || '--')}（仅保留，未监听）` : `:${esc(proxy.listen_port || '--')}（未监听）`;
  return `<section class="console-panel settings-panel diag-report-panel"><header><h2>代理状态</h2><span class="diag-result-status ${proxy.running ? 'good' : 'bad'}">${proxy.running ? '运行中' : '已停止'}</span></header><dl class="diag-rows">${diagReportRow('入口模式', esc(modeLabel))}${proxy.public_host ? diagReportRow('共享域名', esc(proxy.public_host), 'diag-wrap') : ''}${proxy.path_prefix ? diagReportRow('路径前缀', esc(proxy.path_prefix)) : ''}${diagReportRow('端口状态', portValue)}${diagReportRow('总请求数', typeof proxy.total_requests === 'number' ? String(proxy.total_requests) : '--')}</dl></section>`;
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
    return fallback;
  }
  return esc(String(value));
}
