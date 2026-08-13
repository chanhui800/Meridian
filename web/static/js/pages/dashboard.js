let dashSSE = null;
let dashAbortController = null;
let dashRetryTimer = null;
let dashboardTrendValues = [];
let dashboardTrendResizeObserver = null;

function renderDashboard() {
  const page = document.getElementById('page-dashboard');
  page.innerHTML = `
    <h1 class="section-title fade-up">仪表盘</h1>
    <p class="section-sub fade-up stagger-1">Emby 反代服务运行概览 <span class="live-indicator" id="sse-status">● 实时</span></p>
    <div class="form-help fade-up stagger-1" style="margin:-4px 0 18px">当前面板域名：<span class="mono" id="s-panel-domain">—</span></div>
    <div class="stats-row" id="dash-stats">
      <div class="stat-card c-blue fade-up stagger-1">
        <div class="stat-icon-wrap blue">
          <svg viewBox="0 0 24 24"><rect x="2" y="3" width="20" height="14" rx="2"/><line x1="8" y1="21" x2="16" y2="21"/><line x1="12" y1="17" x2="12" y2="21"/></svg>
        </div>
        <div class="stat-number" id="s-total">—</div>
        <div class="stat-title">站点总数</div>
      </div>
      <div class="stat-card c-green fade-up stagger-2">
        <div class="stat-icon-wrap green">
          <svg viewBox="0 0 24 24"><path d="M22 11.08V12a10 10 0 1 1-5.93-9.14"/><polyline points="22 4 12 14.01 9 11.01"/></svg>
        </div>
        <div class="stat-number" id="s-running">—</div>
        <div class="stat-title">运行中</div>
      </div>
      <div class="stat-card c-teal fade-up stagger-3">
        <div class="stat-icon-wrap teal">
          <svg viewBox="0 0 24 24"><polyline points="22 12 18 12 15 21 9 3 6 12 2 12"/></svg>
        </div>
        <div class="stat-number" id="s-traffic">0 B</div>
        <div class="stat-title">总流量</div>
      </div>
      <div class="stat-card c-orange fade-up stagger-4">
        <div class="stat-icon-wrap orange">
          <svg viewBox="0 0 24 24"><circle cx="12" cy="12" r="10"/><polyline points="12 6 12 12 16 14"/></svg>
        </div>
        <div class="stat-number" id="s-uptime">—</div>
        <div class="stat-title">运行时长</div>
      </div>
      <div class="stat-card c-purple fade-up stagger-5">
        <div class="stat-icon-wrap purple">
          <svg viewBox="0 0 24 24"><ellipse cx="12" cy="5" rx="8" ry="3"/><path d="M4 5v6c0 1.7 3.6 3 8 3s8-1.3 8-3V5"/><path d="M4 11v6c0 1.7 3.6 3 8 3s8-1.3 8-3v-6"/></svg>
        </div>
        <div class="stat-number" id="s-cache">0 B</div>
        <div class="stat-title">累计缓存</div>
      </div>
    </div>
    <div class="glass-card fade-up stagger-4">
      <div class="glass-card-header">
        <div class="glass-card-title"><span class="live-dot"></span>站点实时状态</div>
        <div class="glass-card-title" style="font-size:.72rem;color:var(--white-38)" id="s-requests">0 请求</div>
      </div>
      <div style="overflow-x:auto">
        <table>
          <thead><tr>
            <th>站点</th><th>状态</th><th>回源地址</th><th>UA 模式</th><th>入口</th><th>已用流量</th><th>缓存大小</th>
          </tr></thead>
          <tbody id="dash-table"></tbody>
        </table>
      </div>
    </div>
    <div class="dashboard-insights-grid fade-up stagger-5">
      <section class="dashboard-insight-card" id="dashboard-log-health"><div class="dashboard-insight-head"><h2>日志写入</h2><span class="dashboard-health-dot"></span></div><p>正在读取…</p></section>
      <section class="dashboard-insight-card" id="dashboard-schedule-health"><div class="dashboard-insight-head"><h2>定时任务</h2><span class="dashboard-health-dot"></span></div><p>正在读取…</p></section>
    </div>
    <section class="dashboard-trend-card fade-up stagger-5"><div class="glass-card-header"><div class="glass-card-title">请求趋势</div><div class="dashboard-trend-unit">每小时请求总次数</div></div><div class="dashboard-trend-wrap"><canvas id="dashboardRequestTrend" height="230" aria-label="当日每小时请求总次数趋势图"></canvas></div></section>
  `;

  observeDashboardTrendResize();
  startDashSSE();
  loadDashboardTable();
  loadDashboardInsights();
}

async function loadDashboardInsights() {
  try {
    const insights = await API.dashboardInsights();
    if (!insights || Router.current !== 'dashboard') return;
    const log = document.querySelector('#dashboard-log-health p');
    const schedule = document.querySelector('#dashboard-schedule-health p');
    if (log) log.textContent = insights.log_healthy ? `最近写入 ${insights.log_count_today || 0} 条，队列丢弃 ${insights.dropped_logs || 0} 条` : '已关闭';
    if (schedule) schedule.textContent = insights.schedule_enabled ? `${insights.schedule_label || '已启用'}` : '未启用';
    drawDashboardTrend(insights.hourly_requests || []);
  } catch (error) {
    console.warn('Dashboard insights load error', error);
  }
}

function dashboardRequestScale(maxValue) {
  if (!(maxValue > 0)) return { max: 1, step: 1, ticks: 1 };
  const roughStep = maxValue / 4;
  const magnitude = Math.pow(10, Math.floor(Math.log10(roughStep)));
  const fraction = roughStep / magnitude;
  const niceFraction = fraction <= 1 ? 1 : fraction <= 2 ? 2 : fraction <= 2.5 ? 2.5 : fraction <= 5 ? 5 : 10;
  const step = niceFraction * magnitude;
  const max = Math.ceil(maxValue / step) * step;
  return { max, step, ticks: Math.max(1, Math.round(max / step)) };
}

function dashboardTimeLabelIndexes(pointCount, plotWidth) {
  if (pointCount <= 1) return [0];
  const maxLabels = Math.max(2, Math.min(pointCount, Math.floor(plotWidth / 58) + 1));
  const minimumStep = Math.ceil((pointCount - 1) / Math.max(1, maxLabels - 1));
  const step = [1, 2, 3, 4, 6, 8, 12, 24].find(candidate => candidate >= minimumStep) || minimumStep;
  const indexes = [];
  for (let index = 0; index < pointCount; index += step) indexes.push(index);
  if (indexes[indexes.length - 1] !== pointCount - 1) indexes.push(pointCount - 1);
  return indexes;
}

function drawDashboardTrend(values) {
  dashboardTrendValues = Array.isArray(values) ? values.map(value => Math.max(0, Number(value) || 0)) : [];
  if (dashboardTrendValues.length === 0) dashboardTrendValues = Array(24).fill(0);
  const canvas = document.getElementById('dashboardRequestTrend');
  if (!canvas || !canvas.getContext) return;
  const ctx = canvas.getContext('2d');
  const width = canvas.clientWidth || canvas.parentElement?.clientWidth || 800;
  const height = Math.max(180, Math.round(canvas.clientHeight || 230));
  const ratio = window.devicePixelRatio || 1;
  canvas.width = width * ratio; canvas.height = height * ratio;
  ctx.setTransform(ratio, 0, 0, ratio, 0, 0);
  ctx.clearRect(0, 0, width, height);
  const scale = dashboardRequestScale(Math.max(0, ...dashboardTrendValues));
  const left = width <= 480 ? 43 : 50, right = width <= 480 ? 8 : 14, top = 16, bottom = 30;
  const plotW = width - left - right, plotH = height - top - bottom;
  ctx.font = `${width <= 480 ? 11 : 12}px system-ui`;
  ctx.fillStyle = '#64748b';
  ctx.textBaseline = 'middle';
  for (let i = 0; i <= scale.ticks; i++) {
    const value = scale.max - scale.step * i;
    const y = top + plotH * i / scale.ticks;
    ctx.strokeStyle = 'rgba(100,116,139,.18)'; ctx.lineWidth = 1;
    ctx.beginPath(); ctx.moveTo(left, y); ctx.lineTo(width - right, y); ctx.stroke();
    ctx.textAlign = 'right';
    ctx.fillText(formatNumber(Math.round(value)), left - 8, y);
  }
  ctx.strokeStyle = 'rgba(100,116,139,.34)';
  ctx.beginPath(); ctx.moveTo(left, top); ctx.lineTo(left, top + plotH); ctx.lineTo(width - right, top + plotH); ctx.stroke();

  const points = dashboardTrendValues.map((value, index) => ({ x: left + plotW * index / Math.max(1, dashboardTrendValues.length - 1), y: top + plotH * (1 - value / scale.max) }));
  ctx.beginPath(); points.forEach((point, index) => index ? ctx.lineTo(point.x, point.y) : ctx.moveTo(point.x, point.y));
  ctx.strokeStyle = '#3b82f6'; ctx.lineWidth = 3; ctx.lineJoin = 'round'; ctx.stroke();
  ctx.fillStyle = '#64748b'; ctx.textAlign = 'center'; ctx.textBaseline = 'alphabetic';
  const lastIndex = Math.max(1, dashboardTrendValues.length - 1);
  dashboardTimeLabelIndexes(dashboardTrendValues.length, plotW).forEach(index => {
    const hour = index % 24;
    ctx.fillText(`${String(hour).padStart(2, '0')}:00`, left + plotW * index / lastIndex, height - 7);
  });
}

function observeDashboardTrendResize() {
  if (dashboardTrendResizeObserver) {
    dashboardTrendResizeObserver.disconnect();
    dashboardTrendResizeObserver = null;
  }
  const wrap = document.querySelector('.dashboard-trend-wrap');
  if (!wrap || typeof ResizeObserver !== 'function') return;
  dashboardTrendResizeObserver = new ResizeObserver(() => {
    if (Router.current === 'dashboard') drawDashboardTrend(dashboardTrendValues);
  });
  dashboardTrendResizeObserver.observe(wrap);
}

function startDashSSE() {
  stopDashSSE();
  startFetchSSE();
}

function queueDashSSERetry() {
  if (dashRetryTimer) clearTimeout(dashRetryTimer);
  dashRetryTimer = setTimeout(() => {
    if (Router.current === 'dashboard' && API.authenticated) startFetchSSE();
  }, 5000);
}

async function startFetchSSE() {
  const statusEl = document.getElementById('sse-status');
  const controller = new AbortController();
  dashAbortController = controller;

  try {
    const resp = await fetch('/api/events', {
      credentials: 'same-origin',
      signal: controller.signal,
    });

    if (resp.status === 401) {
      await API.logout();
      window.location.reload();
      return;
    }
    if (!resp.ok) throw new Error('SSE failed');
    if (dashAbortController !== controller) return;

    if (statusEl) statusEl.style.color = 'var(--green)';

    const reader = resp.body.getReader();
    const decoder = new TextDecoder();
    let buffer = '';

    while (true) {
      const { done, value } = await reader.read();
      if (done || controller.signal.aborted) break;

      buffer += decoder.decode(value, { stream: true });
      const lines = buffer.split('\n');
      buffer = lines.pop();

      for (const line of lines) {
        if (!line.startsWith('data: ')) continue;
        try {
          updateDashboardLive(JSON.parse(line.slice(6)));
        } catch (e) {
          // Skip malformed chunks and keep stream alive.
        }
      }
    }

    if (!controller.signal.aborted && dashAbortController === controller && Router.current === 'dashboard') {
      if (statusEl) statusEl.style.color = 'var(--red)';
      queueDashSSERetry();
    }
  } catch (e) {
    if (controller.signal.aborted || dashAbortController !== controller) return;
    console.warn('SSE connection lost, retrying in 5s...', e);
    if (statusEl) statusEl.style.color = 'var(--red)';
    queueDashSSERetry();
  }
}

function updateDashboardLive(stats) {
	const panelDomainEl = document.getElementById('s-panel-domain');
	const currentPanelURL = dashboardCurrentPanelURL(stats.panel_access_url);
	if (panelDomainEl && currentPanelURL) panelDomainEl.textContent = currentPanelURL;
  animateValue('s-total', stats.total_sites || 0);
  animateValue('s-running', stats.running_sites || 0);

  const trafficEl = document.getElementById('s-traffic');
  if (trafficEl) trafficEl.textContent = formatBytes(stats.total_traffic || 0);

  const uptimeEl = document.getElementById('s-uptime');
  if (uptimeEl) uptimeEl.textContent = formatUptime(stats.uptime_seconds || 0);

  const requestsEl = document.getElementById('s-requests');
  if (requestsEl) requestsEl.textContent = formatNumber(stats.total_requests || 0) + ' 请求';
}

function dashboardCurrentPanelURL(fallback) {
  if (typeof window !== 'undefined' && window.location && /^https?:$/.test(window.location.protocol) && window.location.host) {
    return `${window.location.protocol}//${window.location.host}`;
  }
  return fallback || '';
}

function formatUptime(seconds) {
  if (seconds < 60) return seconds + 's';
  if (seconds < 3600) return Math.floor(seconds / 60) + '分';
  if (seconds < 86400) return Math.floor(seconds / 3600) + '时' + Math.floor((seconds % 3600) / 60) + '分';
  return Math.floor(seconds / 86400) + '天' + Math.floor((seconds % 86400) / 3600) + '时';
}

function formatNumber(n) {
  return n.toLocaleString();
}

function animateValue(id, newVal) {
  const el = document.getElementById(id);
  if (!el) return;
  const current = parseInt(el.textContent, 10) || 0;
  if (current === newVal) return;
  el.textContent = newVal;
  el.style.transition = 'transform .15s';
  el.style.transform = 'scale(1.08)';
  setTimeout(() => { el.style.transform = ''; }, 150);
}

function stopDashSSE() {
  if (dashboardTrendResizeObserver) {
    dashboardTrendResizeObserver.disconnect();
    dashboardTrendResizeObserver = null;
  }
  if (dashRetryTimer) {
    clearTimeout(dashRetryTimer);
    dashRetryTimer = null;
  }
  if (dashAbortController) {
    dashAbortController.abort();
    dashAbortController = null;
  }
  if (dashSSE) {
    dashSSE.close();
    dashSSE = null;
  }
}

async function loadDashboardTable() {
  try {
    const sites = await API.listSites();
    const tbody = document.getElementById('dash-table');
    if (!tbody) return;

    const totalCache = (sites || []).reduce((total, site) => total + Number(site.cache_size_bytes || 0), 0);
    const cacheEl = document.getElementById('s-cache');
    if (cacheEl) cacheEl.textContent = formatBytes(totalCache);

    if (!sites || sites.length === 0) {
      tbody.innerHTML = '<tr><td colspan="7" style="text-align:center;color:var(--white-38);padding:40px">暂无站点，前往站点管理添加</td></tr>';
      return;
    }

    tbody.innerHTML = sites.map(s => `
      <tr>
        <td style="font-weight:600">${esc(s.name)}</td>
        <td><span class="status-badge"><span class="status-led ${s.running ? 'on' : 'off'}"></span>${s.running ? '运行中' : '已停止'}</span></td>
        <td class="mono">${esc(s.target_url)}</td>
        <td><span class="pill ${uaClassMap[s.ua_mode] || 'pill-blue'}">${esc(uaNameMap[s.ua_mode] || s.ua_mode)}</span></td>
        <td class="mono">${dashboardIngressLabel(s)}</td>
        <td>${formatBytes(s.traffic_used)}</td>
        <td>${formatBytes(s.cache_size_bytes)}</td>
      </tr>
    `).join('');
  } catch (e) {
    console.error('Dashboard table load error:', e);
  }
}

function dashboardIngressLabel(site) {
	const mode = String(site.ingress_mode || (site.public_host ? 'host' : 'port')).toLowerCase();
	if (mode === 'host') return `Host: ${esc(site.public_host || '')}`;
	if (mode === 'path') return `Path: ${esc(site.path_prefix || '')}`;
	if (mode === 'both') return `Host + :${site.listen_port}`;
	return `:${site.listen_port}`;
}

async function loadDashboardData() {
  loadDashboardTable();
}

const uaClassMap = { infuse: 'pill-blue', web: 'pill-green', client: 'pill-orange', custom: 'pill-purple', passthrough: 'pill-blue' };
const uaNameMap = { infuse: 'Infuse', web: 'Web', client: '客户端', custom: '自定义', passthrough: '透传' };

function formatBytes(bytes) {
  if (!bytes || bytes === 0) return '0 B';
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  const i = Math.floor(Math.log(bytes) / Math.log(1024));
  return (bytes / Math.pow(1024, i)).toFixed(i > 1 ? 1 : 0) + ' ' + units[i];
}
