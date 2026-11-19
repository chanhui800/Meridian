let requestLogCategoryFilter = 'all';
let requestLogStatusFilter = 'all';
let requestLogSearchTimer = null;
let requestLogRefreshTimer = null;
let requestLogLoadGeneration = 0;
let requestLogLoading = false;
let requestLogReloadQueued = false;
let requestLogLatestID = 0;
let requestLogLatestCursor = '';
let requestLogDisplaySettings = { node: true, category: true, status: true, client_ip: true, ua: true, upstream_ua: true, backend_address: true, timeline: true };
let requestLogLastDisplaySettings = null;
let requestLogEntries = new Map();
const requestLogUAWidthStorageKey = 'meridian-request-log-ua-width';

function requestLogDateOnlyValue(value) {
  if (typeof meridianDateOnlyValue === 'function') return meridianDateOnlyValue(value);
  const date = value instanceof Date ? value : new Date(value);
  const pad = number => String(number).padStart(2, '0');
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}`;
}

function requestLogParseDateOnly(value, endOfDay) {
  if (typeof meridianParseDateOnly === 'function') return meridianParseDateOnly(value, endOfDay);
  const date = new Date(`${value}T${endOfDay ? '23:59:59.999' : '00:00:00'}`);
  return date.getTime();
}

function requestLogFormatDateTime(timestamp) {
  if (typeof meridianFormatDateTime === 'function') return meridianFormatDateTime(timestamp);
  return new Date(Number(timestamp)).toLocaleString('zh-CN', { hour12: false });
}

function requestLogFormatDate(timestamp) {
  if (typeof meridianFormatDate === 'function') return meridianFormatDate(timestamp);
  return new Date(Number(timestamp)).toLocaleDateString('zh-CN');
}

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
  document.querySelectorAll?.('col.request-log-col-ua, col.request-log-col-upstream-ua, th[data-log-field="ua"], th[data-log-field="upstream-ua"]').forEach(node => {
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
  requestLogLastDisplaySettings = settings || requestLogLastDisplaySettings;
  requestLogDisplaySettings = {
    node: settings?.log_display_node !== false,
    category: settings?.log_display_category !== false,
    status: settings?.log_display_status !== false,
    client_ip: settings?.log_display_client_ip !== false,
    ua: settings?.log_display_ua !== false,
    upstream_ua: settings?.log_display_upstream_ua !== false,
    backend_address: settings?.log_display_backend_address !== false,
    timeline: settings?.log_display_timeline !== false,
  };
  document.querySelectorAll('[data-log-field="node"]').forEach(node => { node.hidden = !requestLogDisplaySettings.node; });
  document.querySelectorAll('[data-log-field="category"]').forEach(node => { node.hidden = !requestLogDisplaySettings.category; });
  document.querySelectorAll('[data-log-field="status"]').forEach(node => { node.hidden = !requestLogDisplaySettings.status; });
  document.querySelectorAll('[data-log-field="ip"]').forEach(node => { node.hidden = !requestLogDisplaySettings.client_ip; });
  document.querySelectorAll('[data-log-field="ua"]').forEach(node => { node.hidden = !requestLogDisplaySettings.ua; });
  document.querySelectorAll('[data-log-field="upstream-ua"]').forEach(node => { node.hidden = !requestLogDisplaySettings.upstream_ua; });
  document.querySelectorAll('[data-log-field="backend-address"]').forEach(node => { node.hidden = !requestLogDisplaySettings.backend_address; });
  document.querySelectorAll('[data-log-field="timeline"]').forEach(node => { node.hidden = !requestLogDisplaySettings.timeline; });
}

function requestLogDateInputValue(date) {
  return requestLogDateOnlyValue(date);
}

function requestLogRangeMilliseconds(fromValue, toValue) {
  const from = requestLogParseDateOnly(fromValue, false);
  const to = requestLogParseDateOnly(toValue, true);
  return {
    from_ms: Number.isFinite(from) ? from : 0,
    to_ms: Number.isFinite(to) ? to : 0,
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
  return requestLogFormatDate(Number(timestamp || 0));
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
  requestLogLatestID = 0;
  requestLogLatestCursor = '';
  requestLogEntries = new Map();
  const today = Date.now();
  const yesterday = today - 24 * 60 * 60 * 1000;
  requestLogCategoryFilter = 'all';
  requestLogStatusFilter = 'all';

  page.innerHTML = `
    <div class="console-page management-page management-log-page">
      <header class="console-heading"><div><p class="console-eyebrow">访问与排查</p><h1>请求日志</h1><p class="console-lead">按时间、站点和客户端快速定位请求。</p></div><button type="button" class="console-button" id="request-log-refresh"><svg viewBox="0 0 24 24" aria-hidden="true"><path d="M20 7v5h-5M4 17v-5h5M6 6a8 8 0 0 1 14 6M4 12a8 8 0 0 0 14 6"/></svg>刷新日志</button></header>
      <section class="console-panel management-log-controls" aria-label="查找日志">
        <div class="management-log-search-row">
          <label class="management-search"><span class="sr-only">搜索日志</span><svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="11" cy="11" r="7"/><path d="m16 16 5 5"/></svg><input type="search" class="form-input" id="request-log-search" placeholder="搜索节点、客户端 IP、UA、路径或状态码" autocomplete="off"></label>
          <label class="management-date-field"><span>开始日期</span><input type="date" class="form-input" id="request-log-from" value="${requestLogDateInputValue(yesterday)}"></label>
          <label class="management-date-field"><span>结束日期</span><input type="date" class="form-input" id="request-log-to" value="${requestLogDateInputValue(today)}"></label>
        </div>
        <details class="management-log-filters" id="request-log-advanced-filters">
          <summary><span id="request-log-filter-summary">筛选与显示</span><svg viewBox="0 0 24 24" aria-hidden="true"><path d="m6 9 6 6 6-6"/></svg></summary>
          <div class="management-log-filter-content">
            <div class="management-filter-group"><span class="management-field-label">资源类型</span><div class="request-log-pills" id="request-log-category-pills">
              <button type="button" class="request-log-pill active" data-category="all" aria-pressed="true">全部</button>
              <button type="button" class="request-log-pill" data-category="playback" aria-pressed="false">播放信息</button>
              <button type="button" class="request-log-pill" data-category="playback_sync" aria-pressed="false">播放状态同步</button>
              <button type="button" class="request-log-pill" data-category="stream" aria-pressed="false">主视频流</button>
              <button type="button" class="request-log-pill" data-category="manifest" aria-pressed="false">播放清单</button>
              <button type="button" class="request-log-pill" data-category="segment" aria-pressed="false">媒体分片</button>
              <button type="button" class="request-log-pill" data-category="image" aria-pressed="false">图片海报</button>
              <button type="button" class="request-log-pill" data-category="metadata" aria-pressed="false">媒体元数据</button>
              <button type="button" class="request-log-pill" data-category="subtitle" aria-pressed="false">字幕</button>
              <button type="button" class="request-log-pill" data-category="asset" aria-pressed="false">静态资源</button>
              <button type="button" class="request-log-pill" data-category="websocket" aria-pressed="false">WebSocket</button>
              <button type="button" class="request-log-pill" data-category="api" aria-pressed="false">常规 API</button>
              <button type="button" class="request-log-pill" data-category="auth" aria-pressed="false">用户认证</button>
            </div></div>
            <div class="management-filter-group"><span class="management-field-label">响应状态</span><div class="request-log-pills" id="request-log-status-pills">
              <button type="button" class="request-log-pill active" data-status="all" aria-pressed="true">全部状态</button><button type="button" class="request-log-pill" data-status="4xx" aria-pressed="false">只看 4XX</button><button type="button" class="request-log-pill" data-status="5xx" aria-pressed="false">只看 5XX</button>
            </div></div>
            <div class="management-log-storage"><div class="request-log-ua-width-control"><label for="request-log-ua-width">UA 列宽</label><input type="range" id="request-log-ua-width" min="180" max="420" step="10" value="${requestLogGetUAWidth()}"><output id="request-log-ua-width-value">${requestLogGetUAWidth()} px</output></div><div class="management-log-storage-actions"><button type="button" class="console-button" id="request-cache-clear">清除资源缓存</button><button type="button" class="console-button is-danger" id="request-log-clear">清空全部日志</button></div></div>
          </div>
        </details>
      </section>
      <section class="console-panel management-log-results" aria-label="请求日志列表">
        <div class="management-list-meta"><span class="request-log-summary" id="request-log-summary">正在读取日志…</span><span class="console-muted">每 5 秒更新 · 点击记录查看详情</span></div>
        <div class="request-log-table-scroll">
          <table class="request-log-table">
            <colgroup><col class="request-log-col-site"><col class="request-log-col-node"><col class="request-log-col-category"><col class="request-log-col-status"><col class="request-log-col-ip"><col class="request-log-col-ua"><col class="request-log-col-upstream-ua"><col class="request-log-col-backend"><col class="request-log-col-time"></colgroup>
            <thead><tr><th data-log-field="site">站点</th><th data-log-field="node">节点</th><th data-log-field="category">资源类别</th><th data-log-field="status">状态</th><th data-log-field="ip">客户端 IP</th><th data-log-field="ua">客户端 UA</th><th data-log-field="upstream-ua">上游 UA</th><th data-log-field="backend-address">后端地址</th><th data-log-field="timeline">时间线</th></tr></thead>
            <tbody id="request-log-body"><tr><td colspan="9" class="request-log-empty">正在加载…</td></tr></tbody>
          </table>
        </div>
      </section>
      <p class="management-footnote">日志不保存查询参数、令牌、Cookie 或正文。清空日志与资源缓存可以分别操作。</p>
    </div>
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
  document.getElementById('request-log-body').onclick = event => {
    const button = event.target.closest?.('[data-log-detail-id]');
    const entry = button && requestLogEntries.get(Number(button.dataset.logDetailId));
    if (entry) showRequestLogDetails(entry);
  };
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
  if (API.getSystemSettings) API.getSystemSettings().then(settings => {
    if (typeof meridianSetTimezoneOffset === 'function') meridianSetTimezoneOffset(settings?.schedule_timezone_offset);
    requestLogApplyDisplaySettings(settings);
  }).catch(() => requestLogApplyDisplaySettings(null));
  loadRequestLogs({ showLoading: true });
  if (requestLogRefreshTimer) clearInterval(requestLogRefreshTimer);
  requestLogRefreshTimer = setInterval(() => {
    if (Router.current === 'request-logs') loadRequestLogs({ showLoading: false, incremental: true });
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
    button.setAttribute?.('aria-pressed', String(button === activeButton));
  });
  const summary = document.getElementById('request-log-filter-summary');
  if (summary) {
    const selected = [];
    if (requestLogCategoryFilter !== 'all') selected.push(requestLogCategoryLabel(requestLogCategoryFilter));
    if (requestLogStatusFilter !== 'all') selected.push(requestLogStatusFilter.toUpperCase());
    summary.textContent = selected.length ? `筛选与显示 · ${selected.join(' / ')}` : '筛选与显示';
  }
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
  const incremental = options.incremental === true && requestLogLatestID > 0;
  if (!incremental) requestLogLatestID = 0;
  const generation = ++requestLogLoadGeneration;
  const scroller = document.scrollingElement || body.closest('.request-log-table-scroll');
  const previousScrollTop = scroller ? scroller.scrollTop : 0;
  const previousScrollHeight = scroller ? scroller.scrollHeight : 0;
  const preserveViewport = previousScrollTop > 0;
  requestLogLoading = true;
  if (options.showLoading === true && !body.querySelector('tr[data-log-id]')) {
    body.innerHTML = '<tr><td colspan="9" class="request-log-empty">正在加载…</td></tr>';
  }
  try {
    const response = await API.getRequestLogs({
      ...range,
      category: requestLogCategoryFilter,
      status: requestLogStatusFilter,
      q: document.getElementById('request-log-search').value.trim(),
      limit: incremental ? 100 : 500,
      ...(incremental ? (requestLogLatestCursor ? { after_cursor: requestLogLatestCursor } : { after_id: requestLogLatestID }) : {}),
    });
    if (generation !== requestLogLoadGeneration || Router.current !== 'request-logs' || !response) return;
    const incoming = response.logs || [];
    if (incremental) {
      if (incoming.length) {
        body.insertAdjacentHTML('afterbegin', incoming.map(requestLogRowHTML).join(''));
        while (body.querySelectorAll('tr[data-log-id]').length > 500) {
          const last = body.lastElementChild;
          requestLogEntries.delete(Number(last?.dataset?.logId));
          last?.remove();
        }
        requestLogApplyDisplaySettings(requestLogLastDisplaySettings);
      }
    } else {
      renderRequestLogRows(incoming);
    }
    for (const entry of incoming) requestLogLatestID = Math.max(requestLogLatestID, Number(entry.id || 0));
    const newest = incoming[0];
    const newestCursorAt = Number(newest?.cursor_at_ms || 0);
    if (newestCursorAt > 0 && Number(newest?.id || 0) > 0) {
      requestLogLatestCursor = btoa(`${newestCursorAt}:${Number(newest.id)}`).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
    }
    if (scroller && preserveViewport) {
      const addedHeight = Math.max(0, scroller.scrollHeight - previousScrollHeight);
      scroller.scrollTop = previousScrollTop + addedHeight;
    }
    const dropped = Number(response.dropped_logs || 0);
    const visibleCount = body.querySelectorAll('tr[data-log-id]').length;
    document.getElementById('request-log-summary').textContent = dropped > 0
      ? `显示 ${visibleCount} 条，繁忙时已丢弃 ${dropped} 条`
      : `显示 ${visibleCount} 条（最多 500 条）`;
  } catch (error) {
    if (generation !== requestLogLoadGeneration) return;
    if (!body.querySelector('tr[data-log-id]')) {
      body.innerHTML = '<tr><td colspan="9" class="request-log-empty request-log-error">日志读取失败</td></tr>';
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
  requestLogEntries = new Map();
  if (!logs.length) {
    body.innerHTML = '<tr><td colspan="9" class="request-log-empty">当前条件下暂无日志</td></tr>';
    return;
  }
  body.innerHTML = logs.map(requestLogRowHTML).join('');
  // Column visibility lives on the parsed cells, so a fresh innerHTML drops it
  // while the header stays in its previous state. The incremental path re-applies
  // the settings after inserting and the UA width is re-applied on every render;
  // this full-render path has to match, or a filter, refresh or search makes the
  // operator-hidden columns reappear and the table misalign.
  requestLogApplyDisplaySettings(requestLogLastDisplaySettings);
}

function requestLogRowHTML(entry) {
    if (Number(entry.id) > 0) requestLogEntries.set(Number(entry.id), entry);
    const status = Number(entry.status_code || 0);
    const recordedAtMS = Number(entry.recorded_at_ms || 0);
    const exactTime = recordedAtMS ? requestLogFormatDateTime(recordedAtMS) : '未写入时间线';
    const requestTitle = `${String(entry.method || 'GET')} ${String(entry.path || '/')}`;
    return `
      <tr data-log-id="${esc(entry.id || '')}" title="${esc(requestTitle)}">
        <td data-log-field="site"><button type="button" class="management-log-open" data-log-detail-id="${esc(entry.id || '')}" aria-label="查看 ${esc(entry.site_name || '站点')} 请求详情"><strong>${esc(entry.site_name || '—')}</strong><span class="management-log-path">${esc(requestTitle)}</span></button></td>
        <td data-log-field="node"><span class="request-log-node">${esc(entry.final_node || '—')}</span></td>
        <td data-log-field="category"><span class="request-log-category">${esc(requestLogCategoryLabel(entry.resource_category))}</span></td>
        <td data-log-field="status"><span class="request-log-status ${requestLogStatusClass(status)}">${status || '—'}</span></td>
        <td data-log-field="ip"><span class="request-log-ip mono">${esc(entry.client_ip || '—')}</span><small class="request-log-region">${esc(entry.client_region || '')}</small></td>
        <td data-log-field="ua"><span class="request-log-ua">${esc(entry.user_agent || '—')}</span></td>
        <td data-log-field="upstream-ua"><span class="request-log-ua">${esc(entry.upstream_user_agent || '—')}</span></td>
        <td data-log-field="backend-address"><span class="request-log-backend mono">${esc(entry.backend_address || '—')}</span></td>
        <td data-log-field="timeline"><time class="request-log-time"${recordedAtMS ? ` datetime="${new Date(recordedAtMS).toISOString()}"` : ''} title="${esc(exactTime)}">${esc(requestLogRelativeTime(recordedAtMS))}</time></td>
      </tr>
    `;
}

function showRequestLogDetails(entry) {
  const time = Number(entry.recorded_at_ms || 0);
  const fields = [
    ['站点', entry.site_name], ['请求', `${entry.method || 'GET'} ${entry.path || '/'}`],
    ['节点', entry.final_node, requestLogDisplaySettings.node],
    ['资源类型', requestLogCategoryLabel(entry.resource_category), requestLogDisplaySettings.category],
    ['响应状态', entry.status_code, requestLogDisplaySettings.status],
    ['记录时间', time ? requestLogFormatDateTime(time) : '未写入时间线', requestLogDisplaySettings.timeline],
    ['客户端 IP', `${entry.client_ip || '—'}${entry.client_region ? ` · ${entry.client_region}` : ''}`, requestLogDisplaySettings.client_ip],
    ['客户端 UA', entry.user_agent, requestLogDisplaySettings.ua],
    ['上游 UA', entry.upstream_user_agent, requestLogDisplaySettings.upstream_ua],
    ['后端地址', entry.backend_address, requestLogDisplaySettings.backend_address],
  ].filter(([, , visible]) => visible !== false);
  document.getElementById('modal-title').textContent = '请求详情';
  document.getElementById('modal-body').innerHTML = `<dl class="management-log-detail">${fields.map(([label, value]) => `<div><dt>${esc(label)}</dt><dd>${esc(value == null || value === '' ? '—' : value)}</dd></div>`).join('')}</dl>`;
  document.getElementById('modal-footer').innerHTML = '<button type="button" class="console-button" id="request-log-detail-close">关闭</button>';
  openModal({ closeOnBackdrop: true, modalClass: 'management-log-modal' });
  document.getElementById('request-log-detail-close').onclick = closeModal;
}

async function clearRequestLogs() {
  if (!confirm('确认清空全部请求日志？该操作不可撤销。')) return;
  try {
    await API.clearRequestLogs();
    Toast.success('请求日志已清空');
    requestLogLatestID = 0;
    requestLogLatestCursor = '';
    loadRequestLogs();
  } catch (error) {
    Toast.error(error.message);
  }
}

async function clearAssetCache() {
  if (!confirm('确认清除所有站点的图片与静态资源缓存？站点配置和请求日志不会受到影响。')) return;
  try {
    const result = await API.clearAssetCache();
    if (result?.status === 'pending' && Number(result.nodes_pending || 0) > 0) {
      Toast.success(`主控缓存已清除，${result.nodes_pending} 个节点将在下次配置同步后清除`);
    } else {
      Toast.success('资产缓存已清除');
    }
  } catch (error) {
    Toast.error(error.message);
  }
}
