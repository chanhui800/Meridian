let requestLogCategoryFilter = 'all';
let requestLogStatusFilter = 'all';
let requestLogSearchTimer = null;
let requestLogRefreshTimer = null;
let requestLogLoadGeneration = 0;
let requestLogLoading = false;
let requestLogReloadQueued = false;
let requestLogLatestID = 0;
let requestLogLatestCursor = '';
let requestLogAppliedFilters = '';
let requestLogDisplaySettings = { node: true, category: true, status: true, client_ip: true, ua: true, upstream_ua: true, backend_address: true, timeline: true };
let requestLogLastDisplaySettings = null;
let requestLogEntries = new Map();
let requestLogDetailEntry = null;
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
  requestLogRefreshOpenDetails();
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

function requestLogHasDiagnostics(entry) {
  return Number(entry?.diagnostics_version) === 1;
}

function requestLogDiagnosticEnum(value, labels) {
  return Object.prototype.hasOwnProperty.call(labels, value) ? labels[value] : '未报告';
}

function requestLogTerminationLabel(value) {
  return requestLogDiagnosticEnum(value, {
    completed: '已完成', client_canceled: '客户端已取消', client_deadline: '客户端超时',
    upstream_error: '上游处理失败', downstream_error: '正文写入失败',
    proxy_aborted: '传输中断', site_stopping: '站点停止',
  });
}

function requestLogStatusLabel(entry) {
  const status = Number(entry.status_code || 0);
  const actual = Number(entry.response_status_code || 0);
  if (requestLogHasDiagnostics(entry) && status && actual >= 100 && actual <= 599 && actual !== status) {
    if (entry.termination_reason === 'client_canceled') return `${actual}·已取消`;
    if (entry.termination_reason === 'client_deadline') return `${actual}·已超时`;
    return `${actual}·${status}`;
  }
  return String(status || '—');
}

function requestLogDiagnosticMilliseconds(value) {
  return typeof value === 'number' && Number.isFinite(value) && value >= 0 && value <= 604800000 ? `${value} ms` : '未报告';
}

function requestLogDiagnosticBytes(value) {
  return typeof value === 'number' && Number.isFinite(value) && value >= 0 ? `${value.toLocaleString('zh-CN')} B` : '未报告';
}

function requestLogDiagnosticCount(value) {
  return typeof value === 'number' && Number.isSafeInteger(value) && value >= 0 ? `${value} 次` : '未报告';
}

function requestLogConnectionLabel(value) {
  return requestLogDiagnosticEnum(value, { new: '新建连接', reused: '复用连接', mixed: '新建与复用均有' });
}

function requestLogDiagnosticAuthority(value) {
  return typeof value === 'string' && value.length <= 1024 && /^https?:\/\/[^/?#@\s"'<>\\]+$/i.test(value)
    ? value : '未报告';
}

function requestLogOriginProcessHTML(entry) {
  const settings = requestLogDisplaySettings;
  if (!settings.backend_address && !settings.timeline && !settings.status && !settings.category) return '';
  if (!requestLogHasDiagnostics(entry)) {
    return '<section class="management-log-origin"><h4>回源过程</h4><p class="management-log-origin-empty">未报告（旧 Agent 或历史记录）</p></section>';
  }
  const summary = settings.backend_address ? `<dl class="management-log-origin-summary"><div><dt>跳转次数</dt><dd>${esc(requestLogDiagnosticCount(entry.redirect_count))}</dd></div><div><dt>重试次数</dt><dd>${esc(requestLogDiagnosticCount(entry.retry_count))}</dd></div><div><dt>最后连接</dt><dd>${esc(requestLogConnectionLabel(entry.final_connection_state))}</dd></div></dl>` : '';
  const reported = Array.isArray(entry.upstream_attempts) ? entry.upstream_attempts.filter(attempt => attempt && typeof attempt === 'object' && !Array.isArray(attempt)) : [];
  const attempts = reported.length > 8 ? reported.slice(0, 7).concat(reported.at(-1)) : reported;
  let previousIndex = 0;
  const steps = attempts.map(attempt => {
    const index = typeof attempt.attempt_index === 'number' && Number.isSafeInteger(attempt.attempt_index) && attempt.attempt_index > 0 ? attempt.attempt_index : 0;
    const gap = index && previousIndex && index > previousIndex + 1 ? `<li class="management-log-origin-gap">${settings.backend_address ? `中间 ${esc(index - previousIndex - 1)} 次回源未展开` : '部分回源步骤未展开'}</li>` : '';
    previousIndex = index;
    const role = settings.category ? `<span class="management-log-origin-role">${esc(requestLogDiagnosticEnum(attempt.source_role, { configured: '配置上游', dynamic: '动态回源' }))}</span>` : '';
    const status = typeof attempt.response_status_code === 'number' && Number.isInteger(attempt.response_status_code) && attempt.response_status_code >= 100 && attempt.response_status_code <= 599 ? String(attempt.response_status_code) : '未报告';
    const outcome = requestLogDiagnosticEnum(attempt.outcome, { response: '已收到响应', error: '回源失败', canceled: '已取消', deadline: '回源超时' });
    const result = settings.status ? `<span class="management-log-origin-result">响应 ${esc(status)} · ${esc(outcome)}</span>` : '';
    const duration = settings.timeline ? `<span class="management-log-origin-duration">回源耗时 ${esc(requestLogDiagnosticMilliseconds(attempt.duration_ms))}</span>` : '';
    const address = settings.backend_address ? `<div class="management-log-origin-address"><span>${esc(requestLogDiagnosticAuthority(attempt.authority))}</span><small>${esc(requestLogConnectionLabel(attempt.connection_state))}</small></div>` : '';
    const stages = settings.timeline ? [
      ['请求内起点', attempt.started_ms], ['首个响应字节', attempt.first_byte_ms], ['最终响应头', attempt.response_headers_ms],
      ...(settings.backend_address ? [['DNS', attempt.dns_duration_ms], ['TCP', attempt.tcp_duration_ms], ['TLS', attempt.tls_duration_ms]] : []),
    ] : [];
    return `${gap}<li class="management-log-origin-step"${index ? ` value="${esc(index)}"` : ''}><div class="management-log-origin-step-heading"><strong>${index ? `第 ${esc(index)} 次回源` : '回源次序未报告'}</strong>${role}${result}${duration}</div>${address}${stages.length ? `<dl class="management-log-origin-stages">${stages.map(([label, value]) => `<div><dt>${esc(label)}</dt><dd>${esc(requestLogDiagnosticMilliseconds(value))}</dd></div>`).join('')}</dl>` : ''}</li>`;
  }).join('');
  const note = settings.timeline && attempts.length ? '<p class="management-log-origin-note">起点相对请求接入；首个响应字节与最终响应头相对本次回源。回源耗时截至响应头或失败返回。首个响应字节可能是信息响应，正文写入时间属于代理侧记录。</p>' : '';
  return `<section class="management-log-origin"><h4>回源过程</h4>${summary}${attempts.length ? `<ol class="management-log-origin-steps">${steps}</ol>` : '<p class="management-log-origin-empty">逐次回源未报告</p>'}${note}</section>`;
}

function requestLogCacheReasonLabel(value) {
  return requestLogDiagnosticEnum(value, {
    disabled: '缓存已关闭', method: '请求方法不适用', rule_no_match: '未匹配规则', range: 'Range 请求',
    dynamic: '动态回源', head_no_fill: 'HEAD 请求不填充', response_status: '响应状态不适用',
    set_cookie: '响应携带 Cookie', no_store: '禁止存储', no_cache: '需要重新验证', private: '私有响应',
    vary: '存在未支持的响应变体', mime: '响应类型不适用', too_large: '超过大小限制', empty_body: '正文为空',
    permission: '缓存权限不足', no_space: '缓存空间不足', readonly: '缓存目录只读', io: '读取或写入失败',
    expired: '缓存已过期', invalid_entry: '缓存记录无效', evicted: '已按容量限制淘汰',
    canceled: '请求已取消', incomplete: '正文未完整读取', length_mismatch: '正文长度不符', generation_changed: '缓存配置已更新',
  });
}

function requestLogDiagnosticFields(entry) {
  const settings = requestLogDisplaySettings;
  if (!requestLogHasDiagnostics(entry)) return [['诊断信息', '未报告（旧 Agent 或历史记录）', settings.status || settings.timeline || settings.category || settings.backend_address]];
  const actual = Number(entry.response_status_code || 0);
  return [
    ['实际响应状态', actual >= 100 && actual <= 599 ? actual : '未报告', settings.status],
    ['结束原因', requestLogTerminationLabel(entry.termination_reason), settings.status],
    ['请求总耗时', requestLogDiagnosticMilliseconds(entry.duration_ms), settings.timeline],
    ['末次上游首字节', requestLogDiagnosticMilliseconds(entry.upstream_first_byte_ms), settings.timeline],
    ['首个正文写入', requestLogDiagnosticMilliseconds(entry.body_first_byte_ms), settings.timeline],
    ['正文已写入', requestLogDiagnosticBytes(entry.body_bytes_written), settings.timeline],
    ['Range 请求', requestLogDiagnosticEnum(String(entry.range_mode), { 1: '无 Range', 2: '有 Range' }), settings.category],
    ['连接使用', requestLogConnectionLabel(entry.connection_state), settings.backend_address],
    ['DNS 等待累计', requestLogDiagnosticMilliseconds(entry.dns_duration_ms), settings.timeline && settings.backend_address],
    ['TCP 连接累计', requestLogDiagnosticMilliseconds(entry.tcp_duration_ms), settings.timeline && settings.backend_address],
    ['TLS 握手累计', requestLogDiagnosticMilliseconds(entry.tls_duration_ms), settings.timeline && settings.backend_address],
    ['缓存查找', requestLogDiagnosticEnum(entry.cache_lookup, { disabled: '已关闭', bypass: '已绕过', hit: '命中', miss: '未命中', read_error: '读取失败' }), settings.category],
    ['查找原因', requestLogCacheReasonLabel(entry.cache_lookup_reason || entry.cache_reason), settings.category],
    ['缓存写入', requestLogDiagnosticEnum(entry.cache_write, { not_attempted: '未尝试', stored: '已存储', skipped: '已跳过', write_error: '写入失败' }), settings.category],
    ['写入原因', requestLogCacheReasonLabel(entry.cache_write_reason), settings.category],
    ['响应内容类型', requestLogDiagnosticEnum(entry.response_type, {
      json: 'JSON', manifest: '播放清单', subtitle: '字幕', image: '图片', video: '视频', audio: '音频', font: '字体',
      script: '脚本', style: '样式', html: 'HTML', text: '文本', binary: '二进制', other: '其他',
    }), settings.category],
  ];
}

function renderRequestLogs() {
  const page = document.getElementById('page-request-logs');
  requestLogApplyUAWidth();
  requestLogLatestID = 0;
  requestLogLatestCursor = '';
  requestLogAppliedFilters = '';
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
  const overlay = document.getElementById('modal-overlay');
  const modal = document.getElementById('modal');
  if (overlay?.classList.contains('active') && modal?.classList.contains('management-log-modal')) {
    closeModal({ restoreFocus: false });
  }
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
  requestLogDetailEntry = null;
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
    if (options.incremental !== true) requestLogLoadGeneration += 1;
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
  const category = requestLogCategoryFilter;
  const status = requestLogStatusFilter;
  const query = document.getElementById('request-log-search').value.trim();
  const filterKey = JSON.stringify([from, to, category, status, query]);
  const incremental = options.incremental === true && requestLogLatestID > 0 && requestLogAppliedFilters === filterKey;
  if (!incremental) {
    requestLogLatestID = 0;
    requestLogLatestCursor = '';
  }
  const filtersUnchanged = () => from === document.getElementById('request-log-from')?.value
    && to === document.getElementById('request-log-to')?.value
    && category === requestLogCategoryFilter && status === requestLogStatusFilter
    && query === document.getElementById('request-log-search')?.value.trim();
  const generation = ++requestLogLoadGeneration;
  requestLogLoading = true;
  if (options.showLoading === true && !body.querySelector('tr[data-log-id]')) {
    body.innerHTML = '<tr><td colspan="9" class="request-log-empty">正在加载…</td></tr>';
  }
  try {
    const response = await API.getRequestLogs({
      ...range,
      category,
      status,
      q: query,
      // DESC polling must cover the entire retained window before advancing
      // to its newest cursor, including bursts larger than the old 100-row page.
      limit: 500,
      ...(incremental ? (requestLogLatestCursor ? { after_cursor: requestLogLatestCursor } : { after_id: requestLogLatestID }) : {}),
    });
    if (generation !== requestLogLoadGeneration || Router.current !== 'request-logs' || !response) return;
    if (!filtersUnchanged()) {
      requestLogReloadQueued = true;
      return;
    }
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
      requestLogAppliedFilters = filterKey;
    }
    for (const entry of incoming) requestLogLatestID = Math.max(requestLogLatestID, Number(entry.id || 0));
    const newest = incoming[0];
    const newestCursorAt = Number(newest?.cursor_at_ms || 0);
    if (newestCursorAt > 0 && Number(newest?.id || 0) > 0) {
      requestLogLatestCursor = btoa(`${newestCursorAt}:${Number(newest.id)}`).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
    }
    const dropped = Number(response.dropped_logs || 0);
    const visibleCount = body.querySelectorAll('tr[data-log-id]').length;
    document.getElementById('request-log-summary').textContent = dropped > 0
      ? `显示 ${visibleCount} 条，繁忙时已丢弃 ${dropped} 条`
      : `显示 ${visibleCount} 条（最多 500 条）`;
  } catch (error) {
    if (generation !== requestLogLoadGeneration || Router.current !== 'request-logs') return;
    if (!filtersUnchanged()) {
      requestLogReloadQueued = true;
      return;
    }
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
    const statusLabel = requestLogStatusLabel(entry);
    const recordedAtMS = Number(entry.recorded_at_ms || 0);
    const exactTime = recordedAtMS ? requestLogFormatDateTime(recordedAtMS) : '未写入时间线';
    const requestTitle = `${String(entry.method || 'GET')} ${String(entry.path || '/')}`;
    return `
      <tr data-log-id="${esc(entry.id || '')}" title="${esc(requestTitle)}">
        <td data-log-field="site"><button type="button" class="management-log-open" data-log-detail-id="${esc(entry.id || '')}" aria-label="查看 ${esc(entry.site_name || '站点')} 请求详情${requestLogDisplaySettings.status ? ` · ${esc(statusLabel)}` : ''}"><strong>${esc(entry.site_name || '—')}</strong><span class="management-log-path">${esc(requestTitle)}</span></button></td>
        <td data-log-field="node"><span class="request-log-node">${esc(entry.final_node || '—')}</span></td>
        <td data-log-field="category"><span class="request-log-category">${esc(requestLogCategoryLabel(entry.resource_category))}</span></td>
        <td data-log-field="status"><span class="request-log-status ${requestLogStatusClass(status)}" title="${esc(statusLabel)}">${esc(statusLabel)}</span></td>
        <td data-log-field="ip"><span class="request-log-ip mono">${esc(entry.client_ip || '—')}</span><small class="request-log-region">${esc(entry.client_region || '')}</small></td>
        <td data-log-field="ua"><span class="request-log-ua">${esc(entry.user_agent || '—')}</span></td>
        <td data-log-field="upstream-ua"><span class="request-log-ua">${esc(entry.upstream_user_agent || '—')}</span></td>
        <td data-log-field="backend-address"><span class="request-log-backend mono">${esc(entry.backend_address || '—')}</span></td>
        <td data-log-field="timeline"><time class="request-log-time"${recordedAtMS ? ` datetime="${new Date(recordedAtMS).toISOString()}"` : ''} title="${esc(exactTime)}">${esc(requestLogRelativeTime(recordedAtMS))}</time></td>
      </tr>
    `;
}

function requestLogDetailsHTML(entry) {
  const time = Number(entry.recorded_at_ms || 0);
  const fields = [
    ['站点', entry.site_name], ['请求', `${entry.method || 'GET'} ${entry.path || '/'}`],
    ['节点', entry.final_node, requestLogDisplaySettings.node],
    ['资源类型', requestLogCategoryLabel(entry.resource_category), requestLogDisplaySettings.category],
    ['日志状态', entry.status_code, requestLogDisplaySettings.status],
    ['记录时间', time ? requestLogFormatDateTime(time) : '未写入时间线', requestLogDisplaySettings.timeline],
    ['客户端 IP', `${entry.client_ip || '—'}${entry.client_region ? ` · ${entry.client_region}` : ''}`, requestLogDisplaySettings.client_ip],
    ['客户端 UA', entry.user_agent, requestLogDisplaySettings.ua],
    ['上游 UA', entry.upstream_user_agent, requestLogDisplaySettings.upstream_ua],
    ['后端地址', entry.backend_address, requestLogDisplaySettings.backend_address],
  ].filter(([, , visible]) => visible !== false);
  const diagnostics = requestLogDiagnosticFields(entry).filter(([, , visible]) => visible !== false);
  const timingNote = requestLogHasDiagnostics(entry) && requestLogDisplaySettings.timeline
    ? `<p class="management-footnote">请求总耗时、末次上游首字节与首个正文写入从请求接入起计算${requestLogDisplaySettings.backend_address ? '；DNS、TCP、TLS 为整条请求的累计耗时' : ''}。未观测到的阶段显示“未报告”。</p>` : '';
  const fieldHTML = values => values.map(([label, value]) => `<div><dt>${esc(label)}</dt><dd>${esc(value == null || value === '' ? '—' : value)}</dd></div>`).join('');
  return `<dl class="management-log-detail">${fieldHTML(fields)}</dl>${requestLogOriginProcessHTML(entry)}${diagnostics.length ? `<section class="management-log-diagnostics"><h4>请求与缓存</h4><dl class="management-log-detail is-diagnostics">${fieldHTML(diagnostics)}</dl>${timingNote}</section>` : ''}`;
}

function requestLogRefreshOpenDetails() {
  if (!requestLogDetailEntry) return;
  const modal = document.getElementById('modal');
  const overlay = document.getElementById('modal-overlay');
  if (!modal?.classList?.contains('management-log-modal') || !overlay?.classList?.contains('active')) return;
  if (typeof Router !== 'undefined' && Router.current !== 'request-logs') return;
  const body = document.getElementById('modal-body');
  if (body) body.innerHTML = requestLogDetailsHTML(requestLogDetailEntry);
}

function showRequestLogDetails(entry) {
  requestLogDetailEntry = entry;
  document.getElementById('modal-title').textContent = '请求详情';
  document.getElementById('modal-body').innerHTML = requestLogDetailsHTML(entry);
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
