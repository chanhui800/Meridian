let requestLogCategoryFilter = 'all';
let requestLogStatusFilter = 'all';
let requestLogSearchTimer = null;
let requestLogRefreshTimer = null;
let requestLogLoadGeneration = 0;

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
    video: '视频流',
    image: '图片海报',
    api: '常规 API',
    auth: '用户认证',
  })[category] || '常规 API';
}

function requestLogRelativeTime(timestamp, now) {
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
          <button type="button" class="request-log-pill active" data-category="all">全部模式</button>
          <button type="button" class="request-log-pill" data-category="playback">只看播放信息</button>
          <button type="button" class="request-log-pill" data-category="video">只看视频流</button>
          <button type="button" class="request-log-pill" data-category="image">只看图片海报</button>
          <button type="button" class="request-log-pill" data-category="api">只看常规 API</button>
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

      <div class="request-log-actions">
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

    <section class="request-log-table-card fade-up">
      <div class="request-log-table-scroll">
        <table class="request-log-table">
          <thead>
            <tr>
              <th>节点</th>
              <th>资源类别</th>
              <th>状态</th>
              <th>客户端 IP</th>
              <th>UA</th>
              <th>时间线</th>
            </tr>
          </thead>
          <tbody id="request-log-body">
            <tr><td colspan="6" class="request-log-empty">正在加载…</td></tr>
          </tbody>
        </table>
      </div>
    </section>
  `;

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
  loadRequestLogs();
  if (requestLogRefreshTimer) clearInterval(requestLogRefreshTimer);
  requestLogRefreshTimer = setInterval(() => {
    if (Router.current === 'request-logs') loadRequestLogs();
  }, 5000);
}

function stopRequestLogRefresh() {
  if (requestLogRefreshTimer) {
    clearInterval(requestLogRefreshTimer);
    requestLogRefreshTimer = null;
  }
  if (requestLogSearchTimer) {
    clearTimeout(requestLogSearchTimer);
    requestLogSearchTimer = null;
  }
}

function setRequestLogActivePill(containerId, activeButton) {
  document.querySelectorAll(`#${containerId} .request-log-pill`).forEach(button => {
    button.classList.toggle('active', button === activeButton);
  });
}

async function loadRequestLogs() {
  const body = document.getElementById('request-log-body');
  if (!body) return;
  const from = document.getElementById('request-log-from').value;
  const to = document.getElementById('request-log-to').value;
  const range = requestLogRangeMilliseconds(from, to);
  if (!range.from_ms || !range.to_ms || range.from_ms > range.to_ms) {
    Toast.error('请选择有效的日志日期范围');
    return;
  }
  const generation = ++requestLogLoadGeneration;
  body.innerHTML = '<tr><td colspan="6" class="request-log-empty">正在加载…</td></tr>';
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
    const dropped = Number(response.dropped_logs || 0);
    document.getElementById('request-log-summary').textContent = dropped > 0
      ? `显示 ${response.logs.length} 条，繁忙时已丢弃 ${dropped} 条`
      : `显示 ${response.logs.length} 条（最多 500 条）`;
  } catch (error) {
    if (generation !== requestLogLoadGeneration) return;
    body.innerHTML = '<tr><td colspan="6" class="request-log-empty request-log-error">日志读取失败</td></tr>';
    Toast.error(error.message);
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
    const exactTime = new Date(Number(entry.recorded_at_ms || 0)).toLocaleString('zh-CN', { hour12: false });
    const requestTitle = `${String(entry.method || 'GET')} ${String(entry.path || '/')}`;
    return `
      <tr>
        <td><span class="request-log-node" title="${esc(entry.site_name || '')}">${esc(entry.site_name || '—')}</span></td>
        <td><span class="request-log-category" title="${esc(requestTitle)}">${esc(requestLogCategoryLabel(entry.resource_category))}</span></td>
        <td><span class="request-log-status ${requestLogStatusClass(status)}">${status || '—'}</span></td>
        <td><span class="request-log-ip mono">${esc(entry.client_ip || 'unknown')}</span></td>
        <td><span class="request-log-ua" title="${esc(entry.user_agent || '未提供 UA')}">${esc(entry.user_agent || '未提供 UA')}</span></td>
        <td><time class="request-log-time" datetime="${new Date(Number(entry.recorded_at_ms || 0)).toISOString()}" title="${esc(exactTime)}">${esc(requestLogRelativeTime(entry.recorded_at_ms))}</time></td>
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
