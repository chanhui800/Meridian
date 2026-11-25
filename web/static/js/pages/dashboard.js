let dashSSE = null;
let dashAbortController = null;
let dashRetryTimer = null;
let dashboardBootstrapPromise = null;
let dashboardBootstrapAbortController = null;
let dashboardSecondaryPromise = null;
let dashboardTrendAbortController = null;
let dashboardSecondaryRefreshTimer = null;
let dashboardTrendRefreshTimer = null;
let dashboardTrendResizeObserver = null;
let dashboardTrendState = { siteId: 'all', range: 'realtime', customStart: '', customEnd: '' };
let dashboardTrendCharts = new Map();
let dashboardTrendData = null;
let dashboardSites = [];
let dashboardSitesInitialized = false;
let dashboardSpeedSamples = new Map();
let dashboardLiveSpeeds = new Map();
let dashboardRealtimeTrendSamples = new Map();
let dashboardRealtimeTrendSiteSamples = new Map();
let dashboardLatestRatesBySite = new Map();
let dashboardRealtimeSmoothingCache = new Map();
let dashboardBillingMode = null;
let dashboardRealtimePersistedBillingMode = null;
// Once the Controller exposes its authoritative realtime sequence, the
// browser only renders/merges those points. The local path remains as a
// compatibility fallback for older Controllers and unit harnesses.
let dashboardControllerRealtimeEnabled = false;
let dashboardRealtimeEpoch = '';
let dashboardRealtimeRevision = 0;
let dashboardRealtimeSiteRevisions = new Map();
let dashboardRealtimeProtocolObserved = false;
let dashboardRealtimeRetiredEpochs = new Set();
let dashboardRealtimeNeedsRecovery = false;
let dashboardRealtimeRecoveryAbortController = null;
let dashboardRealtimeLastRecoveryMS = 0;
const dashboardRealtimeStorageKey = 'meridian.dashboard.realtime-trends.v1';
const dashboardRealtimePersistIntervalMS = 15000;
const dashboardRealtimeStorageMaxChars = 1024 * 1024;
let dashboardRealtimePersistTimer = 0;
let dashboardRealtimePersistDirty = false;
let dashboardRealtimePersistRetryAt = 0;
let dashboardRealtimePersistFailures = 0;
let dashboardRealtimePersistLimit = dashboardRealtimeStorageMaxChars;
let dashboardRealtimePagehideBound = false;
let dashboardLastSnapshotMS = 0;
let dashboardLastObservedSiteCount = -1;
let dashboardConsoleMetric = 'speed';
let dashboardConsoleQuery = '';
let dashboardConsoleExpandedSites = new Set();

function dashboardSelectConsoleMetric(metric, focus = false) {
  if (!['speed', 'requests', 'traffic'].includes(metric)) return;
  dashboardConsoleMetric = metric;
  ['speed', 'requests', 'traffic'].forEach(key => {
    const tab = document.getElementById(`console-tab-${key}`);
    const panel = document.getElementById(`console-chart-${key}`);
    if (tab) {
      tab.setAttribute('aria-selected', String(key === metric));
      tab.tabIndex = key === metric ? 0 : -1;
      if (key === metric && focus) tab.focus();
    }
    if (panel) panel.hidden = key !== metric;
  });
  drawDashboardTrendChart(metric);
}

function setupDashboardConsoleControls() {
  const metrics = ['speed', 'requests', 'traffic'];
  metrics.forEach((metric, index) => {
    const tab = document.getElementById(`console-tab-${metric}`);
    if (!tab) return;
    tab.onclick = () => dashboardSelectConsoleMetric(metric);
    tab.onkeydown = event => {
      const next = event.key === 'ArrowRight' ? metrics[(index + 1) % metrics.length]
        : event.key === 'ArrowLeft' ? metrics[(index + metrics.length - 1) % metrics.length]
        : event.key === 'Home' ? metrics[0] : event.key === 'End' ? metrics.at(-1) : null;
      if (next) { event.preventDefault(); dashboardSelectConsoleMetric(next, true); }
    };
  });
  const search = document.getElementById('console-site-search');
  if (search) search.oninput = () => {
    dashboardConsoleQuery = search.value.trim().toLocaleLowerCase();
    renderDashboardTableRows();
  };
  const retrySites = document.getElementById('console-bootstrap-retry');
  const retryTrends = document.getElementById('console-trend-retry');
  if (retrySites) retrySites.onclick = () => { void loadDashboardBootstrap(); };
  if (retryTrends) retryTrends.onclick = () => { void loadDashboardTrends(); };
  const sites = document.getElementById('dash-table');
  if (sites) sites.addEventListener('toggle', event => {
    if (event.target?.isConnected === false) return;
    const siteID = Number(event.target?.dataset?.consoleSite);
    if (!siteID) return;
    if (event.target.open) dashboardConsoleExpandedSites.add(siteID);
    else dashboardConsoleExpandedSites.delete(siteID);
  }, true);
}

function dashboardUpdateConsoleRates() {
  const downEl = document.getElementById('console-current-download');
  const upEl = document.getElementById('console-current-upload');
  if (!downEl && !upEl) return;
  let down = 0, up = 0, unavailable = false;
  dashboardSites.forEach(site => {
    if (!site.running) return;
    const speed = site._liveSpeed;
    const gaugeReceived = Number(site.agent_rate_received_at_ms || site.agent_received_at_ms || 0);
    const snapshotMS = dashboardLastSnapshotMS || Date.now();
    const agentGaugeUnavailable = Number(site.agent_received_at_ms || 0) > 0 &&
      (site.agent_rate_valid !== true || gaugeReceived <= 0 || snapshotMS < gaugeReceived || snapshotMS - gaugeReceived > 6000);
    if (!speed || agentGaugeUnavailable) {
      unavailable = true;
      return;
    }
    down += Math.max(0, Number(speed.down || 0));
    up += Math.max(0, Number(speed.up || 0));
  });
  if (downEl) downEl.textContent = unavailable ? '暂不可用' : formatRate(down);
  if (upEl) upEl.textContent = unavailable ? '等待实时采样' : `上传 ${formatRate(up)}`;
}

function dashboardCreateAbortController() {
  if (typeof AbortController === 'function') return new AbortController();
  const signal = { aborted: false };
  return { signal, abort() { signal.aborted = true; } };
}

function renderDashboard() {
  const page = document.getElementById('page-dashboard');
  dashboardConsoleMetric = 'speed';
  dashboardConsoleQuery = '';
  dashboardConsoleExpandedSites = new Set();
  page.innerHTML = `
    <div class="console-page">
      <header class="console-heading">
        <div><div class="console-eyebrow">Meridian / overview</div><h1>仪表盘</h1><p class="console-lead">站点运行情况与流量变化，一目了然。</p></div>
        <div class="console-heading-actions"><span class="console-status" id="sse-status">正在连接</span><a class="console-button" href="#sites">管理站点<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M5 12h14M13 6l6 6-6 6"/></svg></a></div>
      </header>
      <section class="console-metrics" aria-label="运行摘要">
        <div class="console-metric"><div class="console-metric-label">运行站点</div><div class="console-metric-value"><strong id="s-running">—</strong><span class="console-metric-fraction">/ <span id="s-total">—</span></span></div><div class="console-metric-note">已启用的反代入口</div></div>
        <div class="console-metric"><div class="console-metric-label" id="s-traffic-title">已用流量</div><div class="console-metric-value" id="s-traffic">—</div><div class="console-metric-note" id="console-cycle-label">当前计费周期</div></div>
        <div class="console-metric"><div class="console-metric-label">本月请求</div><div class="console-metric-value" id="s-requests">—</div><div class="console-metric-note">全部站点 · 自然月</div></div>
        <div class="console-metric"><div class="console-metric-label">当前下载</div><div class="console-metric-value" id="console-current-download">—</div><div class="console-metric-note" id="console-current-upload">上传 —</div></div>
      </section>
      <div class="console-load-error" id="console-bootstrap-error" role="alert" hidden><span>站点信息暂时无法读取，稍后重试。</span><button type="button" class="console-button" id="console-bootstrap-retry">重新读取</button></div>
      <div class="console-dashboard-body">
        <section class="console-panel" aria-label="数据趋势">
          <div class="console-panel-heading console-trend-head"><div><h2>流量与访问</h2><p>同一时间窗口，查看不同维度</p></div><div class="console-trend-filters">
            <label><span class="sr-only">站点</span><select id="dashboard-trend-site" class="form-select" aria-label="选择趋势站点"><option value="all">全部站点</option></select></label>
            <label><span class="sr-only">时间</span><select id="dashboard-trend-range" class="form-select" aria-label="选择趋势时间范围"><option value="realtime">实时 · 5 分钟</option><option value="hour">过去 1 小时</option><option value="6h">过去 6 小时</option><option value="day">过去 1 天</option><option value="7d">过去 7 天</option><option value="month">本月</option><option value="custom">自定义时间</option></select></label>
          </div></div>
          <div class="dashboard-trend-custom" id="dashboard-trend-custom" hidden>
            <label>开始时间<input type="datetime-local" id="dashboard-trend-start" class="form-input" step="60" aria-label="趋势开始时间"></label><span class="dashboard-trend-custom-separator" aria-hidden="true">至</span>
            <label>结束时间<input type="datetime-local" id="dashboard-trend-end" class="form-input" step="60" aria-label="趋势结束时间"></label><button type="button" class="console-button is-primary" id="dashboard-trend-apply">应用</button>
          </div>
          <div class="console-trend-error" id="dashboard-trend-custom-error" role="alert" hidden></div>
          <div class="console-load-error" id="console-trend-load-error" role="alert" hidden><span>趋势暂时无法读取，已保留当前数据。</span><button type="button" class="console-button" id="console-trend-retry">重试</button></div>
          <div class="console-trend-loading" id="console-trend-loading" role="status">正在读取趋势…</div>
          <div class="console-trend-tabs" role="tablist" aria-label="趋势指标">
            <button type="button" class="console-trend-tab" id="console-tab-speed" role="tab" aria-controls="console-chart-speed" aria-selected="true" tabindex="0" data-console-metric="speed">网速</button>
            <button type="button" class="console-trend-tab" id="console-tab-requests" role="tab" aria-controls="console-chart-requests" aria-selected="false" tabindex="-1" data-console-metric="requests">请求</button>
            <button type="button" class="console-trend-tab" id="console-tab-traffic" role="tab" aria-controls="console-chart-traffic" aria-selected="false" tabindex="-1" data-console-metric="traffic">计费流量</button>
          </div>
          <section class="console-chart" id="console-chart-speed" role="tabpanel" aria-labelledby="console-tab-speed" data-dashboard-chart="speed">
            <div class="console-chart-heading"><span class="dashboard-trend-unit">所选时间内峰值</span><strong class="dashboard-trend-summary" id="dashboard-speed-summary">—</strong></div><div class="dashboard-trend-wrap"><canvas id="dashboardSpeedTrend" aria-label="下载和上传速度趋势图"></canvas><div class="dashboard-chart-tooltip" hidden></div></div><div class="dashboard-trend-legend"><span><i class="download"></i>下载</span><span><i class="upload"></i>上传</span></div>
          </section>
          <section class="console-chart" id="console-chart-requests" role="tabpanel" aria-labelledby="console-tab-requests" data-dashboard-chart="requests" hidden>
            <div class="console-chart-heading"><span class="dashboard-trend-unit">所选时间内请求</span><strong class="dashboard-trend-summary" id="dashboard-requests-summary">—</strong></div><div class="dashboard-trend-wrap"><canvas id="dashboardRequestsTrend" aria-label="请求次数趋势图"></canvas><div class="dashboard-chart-tooltip" hidden></div></div><div class="dashboard-trend-legend"><span><i class="requests"></i>请求次数</span></div>
          </section>
          <section class="console-chart" id="console-chart-traffic" role="tabpanel" aria-labelledby="console-tab-traffic" data-dashboard-chart="traffic" hidden>
            <div class="console-chart-heading"><span class="dashboard-trend-unit" id="dashboard-traffic-unit">所选时间内计费流量</span><strong class="dashboard-trend-summary" id="dashboard-traffic-summary">—</strong></div><div class="dashboard-trend-wrap"><canvas id="dashboardTrafficTrend" aria-label="计费流量趋势图"></canvas><div class="dashboard-chart-tooltip" hidden></div></div><div class="dashboard-trend-legend"><span><i class="traffic"></i>计费流量</span></div>
          </section>
          <div class="console-trend-help" id="dashboard-trend-help">数据时间 <span id="dashboard-trend-timezone">UTC+08:00</span></div>
        </section>
        <section class="console-panel" aria-label="站点运行状态">
          <div class="console-panel-heading"><div><h2>站点状态</h2><p>展开站点可查看入口与回源信息</p></div><label><span class="sr-only">搜索站点</span><input class="console-site-search" type="search" id="console-site-search" placeholder="搜索站点" autocomplete="off"></label></div>
          <div class="console-sites-head" aria-hidden="true"><span>站点</span><span>状态</span><span>实时网速</span><span style="text-align:right">已用流量</span><span></span></div>
          <div id="dash-table"><div class="console-empty">正在读取站点…</div></div>
        </section>
      </div>
      <details class="console-system"><summary><svg viewBox="0 0 24 24" aria-hidden="true"><path d="M3 12h4l3-7 4 14 3-7h4"/></svg><span>系统状态</span><span class="console-system-meta">运行 <span id="s-uptime">—</span> · 缓存 <span id="s-cache">—</span></span></summary>
        <div class="console-system-grid"><section id="dashboard-log-health"><div class="dashboard-insight-head"><h2>日志写入</h2><span class="dashboard-health-dot is-disabled" aria-hidden="true"></span></div><p>正在读取…</p></section><section id="dashboard-schedule-health"><div class="dashboard-insight-head"><h2>定时任务</h2><span class="dashboard-health-dot is-disabled" aria-hidden="true"></span></div><p>正在读取…</p></section></div>
      </details>
      <footer class="console-page-foot"><span>面板入口 <span id="s-panel-domain" class="console-mono">—</span></span><span>Meridian · 服务控制台</span></footer>
    </div>`;

  startDashSSE();
  startDashboardRefreshTimers();
  restoreDashboardRealtimeSamples();
  setupDashboardTrendControls();
  setupDashboardConsoleControls();
  observeDashboardTrendResize();
  void loadDashboardBootstrap();
  const deferTrends = typeof requestAnimationFrame === 'function' ? requestAnimationFrame : fn => setTimeout(fn, 0);
  deferTrends(() => { if (Router.current === 'dashboard') void loadDashboardTrends(); });
}
function applyDashboardInsights(insights) {
  if (!insights || Router.current !== 'dashboard') return;
  const setHealthState = (id, state) => {
    const dot = document.querySelector(`#${id} .dashboard-health-dot`);
    if (!dot) return;
    dot.classList.remove('is-healthy', 'is-degraded', 'is-disabled', 'is-error');
    dot.classList.add(`is-${state || 'disabled'}`);
  };
  const log = document.querySelector('#dashboard-log-health p');
  const schedule = document.querySelector('#dashboard-schedule-health p');
  const latestLog = insights.latest_log_ms ? meridianFormatDateTime(insights.latest_log_ms) : '暂无记录';
  const logStatus = insights.log_status || (insights.log_healthy ? (insights.dropped_logs ? 'degraded' : 'healthy') : 'disabled');
  const scheduleStatus = insights.schedule_status || (insights.schedule_enabled ? 'healthy' : 'disabled');
  setHealthState('dashboard-log-health', logStatus);
  setHealthState('dashboard-schedule-health', scheduleStatus);
  if (log) log.textContent = insights.log_healthy ? `今日写入 ${formatNumber(insights.log_count_today || 0)} 条 · 最近写入 ${latestLog}` : '已关闭';
  if (log && logStatus === 'degraded') log.textContent += ' · 有日志丢弃';
  if (schedule) schedule.textContent = insights.schedule_enabled ? `Telegram 日报 · ${insights.schedule_label || '已启用'}` : 'Telegram 日报 · 未启用';
}

function reconcileDashboardTrendSiteSelection() {
  if (dashboardTrendState.siteId === 'all') return false;
  const exists = dashboardSites.some(site => String(site.id) === String(dashboardTrendState.siteId));
  if (exists) return false;
  dashboardTrendState.siteId = 'all';
  return true;
}

async function loadDashboardBootstrap() {
  if (dashboardBootstrapPromise) return dashboardBootstrapPromise;
  const loadError = document.getElementById('console-bootstrap-error');
  if (loadError) loadError.hidden = true;
  dashboardBootstrapAbortController = dashboardCreateAbortController();
  const signal = dashboardBootstrapAbortController.signal;
  dashboardBootstrapPromise = API.dashboardBootstrap(signal).then(data => {
    if (signal.aborted || !data || Router.current !== 'dashboard') return data;
    const currentSites = new Map(dashboardSites.map(site => [Number(site.id), site]));
    dashboardSites = Array.isArray(data.sites) ? data.sites.map(site => {
      const current = currentSites.get(Number(site.id));
      const speed = dashboardLiveSpeeds.get(Number(site.id)) || current?._liveSpeed;
      return speed ? { ...site, _liveSpeed: speed } : site;
    }) : [];
    dashboardSitesInitialized = true;
    const selectionReset = reconcileDashboardTrendSiteSelection();
    const cacheEl = document.getElementById('s-cache');
    if (cacheEl) cacheEl.textContent = formatBytes(dashboardSites.reduce((total, site) => total + Number(site.cache_size_bytes || 0), 0));
    if (data.snapshot) updateDashboardLive(data.snapshot);
    applyDashboardInsights(data.insights);
    renderDashboardTrendSites();
    renderDashboardTableRows();
    if (selectionReset && dashboardTrendState.range) void loadDashboardTrends();
    return data;
  }).catch(error => {
    if (error && error.name === 'AbortError') return null;
    if (signal.aborted || Router.current !== 'dashboard') return null;
    console.warn('Dashboard bootstrap load error', error);
    if (loadError) loadError.hidden = false;
    const sites = document.getElementById('dash-table');
    if (sites && !dashboardSitesInitialized) sites.innerHTML = '<div class="console-empty">暂时无法读取站点信息</div>';
    return null;
  }).finally(() => {
    if (dashboardBootstrapAbortController?.signal === signal) {
      dashboardBootstrapAbortController = null;
      dashboardBootstrapPromise = null;
    }
  });
  return dashboardBootstrapPromise;
}

function dashboardRequestScale(maxValue, metric = '') {
  if (metric === 'requests') {
    // Request deltas are integer counts. Keep the axis integer-valued so a
    // single request is not rendered with rounded fractional labels such as
    // 000111, which makes the chart look like it contains duplicate events.
    const ceiling = Math.max(5, Math.ceil(Number(maxValue) || 0));
    if (ceiling <= 6) return { max: ceiling, step: 1, ticks: ceiling };
    const roughStep = ceiling / 6;
    const magnitude = Math.pow(10, Math.floor(Math.log10(roughStep)));
    const fraction = roughStep / magnitude;
    const niceFraction = fraction <= 1 ? 1 : fraction <= 2 ? 2 : fraction <= 5 ? 5 : 10;
    const step = Math.max(1, Math.ceil(niceFraction * magnitude));
    const max = step * Math.ceil(ceiling / step);
    return { max, step, ticks: Math.max(1, Math.ceil(max / step)) };
  }
  // Keep six horizontal bands on speed and traffic charts. Only the step
  // changes with the data range, so cards remain visually comparable.
  const ticks = 6;
  if (!(maxValue > 0)) return { max: ticks, step: 1, ticks };
  const roughStep = maxValue / ticks;
  const magnitude = Math.pow(10, Math.floor(Math.log10(roughStep)));
  const fraction = roughStep / magnitude;
  const niceFraction = fraction <= 1 ? 1 : fraction <= 2 ? 2 : fraction <= 2.5 ? 2.5 : fraction <= 5 ? 5 : 10;
  const step = niceFraction * magnitude;
  return { max: step * ticks, step, ticks };
}

function dashboardTimeLabelIndexes(pointCount, plotWidth, range, points = null, startMS = 0, endMS = 0) {
  if (pointCount <= 1) return [0];
  // Realtime charts follow Komari's sparse boundary labels. Historical views
  // keep the denser adaptive labels because their buckets cover longer spans.
  if (range === 'realtime') return [0, pointCount - 1];
  const minimumGap = range === 'realtime' ? 70 : 76;
  if (plotWidth < minimumGap * 1.7) return [0];
  const maxLabels = Math.max(2, Math.min(pointCount, Math.floor(plotWidth / minimumGap) + 1));
  if (Array.isArray(points) && points.length > 1 && Number(endMS) > Number(startMS)) {
    const indexes = [];
    const used = new Set();
    for (let label = 0; label < maxLabels; label += 1) {
      const target = Number(startMS) + (Number(endMS) - Number(startMS)) * label / Math.max(1, maxLabels - 1);
      let nearest = 0;
      let distance = Infinity;
      points.forEach((point, index) => {
        const timestamp = Number(point?.timestamp_ms || 0);
        const candidateDistance = Math.abs(timestamp - target);
        if (candidateDistance < distance) {
          distance = candidateDistance;
          nearest = index;
        }
      });
      if (!used.has(nearest)) {
        used.add(nearest);
        indexes.push(nearest);
      }
    }
    return indexes.sort((a, b) => a - b);
  }
  const step = Math.max(1, Math.ceil((pointCount - 1) / Math.max(1, maxLabels - 1)));
  const indexes = [0];
  for (let index = step; index < pointCount - 1; index += step) indexes.push(index);
  const lastIndex = pointCount - 1;
  const lastLabelPixelGap = (lastIndex - indexes[indexes.length - 1]) * plotWidth / Math.max(1, lastIndex);
  if (lastLabelPixelGap >= minimumGap || indexes.length === 1) indexes.push(lastIndex);
  return indexes;
}

function dashboardTrendAxisLabel(index, points, range, startMS, endMS) {
  if (range === 'realtime' && Array.isArray(points) && points.length > 1) {
    return index === 0 ? Number(startMS) : Number(endMS);
  }
  return Number(points?.[index]?.timestamp_ms || (index === 0 ? startMS : endMS));
}

function dashboardTrendMetricValue(point, metric) {
  if (metric === 'speed') return Math.max(0, Number(point.download_bps || 0), Number(point.upload_bps || 0));
  if (metric === 'traffic') return Math.max(0, Number(point.traffic_bytes || 0));
  return Math.max(0, Number(point.requests || 0));
}

function dashboardTrendValueLabel(value, metric) {
  if (metric === 'speed' || metric === 'traffic') return formatBytes(value);
  return formatNumber(Math.round(value));
}

function dashboardTrendPointerState(rect, geometry, event, pointsOrCount, chartStartMS = 0, chartEndMS = 0) {
  const width = Math.max(1, Number(rect?.width) || 1);
  const height = Math.max(1, Number(rect?.height) || 1);
  const scaleX = Math.max(1, Number(geometry?.width) || width) / width;
  const scaleY = Math.max(1, Number(geometry?.height) || height) / height;
  const rawX = (Number(event?.clientX) - (Number(rect?.left) || 0)) * scaleX;
  const rawY = (Number(event?.clientY) - (Number(rect?.top) || 0)) * scaleY;
  const left = Number(geometry?.left) || 0;
  const top = Number(geometry?.top) || 0;
  const plotW = Math.max(1, Number(geometry?.plotW) || 1);
  const plotH = Math.max(1, Number(geometry?.plotH) || 1);
  const x = Math.max(left, Math.min(left + plotW, Number.isFinite(rawX) ? rawX : left));
  const y = Math.max(top, Math.min(top + plotH, Number.isFinite(rawY) ? rawY : top));
  const points = Array.isArray(pointsOrCount) ? pointsOrCount : null;
  const pointCount = points ? points.length : Math.max(0, Number(pointsOrCount) || 0);
  let index = Math.max(0, Math.min(Math.max(0, pointCount - 1), Math.round(((x - left) / plotW) * Math.max(0, pointCount - 1))));
  const start = Number(chartStartMS || geometry?.chartStartMS || points?.[0]?.timestamp_ms || 0);
  const end = Number(chartEndMS || geometry?.chartEndMS || points?.[points.length - 1]?.timestamp_ms || start);
  if (points && points.length > 1 && end > start) {
    const target = start + ((x - left) / plotW) * (end - start);
    let bestDistance = Infinity;
    points.forEach((point, candidate) => {
      const timestamp = Number(point?.timestamp_ms || 0);
      const distance = Math.abs(timestamp - target);
      if (distance < bestDistance) {
        bestDistance = distance;
        index = candidate;
      }
    });
  }
  return { x, y, index };
}

function dashboardTrendPointerInside(rect, event) {
  if (!rect || !event) return false;
  const x = Number(event.clientX);
  const y = Number(event.clientY);
  if (!Number.isFinite(x) || !Number.isFinite(y)) return false;
  const left = Number(rect.left) || 0;
  const top = Number(rect.top) || 0;
  const right = Number.isFinite(Number(rect.right)) ? Number(rect.right) : left + (Number(rect.width) || 0);
  const bottom = Number.isFinite(Number(rect.bottom)) ? Number(rect.bottom) : top + (Number(rect.height) || 0);
  return x >= left && x <= right && y >= top && y <= bottom;
}

function dashboardTooltipPosition(pointerX, pointerY, wrapWidth, wrapHeight, tooltipWidth, tooltipHeight) {
  const width = Math.max(1, Number(wrapWidth) || 1);
  const height = Math.max(1, Number(wrapHeight) || 1);
  const cardWidth = Math.max(1, Number(tooltipWidth) || 180);
  const cardHeight = Math.max(1, Number(tooltipHeight) || 56);
  const gap = 14;
  const padding = 6;
  const rightSpace = width - pointerX - gap - padding;
  const leftSpace = pointerX - gap - padding;
  const clampLeft = left => Math.max(padding, Math.min(Math.max(padding, width - cardWidth - padding), left));

  // Keep the tooltip beside the pointer whenever possible. This avoids
  // covering the pointer even when the contents contain many site rows.
  if (rightSpace >= cardWidth) {
    let top = pointerY - cardHeight - gap;
    if (top < padding) top = pointerY + gap;
    return { left: clampLeft(pointerX + gap), top };
  }
  if (leftSpace >= cardWidth) {
    let top = pointerY - cardHeight - gap;
    if (top < padding) top = pointerY + gap;
    return { left: clampLeft(pointerX - cardWidth - gap), top };
  }

  // If neither side has enough room, place the full card above or below the
  // pointer. It is intentionally allowed to overflow the chart wrapper so
  // long all-site tooltips keep all rows visible without covering the pointer.
  const aboveSpace = Math.max(0, pointerY - gap - padding);
  const belowSpace = Math.max(0, height - pointerY - gap - padding);
  const below = belowSpace >= aboveSpace;
  return { left: clampLeft(pointerX - cardWidth / 2), top: below ? pointerY + gap : pointerY - cardHeight - gap };
}

function dashboardTrendTimeLabel(timestamp, range) {
  const date = meridianTimezoneDate(timestamp);
  const pad = value => String(value).padStart(2, '0');
  if (range === '7d' || range === 'day' || range === 'month') return `${date.getUTCMonth() + 1}/${date.getUTCDate()} ${pad(date.getUTCHours())}:00`;
  if (range === 'realtime') return `${pad(date.getUTCHours())}:${pad(date.getUTCMinutes())}:${pad(date.getUTCSeconds())}`;
  if (range === 'custom') return `${date.getUTCMonth() + 1}/${date.getUTCDate()} ${pad(date.getUTCHours())}:${pad(date.getUTCMinutes())}`;
  return `${pad(date.getUTCHours())}:${pad(date.getUTCMinutes())}`;
}

function dashboardTrendMetricLine(point, metric) {
  if (!point) return '暂无数据';
  if (metric === 'speed' && point.speed_unavailable === true) return '速度暂不可用';
  if (metric === 'speed') return `↓ ${formatRate(point.download_bps)} · ↑ ${formatRate(point.upload_bps)}`;
  if (metric === 'requests') return `请求 ${formatNumber(point.requests || 0)} 次`;
  if (metric === 'traffic') return `流量 ${formatBytes(point.traffic_bytes || 0)}`;
  return '';
}

function dashboardTrendTooltipTime(point, range) {
  const bucketStart = Number(point?.bucket_start_ms || 0);
  const bucketEnd = Number(point?.bucket_end_ms || point?.timestamp_ms || 0);
  if (bucketStart > 0 && bucketEnd > bucketStart) {
    return `${meridianFormatDateTime(bucketStart)} - ${dashboardTrendTimeLabel(bucketEnd, range)}`;
  }
  return meridianFormatDateTime(point?.timestamp_ms);
}

function dashboardTrendTooltip(point, metric, range, pointIndex = -1) {
  const time = dashboardTrendTooltipTime(point, range);
  const siteSelect = document.getElementById('dashboard-trend-site');
  const selectedOption = siteSelect?.selectedOptions?.[0];
  const selectedSiteID = dashboardTrendState.siteId === 'all' ? null : String(dashboardTrendState.siteId);
  const allSeries = dashboardTrendData?.site_series || [];
  const realtimeSeries = dashboardRealtimeTrendSiteSamples;
  const siteRows = [];
  if (selectedSiteID === null) {
    const realtimeIndex = pointIndex;
    if (range === 'realtime' && realtimeSeries.size && realtimeIndex >= 0) {
      const knownSites = dashboardSites.length ? dashboardSites : allSeries.map(series => ({ id: series.site_id, name: series.site_name }));
      knownSites.forEach(site => {
        // A site may report at a different cadence from the other sites. The
        // per-site realtime arrays therefore contain only samples emitted by
        // that site and cannot be indexed by the aggregate point index. Use
        // the latest sample at or before the hovered timestamp instead of
        // turning a sparse array lookup into a misleading zero.
        let sample;
        if (metric === 'speed') {
          sample = point.site_contributions?.[String(site.id)]
            || dashboardRealtimeSiteSampleAt(site.id, point.timestamp_ms)
            || { download_bps: 0, upload_bps: 0, requests: 0, traffic_bytes: 0, speed_unavailable: true };
        } else {
          // Traffic and request values describe the contribution to this
          // aggregate interval. Carrying forward the previous sparse sample
          // would count the same bytes again when another site reports.
          sample = point.site_contributions?.[String(site.id)] || { download_bps: 0, upload_bps: 0, requests: 0, traffic_bytes: 0 };
        }
        siteRows.push(`<div class="dashboard-chart-tooltip-row"><strong>${esc(site.name || `站点 ${site.id}`)}</strong><span>${dashboardTrendMetricLine(sample, metric)}</span></div>`);
      });
    } else {
      allSeries.forEach(series => {
        const sitePoint = series.points?.[pointIndex];
        if (sitePoint) siteRows.push(`<div class="dashboard-chart-tooltip-row"><strong>${esc(series.site_name || `站点 ${series.site_id}`)}</strong><span>${dashboardTrendMetricLine(sitePoint, metric)}</span></div>`);
      });
    }
  } else {
    const siteName = selectedOption?.textContent?.trim() || allSeries.find(series => String(series.site_id) === selectedSiteID)?.site_name || `站点 ${selectedSiteID}`;
    siteRows.push(`<div class="dashboard-chart-tooltip-row"><strong>${esc(siteName)}</strong><span>${dashboardTrendMetricLine(point, metric)}</span></div>`);
  }
  const lines = [`<span>${esc(time)}</span>`];
  if (siteRows.length) lines.push(siteRows.join(''));
  else lines.push(`<div class="dashboard-chart-tooltip-row"><strong>${esc(selectedSiteID === null ? '全部站点' : (selectedOption?.textContent?.trim() || `站点 ${selectedSiteID}`))}</strong><span>${dashboardTrendMetricLine(point, metric)}</span></div>`);
  return lines.join('');
}

function dashboardRealtimeSiteSampleAt(siteID, timestampMS) {
  const samples = dashboardRealtimeTrendSiteSamples.get(String(siteID)) || [];
  if (!samples.length) return null;
  const target = Number(timestampMS);
  if (!Number.isFinite(target)) return samples[samples.length - 1] || null;
  let low = 0;
  let high = samples.length - 1;
  let latest = null;
  while (low <= high) {
    const middle = (low + high) >> 1;
    const sampledAt = Number(samples[middle]?.timestamp_ms);
    if (!Number.isFinite(sampledAt) || sampledAt > target) {
      high = middle - 1;
    } else {
      latest = samples[middle];
      low = middle + 1;
    }
  }
  return latest;
}

// Match Komari's realtime view: a fixed five-minute window sampled from the
// two-second SSE cadence. The window must stay fixed even when a site has no
// traffic, otherwise sparse site reports make the x-axis expand over time.
const dashboardRealtimeWindowDurationMS = 5 * 60 * 1000;
const dashboardRealtimeSampleIntervalMS = 2 * 1000;
// Keep a bounded FIFO as a second guard for duplicated or unusually frequent
// events. At the normal two-second cadence this is exactly one five-minute
// window.
const dashboardRealtimeMaxPoints = Math.ceil(dashboardRealtimeWindowDurationMS / dashboardRealtimeSampleIntervalMS);

function dashboardRealtimeWindowBounds(now = Date.now()) {
  const current = Number(now) > 0 ? Number(now) : Date.now();
  return { start: current - dashboardRealtimeWindowDurationMS, end: current };
}

function pruneDashboardRealtimeSamples(samples, now = Date.now()) {
  if (!Array.isArray(samples) || !samples.length) return samples;
  const { start, end } = dashboardRealtimeWindowBounds(now);
  const kept = samples
    .filter(point => {
      const timestamp = Number(point?.timestamp_ms || 0);
      return Number.isFinite(timestamp) && timestamp >= start && timestamp <= end + 5000;
    })
    .sort((a, b) => Number(a.timestamp_ms || 0) - Number(b.timestamp_ms || 0));
  if (kept.length > dashboardRealtimeMaxPoints) {
    kept.splice(0, kept.length - dashboardRealtimeMaxPoints);
  }
  samples.splice(0, samples.length, ...kept);
  return samples;
}

function dashboardRealtimeStorage() {
  try {
    if (typeof window === 'undefined' || !window.localStorage) return null;
    return window.localStorage;
  } catch (_) {
    // Private browsing modes and restrictive storage policies can throw even
    // when localStorage is exposed. Trend persistence is only an enhancement,
    // so the live SSE path must continue without it.
    return null;
  }
}

function dashboardNormalizeRealtimePoint(value) {
  if (!value || typeof value !== 'object') return null;
  const timestamp = Number(value.timestamp_ms);
  if (!Number.isFinite(timestamp) || timestamp <= 0) return null;
  const numberOrZero = candidate => {
    const number = Number(candidate);
    return Number.isFinite(number) && number >= 0 ? number : 0;
  };
  const point = {
    timestamp_ms: timestamp,
    download_bps: numberOrZero(value.download_bps),
    upload_bps: numberOrZero(value.upload_bps),
    bytes_in: numberOrZero(value.bytes_in),
    bytes_out: numberOrZero(value.bytes_out),
    requests: numberOrZero(value.requests),
    speed_unavailable: value.speed_unavailable === true,
  };
  const revision = Number(value.realtime_revision || 0);
  if (Number.isSafeInteger(revision) && revision > 0) point.realtime_revision = revision;
  // Cumulative counters let the realtime tail start from the exact
  // persisted Agent baseline returned by the Controller. Older localStorage
  // entries may omit them and continue through the legacy timestamp merge.
  if (value.cumulative_bytes_in !== undefined) point.cumulative_bytes_in = numberOrZero(value.cumulative_bytes_in);
  if (value.cumulative_bytes_out !== undefined) point.cumulative_bytes_out = numberOrZero(value.cumulative_bytes_out);
  if (value.cumulative_requests !== undefined) point.cumulative_requests = numberOrZero(value.cumulative_requests);
  if (value.traffic_bytes !== undefined) point.traffic_bytes = numberOrZero(value.traffic_bytes);
  if (value.site_contributions && typeof value.site_contributions === 'object' && !Array.isArray(value.site_contributions)) {
    const contributions = {};
    Object.keys(value.site_contributions).slice(0, 512).forEach(siteID => {
      if (!/^\d+$/.test(siteID)) return;
      const source = value.site_contributions[siteID];
      const contribution = dashboardNormalizeRealtimePoint({ ...source, timestamp_ms: timestamp, realtime_revision: point.realtime_revision || source.realtime_revision });
      if (!contribution) return;
      delete contribution.site_contributions;
      contributions[siteID] = contribution;
    });
    if (Object.keys(contributions).length) point.site_contributions = contributions;
  }
  return point;
}

function dashboardNormalizeRealtimeSamples(values, now = Date.now()) {
  if (!Array.isArray(values)) return [];
  const samples = [];
  values.forEach(value => {
    const point = dashboardNormalizeRealtimePoint(value);
    if (point) upsertDashboardRealtimeSample(samples, point);
  });
  return pruneDashboardRealtimeSamples(samples, now);
}

function dashboardPersistableRealtimeSamples(samples) {
  if (!Array.isArray(samples)) return [];
  return samples.map(dashboardNormalizeRealtimePoint).filter(Boolean);
}

function persistDashboardRealtimeSamples() {
  dashboardRealtimePersistDirty = true;
  if (dashboardRealtimePersistTimer || typeof setTimeout !== 'function') return;
  if (!dashboardRealtimePagehideBound && typeof window !== 'undefined' && typeof window.addEventListener === 'function') {
    window.addEventListener('pagehide', flushDashboardRealtimeSamples);
    dashboardRealtimePagehideBound = true;
  }
  const delay = Math.max(dashboardRealtimePersistIntervalMS, dashboardRealtimePersistRetryAt - Date.now());
  dashboardRealtimePersistTimer = setTimeout(() => {
    dashboardRealtimePersistTimer = 0;
    flushDashboardRealtimeSamples();
  }, delay);
}

function dashboardRealtimeCompactStoragePayload() {
  const all = dashboardPersistableRealtimeSamples(dashboardRealtimeTrendSamples.get('all') || []);
  const byTimestamp = new Map(all.map(point => [point.timestamp_ms, point]));
  const sites = {};
  dashboardRealtimeTrendSiteSamples.forEach((samples, siteID) => {
    const distinct = dashboardPersistableRealtimeSamples(samples).filter(point => {
      const aggregate = byTimestamp.get(point.timestamp_ms);
      const contribution = aggregate?.site_contributions?.[String(siteID)];
      if (!contribution) return true;
      const equivalent = { ...contribution, timestamp_ms: point.timestamp_ms };
      if (aggregate.realtime_revision && !equivalent.realtime_revision) equivalent.realtime_revision = aggregate.realtime_revision;
      const keys = new Set([...Object.keys(point), ...Object.keys(equivalent)]);
      return [...keys].some(key => point[key] !== equivalent[key]);
    });
    if (distinct.length) sites[String(siteID)] = distinct;
  });
  return {
    version: 2,
    saved_at_ms: Date.now(),
    billing_mode: dashboardBillingMode || dashboardRealtimePersistedBillingMode || null,
    realtime_epoch: dashboardRealtimeEpoch || undefined,
    realtime_revision: dashboardRealtimeRevision || undefined,
    all, sites,
  };
}

function flushDashboardRealtimeSamples() {
  if (dashboardRealtimePersistTimer && typeof clearTimeout === 'function') clearTimeout(dashboardRealtimePersistTimer);
  dashboardRealtimePersistTimer = 0;
  if (!dashboardRealtimePersistDirty || Date.now() < dashboardRealtimePersistRetryAt) return false;
  const storage = dashboardRealtimeStorage();
  if (!storage) return false;
  try {
    const payload = dashboardRealtimeCompactStoragePayload();
    let encoded = JSON.stringify(payload);
    // This is a recovery enhancement; the Controller owns the full window.
    // Keep the newest tail within a conservative UTF-16 storage byte budget.
    while (encoded.length > dashboardRealtimePersistLimit) {
      let removed = false;
      if (payload.all.length > 1) {
        payload.all.splice(0, Math.ceil(payload.all.length / 2));
        removed = true;
      }
      for (const siteID of Object.keys(payload.sites)) {
        const samples = payload.sites[siteID];
        if (samples.length > 1) {
          samples.splice(0, Math.ceil(samples.length / 2));
          removed = true;
        }
      }
      if (!removed) {
        // An extreme one-point/site map can itself exceed the cap. Retain
        // the aggregate numeric baseline and let server recovery fill sites.
        payload.sites = {};
        payload.all = payload.all.map(({ site_contributions, ...point }) => point);
      }
      encoded = JSON.stringify(payload);
      if (!removed && encoded.length > dashboardRealtimePersistLimit) return false;
    }
    storage.setItem(dashboardRealtimeStorageKey, encoded);
    dashboardRealtimePersistDirty = false;
    dashboardRealtimePersistFailures = 0;
    dashboardRealtimePersistRetryAt = 0;
    return true;
  } catch (_) {
    dashboardRealtimePersistFailures += 1;
    dashboardRealtimePersistRetryAt = Date.now() + Math.min(300000, 60000 * 2 ** Math.min(3, dashboardRealtimePersistFailures - 1));
    dashboardRealtimePersistLimit = Math.max(64 * 1024, Math.floor(dashboardRealtimePersistLimit / 2));
    return false;
  }
}

function clearDashboardRealtimeSamplesStorage() {
  if (dashboardRealtimePersistTimer && typeof clearTimeout === 'function') clearTimeout(dashboardRealtimePersistTimer);
  dashboardRealtimePersistTimer = 0;
  dashboardRealtimePersistDirty = false;
  const storage = dashboardRealtimeStorage();
  if (!storage) return;
  try {
    storage.removeItem(dashboardRealtimeStorageKey);
  } catch (_) {
    // Ignore storage policy failures; the in-memory sequence is still reset.
  }
}

function restoreDashboardRealtimeSamples(now = Date.now()) {
  const storage = dashboardRealtimeStorage();
  if (!storage) return false;
  let payload;
  try {
    const raw = storage.getItem(dashboardRealtimeStorageKey);
    if (!raw || raw.length > 5 * dashboardRealtimeStorageMaxChars) return false;
    payload = JSON.parse(raw);
  } catch (_) {
    return false;
  }
  const version = Number(payload?.version || 1);
  if (!payload || typeof payload !== 'object' || (version !== 1 && version !== 2)) return false;
  const all = dashboardNormalizeRealtimeSamples(payload.all, now);
  const sites = new Map();
  if (version === 2) {
    for (const point of all) {
      for (const [siteID, contribution] of Object.entries(point.site_contributions || {})) {
        const samples = sites.get(siteID) || [];
        upsertDashboardRealtimeSample(samples, dashboardNormalizeRealtimePoint({ ...contribution, timestamp_ms: point.timestamp_ms, realtime_revision: contribution.realtime_revision || point.realtime_revision }));
        sites.set(siteID, samples);
      }
    }
  }
  if (payload.sites && typeof payload.sites === 'object' && !Array.isArray(payload.sites)) {
    Object.keys(payload.sites).forEach(siteID => {
      if (!/^\d+$/.test(siteID)) return;
      const samples = dashboardNormalizeRealtimeSamples(payload.sites[siteID], now);
      if (samples.length) {
        const merged = sites.get(siteID) || [];
        samples.forEach(point => upsertDashboardRealtimeSample(merged, point));
        sites.set(siteID, pruneDashboardRealtimeSamples(merged, now));
      }
    });
  }
  dashboardRealtimeTrendSamples = new Map();
  dashboardRealtimeTrendSiteSamples = sites;
  if (all.length) dashboardRealtimeTrendSamples.set('all', all);
  const billingMode = String(payload.billing_mode || '').toLowerCase();
  dashboardRealtimePersistedBillingMode = billingMode === 'outbound' || billingMode === 'bidirectional' ? billingMode : null;
  const metadata = dashboardRealtimeMetadata(payload);
  if (metadata) {
    dashboardRealtimeEpoch = metadata.epoch;
    dashboardRealtimeRevision = metadata.revision;
  }
  return all.length > 0 || sites.size > 0;
}

function dashboardRealtimeMetadata(value) {
  const epoch = String(value?.realtime_epoch || '').trim();
  const revision = Number(value?.realtime_revision || 0);
  if (!epoch || epoch.length > 128 || !Number.isSafeInteger(revision) || revision <= 0) return null;
  const base = Number(value?.realtime_base_revision || 0);
  return { epoch, revision, base: Number.isSafeInteger(base) && base >= 0 ? base : 0 };
}

function dashboardResetRealtimeEpoch(epoch) {
  if (dashboardRealtimeRecoveryAbortController) {
    dashboardRealtimeRecoveryAbortController.abort();
    dashboardRealtimeRecoveryAbortController = null;
  }
  dashboardRealtimeLastRecoveryMS = 0;
  if (dashboardRealtimeEpoch && dashboardRealtimeEpoch !== epoch) {
    dashboardRealtimeRetiredEpochs.add(dashboardRealtimeEpoch);
    while (dashboardRealtimeRetiredEpochs.size > 64) {
      dashboardRealtimeRetiredEpochs.delete(dashboardRealtimeRetiredEpochs.values().next().value);
    }
  }
  dashboardRealtimeEpoch = epoch;
  dashboardRealtimeRevision = 0;
  dashboardRealtimeSiteRevisions = new Map();
  dashboardRealtimeNeedsRecovery = false;
  dashboardRealtimeTrendSamples = new Map();
  dashboardRealtimeTrendSiteSamples = new Map();
  dashboardRealtimeSmoothingCache = new Map();
  dashboardSpeedSamples = new Map();
  dashboardLiveSpeeds = new Map();
  dashboardLatestRatesBySite = new Map();
  dashboardSites = dashboardSites.map(({ _liveSpeed, ...site }) => site);
  dashboardLastSnapshotMS = 0;
  if (dashboardTrendData) {
    dashboardTrendData = { ...dashboardTrendData, points: [], site_series: [], live_baselines: {} };
  }
  clearDashboardRealtimeSamplesStorage();
}

function dashboardAcceptRealtimeEpoch(metadata, requestedEpoch) {
  if (dashboardRealtimeRetiredEpochs.has(metadata.epoch)) return false;
  if (metadata.epoch !== dashboardRealtimeEpoch) {
    if (requestedEpoch !== undefined && requestedEpoch !== dashboardRealtimeEpoch) return false;
    dashboardResetRealtimeEpoch(metadata.epoch);
  }
  dashboardRealtimeProtocolObserved = true;
  return true;
}

function dashboardMergeServerRealtimePoints(data, requestedEpoch, requestedKey) {
  if (!data || !Object.prototype.hasOwnProperty.call(data, 'realtime_points')) return true;
  const metadata = dashboardRealtimeMetadata(data);
  const incomingBillingMode = String(data.billing_mode || '').toLowerCase();
  const epochChanged = metadata && metadata.epoch !== dashboardRealtimeEpoch;
  if (metadata && !epochChanged && dashboardBillingMode &&
      (incomingBillingMode === 'outbound' || incomingBillingMode === 'bidirectional') &&
      incomingBillingMode !== dashboardBillingMode) return false;
  if (metadata && !dashboardAcceptRealtimeEpoch(metadata, requestedEpoch)) return false;
  if (!metadata && dashboardRealtimeProtocolObserved) return false;
  if (metadata && (incomingBillingMode === 'outbound' || incomingBillingMode === 'bidirectional')) {
    dashboardBillingMode = incomingBillingMode;
    dashboardRealtimePersistedBillingMode = incomingBillingMode;
  }
  dashboardControllerRealtimeEnabled = true;
  const key = String(requestedKey || data.site_id || (dashboardTrendState.siteId === 'all' ? 'all' : dashboardTrendState.siteId));
  const serverValues = Array.isArray(data.realtime_points) ? data.realtime_points : [];
  const normalized = dashboardNormalizeRealtimeSamples(
    metadata ? serverValues.map(point => ({ ...point, realtime_revision: metadata.revision })) : serverValues,
    Date.now(),
  );
  const mergeAuthoritative = (existing, serverPoints) => {
    if (metadata) {
      const merged = serverPoints.slice();
      (existing || []).forEach(point => {
        if (Number(point.realtime_revision || 0) > metadata.revision) upsertDashboardRealtimeSample(merged, point);
      });
      pruneDashboardRealtimeSamples(merged);
      return merged;
    }
    if (!serverPoints.length) return [];
    const latestServer = Number(serverPoints[serverPoints.length - 1]?.timestamp_ms || 0);
    // Keep only points that could have arrived through the in-flight SSE
    // stream after this HTTP response. Older persisted points and points from
    // a previous Controller process must never outrank the server window.
    const tailLimit = latestServer + (dashboardRealtimeSampleIntervalMS * 3);
    const tail = (existing || []).filter(point => {
      const timestamp = Number(point?.timestamp_ms || 0);
      return timestamp > latestServer && timestamp <= tailLimit;
    });
    const merged = serverPoints.slice();
    tail.forEach(point => upsertDashboardRealtimeSample(merged, point));
    pruneDashboardRealtimeSamples(merged);
    return merged;
  };
  const existing = dashboardRealtimeTrendSamples.get(key) || [];
  const merged = mergeAuthoritative(existing, normalized);
  if (merged.length) dashboardRealtimeTrendSamples.set(key, merged);
  else dashboardRealtimeTrendSamples.delete(key);

  if (key === 'all') {
    // The aggregate server point carries each site's contribution. Rebuild
    // the per-site view from the same authoritative window so selecting a
    // site in another browser produces the same line.
    const existingSites = dashboardRealtimeTrendSiteSamples;
    dashboardRealtimeTrendSiteSamples = new Map();
    const serverSites = new Map();
    merged.forEach(point => {
      if (!point.site_contributions || typeof point.site_contributions !== 'object') return;
      Object.keys(point.site_contributions).forEach(siteID => {
        const contribution = dashboardNormalizeRealtimePoint({ ...point.site_contributions[siteID], timestamp_ms: point.timestamp_ms, realtime_revision: point.realtime_revision });
        if (!contribution) return;
        const samples = serverSites.get(siteID) || [];
        upsertDashboardRealtimeSample(samples, contribution);
        serverSites.set(siteID, samples);
      });
    });
    serverSites.forEach((serverPoints, siteID) => {
      const mergedSite = mergeAuthoritative(existingSites.get(siteID) || [], serverPoints);
      if (mergedSite.length) {
        dashboardRealtimeTrendSiteSamples.set(siteID, mergedSite);
        dashboardRealtimeTrendSamples.set(siteID, mergedSite);
      }
    });
  } else {
    dashboardRealtimeTrendSiteSamples.set(key, merged);
  }
  if (metadata) {
    if (key === 'all') {
      if (metadata.revision >= dashboardRealtimeRevision) dashboardRealtimeNeedsRecovery = false;
      dashboardRealtimeRevision = Math.max(dashboardRealtimeRevision, metadata.revision);
      dashboardRealtimeTrendSiteSamples.forEach((_, siteID) => {
        dashboardRealtimeSiteRevisions.set(siteID, Math.max(dashboardRealtimeSiteRevisions.get(siteID) || 0, metadata.revision));
      });
    } else {
      // A selected-site HTTP window is only authoritative for that site.
      // Advancing the aggregate watermark here would discard an SSE packet
      // at the same revision before its other-site corrections were applied.
      dashboardRealtimeSiteRevisions.set(key, Math.max(dashboardRealtimeSiteRevisions.get(key) || 0, metadata.revision));
    }
  }
  persistDashboardRealtimeSamples();
  return true;
}

function dashboardAppendServerRealtimeTrendSample(value, persist = true) {
  const point = dashboardNormalizeRealtimePoint(value);
  if (!point) return false;
  const all = dashboardRealtimeTrendSamples.get('all') || [];
  upsertDashboardRealtimeSample(all, point);
  pruneDashboardRealtimeSamples(all);
  dashboardRealtimeTrendSamples.set('all', all);
  if (point.site_contributions && typeof point.site_contributions === 'object') {
    Object.keys(point.site_contributions).forEach(siteID => {
      const contribution = dashboardNormalizeRealtimePoint({ ...point.site_contributions[siteID], timestamp_ms: point.timestamp_ms, realtime_revision: point.realtime_revision });
      if (!contribution) return;
      const samples = dashboardRealtimeTrendSiteSamples.get(siteID) || [];
      upsertDashboardRealtimeSample(samples, contribution);
      pruneDashboardRealtimeSamples(samples);
      dashboardRealtimeTrendSiteSamples.set(siteID, samples);
      dashboardRealtimeTrendSamples.set(siteID, samples);
    });
  }
  // When a site is selected, its chart reads the site-keyed map. Keep that
  // view on the same SSE tick so it remains live between 15-second trend
  // endpoint refreshes.
  if (dashboardTrendState.siteId !== 'all' && point.site_contributions && typeof point.site_contributions === 'object') {
    const siteID = String(dashboardTrendState.siteId);
    const contribution = point.site_contributions[siteID];
    if (contribution) {
      const selected = dashboardRealtimeTrendSamples.get(siteID) || [];
      upsertDashboardRealtimeSample(selected, dashboardNormalizeRealtimePoint({ ...contribution, timestamp_ms: point.timestamp_ms }));
      pruneDashboardRealtimeSamples(selected);
      dashboardRealtimeTrendSamples.set(siteID, selected);
    }
  }
  if (persist) persistDashboardRealtimeSamples();
  return true;
}

function dashboardApplyRealtimePacket(stats, metadata) {
  if (metadata.revision <= dashboardRealtimeRevision) {
    if (dashboardRealtimeNeedsRecovery) dashboardRecoverRealtimeWindow();
    return;
  }
  const previousRevision = dashboardRealtimeRevision;
  const updates = Array.isArray(stats.realtime_updates) ? stats.realtime_updates : [];
  updates.forEach(point => dashboardAppendServerRealtimeTrendSample({ ...point, realtime_revision: metadata.revision }, false));
  if (stats.realtime_trend) {
    dashboardAppendServerRealtimeTrendSample({ ...stats.realtime_trend, realtime_revision: metadata.revision }, false);
  }
  updates.forEach(point => {
    Object.keys(point.site_contributions || {}).forEach(siteID => {
      dashboardRealtimeSiteRevisions.set(siteID, Math.max(dashboardRealtimeSiteRevisions.get(siteID) || 0, metadata.revision));
    });
  });
  dashboardRealtimeRevision = metadata.revision;
  if (metadata.base !== previousRevision) dashboardRealtimeNeedsRecovery = true;
  persistDashboardRealtimeSamples();
  if (dashboardRealtimeNeedsRecovery) dashboardRecoverRealtimeWindow();
}

async function dashboardRecoverRealtimeWindow() {
  if (!dashboardRealtimeNeedsRecovery || dashboardRealtimeRecoveryAbortController || Router.current !== 'dashboard' || dashboardTrendState.range !== 'realtime') return;
  const now = Date.now();
  if (dashboardRealtimeLastRecoveryMS > 0 && now - dashboardRealtimeLastRecoveryMS < 5000) return;
  dashboardRealtimeLastRecoveryMS = now;
  const controller = dashboardCreateAbortController();
  dashboardRealtimeRecoveryAbortController = controller;
  const epoch = dashboardRealtimeEpoch;
  try {
    // Recover the aggregate window so every site's cached view receives the
    // missing revision, even while the selector is showing just one site.
    const data = await API.dashboardTrends('all', 'realtime', '', '', controller.signal);
    if (controller.signal.aborted || Router.current !== 'dashboard' || dashboardRealtimeEpoch !== epoch) return;
    if (dashboardMergeServerRealtimePoints(data, epoch, 'all')) updateDashboardTrendRealtime();
  } catch (error) {
    if (error?.name !== 'AbortError') console.warn('Dashboard realtime recovery failed', error);
  } finally {
    if (dashboardRealtimeRecoveryAbortController === controller) dashboardRealtimeRecoveryAbortController = null;
  }
}

function upsertDashboardRealtimeSample(samples, point) {
  if (!Array.isArray(samples) || !point) return samples;
  const timestamp = Number(point.timestamp_ms || 0);
  if (!Number.isFinite(timestamp) || timestamp <= 0) return samples;
  let low = 0;
  let high = samples.length - 1;
  while (low <= high) {
    const middle = (low + high) >> 1;
    const candidate = Number(samples[middle]?.timestamp_ms || 0);
    if (candidate === timestamp) {
      if (Number(point.realtime_revision || 0) > 0 &&
          Number(samples[middle].realtime_revision || 0) > Number(point.realtime_revision)) return samples;
      samples[middle] = point;
      return samples;
    }
    if (candidate < timestamp) low = middle + 1;
    else high = middle - 1;
  }
  samples.splice(low, 0, point);
  return samples;
}

function dashboardRealtimeTrendPoints() {
  const key = dashboardTrendState.siteId === 'all' ? 'all' : String(dashboardTrendState.siteId);
  const points = dashboardRealtimeTrendSamples.get(key) || [];
  const baseline = dashboardRealtimeBaselineForKey(key);
  const anchor = dashboardRealtimeHistoryCutoffMS(baseline);
  const newer = !Number.isFinite(anchor) || anchor <= 0
    ? points
    : points.filter(point => Number(point?.timestamp_ms || 0) > anchor);
  if (!baseline || !newer.length || !Number.isFinite(Number(baseline.sampled_at_ms)) || Number(baseline.sampled_at_ms) <= 0) return newer;
  let previous = {
    bytesIn: Math.max(0, Number(baseline.bytes_in || 0)),
    bytesOut: Math.max(0, Number(baseline.bytes_out || 0)),
    requests: Math.max(0, Number(baseline.requests || 0)),
    timestamp: Number(baseline.sampled_at_ms),
  };
  return newer.map(point => {
    const hasCumulative = point.cumulative_bytes_in !== undefined || point.cumulative_bytes_out !== undefined || point.cumulative_requests !== undefined;
    if (!hasCumulative) return point;
    const current = {
      bytesIn: Math.max(0, Number(point.cumulative_bytes_in || 0)),
      bytesOut: Math.max(0, Number(point.cumulative_bytes_out || 0)),
      requests: Math.max(0, Number(point.cumulative_requests || 0)),
      timestamp: Number(point.timestamp_ms || previous.timestamp),
    };
    const deltaIn = current.bytesIn >= previous.bytesIn ? current.bytesIn - previous.bytesIn : current.bytesIn;
    const deltaOut = current.bytesOut >= previous.bytesOut ? current.bytesOut - previous.bytesOut : current.bytesOut;
    const deltaRequests = current.requests >= previous.requests ? current.requests - previous.requests : current.requests;
    const seconds = Math.max(0, (current.timestamp - previous.timestamp) / 1000);
    const next = {
      ...point,
      bytes_in: deltaIn,
      bytes_out: deltaOut,
      requests: deltaRequests,
      download_bps: seconds > 0 ? deltaOut / seconds : 0,
      upload_bps: seconds > 0 ? deltaIn / seconds : 0,
    };
    if (point.traffic_bytes !== undefined) {
      const mode = dashboardTrendData?.billing_mode;
      next.traffic_bytes = mode === 'outbound' ? deltaOut : 2 * (deltaIn + deltaOut);
    }
    previous = current;
    return next;
  });
}

function dashboardRealtimeBaselineForKey(key) {
  const raw = dashboardTrendData?.live_baselines;
  if (!raw || typeof raw !== 'object') return null;
  if (key !== 'all') return raw[key] || raw[String(key)] || null;
  const series = Array.isArray(dashboardTrendData?.site_series) ? dashboardTrendData.site_series : [];
  if (!series.length) return null;
  const aggregate = { bytes_in: 0, bytes_out: 0, requests: 0, sampled_at_ms: 0 };
  for (const site of series) {
    const baseline = raw[String(site.site_id)] || raw[site.site_id];
    if (!baseline || Number(baseline.sampled_at_ms || 0) <= 0) return null;
    aggregate.bytes_in += Math.max(0, Number(baseline.bytes_in || 0));
    aggregate.bytes_out += Math.max(0, Number(baseline.bytes_out || 0));
    aggregate.requests += Math.max(0, Number(baseline.requests || 0));
    const sampledAt = Number(baseline.sampled_at_ms);
    if (!aggregate.sampled_at_ms || sampledAt < aggregate.sampled_at_ms) aggregate.sampled_at_ms = sampledAt;
  }
  return aggregate.sampled_at_ms > 0 ? aggregate : null;
}

function dashboardRealtimeHistoryCutoffMS(baseline = null) {
  const persisted = Number(baseline?.sampled_at_ms || 0);
  if (Number.isFinite(persisted) && persisted > 0) return persisted;
  // Current responses include live_baselines even when a mixed local/Agent
  // selection cannot produce one complete watermark. In that case `as_of_ms`
  // is only a request-time fallback and must not erase browser-restored live
  // samples after a refresh. Older responses without live_baselines retain
  // the legacy as_of_ms cutoff for compatibility.
  if (dashboardTrendData?.live_baselines && typeof dashboardTrendData.live_baselines === 'object') return 0;
  const fallback = Number(dashboardTrendData?.as_of_ms || 0);
  return Number.isFinite(fallback) && fallback > 0 ? fallback : 0;
}

function dashboardTrendCutoffMS() {
  const key = dashboardTrendState.siteId === 'all' ? 'all' : String(dashboardTrendState.siteId);
  const baseline = dashboardRealtimeBaselineForKey(key);
  const value = dashboardRealtimeHistoryCutoffMS(baseline);
  return Number.isFinite(value) && value > 0 ? value : 0;
}

function dashboardRealtimeHistoricalPoints() {
  const historical = dashboardTrendData?.points || [];
  if (dashboardTrendState.range !== 'realtime') return historical;
  const { start, end } = dashboardRealtimeWindowBounds();
  const windowed = historical.filter(point => {
    const timestamp = Number(point?.timestamp_ms || 0);
    const anchor = dashboardTrendCutoffMS();
    return timestamp >= start && timestamp <= end && (!anchor || timestamp <= anchor);
  });
  const realtime = dashboardRealtimeTrendPoints();
  if (!realtime.length) return windowed;
  const anchor = dashboardTrendCutoffMS();
  if (Number.isFinite(anchor) && anchor > 0) return windowed;
  const firstRealtime = Number(realtime[0]?.timestamp_ms || 0);
  return windowed.filter(point => Number(point?.timestamp_ms || 0) < firstRealtime);
}

function dashboardTrendRealtimeOffset() {
  return dashboardRealtimeHistoricalPoints().length;
}

function dashboardTrendPoints() {
  if (dashboardTrendState.range === 'realtime' && dashboardRealtimeProtocolObserved) {
    return dashboardRealtimeChartTrendPoints();
  }
  const historicalPoints = dashboardRealtimeHistoricalPoints();
  if (dashboardTrendState.range !== 'realtime') return historicalPoints;
  const realtimePoints = dashboardRealtimeTrendPoints();
  if (!realtimePoints.length || !historicalPoints.length) {
    return realtimePoints.length ? realtimePoints : historicalPoints;
  }
  const offset = dashboardTrendRealtimeOffset();
  return historicalPoints.slice(0, offset).concat(realtimePoints);
}

function dashboardTrendChartPoints() {
  if (dashboardTrendState.range === 'realtime') {
    // Keep the complete five-minute client-side tail for drawing. The
    // history merge below intentionally returns only samples newer than the
    // persisted watermark, but dropping older live samples here made every
    // 15-second trend refresh (and every page reload) collapse the chart to a
    // nearly empty or all-zero tail.
    const realtimePoints = dashboardRealtimeChartTrendPoints();
    if (realtimePoints.length) return realtimePoints;
  }
  return dashboardTrendPoints();
}

function dashboardRealtimeChartTrendPoints() {
  const key = dashboardTrendState.siteId === 'all' ? 'all' : String(dashboardTrendState.siteId);
  const points = dashboardRealtimeTrendSamples.get(key) || [];
  if (!points.length) return [];
  if (dashboardRealtimeProtocolObserved) return points;
  const baseline = dashboardRealtimeBaselineForKey(key);
  const anchor = dashboardRealtimeHistoryCutoffMS(baseline);
  if (!baseline || !Number.isFinite(anchor) || anchor <= 0) return points;

  let previous = {
    bytesIn: Math.max(0, Number(baseline.bytes_in || 0)),
    bytesOut: Math.max(0, Number(baseline.bytes_out || 0)),
    requests: Math.max(0, Number(baseline.requests || 0)),
    timestamp: anchor,
  };
  return points.map(point => {
    const timestamp = Number(point?.timestamp_ms || 0);
    // Samples at or before the durable watermark already contain the deltas
    // captured during their original live interval. Keep them as-is so a
    // trend API refresh cannot erase the visible history.
    if (!Number.isFinite(timestamp) || timestamp <= anchor) return point;
    const hasCumulative = point.cumulative_bytes_in !== undefined
      || point.cumulative_bytes_out !== undefined
      || point.cumulative_requests !== undefined;
    if (!hasCumulative) return point;
    const current = {
      bytesIn: Math.max(0, Number(point.cumulative_bytes_in || 0)),
      bytesOut: Math.max(0, Number(point.cumulative_bytes_out || 0)),
      requests: Math.max(0, Number(point.cumulative_requests || 0)),
      timestamp,
    };
    const deltaIn = current.bytesIn >= previous.bytesIn ? current.bytesIn - previous.bytesIn : current.bytesIn;
    const deltaOut = current.bytesOut >= previous.bytesOut ? current.bytesOut - previous.bytesOut : current.bytesOut;
    const deltaRequests = current.requests >= previous.requests ? current.requests - previous.requests : current.requests;
    const seconds = Math.max(0, (current.timestamp - previous.timestamp) / 1000);
    const next = {
      ...point,
      bytes_in: deltaIn,
      bytes_out: deltaOut,
      requests: deltaRequests,
      download_bps: seconds > 0 ? deltaOut / seconds : 0,
      upload_bps: seconds > 0 ? deltaIn / seconds : 0,
    };
    if (point.traffic_bytes !== undefined) {
      const mode = dashboardTrendData?.billing_mode;
      next.traffic_bytes = mode === 'outbound' ? deltaOut : 2 * (deltaIn + deltaOut);
    }
    previous = current;
    return next;
  });
}

function dashboardRealtimeChartBounds(points) {
  // Keep the realtime x-axis anchored to the moving five-minute window. Do
  // not derive it from the first/last point: unchanged sites intentionally
  // still receive a sample every SSE tick, and sparse data must not stretch
  // the visible time range.
  return dashboardRealtimeWindowBounds();
}

function dashboardTrendSummary(data) {
  const points = dashboardTrendPoints();
  const requests = points.reduce((sum, point) => sum + Math.max(0, Number(point.requests || 0)), 0);
  const traffic = points.reduce((sum, point) => sum + Math.max(0, Number(point.traffic_bytes || 0)), 0);
  const request = document.getElementById('dashboard-requests-summary');
  const trafficEl = document.getElementById('dashboard-traffic-summary');
  const unit = document.getElementById('dashboard-traffic-unit');
  if (!points.length) dashboardTrendSpeedSummary([[], []]);
  if (request) request.textContent = `${formatNumber(requests)} 次`;
  if (trafficEl) trafficEl.textContent = formatBytes(traffic);
  if (unit) unit.textContent = data?.billing_mode === 'outbound' ? '单向计费流量总数' : '双向计费流量总数';
}

function dashboardUpdateTrendHelp(resetEnabled, telemetry) {
  const help = document.getElementById('dashboard-trend-help');
  if (!help) return;
  const prefix = resetEnabled
    ? '“本月”按自然月统计；已用流量按重置周期统计'
    : '“本月”按自然月统计；已用流量不自动重置';
  const agentSites = Number(telemetry?.agent_sites || 0);
  const staleSites = Number(telemetry?.stale_sites || 0);
  let telemetryLabel = '';
  if (agentSites > 0) {
    telemetryLabel = staleSites > 0
      ? ` · Agent 实时数据 ${staleSites}/${agentSites} 个站点暂不可用`
      : '';
  }
  help.textContent = `${prefix}${telemetryLabel} · 数据时间 `;
  const timezone = document.createElement('span');
  timezone.id = 'dashboard-trend-timezone';
  timezone.textContent = meridianTimezoneLabel();
  help.appendChild(timezone);
}

function dashboardRoundRect(ctx, x, y, width, height, radius) {
  if (typeof ctx.roundRect === 'function') {
    ctx.roundRect(x, y, width, height, radius);
    return;
  }
  const r = Math.min(radius, width / 2, height / 2);
  ctx.moveTo(x + r, y);
  ctx.lineTo(x + width - r, y);
  ctx.quadraticCurveTo(x + width, y, x + width, y + r);
  ctx.lineTo(x + width, y + height - r);
  ctx.quadraticCurveTo(x + width, y + height, x + width - r, y + height);
  ctx.lineTo(x + r, y + height);
  ctx.quadraticCurveTo(x, y + height, x, y + height - r);
  ctx.lineTo(x, y + r);
  ctx.quadraticCurveTo(x, y, x + r, y);
  ctx.closePath();
}

function dashboardTrendLineSegments(points) {
  if (!Array.isArray(points) || !points.length) return [];
  const segments = [];
  let current = [];
  points.forEach(point => {
    if (point && point.valid !== false && Number.isFinite(point.x) && Number.isFinite(point.y)) {
      current.push(point);
      return;
    }
    if (current.length) segments.push(current);
    current = [];
  });
  if (current.length) segments.push(current);
  return segments;
}

function dashboardTrendDisplayPointTarget(plotWidth) {
  const width = Math.max(1, Number(plotWidth) || 1);
  return Math.max(30, Math.min(50, Math.round(width / 10)));
}

function dashboardAggregateTrendValues(samples) {
  let downloadPeak = 0;
  let uploadPeak = 0;
  let bytesIn = 0;
  let bytesOut = 0;
  let requests = 0;
  let trafficBytes = 0;
  let availableSamples = 0;
  samples.forEach(sample => {
    const unavailable = sample?.speed_unavailable === true;
    const download = unavailable ? 0 : Math.max(0, Number(sample?.download_bps || 0));
    const upload = unavailable ? 0 : Math.max(0, Number(sample?.upload_bps || 0));
    downloadPeak = Math.max(downloadPeak, download);
    uploadPeak = Math.max(uploadPeak, upload);
    bytesIn += Math.max(0, Number(sample?.bytes_in || 0));
    bytesOut += Math.max(0, Number(sample?.bytes_out || 0));
    requests += Math.max(0, Number(sample?.requests || 0));
    trafficBytes += Math.max(0, Number(sample?.traffic_bytes || 0));
    if (!unavailable) availableSamples++;
  });
  return {
    download_bps: downloadPeak,
    upload_bps: uploadPeak,
    bytes_in: bytesIn,
    bytes_out: bytesOut,
    requests,
    traffic_bytes: trafficBytes,
    speed_unavailable: availableSamples === 0,
  };
}

function dashboardAggregateTrendBucket(samples, bucket = null) {
  const first = samples[0];
  const last = samples[samples.length - 1];
  const aggregate = {
    ...last,
    ...dashboardAggregateTrendValues(samples),
    timestamp_ms: Number(last?.timestamp_ms || 0),
    bucket_start_ms: Number(first?.timestamp_ms || 0),
    bucket_end_ms: Number(last?.timestamp_ms || 0),
    sample_count: samples.length,
  };
  const bucketID = Number(bucket?.id);
  const bucketStart = Number(bucket?.start);
  const bucketEnd = Number(bucket?.end);
  if (Number.isInteger(bucketID) && bucketStart >= 0 && bucketEnd > bucketStart) {
    aggregate.display_bucket_id = bucketID;
    aggregate.display_bucket_duration_ms = bucketEnd - bucketStart;
    aggregate.bucket_start_ms = bucketStart;
    aggregate.bucket_end_ms = bucket.closed ? bucketEnd : Number(last?.timestamp_ms || bucketEnd);
    aggregate.timestamp_ms = aggregate.bucket_end_ms;
  }
  const siteIDs = new Set();
  samples.forEach(point => {
    if (!point?.site_contributions || typeof point.site_contributions !== 'object') return;
    Object.keys(point.site_contributions).forEach(siteID => siteIDs.add(siteID));
  });
  if (siteIDs.size) {
    aggregate.site_contributions = {};
    siteIDs.forEach(siteID => {
      const siteSamples = samples.map(point => point?.site_contributions?.[siteID] || {
        download_bps: 0,
        upload_bps: 0,
        bytes_in: 0,
        bytes_out: 0,
        requests: 0,
        traffic_bytes: 0,
        speed_unavailable: true,
      });
      aggregate.site_contributions[siteID] = {
        ...dashboardAggregateTrendValues(siteSamples),
        timestamp_ms: aggregate.timestamp_ms,
        bucket_start_ms: aggregate.bucket_start_ms,
        bucket_end_ms: aggregate.bucket_end_ms,
        sample_count: samples.length,
      };
    });
  }
  return aggregate;
}

function dashboardTrendBucketDurationMS(target) {
  return Math.max(
    dashboardRealtimeSampleIntervalMS,
    Math.ceil(dashboardRealtimeWindowDurationMS / Math.max(1, Number(target) - 1)),
  );
}

function dashboardTrendDisplayPoints(points, metric, plotWidth) {
  if (!Array.isArray(points) || dashboardTrendState.range !== 'realtime') return points || [];
  const target = dashboardTrendDisplayPointTarget(plotWidth);
  if (points.length <= target) {
    return points.map(point => {
      const timestamp = Number(point?.timestamp_ms || 0);
      if (!Number.isFinite(timestamp) || timestamp < 0) return point;
      return {
        ...point,
        display_bucket_id: timestamp,
        display_bucket_duration_ms: dashboardRealtimeSampleIntervalMS,
      };
    });
  }
  const bucketDuration = dashboardTrendBucketDurationMS(target);
  const groups = new Map();
  points.forEach(point => {
    const timestamp = Number(point?.timestamp_ms || 0);
    if (!Number.isFinite(timestamp) || timestamp < 0) return;
    const bucketID = Math.floor(timestamp / bucketDuration);
    if (!groups.has(bucketID)) groups.set(bucketID, { samples: [] });
    const group = groups.get(bucketID);
    group.samples.push(point);
  });
  if (!groups.size) return points;
  const bucketIDs = Array.from(groups.keys()).sort((a, b) => a - b);
  const latestBucketID = bucketIDs[bucketIDs.length - 1];
  const result = [];
  bucketIDs.forEach(bucketID => {
    const group = groups.get(bucketID);
    const start = bucketID * bucketDuration;
    result.push(dashboardAggregateTrendBucket(group.samples, {
      id: bucketID,
      start,
      end: start + bucketDuration,
      closed: bucketID < latestBucketID,
    }));
  });
  return result;
}

function dashboardTrendMonotoneControls(points) {
  if (!Array.isArray(points) || points.length < 2) return [];
  const secants = [];
  for (let index = 0; index < points.length - 1; index++) {
    const width = points[index + 1].x - points[index].x;
    secants.push(width > 0 ? (points[index + 1].y - points[index].y) / width : 0);
  }
  const tangents = points.map((_, index) => {
    if (index === 0) return secants[0];
    if (index === points.length - 1) return secants[secants.length - 1];
    return secants[index - 1] * secants[index] <= 0 ? 0 : (secants[index - 1] + secants[index]) / 2;
  });
  secants.forEach((slope, index) => {
    if (slope === 0) {
      tangents[index] = 0;
      tangents[index + 1] = 0;
      return;
    }
    const left = tangents[index] / slope;
    const right = tangents[index + 1] / slope;
    const magnitude = Math.hypot(left, right);
    if (magnitude > 3) {
      const scale = 3 / magnitude;
      tangents[index] = scale * left * slope;
      tangents[index + 1] = scale * right * slope;
    }
  });
  return secants.map((_, index) => {
    const first = points[index];
    const last = points[index + 1];
    const width = last.x - first.x;
    return {
      cp1x: first.x + width / 3,
      cp1y: first.y + tangents[index] * width / 3,
      cp2x: last.x - width / 3,
      cp2y: last.y - tangents[index + 1] * width / 3,
      x: last.x,
      y: last.y,
    };
  });
}

function dashboardTrendTracePath(ctx, points, monotone) {
  if (!Array.isArray(points) || !points.length) return;
  ctx.moveTo(points[0].x, points[0].y);
  if (!monotone || points.length < 3 || typeof ctx.bezierCurveTo !== 'function') {
    points.slice(1).forEach(point => ctx.lineTo(point.x, point.y));
    return;
  }
  dashboardTrendMonotoneControls(points).forEach(control => {
    ctx.bezierCurveTo(control.cp1x, control.cp1y, control.cp2x, control.cp2y, control.x, control.y);
  });
}

// Realtime proxy traffic is sampled in two-second buckets. Video clients fill
// their buffers in bursts, so plotting every raw zero between two chunks makes
// an otherwise healthy stream look disconnected. Smooth only the visual
// series. Speed keeps every positive peak and uses the filter only as a
// decaying floor; persisted counters and request deltas stay untouched.
function dashboardRealtimeSmoothingKey(points, metric, seriesIndex) {
  const bucketDuration = Number(points?.[0]?.display_bucket_duration_ms || 0);
  if (!(bucketDuration > 0) || !points.every(point => Number.isInteger(point?.display_bucket_id))) return '';
  const siteID = dashboardTrendState.siteId === 'all' ? 'all' : String(dashboardTrendState.siteId);
  const billingMode = metric === 'traffic' ? String(dashboardBillingMode || dashboardTrendData?.billing_mode || '') : '';
  return `${siteID}|${metric}|${seriesIndex}|${bucketDuration}|${billingMode}`;
}

function dashboardRealtimeSmoothingState(key) {
  if (!key) return null;
  const existing = dashboardRealtimeSmoothingCache.get(key);
  if (existing) {
    dashboardRealtimeSmoothingCache.delete(key);
    dashboardRealtimeSmoothingCache.set(key, existing);
    return existing;
  }
  const state = new Map();
  dashboardRealtimeSmoothingCache.set(key, state);
  while (dashboardRealtimeSmoothingCache.size > 64) {
    dashboardRealtimeSmoothingCache.delete(dashboardRealtimeSmoothingCache.keys().next().value);
  }
  return state;
}

function dashboardRealtimeSmoothedValues(points, values, alpha = 0.35, cacheKey = '', preservePeaks = false) {
  if (!Array.isArray(values) || !values.length) return [];
  const cache = dashboardRealtimeSmoothingState(cacheKey);
  let smoothed = null;
  let previousTimestamp = 0;
  const rendered = values.map((value, index) => {
    const numeric = Number(value);
    const current = Number.isFinite(numeric) && numeric >= 0 ? numeric : 0;
    const timestamp = Number(points?.[index]?.timestamp_ms || 0);
    const bucketID = Number(points?.[index]?.display_bucket_id);
    const cached = cache && Number.isInteger(bucketID) ? cache.get(bucketID) : null;
    if (cached && cached.raw === current) {
      smoothed = cached.smoothed;
    } else if (smoothed === null) {
      smoothed = current;
    } else {
      const elapsed = timestamp > previousTimestamp && previousTimestamp > 0
        ? timestamp - previousTimestamp
        : 2000;
      // Make the filter stable when a delayed sample spans several nominal
      // intervals. The cap lets a long outage decay quickly without snapping.
      const intervals = Math.max(0.5, Math.min(4, elapsed / 2000));
      const effectiveAlpha = 1 - Math.pow(1 - alpha, intervals);
      smoothed += effectiveAlpha * (current - smoothed);
      if (current === 0 && smoothed < 0.5) smoothed = 0;
    }
    if (cache && Number.isInteger(bucketID)) cache.set(bucketID, { raw: current, smoothed });
    if (timestamp > 0) previousTimestamp = timestamp;
    return preservePeaks ? Math.max(current, smoothed) : smoothed;
  });
  if (cache) {
    const bucketIDs = new Set(points.map(point => Number(point?.display_bucket_id)).filter(Number.isInteger));
    Array.from(cache.keys()).forEach(bucketID => {
      if (!bucketIDs.has(bucketID)) cache.delete(bucketID);
    });
  }
  return rendered;
}

function dashboardTrendRenderSeries(points, metric) {
  const raw = metric === 'speed'
    ? [
        points.map(point => point.speed_unavailable === true ? null : Math.max(0, Number(point.download_bps || 0))),
        points.map(point => point.speed_unavailable === true ? null : Math.max(0, Number(point.upload_bps || 0))),
      ]
    : [points.map(point => dashboardTrendMetricValue(point, metric))];
  if (dashboardTrendState.range !== 'realtime' || metric === 'requests') return raw;
  return raw.map((values, index) => dashboardRealtimeSmoothedValues(
    points,
    values,
    0.35,
    dashboardRealtimeSmoothingKey(points, metric, index),
    metric === 'speed',
  ));
}

function dashboardTrendSpeedSummary(renderValues) {
  const peak = values => (Array.isArray(values) ? values : []).reduce(
    (max, value) => Number.isFinite(value) ? Math.max(max, value) : max,
    0,
  );
  const downloadPeak = peak(renderValues?.[0]);
  const uploadPeak = peak(renderValues?.[1]);
  const speed = document.getElementById('dashboard-speed-summary');
  if (speed) speed.textContent = `↓ ${formatRate(downloadPeak)} · ↑ ${formatRate(uploadPeak)}`;
}

function drawDashboardTrendChart(metric) {
  const chart = dashboardTrendCharts.get(metric);
  const rawPoints = dashboardTrendChartPoints();
  if (!chart || !chart.canvas || !chart.canvas.getContext || !rawPoints.length) return;
  const panel = document.getElementById(`console-chart-${metric}`);
  if (panel?.hidden) return;
  const canvas = chart.canvas;
  const wrap = canvas.parentElement;
  const width = Math.max(220, wrap.clientWidth || 320);
  const height = Math.max(190, wrap.clientHeight || 230);
  const ratio = window.devicePixelRatio || 1;
  canvas.width = Math.round(width * ratio); canvas.height = Math.round(height * ratio);
  canvas.style.width = `${width}px`; canvas.style.height = `${height}px`;
  const ctx = canvas.getContext('2d');
  const theme = typeof window.getComputedStyle === 'function' ? window.getComputedStyle(canvas) : null;
  const chartAccent = theme?.getPropertyValue('--console-accent').trim() || '#148267';
  const chartMuted = theme?.getPropertyValue('--console-muted').trim() || '#61716a';
  const chartLine = theme?.getPropertyValue('--console-line').trim() || '#e2e8e3';
  const chartSecondary = theme?.getPropertyValue('--console-chart-secondary').trim() || '#91aab6';
  const chartLabel = theme?.getPropertyValue('--console-chart-label').trim() || '#236953';
  ctx.setTransform(ratio, 0, 0, ratio, 0, 0);
  ctx.clearRect(0, 0, width, height);
  const preliminaryLeft = Math.min(74, Math.floor(width * .36));
  const points = dashboardTrendDisplayPoints(rawPoints, metric, Math.max(1, width - preliminaryLeft - 12));
  chart.displayPoints = points;
  if (chart.hoverIndex >= points.length) chart.hoverIndex = points.length - 1;
  const renderValues = dashboardTrendRenderSeries(points, metric);
  if (metric === 'speed') dashboardTrendSpeedSummary(renderValues);
  const series = metric === 'speed'
    ? [{ values: renderValues[0], color: chartAccent }, { values: renderValues[1], color: chartSecondary }]
    : [{ values: renderValues[0], color: chartAccent }];
  const scaleValues = series.flatMap(item => item.values).filter(value => Number.isFinite(value));
  const scale = dashboardRequestScale(Math.max(0, ...scaleValues), metric);
  ctx.font = `${width < 360 ? 10 : 11}px system-ui`;
  const yLabelWidth = Math.max(...Array.from({ length: scale.ticks + 1 }, (_, index) => ctx.measureText(dashboardTrendValueLabel(scale.max - scale.step * index, metric)).width));
  const left = Math.min(Math.max(50, Math.ceil(yLabelWidth) + 16), Math.floor(width * .36));
  const right = 12, top = 14, bottom = 30;
  const plotW = Math.max(1, width - left - right), plotH = Math.max(1, height - top - bottom);
  const realtimeBounds = dashboardTrendState.range === 'realtime'
    ? dashboardRealtimeChartBounds(points)
    : null;
  const chartStartMS = dashboardTrendState.range === 'realtime'
    ? realtimeBounds.start
    : Number(dashboardTrendData?.start_ms || points[0]?.timestamp_ms || 0);
  const chartEndMS = dashboardTrendState.range === 'realtime'
    ? realtimeBounds.end
    : Math.max(chartStartMS, Number(dashboardTrendData?.end_ms || points[points.length - 1]?.timestamp_ms || chartStartMS));
  const xForTimestamp = timestamp => chartEndMS > chartStartMS
    ? left + plotW * Math.max(0, Math.min(1, (Number(timestamp || chartStartMS) - chartStartMS) / (chartEndMS - chartStartMS)))
    : left + plotW / 2;
  chart.geometry = { width, height, left, right, top, bottom, plotW, plotH, chartStartMS, chartEndMS };
  ctx.textBaseline = 'middle';
  ctx.fillStyle = 'var(--white-60)';
  if (ctx.setLineDash) ctx.setLineDash([4, 4]);
  for (let i = 0; i <= scale.ticks; i++) {
    const value = scale.max - scale.step * i;
    const y = top + plotH * i / scale.ticks;
    ctx.strokeStyle = chartLine; ctx.lineWidth = 1;
    ctx.beginPath(); ctx.moveTo(left, y); ctx.lineTo(width - right, y); ctx.stroke();
    ctx.textAlign = 'right'; ctx.fillStyle = chartMuted;
    ctx.fillText(dashboardTrendValueLabel(value, metric), left - 7, y);
  }
  if (ctx.setLineDash) ctx.setLineDash([]);
  const canvasSeries = series.map(item => ({ ...item, points: item.values.map((value, index) => ({
    x: xForTimestamp(points[index]?.timestamp_ms),
    y: Number.isFinite(value) ? top + plotH * (1 - (value / (scale.max || 1))) : null,
    valid: Number.isFinite(value),
  })) }));
  canvasSeries.forEach(item => {
    const pointsOnCanvas = item.points;
    const monotone = metric !== 'requests';
    if (metric !== 'speed') {
      dashboardTrendLineSegments(pointsOnCanvas).forEach(segment => {
        ctx.beginPath();
        dashboardTrendTracePath(ctx, segment, monotone);
        ctx.lineTo(segment[segment.length - 1].x, top + plotH); ctx.lineTo(segment[0].x, top + plotH); ctx.closePath();
        ctx.globalAlpha = .12; ctx.fillStyle = item.color; ctx.fill(); ctx.globalAlpha = 1;
      });
    }
    ctx.strokeStyle = item.color; ctx.lineWidth = 2.5; ctx.lineJoin = 'round'; ctx.lineCap = 'round';
    dashboardTrendLineSegments(pointsOnCanvas).forEach(segment => {
      ctx.beginPath();
      dashboardTrendTracePath(ctx, segment, monotone);
      ctx.stroke();
    });
  });
  const pointsOnCanvas = canvasSeries[0].points;
  if (chart.hoverIndex >= 0 && pointsOnCanvas[chart.hoverIndex]?.valid !== false) {
    const point = pointsOnCanvas[chart.hoverIndex];
    ctx.beginPath(); ctx.arc(point.x, point.y, 4, 0, Math.PI * 2); ctx.fillStyle = '#fff'; ctx.fill(); ctx.strokeStyle = canvasSeries[0].color; ctx.lineWidth = 2; ctx.stroke();
  }
  ctx.fillStyle = chartMuted; ctx.textAlign = 'center'; ctx.textBaseline = 'alphabetic';
  dashboardTimeLabelIndexes(points.length, plotW, dashboardTrendState.range, points, chartStartMS, chartEndMS).forEach(index => {
    const labelTimestamp = dashboardTrendAxisLabel(index, points, dashboardTrendState.range, chartStartMS, chartEndMS);
    const label = dashboardTrendTimeLabel(labelTimestamp, dashboardTrendState.range);
    const labelWidth = typeof ctx.measureText === 'function' ? ctx.measureText(label).width : label.length * 7;
    const x = dashboardTrendState.range === 'realtime' && points.length > 1
      ? (index === 0 ? left + labelWidth / 2 : width - right - labelWidth / 2)
      : Math.max(left + labelWidth / 2, Math.min(width - right - labelWidth / 2, pointsOnCanvas[index].x));
    ctx.fillText(label, x, height - 7);
  });
  if (chart.hoverIndex >= 0 && pointsOnCanvas[chart.hoverIndex]) {
    const point = pointsOnCanvas[chart.hoverIndex];
    const crosshairX = Math.max(left, Math.min(left + plotW, Number.isFinite(Number(chart.hoverX)) ? Number(chart.hoverX) : point.x));
    const crosshairY = Math.max(top, Math.min(top + plotH, Number.isFinite(Number(chart.hoverY)) ? Number(chart.hoverY) : point.y));
    const crosshairColor = 'rgba(71, 85, 105, .62)';
    ctx.save();
    ctx.strokeStyle = crosshairColor;
    ctx.lineWidth = 1;
    if (ctx.setLineDash) ctx.setLineDash([5, 4]);
    ctx.beginPath(); ctx.moveTo(crosshairX, top); ctx.lineTo(crosshairX, top + plotH); ctx.stroke();
    ctx.beginPath(); ctx.moveTo(left, crosshairY); ctx.lineTo(width - right, crosshairY); ctx.stroke();
    if (ctx.setLineDash) ctx.setLineDash([]);
    ctx.font = `${width < 360 ? 11 : 12}px system-ui, sans-serif`;
    const yValue = scale.max * (1 - (crosshairY - top) / Math.max(1, plotH));
    const yLabel = dashboardTrendValueLabel(yValue, metric);
    const measureText = text => typeof ctx.measureText === 'function' ? ctx.measureText(text).width : String(text).length * 7;
    const yLabelWidth = Math.max(54, measureText(yLabel) + 18);
    const yLabelTop = Math.max(2, Math.min(height - 24, crosshairY - 12));
    ctx.fillStyle = chartLabel;
    ctx.beginPath();
    dashboardRoundRect(ctx, 2, yLabelTop, yLabelWidth, 24, 5);
    ctx.fill();
    ctx.fillStyle = '#fff'; ctx.textAlign = 'left'; ctx.textBaseline = 'middle';
    ctx.fillText(yLabel, 10, yLabelTop + 12);
    const xLabel = dashboardTrendTimeLabel(points[chart.hoverIndex].timestamp_ms, dashboardTrendState.range);
    const xLabelWidth = Math.max(48, measureText(xLabel) + 18);
    const xLabelLeft = Math.max(left, Math.min(width - right - xLabelWidth, crosshairX - xLabelWidth / 2));
    const xLabelTop = height - bottom + 1;
    ctx.fillStyle = chartLabel;
    ctx.beginPath();
    dashboardRoundRect(ctx, xLabelLeft, xLabelTop, xLabelWidth, 24, 5);
    ctx.fill();
    ctx.fillStyle = '#fff'; ctx.textAlign = 'center';
    ctx.fillText(xLabel, xLabelLeft + xLabelWidth / 2, xLabelTop + 12);
    ctx.restore();
  }
}

function renderDashboardTrendCharts() {
  ['speed', 'requests', 'traffic'].forEach(drawDashboardTrendChart);
}

function dashboardLocalDateTimeValue(date) {
  return meridianDateTimeLocalValue(date);
}

function dashboardDefaultCustomRange() {
  const end = Date.now();
  const start = end - 60 * 60 * 1000;
  return { start: dashboardLocalDateTimeValue(start), end: dashboardLocalDateTimeValue(end) };
}

function dashboardSetCustomRangeControls(visible) {
  const custom = document.getElementById('dashboard-trend-custom');
  const error = document.getElementById('dashboard-trend-custom-error');
  if (custom) custom.hidden = !visible;
  if (!visible && error) {
    error.hidden = true;
    error.textContent = '';
  }
}

function dashboardShowCustomError(message) {
  const error = document.getElementById('dashboard-trend-custom-error');
  if (!error) return;
  error.textContent = message || '';
  error.hidden = !message;
}

function dashboardReadCustomRange() {
  const start = document.getElementById('dashboard-trend-start')?.value || '';
  const end = document.getElementById('dashboard-trend-end')?.value || '';
  if (!start || !end) return { error: '请选择开始时间和结束时间' };
  const startDate = meridianParseDateTimeLocal(start);
  const endDate = meridianParseDateTimeLocal(end);
  if (!Number.isFinite(startDate) || !Number.isFinite(endDate)) {
    return { error: '时间格式无效，请重新选择' };
  }
  if (endDate <= startDate) return { error: '结束时间必须晚于开始时间' };
  return { start, end };
}

function setupDashboardTrendControls() {
  const siteSelect = document.getElementById('dashboard-trend-site');
  const rangeSelect = document.getElementById('dashboard-trend-range');
  if (!siteSelect || !rangeSelect) return;
  siteSelect.onchange = () => { dashboardTrendState.siteId = siteSelect.value; loadDashboardTrends(); };
  const startInput = document.getElementById('dashboard-trend-start');
  const endInput = document.getElementById('dashboard-trend-end');
  const applyButton = document.getElementById('dashboard-trend-apply');
  const customDefault = dashboardDefaultCustomRange();
  if (startInput && !startInput.value) startInput.value = dashboardTrendState.customStart || customDefault.start;
  if (endInput && !endInput.value) endInput.value = dashboardTrendState.customEnd || customDefault.end;
  rangeSelect.onchange = () => {
    dashboardTrendState.range = rangeSelect.value;
    dashboardSetCustomRangeControls(dashboardTrendState.range === 'custom');
    if (dashboardTrendState.range === 'custom') {
      const current = dashboardReadCustomRange();
      if (current.error) {
        dashboardShowCustomError(current.error);
        return;
      }
      dashboardTrendState.customStart = current.start;
      dashboardTrendState.customEnd = current.end;
    } else {
      dashboardShowCustomError('');
    }
    loadDashboardTrends();
  };
  if (applyButton) {
    applyButton.onclick = () => {
      const current = dashboardReadCustomRange();
      if (current.error) {
        dashboardShowCustomError(current.error);
        return;
      }
      dashboardTrendState.customStart = current.start;
      dashboardTrendState.customEnd = current.end;
      dashboardShowCustomError('');
      loadDashboardTrends();
    };
  }
  dashboardSetCustomRangeControls(dashboardTrendState.range === 'custom');
  ['speed', 'requests', 'traffic'].forEach(metric => {
    const canvas = document.getElementById(metric === 'speed' ? 'dashboardSpeedTrend' : metric === 'requests' ? 'dashboardRequestsTrend' : 'dashboardTrafficTrend');
    if (!canvas) return;
    const tooltip = canvas.parentElement.querySelector('.dashboard-chart-tooltip');
    const chart = { canvas, tooltip, hoverIndex: -1, hoverX: null, hoverY: null, pointerActive: false, geometry: null, displayPoints: [] };
    dashboardTrendCharts.set(metric, chart);
    const clearHover = () => {
      chart.hoverIndex = -1; chart.hoverX = null; chart.hoverY = null;
      if (tooltip) tooltip.hidden = true;
      drawDashboardTrendChart(metric);
    };
    const updateHover = event => {
      const points = chart.displayPoints?.length ? chart.displayPoints : dashboardTrendChartPoints();
      if (!points.length) return;
      const rect = canvas.getBoundingClientRect();
      // Touch pointer capture continues delivering pointermove events after
      // the finger leaves the canvas. Do not clamp those events to the edge:
      // hide the tooltip until the finger comes back into the chart.
      if (event.pointerType !== 'mouse' && !dashboardTrendPointerInside(rect, event)) {
        clearHover();
        return;
      }
      const geometry = chart.geometry || { width: rect.width, height: rect.height, left: 0, top: 0, plotW: rect.width, plotH: rect.height };
      const pointer = dashboardTrendPointerState(rect, geometry, event, points, geometry.chartStartMS, geometry.chartEndMS);
      chart.hoverIndex = pointer.index;
      chart.hoverX = pointer.x;
      chart.hoverY = pointer.y;
      if (tooltip) {
        tooltip.innerHTML = dashboardTrendTooltip(points[pointer.index], metric, dashboardTrendState.range, pointer.index);
        tooltip.hidden = false;
        const wrap = canvas.parentElement;
        const wrapRect = wrap?.getBoundingClientRect ? wrap.getBoundingClientRect() : rect;
        const wrapWidth = wrap?.clientWidth || wrapRect.width || geometry.width;
        const wrapHeight = wrap?.clientHeight || wrapRect.height || geometry.height;
        const tooltipPosition = dashboardTooltipPosition(
          event.clientX - (Number(wrapRect.left) || 0),
          event.clientY - (Number(wrapRect.top) || 0),
          wrapWidth,
          wrapHeight,
          tooltip.offsetWidth || 180,
          tooltip.offsetHeight || 56,
        );
        tooltip.style.left = `${tooltipPosition.left}px`;
        tooltip.style.top = `${tooltipPosition.top}px`;
      }
      drawDashboardTrendChart(metric);
    };
    canvas.addEventListener('pointerdown', event => {
      chart.pointerActive = true;
      if (canvas.setPointerCapture) canvas.setPointerCapture(event.pointerId);
      updateHover(event);
    });
    canvas.addEventListener('pointermove', event => {
      if (event.pointerType === 'mouse' || chart.pointerActive) updateHover(event);
    });
    canvas.addEventListener('pointerup', event => {
      if (event.pointerType !== 'mouse' && !dashboardTrendPointerInside(canvas.getBoundingClientRect(), event)) clearHover();
      chart.pointerActive = false;
      if (canvas.releasePointerCapture && canvas.hasPointerCapture?.(event.pointerId)) canvas.releasePointerCapture(event.pointerId);
    });
    canvas.addEventListener('pointercancel', () => { chart.pointerActive = false; clearHover(); });
    canvas.addEventListener('pointerleave', event => {
      if (event.pointerType !== 'mouse') return;
      clearHover();
    });
  });
}

async function loadDashboardTrends() {
  if (dashboardTrendAbortController) dashboardTrendAbortController.abort();
  dashboardTrendAbortController = dashboardCreateAbortController();
  const signal = dashboardTrendAbortController.signal;
  const loadError = document.getElementById('console-trend-load-error');
  const loading = document.getElementById('console-trend-loading');
  if (loadError) loadError.hidden = true;
  if (loading) loading.hidden = !!dashboardTrendData;
  const requestedEpoch = dashboardRealtimeEpoch;
  const requestedKey = String(dashboardTrendState.siteId);
  try {
    const data = await API.dashboardTrends(
      dashboardTrendState.siteId,
      dashboardTrendState.range,
      dashboardTrendState.customStart,
      dashboardTrendState.customEnd,
      signal,
    );
    if (signal.aborted || !data || Router.current !== 'dashboard') return;
    if (typeof meridianSetTimezoneOffset === 'function') meridianSetTimezoneOffset(data.timezone_offset_minutes);
    const timezone = document.getElementById('dashboard-trend-timezone');
    if (timezone && typeof meridianTimezoneLabel === 'function') timezone.textContent = meridianTimezoneLabel(data.timezone_offset_minutes);
    if (!dashboardTrendState.customStart && !dashboardTrendState.customEnd) {
      const customDefault = dashboardDefaultCustomRange();
      const startInput = document.getElementById('dashboard-trend-start');
      const endInput = document.getElementById('dashboard-trend-end');
      if (startInput) startInput.value = customDefault.start;
      if (endInput) endInput.value = customDefault.end;
    }
    if (!dashboardMergeServerRealtimePoints(data, requestedEpoch, requestedKey)) return;
    dashboardTrendData = data;
    for (const samples of dashboardRealtimeTrendSamples.values()) pruneDashboardRealtimeSamples(samples);
    for (const samples of dashboardRealtimeTrendSiteSamples.values()) pruneDashboardRealtimeSamples(samples);
    reconcileDashboardTrendSiteSelection();
    dashboardTrendSummary(data);
    renderDashboardTrendCharts();
  } catch (error) {
    if (error && error.name === 'AbortError') return;
    if (signal.aborted || Router.current !== 'dashboard') return;
    console.warn('Dashboard trends load error', error);
    if (loadError) loadError.hidden = false;
  } finally {
    if (dashboardTrendAbortController?.signal === signal) {
      dashboardTrendAbortController = null;
      if (loading) loading.hidden = true;
    }
  }
}

function renderDashboardTrendSites() {
  const select = document.getElementById('dashboard-trend-site');
  if (!select) return;
  select.innerHTML = '<option value="all">全部站点</option>' + dashboardSites.map(site => `<option value="${Number(site.id)}">${esc(site.name)}</option>`).join('');
  select.value = dashboardTrendState.siteId;
}

function updateDashboardTrendRealtime() {
  if (dashboardTrendState.range !== 'realtime' || !dashboardTrendData) return;
  dashboardTrendSummary(dashboardTrendData);
  renderDashboardTrendCharts();
}

function observeDashboardTrendResize() {
  if (dashboardTrendResizeObserver) {
    dashboardTrendResizeObserver.disconnect();
    dashboardTrendResizeObserver = null;
  }
  const wrap = document.querySelector('.dashboard-trend-wrap');
  if (!wrap) return;
  renderDashboardTrendSites();
  if (typeof ResizeObserver !== 'function') return;
  dashboardTrendResizeObserver = new ResizeObserver(() => {
    if (Router.current === 'dashboard') renderDashboardTrendCharts();
  });
  document.querySelectorAll('.dashboard-trend-wrap').forEach(element => dashboardTrendResizeObserver.observe(element));
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

    if (statusEl) { statusEl.style.color = 'var(--console-accent, var(--green))'; statusEl.textContent = '实时连接'; }

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
      if (statusEl) { statusEl.style.color = 'var(--red)'; statusEl.textContent = '连接中断'; }
      queueDashSSERetry();
    }
  } catch (e) {
    if (controller.signal.aborted || dashAbortController !== controller) return;
    console.warn('SSE connection lost, retrying in 5s...', e);
    if (statusEl) { statusEl.style.color = 'var(--red)'; statusEl.textContent = '正在重连'; }
    queueDashSSERetry();
  }
}

function updateDashboardLive(stats) {
  const metadata = dashboardRealtimeMetadata(stats);
  if (!metadata && dashboardRealtimeProtocolObserved) return;
  if (metadata) {
    if (!dashboardAcceptRealtimeEpoch(metadata) || metadata.revision < dashboardRealtimeRevision) return;
  } else if (dashboardRealtimeEpoch && !dashboardRealtimeProtocolObserved) {
    // A restored cache may come from a newer Controller than the currently
    // connected server. Missing metadata retains the legacy merge path.
    dashboardResetRealtimeEpoch('');
  }
	const generatedAt = Number(stats?.generated_at_ms || 0);
	if (generatedAt > 0 && generatedAt < dashboardLastSnapshotMS) return;
	if (generatedAt > 0) dashboardLastSnapshotMS = generatedAt;
	const incomingBillingMode = String(stats?.billing_mode || '').toLowerCase();
	if (incomingBillingMode === 'outbound' || incomingBillingMode === 'bidirectional') {
	  const previousBillingMode = dashboardBillingMode || dashboardRealtimePersistedBillingMode;
	  if (previousBillingMode && previousBillingMode !== incomingBillingMode) {
	    // Existing realtime traffic was calculated under the old policy. Drop
	    // only that derived sequence; counter baselines and live rates remain
	    // valid and will continue on the next sample.
	    dashboardRealtimeTrendSamples = new Map();
	    dashboardRealtimeTrendSiteSamples = new Map();
	    clearDashboardRealtimeSamplesStorage();
	  }
	  dashboardBillingMode = incomingBillingMode;
	  dashboardRealtimePersistedBillingMode = incomingBillingMode;
	  if (metadata && dashboardTrendData && dashboardTrendState.range === 'realtime') {
	    dashboardTrendData = { ...dashboardTrendData, billing_mode: incomingBillingMode };
	  }
	}
	const panelDomainEl = document.getElementById('s-panel-domain');
	const currentPanelURL = dashboardCurrentPanelURL(stats.panel_access_url);
	if (panelDomainEl && currentPanelURL) panelDomainEl.textContent = currentPanelURL;
	animateValue('s-total', stats.total_sites || 0);
	animateValue('s-running', stats.running_sites || 0);
	const totalSites = Number(stats?.total_sites);
	if (Number.isFinite(totalSites)) {
		if (dashboardLastObservedSiteCount >= 0 && dashboardLastObservedSiteCount !== totalSites && dashboardSitesInitialized && dashboardSites.length !== totalSites) {
			void refreshDashboardSecondary();
		}
		dashboardLastObservedSiteCount = totalSites;
	}

  const trafficEl = document.getElementById('s-traffic');
  if (trafficEl) trafficEl.textContent = formatBytes(stats.monthly_traffic != null ? stats.monthly_traffic : (stats.total_traffic || 0));
	const resetEnabled = Number(stats.traffic_reset_day == null ? 1 : stats.traffic_reset_day) !== 0;
	const trafficTitle = document.getElementById('s-traffic-title');
	if (trafficTitle) trafficTitle.textContent = '已用流量';
  const cycleLabel = document.getElementById('console-cycle-label');
  if (cycleLabel) cycleLabel.textContent = resetEnabled ? `每月 ${Number(stats.traffic_reset_day || 1)} 日重置` : '累计流量 · 不自动重置';
  dashboardUpdateTrendHelp(resetEnabled, stats?.realtime_telemetry);

  const uptimeEl = document.getElementById('s-uptime');
  if (uptimeEl) uptimeEl.textContent = formatUptime(stats.uptime_seconds || 0);

  dashboardUpdateMonthlyRequestSummary(stats);
  if (stats?.realtime_trend) dashboardControllerRealtimeEnabled = true;
	updateDashboardSiteSpeeds(stats.live_sites || [], generatedAt, dashboardBillingMode);
  dashboardUpdateConsoleRates();
  if (metadata) dashboardApplyRealtimePacket(stats, metadata);
  else if (stats?.realtime_trend) dashboardAppendServerRealtimeTrendSample(stats.realtime_trend);
  updateDashboardTrendRealtime();
}

function dashboardUpdateMonthlyRequestSummary(stats) {
  const requestsEl = document.getElementById('s-requests');
  if (!requestsEl) return;
  if (stats?.monthly_requests == null) {
    requestsEl.textContent = '—';
    return;
  }
  requestsEl.textContent = `${formatNumber(stats.monthly_requests)} 请求`;
}

function updateDashboardSiteSpeeds(liveSites, snapshotMS, billingModeOverride) {
  const snapshotValue = Number(snapshotMS || 0);
  const liveMap = new Map();
  const trendDeltas = new Map();
  const rateSamples = new Map();
  const changedSiteIDs = new Set();
  let totalDeltaIn = 0;
  let totalDeltaOut = 0;
  let totalRateIn = 0;
  let totalRateOut = 0;
  let totalDeltaRequests = 0;
  // Runtime samples must never guess the billing policy while bootstrap and
  // trends are still racing. Direct callers may provide an explicit legacy
  // fallback, but the normal SSE path passes dashboardBillingMode (which can
  // intentionally remain null until the authoritative snapshot arrives).
  const billingMode = billingModeOverride === undefined
    ? (dashboardBillingMode || dashboardTrendData?.billing_mode || null)
    : billingModeOverride;
  const billingKnown = billingMode === 'outbound' || billingMode === 'bidirectional';
  const freshnessWindowMS = 45 * 1000;
  for (const site of (liveSites || [])) {
    const siteID = Number(site.id);
    if (!Number.isFinite(siteID)) continue;
    liveMap.set(siteID, site);
    const sourceTimestamp = Number(site.sampled_at_ms || snapshotValue || Date.now());
    const running = site.running === undefined ? true : site.running === true;
    const fresh = running && sourceTimestamp > 0 && (snapshotValue <= 0 || (sourceTimestamp <= snapshotValue + 5000 && snapshotValue - sourceTimestamp <= freshnessWindowMS));
    const current = {
      trafficUsed: Number(site.monthly_traffic != null ? site.monthly_traffic : (site.traffic_used || 0)),
      bytesIn: Number(site.cumulative_bytes_in != null ? site.cumulative_bytes_in : (site.bytes_in || site.bytes_in_total || 0)),
      bytesOut: Number(site.cumulative_bytes_out != null ? site.cumulative_bytes_out : (site.bytes_out || site.bytes_out_total || 0)),
      requests: Number(site.requests || 0),
      timestamp: sourceTimestamp,
      running,
      fresh,
    };
    const previous = dashboardSpeedSamples.get(siteID);
    const gaugeReceivedAt = Number(site.agent_rate_received_at_ms || 0);
    const hasAgentGauge = gaugeReceivedAt > 0 || Number(site.agent_received_at_ms || 0) > 0;
    if (!previous) {
      dashboardLiveSpeeds.set(siteID, { down: 0, up: 0 });
      changedSiteIDs.add(siteID);
      rateSamples.set(String(siteID), { download_bps: 0, upload_bps: 0 });
      trendDeltas.set(siteID, { bytesIn: 0, bytesOut: 0, requests: 0 });
    } else if (!fresh) {
      // A stale/offline sample must immediately clear a previously displayed
      // rate, even when its timestamp did not advance. Keep the current
      // counters as the recovery baseline.
      if (previous.running || previous.fresh || (dashboardLiveSpeeds.get(siteID)?.down || 0) !== 0 || (dashboardLiveSpeeds.get(siteID)?.up || 0) !== 0) {
        changedSiteIDs.add(siteID);
        rateSamples.set(String(siteID), { download_bps: 0, upload_bps: 0 });
        trendDeltas.set(siteID, { bytesIn: 0, bytesOut: 0, requests: 0 });
      }
      dashboardLiveSpeeds.set(siteID, { down: 0, up: 0 });
    } else if (current.timestamp > previous.timestamp) {
      changedSiteIDs.add(siteID);
      // After an offline interval, establish a fresh baseline rather than
      // charging the whole outage as a single speed sample.
      const recovering = previous.running !== true || previous.fresh !== true;
      const seconds = (current.timestamp - previous.timestamp) / 1000;
      const down = current.bytesOut - previous.bytesOut;
      const up = current.bytesIn - previous.bytesIn;
      if (!recovering && seconds > 0 && down >= 0 && up >= 0) {
        const downRate = down / seconds;
        const upRate = up / seconds;
        dashboardLiveSpeeds.set(siteID, { down: downRate, up: upRate });
        rateSamples.set(String(siteID), { download_bps: downRate, upload_bps: upRate });
        const requests = Math.max(0, current.requests - previous.requests);
        trendDeltas.set(siteID, { bytesIn: up, bytesOut: down, requests });
        totalDeltaIn += up;
        totalDeltaOut += down;
        totalDeltaRequests += requests;
      } else {
        // A site process restart resets the cumulative runtime counters. Show
        // zero until the next monotonic pair instead of flashing a placeholder.
        dashboardLiveSpeeds.set(siteID, { down: 0, up: 0 });
        rateSamples.set(String(siteID), { download_bps: 0, upload_bps: 0 });
        trendDeltas.set(siteID, { bytesIn: 0, bytesOut: 0, requests: 0 });
      }
    } else if (current.timestamp === previous.timestamp) {
      // The Agent has not emitted a new sample. Keep its last valid rate; the
      // aggregate speed is recomputed from all such rates below.
      if (current.fresh && previous.fresh) {
        const existing = dashboardLiveSpeeds.get(siteID) || { down: 0, up: 0 };
        dashboardLiveSpeeds.set(siteID, existing);
      }
    } else {
      // A stale sample must never move the counter baseline backwards.
      if (!hasAgentGauge) continue;
    }
    if (hasAgentGauge) {
      // Counter and gauge snapshots can arrive independently. Use the Agent's
      // measured rate even on the first browser sample or when only its gauge
      // advanced, and never refresh it with a full-report counter timestamp.
      const gaugeFresh = site.agent_rate_valid === true && gaugeReceivedAt > 0 &&
        (snapshotValue <= 0 || (snapshotValue >= gaugeReceivedAt && snapshotValue - gaugeReceivedAt <= 6000));
      const measuredDown = Number(site.agent_download_bps || 0);
      const measuredUp = Number(site.agent_upload_bps || 0);
      const down = gaugeFresh && Number.isFinite(measuredDown) && measuredDown >= 0 ? measuredDown : 0;
      const up = gaugeFresh && Number.isFinite(measuredUp) && measuredUp >= 0 ? measuredUp : 0;
      dashboardLiveSpeeds.set(siteID, { down, up });
      changedSiteIDs.add(siteID);
      rateSamples.set(String(siteID), { download_bps: down, upload_bps: up });
    }
    // Keep the latest sample even when a later SSE payload omits another site.
    if (!previous || current.timestamp >= previous.timestamp) dashboardSpeedSamples.set(siteID, current);
    const speed = dashboardLiveSpeeds.get(siteID) || { down: 0, up: 0 };
    dashboardLatestRatesBySite.set(siteID, { down: speed.down || 0, up: speed.up || 0, sampledAt: current.timestamp, running: current.running, fresh: current.fresh });
  }
  // Sum the latest valid rate for every site, not only sites that emitted in
  // this SSE payload. Agents report asynchronously, so this is the only way
  // the all-sites card can remain an accurate aggregate.
  for (const [siteID, latest] of dashboardLatestRatesBySite) {
    const current = liveMap.get(siteID);
    if (current) {
      const timestamp = Number(current.sampled_at_ms || snapshotValue || latest.sampledAt || 0);
      latest.running = current.running === undefined ? latest.running : current.running === true;
      latest.fresh = latest.running && timestamp > 0 && (snapshotValue <= 0 || (timestamp <= snapshotValue + 5000 && snapshotValue - timestamp <= freshnessWindowMS));
      latest.sampledAt = timestamp || latest.sampledAt;
    } else if (snapshotValue > 0 && latest.sampledAt > 0 && snapshotValue - latest.sampledAt > freshnessWindowMS) {
      const wasActive = latest.running && latest.fresh;
      latest.running = false;
      latest.fresh = false;
      // A missing site sample is also an offline signal once it ages out of
      // the freshness window. Mark the stored counter baseline stale so a
      // later recovery establishes a new baseline instead of charging the
      // whole outage interval as one speed sample.
      const baseline = dashboardSpeedSamples.get(siteID);
      if (baseline) {
        baseline.running = false;
        baseline.fresh = false;
      }
      const existingSpeed = dashboardLiveSpeeds.get(siteID) || { down: 0, up: 0 };
      if (wasActive || existingSpeed.down !== 0 || existingSpeed.up !== 0) {
        dashboardLiveSpeeds.set(siteID, { down: 0, up: 0 });
        changedSiteIDs.add(siteID);
        rateSamples.set(String(siteID), { download_bps: 0, upload_bps: 0 });
        trendDeltas.set(siteID, { bytesIn: 0, bytesOut: 0, requests: 0 });
      }
    }
    if (latest.running && latest.fresh) {
      totalRateOut += Math.max(0, Number(latest.down || 0));
      totalRateIn += Math.max(0, Number(latest.up || 0));
    }
  }
  let totalCumulativeIn = 0;
  let totalCumulativeOut = 0;
  let totalCumulativeRequests = 0;
  dashboardSpeedSamples.forEach(sample => {
    totalCumulativeIn += Math.max(0, Number(sample?.bytesIn || 0));
    totalCumulativeOut += Math.max(0, Number(sample?.bytesOut || 0));
    totalCumulativeRequests += Math.max(0, Number(sample?.requests || 0));
  });
  const sampledAt = Number(snapshotMS || Date.now());
  if (dashboardControllerRealtimeEnabled) {
    dashboardSites = dashboardSites.map(site => {
      const siteID = Number(site.id);
      const live = liveMap.get(siteID);
      if (!live) return site;
      const speed = dashboardLiveSpeeds.get(siteID);
      if (!speed) {
        const { _liveSpeed, ...siteWithoutSpeed } = site;
        return { ...siteWithoutSpeed, ...live };
      }
      return { ...site, ...live, _liveSpeed: speed };
    });
    renderDashboardTableRows();
    persistDashboardRealtimeSamples();
    return;
  }
  const appendRealtimeTrendSample = (key, sample, sampleTimestamp = sampledAt, contributions = null) => {
    const samples = dashboardRealtimeTrendSamples.get(key) || [];
    const bytesIn = Math.max(0, Number(sample?.bytesIn || 0));
    const bytesOut = Math.max(0, Number(sample?.bytesOut || 0));
    const point = {
      timestamp_ms: Number(sampleTimestamp || sampledAt),
      download_bps: Math.max(0, Number(sample?.download_bps || 0)),
      upload_bps: Math.max(0, Number(sample?.upload_bps || 0)),
      bytes_in: bytesIn,
      bytes_out: bytesOut,
      requests: Math.max(0, Number(sample?.requests || 0)),
    };
    if (sample?.cumulativeBytesIn !== undefined) point.cumulative_bytes_in = Math.max(0, Number(sample.cumulativeBytesIn || 0));
    if (sample?.cumulativeBytesOut !== undefined) point.cumulative_bytes_out = Math.max(0, Number(sample.cumulativeBytesOut || 0));
    if (sample?.cumulativeRequests !== undefined) point.cumulative_requests = Math.max(0, Number(sample.cumulativeRequests || 0));
    if (billingKnown) point.traffic_bytes = billingMode === 'outbound' ? bytesOut : 2 * (bytesIn + bytesOut);
    if (contributions && Object.keys(contributions).length) point.site_contributions = contributions;
    upsertDashboardRealtimeSample(samples, point);
    pruneDashboardRealtimeSamples(samples);
    dashboardRealtimeTrendSamples.set(key, samples);
  };
  // Append one aggregate point on every SSE tick, even if no site's counter
  // changed. This keeps the realtime chart on a fixed two-second cadence;
  // unchanged sites retain their latest valid rate while their byte/request
  // deltas remain zero for this interval.
  const contributions = {};
  for (const siteID of changedSiteIDs) {
    const delta = trendDeltas.get(siteID) || {};
    const rate = rateSamples.get(String(siteID)) || {};
    contributions[String(siteID)] = {
      download_bps: Math.max(0, Number(rate.download_bps || 0)),
      upload_bps: Math.max(0, Number(rate.upload_bps || 0)),
      bytes_in: Math.max(0, Number(delta.bytesIn || 0)),
      bytes_out: Math.max(0, Number(delta.bytesOut || 0)),
      requests: Math.max(0, Number(delta.requests || 0)),
    };
    const cumulative = dashboardSpeedSamples.get(siteID);
    if (cumulative) {
      contributions[String(siteID)].cumulative_bytes_in = Math.max(0, Number(cumulative.bytesIn || 0));
      contributions[String(siteID)].cumulative_bytes_out = Math.max(0, Number(cumulative.bytesOut || 0));
      contributions[String(siteID)].cumulative_requests = Math.max(0, Number(cumulative.requests || 0));
    }
    if (billingKnown) contributions[String(siteID)].traffic_bytes = billingMode === 'outbound'
      ? contributions[String(siteID)].bytes_out
      : 2 * (contributions[String(siteID)].bytes_in + contributions[String(siteID)].bytes_out);
  }
  appendRealtimeTrendSample('all', {
    download_bps: totalRateOut,
    upload_bps: totalRateIn,
    bytesIn: totalDeltaIn,
    bytesOut: totalDeltaOut,
    requests: totalDeltaRequests,
    cumulativeBytesIn: totalCumulativeIn,
    cumulativeBytesOut: totalCumulativeOut,
    cumulativeRequests: totalCumulativeRequests,
  }, sampledAt, contributions);

  // Site charts use the same controller snapshot timestamp rather than the
  // Agent's last site sample timestamp. Otherwise a site that reports less
  // often would overwrite the same point repeatedly and its 150-point FIFO
  // would span an ever-growing period.
  for (const [siteID, latest] of dashboardLatestRatesBySite) {
    const rate = {
      download_bps: Math.max(0, Number(latest?.down || 0)),
      upload_bps: Math.max(0, Number(latest?.up || 0)),
    };
    const delta = trendDeltas.get(siteID) || {};
    const currentSample = dashboardSpeedSamples.get(siteID);
    const siteSamples = dashboardRealtimeTrendSiteSamples.get(String(siteID)) || [];
    const siteSample = {
      timestamp_ms: sampledAt,
      download_bps: rate.download_bps,
      upload_bps: rate.upload_bps,
      bytes_in: Math.max(0, Number(delta.bytesIn || 0)),
      bytes_out: Math.max(0, Number(delta.bytesOut || 0)),
      requests: Math.max(0, Number(delta.requests || 0)),
      cumulative_bytes_in: Math.max(0, Number(currentSample?.bytesIn || 0)),
      cumulative_bytes_out: Math.max(0, Number(currentSample?.bytesOut || 0)),
      cumulative_requests: Math.max(0, Number(currentSample?.requests || 0)),
    };
    if (billingKnown) siteSample.traffic_bytes = billingMode === 'outbound' ? siteSample.bytes_out : 2 * (siteSample.bytes_in + siteSample.bytes_out);
    appendRealtimeTrendSample(String(siteID), {
      ...rate,
      ...delta,
      cumulativeBytesIn: Math.max(0, Number(currentSample?.bytesIn || 0)),
      cumulativeBytesOut: Math.max(0, Number(currentSample?.bytesOut || 0)),
      cumulativeRequests: Math.max(0, Number(currentSample?.requests || 0)),
    }, sampledAt);
    upsertDashboardRealtimeSample(siteSamples, siteSample);
    pruneDashboardRealtimeSamples(siteSamples);
    dashboardRealtimeTrendSiteSamples.set(String(siteID), siteSamples);
  }
  dashboardSites = dashboardSites.map(site => {
    const siteID = Number(site.id);
    const live = liveMap.get(siteID);
    if (!live) return site;
    const speed = dashboardLiveSpeeds.get(siteID);
    if (!speed) {
      const { _liveSpeed, ...siteWithoutSpeed } = site;
      return { ...siteWithoutSpeed, ...live };
    }
    return { ...site, ...live, _liveSpeed: speed };
  });
  renderDashboardTableRows();
  persistDashboardRealtimeSamples();
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
  flushDashboardRealtimeSamples();
  dashboardRealtimePersistDirty = false;
  dashboardSpeedSamples = new Map();
  dashboardLiveSpeeds = new Map();
  dashboardLatestRatesBySite = new Map();
  dashboardBillingMode = null;
  dashboardRealtimePersistedBillingMode = null;
  dashboardRealtimeTrendSamples = new Map();
  dashboardRealtimeTrendSiteSamples = new Map();
  dashboardRealtimeSmoothingCache = new Map();
  dashboardRealtimeEpoch = '';
  dashboardRealtimeRevision = 0;
  dashboardRealtimeSiteRevisions = new Map();
  dashboardRealtimeProtocolObserved = false;
  dashboardRealtimeRetiredEpochs = new Set();
  dashboardRealtimeNeedsRecovery = false;
  dashboardRealtimeLastRecoveryMS = 0;
  if (dashboardRealtimeRecoveryAbortController) {
    dashboardRealtimeRecoveryAbortController.abort();
    dashboardRealtimeRecoveryAbortController = null;
  }
  dashboardLastSnapshotMS = 0;
  dashboardLastObservedSiteCount = -1;
  dashboardTrendData = null;
  dashboardSitesInitialized = false;
  dashboardTrendCharts = new Map();
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
  if (dashboardTrendAbortController) {
    dashboardTrendAbortController.abort();
    dashboardTrendAbortController = null;
  }
  if (dashboardBootstrapAbortController) {
    dashboardBootstrapAbortController.abort();
    dashboardBootstrapAbortController = null;
    dashboardBootstrapPromise = null;
  }
  dashboardSecondaryPromise = null;
  // The refresh intervals are deliberately NOT cleared here. This function runs
  // on every dashboard entry (through startDashSSE) as well as on teardown, so
  // clearing them here killed the 60 s bootstrap refresh and the 15 s realtime
  // trend refresh on the first navigation away and back: app.js only starts them
  // once per page load. renderDashboard owns starting them, and teardown calls
  // stopDashboardRefreshTimers().
  if (dashSSE) {
    dashSSE.close();
    dashSSE = null;
  }
}

async function loadDashboardTable() {
  try {
    // Kept as a compatibility helper for cached clients and older embedded
    // test harnesses. The live dashboard now calls loadDashboardBootstrap()
    // directly, so this legacy path is never used during normal rendering.
    const sites = await API.listSites();
    const tbody = document.getElementById('dash-table');
    if (!tbody) return;

    const totalCache = (sites || []).reduce((total, site) => total + Number(site.cache_size_bytes || 0), 0);
    const cacheEl = document.getElementById('s-cache');
    if (cacheEl) cacheEl.textContent = formatBytes(totalCache);

    if (!sites || sites.length === 0) {
      dashboardSites = [];
      dashboardSitesInitialized = true;
      renderDashboardTableRows();
      return;
    }

    const currentSites = new Map(dashboardSites.map(site => [Number(site.id), site]));
    dashboardSites = sites.map(site => {
      const siteID = Number(site.id);
      const current = currentSites.get(siteID);
      const speed = dashboardLiveSpeeds.get(siteID) || (current && current._liveSpeed);
      return speed ? { ...site, _liveSpeed: speed } : site;
    });
    renderDashboardTableRows();
  } catch (e) {
    console.error('Dashboard table load error:', e);
  }
}

function dashboardSiteIconMarkup(site) {
  // sites.js is loaded in the normal application shell, but keep the
  // dashboard renderer self-contained for cached pages and test harnesses.
  if (typeof renderSiteIcon === 'function') return renderSiteIcon(site, 'dashboard-site-icon');
  const name = String(site && site.name || '?').trim();
  const iconName = String(site && site.icon_name || '').trim();
  const iconURL = String(site && site.icon_url || '').trim();
  const fallback = esc(name.slice(0, 1) || '?');
  if (!iconName || !/^https:\/\//i.test(iconURL)) {
    return `<span class="site-icon dashboard-site-icon" aria-hidden="true"><span class="site-icon-fallback">${fallback}</span></span>`;
  }
  return `<span class="site-icon dashboard-site-icon" title="${esc(iconName)}"><img class="site-icon-image" src="${esc(iconURL)}" alt="" loading="lazy" referrerpolicy="no-referrer"><span class="site-icon-fallback" hidden>${fallback}</span></span>`;
}

function bindDashboardSiteIconFallbacks(root) {
  if (!root || typeof root.querySelectorAll !== 'function') return;
  root.querySelectorAll('.dashboard-site-icon .site-icon-image').forEach(image => {
    image.addEventListener('error', () => {
      image.hidden = true;
      const fallback = image.parentElement && image.parentElement.querySelector('.site-icon-fallback');
      if (fallback) fallback.hidden = false;
    }, { once: true });
  });
}

function renderDashboardTableRows() {
  const tbody = document.getElementById('dash-table');
  if (!tbody) return;
  if (typeof tbody.querySelectorAll === 'function') {
    // Native toggle events are queued. Read the actual open state before an
    // SSE or search redraw so a just-closed detail cannot reopen in that gap.
    tbody.querySelectorAll('details[data-console-site]').forEach(detail => {
      const siteID = Number(detail.dataset.consoleSite);
      if (detail.open) dashboardConsoleExpandedSites.add(siteID);
      else dashboardConsoleExpandedSites.delete(siteID);
    });
  }
  if (!dashboardSites.length) {
    if (!dashboardSitesInitialized) return;
    dashboardSpeedSamples.clear();
    dashboardLiveSpeeds.clear();
    dashboardLatestRatesBySite.clear();
    dashboardRealtimeTrendSiteSamples.clear();
    tbody.innerHTML = '<div class="console-empty">暂无站点，<a href="#sites">前往站点管理添加</a></div>';
    return;
  }
  const known = new Set(dashboardSites.map(site => Number(site.id)));
  for (const map of [dashboardSpeedSamples, dashboardLiveSpeeds, dashboardLatestRatesBySite]) {
    for (const siteID of map.keys()) if (!known.has(siteID)) map.delete(siteID);
  }
  for (const siteID of dashboardRealtimeTrendSiteSamples.keys()) if (!known.has(Number(siteID))) dashboardRealtimeTrendSiteSamples.delete(siteID);
  const visible = dashboardSites.filter(site => !dashboardConsoleQuery ||
    `${site.name || ''} ${site.public_host || ''}`.toLocaleLowerCase().includes(dashboardConsoleQuery));
  const focusedSite = Number(document.activeElement?.closest?.('[data-console-site]')?.dataset?.consoleSite);
  tbody.innerHTML = visible.length ? visible.map(s => {
    const unavailable = Number(s.agent_received_at_ms || 0) > 0 &&
      (s.agent_rate_valid !== true || (dashboardLastSnapshotMS > 0 &&
        dashboardLastSnapshotMS - Number(s.agent_rate_received_at_ms || s.agent_received_at_ms || 0) > 6000));
    return `<details class="console-site" data-console-site="${Number(s.id)}" ${dashboardConsoleExpandedSites.has(Number(s.id)) ? 'open' : ''}>
      <summary>
        <span class="dashboard-site-identity">${dashboardSiteIconMarkup(s)}<span><span class="dashboard-site-name">${esc(s.name)}</span><small>${s.running ? '运行中' : '已停止'} · ${esc(s.public_host || '独立入口')}</small></span></span>
        <span class="console-status ${s.running ? 'is-online' : ''}">${s.running ? '运行中' : '已停止'}</span>
        <span class="console-site-rate">${unavailable && s.running ? '<span class="console-muted">实时数据暂不可用</span>' : dashboardSpeedMarkup(s._liveSpeed)}</span>
        <span class="console-site-traffic">${formatBytes(s.monthly_traffic != null ? s.monthly_traffic : s.traffic_used)}</span>
        <svg class="console-site-chevron" viewBox="0 0 24 24" aria-hidden="true"><path d="M9 5l7 7-7 7"/></svg>
      </summary>
      <dl class="console-site-detail">
        <div class="console-site-detail-wide"><dt>回源地址</dt><dd class="console-mono">${esc(s.target_url || '—')}</dd></div>
        <div class="console-site-detail-wide"><dt>入口</dt><dd class="console-mono">${dashboardIngressLabel(s)}</dd></div>
        <div><dt>客户端标识</dt><dd>${esc(uaNameMap[s.ua_mode] || s.ua_mode || '—')}</dd></div>
        <div><dt>缓存</dt><dd>${formatBytes(s.cache_size_bytes)}</dd></div>
        <div><dt>已用流量</dt><dd>${formatBytes(s.monthly_traffic != null ? s.monthly_traffic : s.traffic_used)}</dd></div>
        <div><dt>站点管理</dt><dd><a href="#sites">查看设置 →</a></dd></div>
      </dl>
    </details>`;
  }).join('') : '<div class="console-empty">没有匹配的站点</div>';
  if (focusedSite && typeof tbody.querySelector === 'function') {
    tbody.querySelector(`[data-console-site="${focusedSite}"] > summary`)?.focus({ preventScroll: true });
  }
  bindDashboardSiteIconFallbacks(tbody);
}

function dashboardSpeedMarkup(speed) {
  if (!speed) speed = { down: 0, up: 0 };
  return `<span class="dashboard-speed"><span>↓ ${formatRate(speed.down)}</span><span>↑ ${formatRate(speed.up)}</span></span>`;
}

function formatRate(bytesPerSecond) {
  return `${formatBytes(Math.max(0, Number(bytesPerSecond) || 0))}/s`;
}

function dashboardIngressLabel(site) {
	const mode = String(site.ingress_mode || (site.public_host ? 'host' : 'port')).toLowerCase();
	if (mode === 'host') return `Host: ${esc(site.public_host || '')}`;
	if (mode === 'path') return `Path: ${esc(site.path_prefix || '')}`;
	if (mode === 'both') return `Host + :${site.listen_port}`;
	return `:${site.listen_port}`;
}

async function loadDashboardData() {
  if (Router.current === 'dashboard') {
    await refreshDashboardSecondary();
  }
}

function refreshDashboardSecondary() {
  if (dashboardSecondaryPromise) return dashboardSecondaryPromise;
  let promise;
  promise = loadDashboardBootstrap().finally(() => {
    if (dashboardSecondaryPromise === promise) dashboardSecondaryPromise = null;
  });
  dashboardSecondaryPromise = promise;
  return dashboardSecondaryPromise;
}

function startDashboardRefreshTimers() {
  if (dashboardSecondaryRefreshTimer) clearInterval(dashboardSecondaryRefreshTimer);
  if (dashboardTrendRefreshTimer) clearInterval(dashboardTrendRefreshTimer);
  dashboardSecondaryRefreshTimer = setInterval(() => {
    if (Router.current === 'dashboard') void refreshDashboardSecondary();
  }, 60000);
  dashboardTrendRefreshTimer = setInterval(() => {
    if (Router.current === 'dashboard' && dashboardTrendState.range === 'realtime') void loadDashboardTrends();
  }, 15000);
}

function stopDashboardRefreshTimers() {
  if (dashboardSecondaryRefreshTimer) clearInterval(dashboardSecondaryRefreshTimer);
  if (dashboardTrendRefreshTimer) clearInterval(dashboardTrendRefreshTimer);
  dashboardSecondaryRefreshTimer = null;
  dashboardTrendRefreshTimer = null;
}

const uaClassMap = { infuse: 'pill-blue', web: 'pill-green', client: 'pill-orange', custom: 'pill-purple', passthrough: 'pill-blue' };
const uaNameMap = { infuse: 'Infuse', web: 'Web', client: '客户端', custom: '自定义', passthrough: '透传' };

function formatBytes(bytes) {
	const value = Math.max(0, Number(bytes) || 0);
	if (value === 0) return '0 B';
	const units = ['B', 'KB', 'MB', 'GB', 'TB', 'PB', 'EB'];
	const i = Math.min(units.length - 1, Math.floor(Math.log(value) / Math.log(1024)));
	return (value / Math.pow(1024, i)).toFixed(i > 1 ? 1 : 0) + ' ' + units[i];
}
