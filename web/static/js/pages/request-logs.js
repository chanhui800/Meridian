let requestLogCategoryFilter = 'all';
let requestLogStatusFilter = 'all';
let requestLogSearchTimer = null;
let requestLogRefreshTimer = null;
let requestLogLoadGeneration = 0;
let requestLogLoading = false;
let requestLogReloadQueued = false;
let requestLogDisplaySettings = { node: true, category: true, status: true, client_ip: true, ua: true, backend_address: true, timeline: true };
const requestLogUAWidthStorageKey = 'meridian-request-log-ua-width';

function requestLogNormalizeUAWidth(value) {
  return Math.max(180, Math.min(420, Number(value) || 240));
}

function requestLogGetUAWidth() {
  try {
    return requestLogNormalizeUAWidth(window.localStorage.getItem(requestLogUAWidthStorageKey));
  } catch (_) {
    return 240;
  }
}

function requestLogSetUAWidth(value) {
  const width = requestLogNormalizeUAWidth(value);
  const cssWidth = `${width}px`;
  if (document.documentElement?.style?.setProperty) {
    document.documentElement.style.setProperty('--request-log-ua-width', cssWidth);
  }
  const table = document.querySelector?.('.request-log-table');
  if (table?.style?.setProperty) table.style.setProperty('--request-log-ua-width', cssWidth);
  document.querySelectorAll?.('col.request-log-col-ua, th[data-log-field="ua"]').forEach(node => {
    node.style?.setProperty('width', cssWidth, 'important');
  });
  try {
    window.localStorage.setItem(requestLogUAWidthStorageKey, String(width));
  } catch (_) {}
  return width;
}

function requestLogApplyUAWidth() {
  return requestLogSetUAWidth(requestLogGetUAWidth());
}

function requestLogApplyDisplaySettings(settings) {
  requestLogDisplaySettings = {
    node: settings?.log_display_node !== false,
    category: settings?.log_display_category !== false,
    status: settings?.log_display_status !== false,
    client_ip: settings?.log_display_client_ip !== false,
    ua: settings?.log_display_ua !== false,
    backend_address: settings?.log_display_backend_address !== false,
    timeline: settings?.log_display_timeline !== false,
  };
  document.querySelectorAll('[data-log-field="node"]').forEach(node => { node.hidden = !requestLogDisplaySettings.node; });
  document.querySelectorAll('[data-log-field="category"]').forEach(node => { node.hidden = !requestLogDisplaySettings.category; });
  document.querySelectorAll('[data-log-field="status"]').forEach(node => { node.hidden = !requestLogDisplaySettings.status; });
  document.querySelectorAll('[data-log-field="ip"]').forEach(node => { node.hidden = !requestLogDisplaySettings.client_ip; });
  document.querySelectorAll('[data-log-field="ua"]').forEach(node => { node.hidden = !requestLogDisplaySettings.ua; });
  document.querySelectorAll('[data-log-field="backend-address"]').forEach(node => { node.hidden = !requestLogDisplaySettings.backend_address; });
  document.querySelectorAll('[data-log-field="timeline"]').forEach(node => { node.hidden = !requestLogDisplaySettings.timeline; });
}

function requestLogDateInputValue(date) {
  const year = date.getFullYear();
  const month = String(date.getMonth() + 1).padStart(2, '0');
  const day = String(date.getDate()).padStart(2, '0');
  return `${year}-${month}-${day}`;
}

function requestLogRangeMilliseconds(fromValue, toValue) {
  const from = new Date(`${fromValue}T00:00:00`);
  const to = new Date(`${toValue}T23:59:59.999`);
  return {
    from_ms: Number.isFinite(from.getTime()) ? from.getTime() : 0,
    to_ms: Number.isFinite(to.getTime()) ? to.getTime() : 0,
  };
}

function requestLogCategoryLabel(category) {
  return ({
    playback: '播放信息',
    playback_sync: '播放状态同步',
    video: '视频流',
    stream: '主视频流',
    manifest: '播放清单',
    segment: '媒体分片',
    image: '图片海报',
    metadata: '媒体元数据',
    subtitle: '字幕',
    asset: '静态资源',
    websocket: 'WebSocket',
    api: '常规 API',
    auth: '用户认证',
  })[category] || '—';
}

function requestLogRelativeTime(timestamp, now) {
  if (!Number(timestamp)) return '—';
  const delta = Math.max(0, (now === undefined ? Date.now() : now) - Number(timestamp || 0));
  const seconds = Math.floor(delta / 1000);
  if (seconds < 60) return '刚刚';
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes} 分钟前`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours} 小时前`;
  const days = Math.floor(hours / 24);
  if (days < 30) return `${days} 天前`;
  return new Date(Number(timestamp || 0)).toLocaleDateString('zh-CN');
}

function requestLogStatusClass(status) {
  status = Number(status || 0);
  if (status >= 200 && status < 400) return 'request-log-status-ok';
  if (status >= 400 && status < 500) return 'request-log-status-client';
  return 'request-log-status-server';
}

function renderRequestLogs() {
  const page = document.getElementById('page-request-logs');
  requestLogApplyUAWidth();
  const today = new Date();
  const yesterday = new Date(today);
  yesterday.setDate(today.getDate() - 1);
  requestLogCategoryFilter = 'all';
  requestLogStatusFilter = 'all';

  page.innerHTML = `
    <h1 class="section-title fade-up">日志记录</h1>
    <p class="section-sub fade-up">查看各站点的请求状态、客户端 IP 与客户端标识。日志不保存查询参数、令牌、Cookie 或正文。</p>

    <section class="request-log-controls fade-up">
      <div class="request-log-search-row">
        <label class="request-log-date-field">
          <span>开始日期</span>
          <input type="date" class="form-input" id="request-log-from" value="${requestLogDateInputValue(yesterday)}">
        </label>
        <span class="request-log-date-separator">至</span>
        <label class="request-log-date-field">
          <span>结束日期</span>
          <input type="date" class="form-input" id="request-log-to" value="${requestLogDateInputValue(today)}">
        </label>
        <label class="request-log-search-field">
          <span class="sr-only">搜索日志</span>
          <input type="search" class="form-input" id="request-log-search" placeholder="搜索节点、客户端 IP、UA、路径或状态码（如 200）" autocomplete="off">
          <svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="11" cy="11" r="7"/><line x1="16.5" y1="16.5" x2="21" y2="21"/></svg>
        </label>
      </div>

      <div class="request-log-filter-row">
        <span class="request-log-filter-label">筛选模式</span>
        <div class="request-log-pills" id="request-log-category-pills">
          <button type="button" class="request-log-pill active" data-category="all">全部</button>
          <button type="button" class="request-log-pill" data-category="playback">播放信息</button>
          <button type="button" class="request-log-pill" data-category="playback_sync">播放状态同步</button>
          <button type="button" class="request-log-pill" data-category="stream">主视频流</button>
          <button type="button" class="request-log-pill" data-category="manifest">播放清单</button>
          <button type="button" class="request-log-pill" data-category="segment">媒体分片</button>
          <button type="button" class="request-log-pill" data-category="image">图片海报</button>
          <button type="button" class="request-log-pill" data-category="metadata">媒体元数据</button>
          <button type="button" class="request-log-pill" data-category="subtitle">字幕</button>
          <button type="button" class="request-log-pill" data-category="asset">静态资源</button>
          <button type="button" class="request-log-pill" data-category="websocket">WebSocket</button>
          <button type="button" class="request-log-pill" data-category="api">常规 API</button>
          <button type="button" class="request-log-pill" data-category="auth">用户认证</button>
        </div>
      </div>

      <div class="request-log-filter-row">
        <span class="request-log-filter-label">状态筛选</span>
        <div class="request-log-pills" id="request-log-status-pills">
          <button type="button" class="request-log-pill active" data-status="all">全部状态</button>
          <button type="button" class="request-log-pill" data-status="4xx">只看 4XX</button>
          <button type="button" class="request-log-pill" data-status="5xx">只看 5XX</button>
        </div>
      </div>

      <div class="request-log-ua-width-control">
        <label for="request-log-ua-width">UA 列宽</label>
        <input type="range" id="request-log-ua-width" min="180" max="420" step="10" value="${requestLogGetUAWidth()}">
        <output id="request-log-ua-width-value">${requestLogGetUAWidth()} px</output>
      </div>

      <div class="request-log-actions">
        <button type="button" class="request-log-action danger" id="request-cache-clear">
          <svg viewBox="0 0 24 24"><ellipse cx="12" cy="5" rx="8" ry="3"/><path d="M4 5v6c0 1.7 3.6 3 8 3s8-1.3 8-3V5"/><path d="M4 11v6c0 1.7 3.6 3 8 3s8-1.3 8-3v-6"/><line x1="9" y1="10" x2="15" y2="16"/><line x1="15" y1="10" x2="9" y2="16"/></svg>
          清除缓存
        </button>
        <button type="button" class="request-log-action danger" id="request-log-clear">
          <svg viewBox="0 0 24 24"><polyline points="3 6 5 6 21 6"/><path d="M19 6l-1 14H6L5 6"/><path d="M8 6V4h8v2"/></svg>
          清空日志
        </button>
        <button type="button" class="request-log-action" id="request-log-refresh">
          <svg viewBox="0 0 24 24"><polyline points="23 4 23 10 17 10"/><path d="M20.5 15a9 9 0 1 1-2.1-9.4L23 10"/></svg>
          刷新
        </button>
        <span class="request-log-summary" id="request-log-summary">正在读取日志…</span>
      </div>
    </section>

    <section class="request-log-table-card fade-up" aria-label="请求日志列表">
      <div class="request-log-table-scroll">
        <table class="request-log-table">
          <colgroup>
            <col class="request-log-col-node"><col class="request-log-col-category"><col class="request-log-col-status">
            <col class="request-log-col-ip"><col class="request-log-col-ua"><col class="request-log-col-backend"><col class="request-log-col-time">
          </colgroup>
          <thead><tr>
            <th data-log-field="node">节点</th><th data-log-field="category">资源类别</th><th data-log-field="status">状态</th><th data-log-field="ip">客户端 IP</th><th data-log-field="ua">UA</th><th data-log-field="backend-address">后端地址</th><th data-log-field="timeline">时间线</th>
          </tr></thead>
          <tbody id="request-log-body">
            <tr><td colspan="7" class="request-log-empty">正在加载…</td></tr>
          </tbody>
        </table>
      </div>
    </section>
  `;
  requestLogApplyUAWidth();

  document.querySelectorAll('#request-log-category-pills .request-log-pill').forEach(button => {
    button.onclick = () => {
      requestLogCategoryFilter = button.dataset.category;
      setRequestLogActivePill('request-log-category-pills', button);
      loadRequestLogs();
    };
  });
  document.querySelectorAll('#request-log-status-pills .request-log-pill').forEach(button => {
    button.onclick = () => {
      requestLogStatusFilter = button.dataset.status;
      setRequestLogActivePill('request-log-status-pills', button);
      loadRequestLogs();
    };
  });
  document.getElementById('request-log-from').onchange = loadRequestLogs;
  document.getElementById('request-log-to').onchange = loadRequestLogs;
  document.getElementById('request-log-search').oninput = () => {
    if (requestLogSearchTimer) clearTimeout(requestLogSearchTimer);
    requestLogSearchTimer = setTimeout(loadRequestLogs, 300);
  };
  document.getElementById('request-log-refresh').onclick = loadRequestLogs;
  document.getElementById('request-log-clear').onclick = clearRequestLogs;
  document.getElementById('request-cache-clear').onclick = clearAssetCache;
  const uaWidthInput = document.getElementById('request-log-ua-width');
  if (uaWidthInput) {
    const applyUAWidth = () => {
      const width = requestLogSetUAWidth(uaWidthInput.value);
      uaWidthInput.value = String(width);
      const output = document.getElementById('request-log-ua-width-value');
      if (output) output.textContent = `${width} px`;
    };
    uaWidthInput.oninput = applyUAWidth;
    uaWidthInput.onchange = applyUAWidth;
  }
  if (API.getSystemSettings) API.getSystemSettings().then(requestLogApplyDisplaySettings).catch(() => requestLogApplyDisplaySettings(null));
  loadRequestLogs({ showLoading: true });
  if (requestLogRefreshTimer) clearInterval(requestLogRefreshTimer);
  requestLogRefreshTimer = setInterval(() => {
    if (Router.current === 'request-logs') loadRequestLogs({ showLoading: false });
  }, 5000);
}

function stopRequestLogRefresh() {
  requestLogLoadGeneration += 1;
  if (requestLogRefreshTimer) {
    clearInterval(requestLogRefreshTimer);
    requestLogRefreshTimer = null;
  }
  if (requestLogSearchTimer) {
    clearTimeout(requestLogSearchTimer);
    requestLogSearchTimer = null;
  }
  requestLogReloadQueued = false;
}

function setRequestLogActivePill(containerId, activeButton) {
  document.querySelectorAll(`#${containerId} .request-log-pill`).forEach(button => {
    button.classList.toggle('active', button === activeButton);
  });
}

async function loadRequestLogs(options = {}) {
  const body = document.getElementById('request-log-body');
  if (!body) return;
  if (requestLogLoading) {
    requestLogReloadQueued = true;
    return;
  }
  requestLogReloadQueued = false;
  const from = document.getElementById('request-log-from').value;
  const to = document.getElementById('request-log-to').value;
  const range = requestLogRangeMilliseconds(from, to);
  if (!range.from_ms || !range.to_ms || range.from_ms > range.to_ms) {
    Toast.error('请选择有效的日志日期范围');
    return;
  }
  const generation = ++requestLogLoadGeneration;
  const scroller = body.closest('.request-log-table-scroll');
  const previousScrollTop = scroller ? scroller.scrollTop : 0;
  const previousScrollHeight = scroller ? scroller.scrollHeight : 0;
  const preserveViewport = previousScrollTop > 0;
  requestLogLoading = true;
  if (options.showLoading === true && !body.querySelector('tr[data-log-id]')) {
    body.innerHTML = '<tr><td colspan="7" class="request-log-empty">正在加载…</td></tr>';
  }
  try {
    const response = await API.getRequestLogs({
      ...range,
      category: requestLogCategoryFilter,
      status: requestLogStatusFilter,
      q: document.getElementById('request-log-search').value.trim(),
      limit: 500,
    });
    if (generation !== requestLogLoadGeneration || Router.current !== 'request-logs' || !response) return;
    renderRequestLogRows(response.logs || []);
    if (scroller && preserveViewport) {
      const addedHeight = Math.max(0, scroller.scrollHeight - previousScrollHeight);
      scroller.scrollTop = previousScrollTop + addedHeight;
    }
    const dropped = Number(response.dropped_logs || 0);
    document.getElementById('request-log-summary').textContent = dropped > 0
      ? `显示 ${response.logs.length} 条，繁忙时已丢弃 ${dropped} 条`
      : `显示 ${response.logs.length} 条（最多 500 条）`;
  } catch (error) {
    if (generation !== requestLogLoadGeneration) return;
    if (!body.querySelector('tr[data-log-id]')) {
      body.innerHTML = '<tr><td colspan="6" class="request-log-empty request-log-error">日志读取失败</td></tr>';
    }
    Toast.error(error.message);
  } finally {
    requestLogLoading = false;
    if (requestLogReloadQueued && Router.current === 'request-logs') {
      requestLogReloadQueued = false;
      loadRequestLogs({ showLoading: false });
    }
  }
}

function renderRequestLogRows(logs) {
  const body = document.getElementById('request-log-body');
  if (!body) return;
  if (!logs.length) {
    body.innerHTML = '<tr><td colspan="6" class="request-log-empty">当前条件下暂无日志</td></tr>';
    return;
  }
  body.innerHTML = logs.map(entry => {
    const status = Number(entry.status_code || 0);
    const recordedAtMS = Number(entry.recorded_at_ms || 0);
    const exactTime = recordedAtMS ? new Date(recordedAtMS).toLocaleString('zh-CN', { hour12: false }) : '未写入时间线';
    const requestTitle = `${String(entry.method || 'GET')} ${String(entry.path || '/')}`;
    return `
      <tr data-log-id="${esc(entry.id || '')}" title="${esc(requestTitle)}">
        <td data-log-field="node"><span class="request-log-node">${esc(entry.site_name || '—')}</span></td>
        <td data-log-field="category"><span class="request-log-category">${esc(requestLogCategoryLabel(entry.resource_category))}</span></td>
        <td data-log-field="status"><span class="request-log-status ${requestLogStatusClass(status)}">${status || '—'}</span></td>
        <td data-log-field="ip"><span class="request-log-ip mono">${esc(entry.client_ip || '—')}</span><small class="request-log-region">${esc(entry.client_region || '')}</small></td>
        <td data-log-field="ua"><span class="request-log-ua">${esc(entry.user_agent || '—')}</span></td>
        <td data-log-field="backend-address"><span class="request-log-backend mono">${esc(entry.backend_address || '—')}</span></td>
        <td data-log-field="timeline"><time class="request-log-time"${recordedAtMS ? ` datetime="${new Date(recordedAtMS).toISOString()}"` : ''} title="${esc(exactTime)}">${esc(requestLogRelativeTime(recordedAtMS))}</time></td>
      </tr>
    `;
  }).join('');
}

async function clearRequestLogs() {
  if (!confirm('确认清空全部请求日志？该操作不可撤销。')) return;
  try {
    await API.clearRequestLogs();
    Toast.success('请求日志已清空');
    loadRequestLogs();
  } catch (error) {
    Toast.error(error.message);
  }
}

async function clearAssetCache() {
  if (!confirm('确认清除所有站点的图片与静态资源缓存？站点配置和请求日志不会受到影响。')) return;
  try {
    await API.clearAssetCache();
    Toast.success('资产缓存已清除');
  } catch (error) {
    Toast.error(error.message);
  }
}
