// Sites management page
const SITE_REFRESH_INTERVAL_MS = 5000;
let siteSortingCleanup = null;
let siteRefreshTimer = 0;
let siteLoadGeneration = 0;
let siteIconPack = { name: '', description: '', source_url: '', icons: [] };
let siteManagementExpanded = new Set();
let siteManagementLatency = new Map();

function normalizeSiteIconPack(value) {
  const icons = Array.isArray(value && value.icons) ? value.icons.map(icon => ({
    name: String(icon && icon.name || '').trim(),
    url: String(icon && icon.url || '').trim(),
  })).filter(icon => icon.name && /^https:\/\//i.test(icon.url)) : [];
  return {
    name: String(value && value.name || '').trim(),
    description: String(value && value.description || '').trim(),
    source_url: String(value && value.source_url || '').trim(),
    updated_at: String(value && value.updated_at || '').trim(),
    icons,
  };
}

function siteIconFor(site) {
  const name = String(site && site.icon_name || '').trim();
  const url = String(site && site.icon_url || '').trim();
  return name && /^https:\/\//i.test(url) ? { name, url } : null;
}

function renderSiteIcon(site, sizeClass = '') {
  const icon = siteIconFor(site);
  const fallback = esc(String(site && site.name || '?').trim().slice(0, 1) || '?');
  if (!icon) return `<span class="site-icon ${sizeClass}" aria-hidden="true"><span class="site-icon-fallback">${fallback}</span></span>`;
  return `<span class="site-icon ${sizeClass}" title="${esc(icon.name)}"><img class="site-icon-image" src="${esc(icon.url)}" alt="" loading="lazy" referrerpolicy="no-referrer"><span class="site-icon-fallback" hidden>${fallback}</span></span>`;
}

function renderSiteIconButton(site) {
  const label = siteIconFor(site) ? `更换 ${esc(site.name)} 的站点图标` : `为 ${esc(site.name)} 设置站点图标`;
  return `<button type="button" class="site-icon-button" data-site-icon-id="${esc(String(site && site.id || ''))}" aria-label="${label}" title="${label}">${renderSiteIcon(site)}</button>`;
}

function bindSiteIconFallbacks(root) {
  if (!root) return;
  root.querySelectorAll('.site-icon-image').forEach(image => {
    image.addEventListener('error', () => {
      image.hidden = true;
      const fallback = image.parentElement && image.parentElement.querySelector('.site-icon-fallback');
      if (fallback) fallback.hidden = false;
    }, { once: true });
  });
}

function normalizedMediaLibraryCount(value) {
  const count = Number(value);
  return Number.isSafeInteger(count) && count >= 0 ? count : null;
}

function renderSiteMediaLibraryCounts(site) {
  const items = [
    ['电影', normalizedMediaLibraryCount(site && site.media_movie_count)],
    ['剧集', normalizedMediaLibraryCount(site && site.media_series_count)],
    ['集', normalizedMediaLibraryCount(site && site.media_episode_count)],
  ];
  if (items.some(([, count]) => count === null)) {
    return '<div class="site-media-counts is-pending"><span class="site-media-icon" aria-hidden="true"><svg viewBox="0 0 24 24" aria-hidden="true"><rect x="3" y="5" width="18" height="14" rx="2"/><path d="m10 9 5 3-5 3Z"/></svg></span><span>媒体库</span><strong>待同步</strong></div>';
  }
  return `<div class="site-media-counts" aria-label="媒体库数量"><span class="site-media-icon" aria-hidden="true"><svg viewBox="0 0 24 24" aria-hidden="true"><rect x="3" y="5" width="18" height="14" rx="2"/><path d="m10 9 5 3-5 3Z"/></svg></span>${items.map(([label, count]) => `<span>${label} <strong>${count.toLocaleString('zh-CN')}</strong></span>`).join('')}</div>`;
}

function siteAccountRetentionStatus(site, nowMS = Date.now()) {
  const days = Number(site && site.account_retention_days);
  const startedAtMS = Number(site && site.account_retention_started_at_ms);
  if (!Number.isSafeInteger(days) || days <= 0 || !Number.isFinite(startedAtMS) || startedAtMS <= 0) {
    return { enabled: false, remainingDays: 0, urgent: false, overdue: false };
  }
  const remainingMS = startedAtMS + days * 86400000 - Number(nowMS);
  const remainingDays = Math.max(0, Math.ceil(remainingMS / 86400000));
  return {
    enabled: true,
    remainingDays,
    urgent: remainingDays <= 7,
    overdue: remainingMS <= 0,
  };
}

function renderSiteAccountRetention(site) {
  const status = siteAccountRetentionStatus(site);
  if (!status.enabled) return '';
  const value = status.overdue ? '已到期' : `剩余 ${status.remainingDays} 天`;
  return `
    <div class="site-row site-retention-row ${status.urgent ? 'is-urgent' : ''}">
      <span class="site-row-label">保号提醒</span>
      <span class="site-retention-value"><strong>${value}</strong></span>
    </div>`;
}

function renderSiteAccessVisibilityIcon(hidden) {
	if (hidden) {
		return '<svg class="site-access-eye site-access-eye-closed" viewBox="0 0 24 24" aria-hidden="true"><path d="M10.733 5.076a10.744 10.744 0 0 1 11.205 6.575 1 1 0 0 1 0 .696 10.8 10.8 0 0 1-1.444 2.49"></path><path d="M14.084 14.158a3 3 0 0 1-4.242-4.242"></path><path d="M17.479 17.499a10.75 10.75 0 0 1-15.417-5.151 1 1 0 0 1 0-.696 10.75 10.75 0 0 1 4.446-5.143"></path><path d="m2 2 20 20"></path></svg>';
	}
	return '<svg class="site-access-eye site-access-eye-open" viewBox="0 0 24 24" aria-hidden="true"><path d="M2.062 12.348a1 1 0 0 1 0-.696 10.75 10.75 0 0 1 19.876 0 1 1 0 0 1 0 .696 10.75 10.75 0 0 1-19.876 0"></path><circle cx="12" cy="12" r="3"></circle></svg>';
}

function siteCardsRenderSignature(sites, activeSiteIDs) {
  const active = activeSiteIDs instanceof Set ? activeSiteIDs : new Set();
  return JSON.stringify((Array.isArray(sites) ? sites : []).map(site => {
    // The early return in renderSiteCards skips BOTH the repaint and the handler
    // re-binding, and every edit/icon handler closes over the sitesById map built
    // on that path. Anything the card renders or the edit modal pre-fills must
    // therefore be part of this signature: a field missing here leaves the stale
    // object in place, so re-opening 编辑 shows the pre-save value and the next
    // save silently reverts the operator's change.
    const retention = siteAccountRetentionStatus(site);
    return [
      site && site.id, site && site.name, site && site.target_url, site && site.public_host,
      site && site.icon_name, site && site.icon_url,
      site && site.ingress_mode, site && site.listen_port, site && site.ua_mode,
      site && site.running, site && site.enabled, site && site.traffic_quota, site && site.traffic_used,
      site && site.media_movie_count, site && site.media_series_count, site && site.media_episode_count,
      site && site.account_retention_days, site && site.account_retention_started_at_ms,
      site && site.account_retention_last_completed_at_ms,
      retention.enabled, retention.remainingDays, retention.overdue,
      Array.isArray(site && site.upstream_headers) ? site.upstream_headers.length : 0,
      // Edit-modal inputs: changing any of these without a repaint is what makes a
      // later save write the previous value back.
      site && site.playback_target_url, site && site.playback_mode,
      site && site.main_video_stream_mode, site && site.client_ip_mode,
      site && site.speed_limit, site && site.watch_history_enabled,
      site && site.path_prefix, site && site.primary_line_name,
      site && site.asset_cache_enabled, site && site.asset_cache_ttl_sec,
      site && site.asset_cache_max_bytes, site && site.asset_cache_rules,
      site && site.custom_user_agent, site && site.custom_client, site && site.custom_version,
      JSON.stringify(site && site.failover_lines || null),
      JSON.stringify(site && site.stream_hosts || null),
      active.has(String(site && site.id)),
    ];
  }));
}

function renderSites() {
  const page = document.getElementById('page-sites');
  stopSitesRefresh();
  siteManagementExpanded = new Set();
  page.innerHTML = `
    <div class="console-page management-page">
      <header class="console-heading">
        <div><p class="console-eyebrow">站点与入口</p><h1>站点管理</h1><p class="console-lead">集中管理访问入口、回源线路与站点策略。</p></div>
        <button type="button" class="console-button is-primary" id="btn-add-site"><svg viewBox="0 0 24 24" aria-hidden="true"><path d="M12 5v14M5 12h14"/></svg>添加站点</button>
      </header>
      <section class="console-panel management-sites" aria-label="站点列表">
        <div class="management-toolbar">
          <label class="management-search"><span class="sr-only">搜索站点</span><svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="11" cy="11" r="7"/><path d="m16 16 5 5"/></svg><input class="form-input" id="sites-search" type="search" placeholder="搜索站点、图标或回源地址" autocomplete="off"></label>
          <div class="management-toolbar-actions"><button class="console-button" id="btn-site-icon-pack" type="button">管理图标包</button><button class="console-button" id="btn-test-all-sites" type="button">全部测速</button><button class="console-button management-refresh" id="sites-refresh" type="button" aria-label="刷新站点"><svg viewBox="0 0 24 24" aria-hidden="true"><path d="M20 7v5h-5M4 17v-5h5M6 6a8 8 0 0 1 14 6M4 12a8 8 0 0 0 14 6"/></svg></button></div>
        </div>
        <div class="management-list-meta"><span id="sites-count">正在读取站点…</span><span id="sites-load-state" class="console-muted" role="status">每 5 秒更新</span></div>
        <div class="management-site-columns" aria-hidden="true"><span>站点</span><span>运行状态</span><span>已用流量</span><span>回源延迟</span><span></span></div>
        <div class="sites-grid management-site-list" id="sites-grid"><div class="console-empty">正在读取站点…</div></div>
        <div class="console-empty" id="sites-no-match" hidden>没有匹配的站点，试试其他关键词。</div>
      </section>
      <p class="management-footnote">拖动站点左侧的手柄调整顺序；聚焦手柄后也可使用上下方向键。</p>
    </div>
  `;

  document.getElementById('btn-add-site').onclick = () => showSiteModal();
  document.getElementById('btn-site-icon-pack').onclick = showSiteIconPackModal;
  document.getElementById('btn-test-all-sites').onclick = testAllSitesLatency;
  document.getElementById('sites-refresh').onclick = () => loadSites();
  document.getElementById('sites-search').addEventListener('input', event => filterSiteCards(event.target.value));
  void loadSites();
}

function siteManagementRowHTML(s, activeSiteIDs) {
  const quota = Math.max(0, Number(s.traffic_quota || 0));
  const used = Math.max(0, Number(s.traffic_used || 0));
  const percent = quota > 0 ? Math.min(100, used / quota * 100) : 0;
  const upstreamHeaderCount = Array.isArray(s.upstream_headers) ? s.upstream_headers.length : 0;
  const accessAddress = siteAccessAddress(s, siteIngressCapabilities);
  const hasActivePlayback = activeSiteIDs.has(String(s.id));
  const expanded = siteManagementExpanded.has(Number(s.id));
  const ingressReady = normalizedIngressMode(s) !== 'unset';
  const latency = siteManagementLatency.get(Number(s.id)) || { text: '未测试', className: 'site-latency', title: '' };
  return `<article class="site-card management-site-row" data-site-id="${s.id}" data-site-search="${esc(`${s.name} ${s.icon_name || ''} ${s.target_url} ${s.public_host || ''}`.toLowerCase())}">
    <div class="management-site-summary">
      <div class="management-site-identity"><button type="button" class="site-drag-handle" data-site-drag-handle aria-label="调整 ${esc(s.name)} 的顺序" title="拖动或使用上下方向键调整顺序"><svg viewBox="0 0 20 20" aria-hidden="true"><circle cx="7" cy="5" r="1.25"/><circle cx="13" cy="5" r="1.25"/><circle cx="7" cy="10" r="1.25"/><circle cx="13" cy="10" r="1.25"/><circle cx="7" cy="15" r="1.25"/><circle cx="13" cy="15" r="1.25"/></svg></button>${renderSiteIconButton(s)}<div><h2 class="site-name">${esc(s.name)}</h2><p>${esc(siteIngressModeLabel(s))} · ${esc(uaNameMap[s.ua_mode] || s.ua_mode || '透传')}</p></div></div>
      <div class="site-card-state"><span class="console-status ${s.running ? 'is-online' : 'is-muted'}">${!ingressReady ? '待配置' : s.running ? '运行中' : s.enabled ? '启动异常' : '已停用'}</span>${hasActivePlayback ? `<button type="button" class="site-live-link" data-site-action="live" data-site-id="${s.id}"><i class="site-live-pulse" aria-hidden="true"></i>正在播放</button>` : ''}</div>
      <div class="management-site-usage"><strong>${formatBytes(used)}</strong><small>${quota ? `额度 ${formatBytes(quota)}` : '未设额度'}</small>${quota ? `<div class="management-quota-track ${percent >= 85 ? 'is-warning' : ''}" role="meter" aria-label="${esc(s.name)}流量额度" aria-valuemin="0" aria-valuemax="100" aria-valuenow="${Math.round(percent)}"><i style="width:${percent.toFixed(2)}%"></i></div>` : ''}</div>
      <div class="management-site-latency"><button type="button" class="site-latency-button" data-site-action="latency" data-site-id="${s.id}" aria-label="点击重新测速" title="点击重新测速"><strong class="${latency.className}" id="site-latency-${s.id}" title="${esc(latency.title)}">${esc(latency.text)}</strong></button></div>
      <div class="management-site-primary-actions"><button type="button" class="console-button" data-site-action="edit" data-site-id="${s.id}">编辑</button><button type="button" class="console-button management-detail-toggle" data-site-action="details" data-site-id="${s.id}" aria-expanded="${expanded}" aria-controls="site-details-${s.id}">${expanded ? '收起' : '详情'}<svg viewBox="0 0 24 24" aria-hidden="true"><path d="m6 9 6 6 6-6"/></svg></button></div>
    </div>
    <div class="management-site-details" id="site-details-${s.id}" ${expanded ? '' : 'hidden'}>
      <div class="management-site-detail-grid">
        <div class="site-access-row"><span class="management-field-label">访问地址</span><span class="site-access-value"><span class="console-mono site-access-address is-hidden" data-access-address="${esc(accessAddress)}">********</span><button type="button" class="icon-button site-access-toggle" data-site-action="access" data-site-id="${s.id}" aria-label="显示访问地址" aria-pressed="false">${renderSiteAccessVisibilityIcon(true)}</button><button type="button" class="icon-button site-access-copy" data-site-action="copy" data-site-id="${s.id}" aria-label="复制访问地址"><svg viewBox="0 0 24 24" aria-hidden="true"><rect x="9" y="9" width="10" height="10" rx="2"/><path d="M6 15H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h8a2 2 0 0 1 2 2v1"/></svg></button></span></div>
        <div><span class="management-field-label">主回源地址</span><span class="console-mono">${esc(s.target_url)}</span></div>
        <div><span class="management-field-label">上游请求头</span><span>${upstreamHeaderCount ? `${upstreamHeaderCount} 个（加密）` : '未配置'}</span></div>
      </div>
      <div class="management-site-supplement">${renderSiteMediaLibraryCounts(s)}${renderSiteAccountRetention(s)}</div>
      <div class="management-site-secondary-actions"><button type="button" class="console-button" data-site-action="toggle" data-site-id="${s.id}" ${ingressReady ? '' : 'disabled title="请先编辑并配置入口"'}>${!ingressReady ? '待配置' : s.enabled ? '停用站点' : '启用站点'}</button><button type="button" class="console-button is-danger" data-site-action="delete" data-site-id="${s.id}">删除站点</button></div>
    </div>
  </article>`;
}

function toggleSiteManagementDetails(button) {
  const id = Number(button.dataset.siteId);
  const expanded = !siteManagementExpanded.has(id);
  if (expanded) siteManagementExpanded.add(id); else siteManagementExpanded.delete(id);
  const details = document.getElementById(`site-details-${id}`);
  if (details) details.hidden = !expanded;
  button.setAttribute('aria-expanded', String(expanded));
  button.innerHTML = `${expanded ? '收起' : '详情'}<svg viewBox="0 0 24 24" aria-hidden="true"><path d="m6 9 6 6 6-6"/></svg>`;
}

function scheduleSitesRefresh() {
  if (siteRefreshTimer) window.clearTimeout(siteRefreshTimer);
  siteRefreshTimer = 0;
  if (typeof Router === 'undefined' || Router.current !== 'sites') return;
  siteRefreshTimer = window.setTimeout(() => {
    siteRefreshTimer = 0;
    if (typeof Router === 'undefined' || Router.current !== 'sites') return;
    void loadSites({ background: true });
  }, SITE_REFRESH_INTERVAL_MS);
}

function stopSitesRefresh() {
  if (siteRefreshTimer) window.clearTimeout(siteRefreshTimer);
  siteRefreshTimer = 0;
  siteLoadGeneration += 1;
  if (typeof siteSortingCleanup === 'function') siteSortingCleanup();
}

async function loadSites() {
  const page = document.getElementById('page-sites');
  const options = arguments[0] || {};
  const background = options && options.background === true;
  if (!page || typeof Router === 'undefined' || Router.current !== 'sites') return;
  const generation = ++siteLoadGeneration;
  const searchInput = document.getElementById('sites-search');
  const preservedSearch = String((searchInput && searchInput.value) || '');
  try {
	// The active-history read establishes the latest playback snapshot. Read
	// the site list afterwards: its server-side queue barrier then includes
	// media totals observed just before this page was opened.
	const [capabilities, activeResponse, iconPackResponse] = await Promise.all([
		API.ingressCapabilities(),
		API.getActiveWatchHistory({}).catch(() => ({ items: [] })),
		typeof API.getSiteIconPack === 'function'
			? API.getSiteIconPack().catch(() => ({ icons: [] }))
			: Promise.resolve({ icons: [] }),
	]);
	siteIconPack = normalizeSiteIconPack(iconPackResponse);
	const sites = await API.listSites();
	if (generation !== siteLoadGeneration || Router.current !== 'sites' || !page.isConnected) return;
	siteIngressCapabilities = normalizeSiteCapabilities(capabilities);
    const activeItems = Array.isArray(activeResponse && activeResponse.items)
      ? activeResponse.items
      : (Array.isArray(activeResponse && activeResponse.sessions) ? activeResponse.sessions : []);
	const activeSiteIDs = new Set(activeItems
		.map(item => String(item && item.site_id || '').trim())
		.filter(Boolean));
    const count = document.getElementById('sites-count');
    if (count) count.innerHTML = `共 <strong>${sites.length}</strong> 个站点`;
    const loadState = document.getElementById('sites-load-state');
    if (loadState) { loadState.textContent = '每 5 秒更新'; loadState.classList.remove('is-error'); }

    const grid = document.getElementById('sites-grid');
    if (!grid) return;
    if (background && grid.classList?.contains?.('is-reordering')) return;
    const knownSiteIDs = new Set(sites.map(site => Number(site.id)));
    for (const id of siteManagementExpanded) if (!knownSiteIDs.has(id)) siteManagementExpanded.delete(id);
    for (const id of siteManagementLatency.keys()) if (!knownSiteIDs.has(id)) siteManagementLatency.delete(id);
    const renderSignature = siteCardsRenderSignature(sites, activeSiteIDs);
    if (renderSignature === String(grid.dataset.siteRenderSignature || '')) return;
    if (!sites || sites.length === 0) {
      grid.innerHTML = '<div class="console-empty">还没有站点，添加一个访问入口开始使用。</div>';
      grid.dataset.siteRenderSignature = renderSignature;
      return;
    }

    const focused = document.activeElement;
    const focusedCard = focused?.closest?.('.site-card');
    const focusedSiteID = Number(focusedCard?.dataset?.siteId || 0);
    const focusSelector = focused?.dataset?.siteAction ? `[data-site-action="${focused.dataset.siteAction}"]`
      : focused?.dataset?.siteIconId ? '[data-site-icon-id]'
      : focused?.matches?.('[data-site-drag-handle]') ? '[data-site-drag-handle]' : '';
	grid.innerHTML = sites.map(site => siteManagementRowHTML(site, activeSiteIDs)).join('');
    grid.dataset.siteRenderSignature = renderSignature;
    bindSiteIconFallbacks(grid);

    const sitesById = new Map(sites.map(site => [site.id, site]));
    grid.querySelectorAll('.site-icon-button[data-site-icon-id]').forEach(button => {
      button.addEventListener('click', event => {
        event.preventDefault();
        event.stopPropagation();
        const site = sitesById.get(Number(button.dataset.siteIconId));
        if (site) void showSiteIconManager(site);
      });
    });
    grid.querySelectorAll('[data-site-action]').forEach(button => {
      button.addEventListener('click', () => {
        const id = Number(button.dataset.siteId);
        const site = sitesById.get(id);
        if (!site) return;
		if (button.dataset.siteAction === 'details') toggleSiteManagementDetails(button);
		if (button.dataset.siteAction === 'latency') testSiteLatency(id, button);
		if (button.dataset.siteAction === 'access') toggleSiteAccessAddress(button);
		if (button.dataset.siteAction === 'copy') copySiteAccessAddress(button);
		if (button.dataset.siteAction === 'live') showSiteLiveDetails(site, button);
		if (button.dataset.siteAction === 'toggle') toggleSiteAction(id);
        if (button.dataset.siteAction === 'edit') showSiteModal(site);
        if (button.dataset.siteAction === 'delete') deleteSiteAction(id, site.name);
      });
    });
    setupSiteSorting(grid);
    if (focusedSiteID && focusSelector) {
      grid.querySelector?.(`.site-card[data-site-id="${focusedSiteID}"]`)?.querySelector?.(focusSelector)?.focus?.({ preventScroll: true });
    }
    const currentSearch = document.getElementById('sites-search');
    filterSiteCards(String(currentSearch?.value || preservedSearch));
  } catch (e) {
    if (generation !== siteLoadGeneration || Router.current !== 'sites' || !page.isConnected) return;
    const loadState = document.getElementById('sites-load-state');
    if (loadState) { loadState.textContent = '读取失败，点击刷新重试'; loadState.classList.add('is-error'); }
    if (background) console.warn('Sites refresh error', e);
    else Toast.error('加载站点失败: ' + e.message);
  } finally {
    if (generation === siteLoadGeneration && Router.current === 'sites' && page.isConnected) scheduleSitesRefresh();
  }
}

function filterSiteCards(query) {
  const needle = String(query || '').trim().toLowerCase();
  const grid = document.getElementById('sites-grid');
  if (!grid) return;
  grid.classList.toggle('is-filtered', !!needle);
  grid.querySelectorAll('.site-card').forEach(card => {
    card.hidden = !!needle && !String(card.dataset.siteSearch || '').includes(needle);
  });
  const noMatch = document.getElementById('sites-no-match');
  if (noMatch) noMatch.hidden = !needle || [...grid.querySelectorAll('.site-card')].some(card => !card.hidden);
  grid.querySelectorAll('[data-site-drag-handle]').forEach(handle => {
    handle.disabled = !!needle;
    handle.title = needle ? '清除搜索后可调整顺序' : '拖拽调整顺序';
  });
}

function siteOrderFromGrid(grid) {
  return [...grid.querySelectorAll('.site-card[data-site-id]')]
    .map(card => Number(card.dataset.siteId))
    .filter(Number.isSafeInteger);
}

function captureSiteDropSlots(grid, draggedCard) {
  const scrollX = typeof window !== 'undefined' ? (window.scrollX || 0) : 0;
  const scrollY = typeof window !== 'undefined' ? (window.scrollY || 0) : 0;
  return [...grid.children]
    .filter(node => node !== draggedCard && !node.hidden)
    .map(node => {
      const rect = node.getBoundingClientRect();
      return {
        x: rect.left + rect.width / 2 + scrollX,
        y: rect.top + rect.height / 2 + scrollY,
        width: Math.max(rect.width, 1),
        height: Math.max(rect.height, 1),
      };
    });
}

function moveSitePlaceholderAtPoint(grid, draggedCard, placeholder, dropSlots, clientX, clientY) {
  if (!placeholder || !dropSlots.length) return;
  const scrollX = typeof window !== 'undefined' ? (window.scrollX || 0) : 0;
  const scrollY = typeof window !== 'undefined' ? (window.scrollY || 0) : 0;
  const pointerX = clientX + scrollX;
  const pointerY = clientY + scrollY;
  let desiredIndex = 0;
  let nearestDistance = Number.POSITIVE_INFINITY;
  dropSlots.forEach((slot, index) => {
    const dx = (pointerX - slot.x) / slot.width;
    const dy = (pointerY - slot.y) / slot.height;
    const distance = dx * dx + dy * dy;
    if (distance < nearestDistance) {
      desiredIndex = index;
      nearestDistance = distance;
    }
  });

  const orderedCards = [...grid.querySelectorAll('.site-card[data-site-id]')]
    .filter(card => card !== draggedCard);
  const reference = orderedCards[desiredIndex] || null;
  if (reference !== placeholder.nextSibling) grid.insertBefore(placeholder, reference);
}

function positionDraggedSiteCard(card, clientX, clientY, offsetX, offsetY) {
  card.style.left = `${clientX - offsetX}px`;
  card.style.top = `${clientY - offsetY}px`;
  card.style.transform = 'translate3d(0, 0, 0)';
}

async function persistSiteOrder(grid) {
  const siteIds = siteOrderFromGrid(grid);
  const nextOrder = siteIds.join(',');
  if (!siteIds.length || nextOrder === grid.dataset.siteOrder) return;
  siteLoadGeneration += 1;
  grid.classList.add('is-saving-order');
  try {
    await API.reorderSites(siteIds);
    siteLoadGeneration += 1;
    grid.dataset.siteOrder = nextOrder;
    Toast.success('站点顺序已保存，仪表盘已同步');
  } catch (error) {
    Toast.error('保存站点顺序失败: ' + error.message);
    await loadSites();
  } finally {
    grid.classList.remove('is-saving-order');
    scheduleSitesRefresh();
  }
}

function setupSiteSorting(grid) {
  if (typeof siteSortingCleanup === 'function') siteSortingCleanup();
  const cards = [...grid.querySelectorAll('.site-card[data-site-id]')];
  grid.dataset.siteOrder = siteOrderFromGrid(grid).join(',');
  let draggedCard = null;
  let placeholder = null;
  let dragOffsetX = 0;
  let dragOffsetY = 0;
  let dropSlots = [];
  let activeHandle = null;
  let activePointerId = null;

  const beginDrag = (card, event) => {
    const rect = card.getBoundingClientRect();
    draggedCard = card;
    dragOffsetX = event.clientX - rect.left;
    dragOffsetY = event.clientY - rect.top;
    placeholder = document.createElement('div');
    placeholder.className = 'site-card-placeholder';
    placeholder.setAttribute('aria-hidden', 'true');
    placeholder.style.height = `${rect.height}px`;
    grid.insertBefore(placeholder, card);

    card.style.position = 'fixed';
    card.style.left = `${rect.left}px`;
    card.style.top = `${rect.top}px`;
    card.style.width = `${rect.width}px`;
    card.style.height = `${rect.height}px`;
    card.style.margin = '0';
    card.style.transform = 'translate3d(0, 0, 0)';
    card.classList.add('is-dragging');
    grid.classList.add('is-reordering');
    document.body.classList.add('is-site-dragging');
    dropSlots = captureSiteDropSlots(grid, card);
  };
  const finishDrag = card => {
    if (draggedCard !== card) return;
    if (placeholder && placeholder.parentElement === grid) grid.insertBefore(card, placeholder);
    if (placeholder) placeholder.remove();
    placeholder = null;
    dropSlots = [];
    card.style.position = '';
    card.style.left = '';
    card.style.top = '';
    card.style.width = '';
    card.style.height = '';
    card.style.margin = '';
    card.style.transform = '';
    card.classList.remove('is-dragging');
    grid.classList.remove('is-reordering');
    document.body.classList.remove('is-site-dragging');
    draggedCard = null;
    void persistSiteOrder(grid);
  };

  const movePointer = event => {
    if (!draggedCard) return;
    if (event.type === 'pointermove' && activePointerId !== null && event.pointerId !== activePointerId) return;
    if (event.cancelable) event.preventDefault();
    positionDraggedSiteCard(draggedCard, event.clientX, event.clientY, dragOffsetX, dragOffsetY);
    const edge = 64;
    if (typeof window !== 'undefined' && window.scrollBy) {
      if (event.clientY < edge) window.scrollBy(0, -12);
      if (event.clientY > window.innerHeight - edge) window.scrollBy(0, 12);
    }
    moveSitePlaceholderAtPoint(grid, draggedCard, placeholder, dropSlots, event.clientX, event.clientY);
  };

  const finishPointer = event => {
    if (!draggedCard) return;
    if (event.type.startsWith('pointer') && activePointerId !== null && event.pointerId !== activePointerId) return;
    const card = draggedCard;
    if (activeHandle && activePointerId !== null && activeHandle.releasePointerCapture && activeHandle.hasPointerCapture) {
      try {
        if (activeHandle.hasPointerCapture(activePointerId)) activeHandle.releasePointerCapture(activePointerId);
      } catch (_) {}
    }
    activeHandle = null;
    activePointerId = null;
    finishDrag(card);
  };

  cards.forEach(card => {
    const handle = card.querySelector('[data-site-drag-handle]');
    if (!handle) return;

    handle.addEventListener('pointerdown', event => {
      if (handle.disabled || (Number.isFinite(event.button) && event.button !== 0)) return;
      event.preventDefault();
      activeHandle = handle;
      activePointerId = event.pointerId;
      beginDrag(card, event);
      if (handle.setPointerCapture) {
        try { handle.setPointerCapture(event.pointerId); } catch (_) {}
      }
    });
    handle.addEventListener('keydown', event => {
      if (handle.disabled || !['ArrowUp', 'ArrowDown'].includes(event.key)) return;
      event.preventDefault();
      const ordered = [...grid.querySelectorAll('.site-card[data-site-id]')];
      const index = ordered.indexOf(card);
      const target = index + (event.key === 'ArrowUp' ? -1 : 1);
      if (target < 0 || target >= ordered.length) return;
      grid.insertBefore(card, event.key === 'ArrowUp' ? ordered[target] : ordered[target].nextSibling);
      handle.focus({ preventScroll: true });
      void persistSiteOrder(grid);
    });
  });

  window.addEventListener('pointermove', movePointer, { passive: false });
  window.addEventListener('pointerup', finishPointer);
  window.addEventListener('pointercancel', finishPointer);
  // Mouse fallbacks cover browsers and automation surfaces that begin with a
  // PointerEvent but only deliver compatibility mouse movement afterwards.
  window.addEventListener('mousemove', movePointer, { passive: false });
  window.addEventListener('mouseup', finishPointer);
  siteSortingCleanup = () => {
    window.removeEventListener('pointermove', movePointer);
    window.removeEventListener('pointerup', finishPointer);
    window.removeEventListener('pointercancel', finishPointer);
    window.removeEventListener('mousemove', movePointer);
    window.removeEventListener('mouseup', finishPointer);
    if (draggedCard) finishPointer({ type: 'pointercancel', pointerId: activePointerId });
    siteSortingCleanup = null;
  };
}

async function testSiteLatency(id, button) {
  const paint = (text, className, title = '') => {
    siteManagementLatency.set(Number(id), { text, className, title });
    const value = document.getElementById(`site-latency-${id}`);
    if (value) { value.textContent = text; value.className = className; value.title = title; }
  };
  if (button) button.disabled = true;
  paint('测速中…', 'site-latency');
  try {
    const result = await API.diagSite(id);
    const health = result && result.upstreams && result.upstreams.primary
      ? result.upstreams.primary.health || {}
      : (result && result.health) || {};
    if (health.status === 'online' && Number.isFinite(Number(health.latency_ms))) {
      const latency = Number(health.latency_ms);
      paint(`${latency} ms`, `site-latency ${latency < 200 ? 'good' : latency < 800 ? 'warn' : 'bad'}`);
    } else {
      throw new Error(health.error || '回源不可用');
    }
  } catch (error) {
    paint('测速失败', 'site-latency bad', error.message || '测速失败');
  } finally {
    if (button) button.disabled = false;
  }
}

async function testAllSitesLatency() {
  const button = document.getElementById('btn-test-all-sites');
  if (!button || button.disabled) return;
  button.disabled = true;
  button.textContent = '全部测速中…';
  const buttons = [...document.querySelectorAll('[data-site-action="latency"]')];
  await Promise.all(buttons.map(siteButton => testSiteLatency(Number(siteButton.dataset.siteId), siteButton)));
  button.disabled = false;
  button.textContent = '全部测速';
}

// Built-in UA presets are no longer offered in the form. Legacy sites that
// still store one keep their behavior by opening as custom UA with the
// equivalent triplet, which is saved as an editable custom configuration.
const legacyUAPresets = {
  infuse: { custom_user_agent: 'Infuse/7.8.1', custom_client: 'Infuse', custom_version: '7.8.1' },
  web: { custom_user_agent: 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Emby Theater', custom_client: 'Emby Web', custom_version: '4.9.0.42' },
  client: { custom_user_agent: 'Emby-Theater/4.7.0', custom_client: 'Emby Theater', custom_version: '4.7.0' },
};

function uaModeIsCustomLike(mode) {
  return mode === 'custom' || Object.prototype.hasOwnProperty.call(legacyUAPresets, mode);
}

function customUAFormState(mode, site) {
  const legacy = legacyUAPresets[mode];
  const isCustom = uaModeIsCustomLike(mode);
  const source = mode === 'custom' && site ? site : legacy;
  return {
    visible: isCustom,
    required: isCustom,
    customUserAgent: isCustom && source ? (source.custom_user_agent || '') : '',
    customClient: isCustom && source ? (source.custom_client || '') : '',
    customVersion: isCustom && source ? (source.custom_version || '') : '',
  };
}

function buildCustomUAPayload(mode, customUserAgent, customClient, customVersion) {
  if (mode !== 'custom') {
    return {
      custom_user_agent: '',
      custom_client: '',
      custom_version: '',
    };
  }
  return {
    custom_user_agent: String(customUserAgent || '').trim(),
    custom_client: String(customClient || '').trim(),
    custom_version: String(customVersion || '').trim(),
  };
}

function buildUpstreamHeaderPayload(headers) {
	return headers
		.filter(header => header.configured || String(header.name || '').trim() || String(header.value || '').trim())
		.map(header => ({
			name: String(header.name || '').trim(),
			value: String(header.value || '').trim(),
		}));
}

const DEFAULT_MAX_PLAYBACK_ADDRESSES = 128;

function normalizeStreamHosts(value) {
	let hosts = value;
	if (typeof hosts === 'string') {
		try {
			hosts = JSON.parse(hosts || '[]');
		} catch (_) {
			return [];
		}
	}
	if (!Array.isArray(hosts)) return [];
	return hosts
		.filter(host => typeof host === 'string' && host.trim())
		.map(host => host.trim());
}

function normalizeSiteCapabilities(value) {
	const capabilities = value && typeof value === 'object' ? value : {};
	const requestedMax = Number(capabilities.max_playback_addresses);
	const normalized = {
		host_only_available: capabilities.host_only_available !== false,
		upstream_headers_available: capabilities.upstream_headers_available !== false,
		max_playback_addresses: Number.isInteger(requestedMax) && requestedMax > 0
			? requestedMax
			: DEFAULT_MAX_PLAYBACK_ADDRESSES,
	};
	// Keep the legacy capability object's enumerable shape stable for embedded
	// clients while exposing the new fields to the UI.
	Object.defineProperties(normalized, {
		domain_prefix_available: { value: capabilities.domain_prefix_available === undefined ? undefined : capabilities.domain_prefix_available === true, enumerable: false },
		route_domain: { value: String(capabilities.route_domain || '').trim().toLowerCase(), enumerable: false },
		panel_tls_enabled: { value: capabilities.panel_tls_enabled === true, enumerable: false },
		path_ingress_available: { value: capabilities.path_ingress_available !== false, enumerable: false },
		panel_access_url: { value: String(capabilities.panel_access_url || '').replace(/\/$/, ''), enumerable: false },
	});
	return normalized;
}

// The dynamic discovery policy is process-wide (compatible profile, all
// sources); per-site policy settings were removed along with their UI.

function canAddPlaybackAddress(currentCount, maxPlaybackAddresses) {
	return currentCount < maxPlaybackAddresses;
}

function renderUpstreamHeaderRows(headers, upstreamHeadersAvailable) {
	return headers.map((header, idx) => `
		<div class="upstream-header-row">
		  <input type="text" class="form-input m-upstream-header-name" data-idx="${idx}" value="${esc(header.name)}" placeholder="Header 名称" aria-label="请求头名称" maxlength="64" autocapitalize="none" autocorrect="off" spellcheck="false" ${upstreamHeadersAvailable ? '' : 'disabled'}>
		  <input type="password" class="form-input m-upstream-header-value" data-idx="${idx}" value="" placeholder="${header.configured ? '已配置；留空保持不变' : 'Header 值'}" aria-label="请求头值" maxlength="1024" autocomplete="new-password" ${upstreamHeadersAvailable ? '' : 'disabled'}>
		  <button type="button" class="icon-button danger m-upstream-header-remove" data-idx="${idx}" title="删除请求头" aria-label="删除请求头"><svg viewBox="0 0 24 24" aria-hidden="true"><path d="M3 6h18M8 6V4h8v2m-9 0 1 14h8l1-14M10 10v6m4-6v6"/></svg></button>
		</div>
	`).join('');
}

function normalizedIngressMode(site) {
	const mode = String((site && site.ingress_mode) || '').trim().toLowerCase();
	if (mode === 'unset' || mode === 'port' || mode === 'path' || mode === 'host' || mode === 'both') return mode;
	return site && String(site.public_host || '').trim() ? 'host' : 'port';
}

function ingressFormState(mode) {
	const normalized = ['unset', 'port', 'path', 'host', 'both'].includes(mode) ? mode : 'host';
	if (normalized === 'unset') {
		return {
			mode: normalized,
			showPublicHost: false,
			requirePublicHost: false,
			requireListenPort: false,
			showPathPrefix: false,
			requirePathPrefix: false,
			portLabel: '监听端口',
			warning: '该站点来自其他服务器，原入口不适用于当前环境。请选择可用入口并保存后再启用。',
		};
	}
	return {
		mode: normalized,
		showPublicHost: normalized === 'host' || normalized === 'both',
		requirePublicHost: normalized === 'host' || normalized === 'both',
		showPathPrefix: normalized === 'path',
		requirePathPrefix: normalized === 'path',
		requireListenPort: normalized === 'port' || normalized === 'both',
		portLabel: normalized === 'host' || normalized === 'path' ? '独立端口（可选，自动分配）' : '监听端口',
		warning: normalized === 'port'
			? '独立端口会绑定所有网络接口；公网部署时请配置防火墙。'
			: normalized === 'path'
				? '路径入口复用面板域名和端口，例如 https://panel.example.com/emby/。客户端服务器地址需要包含该路径。'
			: normalized === 'both'
				? '兼容模式会同时开放独立端口和共享域名，建议迁移到单一入口。'
				: '域名前缀通过面板端口转发，不会绑定站点端口，例如 https://123.example.com:9090。请先在 TLS 页配置面板域名、泛域名并申请证书，完成后再启用。',
	};
}

function buildIngressPayload(mode, port, publicHost, routePrefix, routeDomain, pathPrefix) {
	const state = ingressFormState(mode);
	const parsedPort = parseInt(port, 10);
	const prefix = String(routePrefix || '').trim().toLowerCase();
	const generatedHost = state.showPublicHost && prefix && routeDomain
		? `${prefix}.${String(routeDomain).trim().toLowerCase()}`
		: String(publicHost || '').trim();
	return {
		ingress_mode: state.mode,
		listen_port: Number.isInteger(parsedPort) ? parsedPort : 0,
		public_host: state.showPublicHost ? generatedHost : '',
		path_prefix: state.showPathPrefix ? String(pathPrefix || '').trim() : '',
		...(state.showPublicHost && prefix ? { route_prefix: prefix } : {}),
	};
}

function defaultIngressMode(capabilities) {
	if (!capabilities) return 'host';
	if (capabilities.host_only_available === false) return 'port';
	if (capabilities.domain_prefix_available !== true) return 'port';
	if (capabilities.panel_tls_enabled !== true) return 'port';
	return 'host';
}

function siteAccessAddress(site, capabilities) {
	const mode = normalizedIngressMode(site);
	const locationObject = typeof window !== 'undefined' && window.location
		? window.location
		: { protocol: 'http:', hostname: '127.0.0.1', port: '' };
	const protocol = capabilities && capabilities.panel_tls_enabled ? 'https' : locationObject.protocol.replace(':', '') || 'http';
	if (mode === 'port') {
		const host = locationObject.hostname || '127.0.0.1';
		return `${protocol}://${host}:${Number(site.listen_port) || ''}`;
	}
	if (mode === 'path') {
		const base = String(capabilities && capabilities.panel_access_url || '').replace(/\/$/, '') || `${protocol}://${locationObject.host || locationObject.hostname || '127.0.0.1'}`;
		const prefix = String(site.path_prefix || '').trim().replace(/^\/+|\/+$/g, '');
		return prefix ? `${base}/${prefix}/` : '';
	}
	const host = String(site.public_host || '').trim();
	const panelPort = locationObject.port || (protocol === 'https' ? '443' : '80');
	return host ? `${protocol}://${host}:${panelPort}` : '';
}

function toggleSiteAccessAddress(button) {
	const row = button.closest('.site-access-row');
	const value = row && row.querySelector('[data-access-address]');
	if (!value) return;
	const hidden = value.classList.toggle('is-hidden');
	value.textContent = hidden ? '********' : value.dataset.accessAddress;
	button.innerHTML = renderSiteAccessVisibilityIcon(hidden);
	button.setAttribute('aria-label', hidden ? '显示访问地址' : '隐藏访问地址');
	button.setAttribute('aria-pressed', String(!hidden));
	button.setAttribute('title', hidden ? '显示访问地址' : '隐藏访问地址');
}

async function copySiteAccessAddress(button) {
	const row = button.closest('.site-access-row');
	const value = row && row.querySelector('[data-access-address]');
	const address = String(value && value.dataset.accessAddress || '').trim();
	if (!address) {
		Toast.error('当前站点没有可复制的访问地址');
		return;
	}

	try {
		if (typeof navigator !== 'undefined' && navigator.clipboard && typeof navigator.clipboard.writeText === 'function') {
			await navigator.clipboard.writeText(address);
		} else {
			if (typeof document === 'undefined' || typeof document.execCommand !== 'function') throw new Error('clipboard unavailable');
			const input = document.createElement('textarea');
			input.value = address;
			input.setAttribute('readonly', '');
			input.style.position = 'fixed';
			input.style.opacity = '0';
			document.body.appendChild(input);
			input.select();
			const copied = document.execCommand('copy');
			input.remove();
			if (!copied) throw new Error('copy failed');
		}
		Toast.success('访问地址已复制');
	} catch (_) {
		Toast.error('复制失败，请手动复制访问地址');
	}
}

async function showSiteLiveDetails(site, button) {
	const siteID = Number(site && site.id);
	if (!Number.isSafeInteger(siteID) || siteID <= 0) return;
	const previousText = button && button.textContent;
	if (button) {
		button.disabled = true;
		button.textContent = '…';
	}
	try {
		const response = await API.getActiveWatchHistory({ site_id: siteID });
		const items = typeof watchHistoryNormalizeItems === 'function' ? watchHistoryNormalizeItems(response) : [];
		if (!items.length) {
			Toast.info('该站点当前没有正在播放的媒体');
			return;
		}
		if (typeof watchHistoryDetailsHTML !== 'function' || typeof watchHistoryBindDetailImages !== 'function') {
			throw new Error('播放详情组件尚未就绪');
		}
		document.getElementById('modal-title').textContent = `${site.name} · 正在播放`;
		document.getElementById('modal-body').innerHTML = watchHistoryDetailsHTML(items[0]);
		document.getElementById('modal-footer').innerHTML = '';
		openModal({ closeOnBackdrop: true, modalClass: 'site-live-modal' });
		watchHistoryBindDetailImages(document.getElementById('modal-body'));
	} catch (error) {
		Toast.error(error && error.message ? error.message : '读取正在播放失败');
	} finally {
		if (button && button.isConnected) {
			button.disabled = false;
			button.textContent = previousText || 'live';
		}
	}
}

let siteIngressCapabilities = normalizeSiteCapabilities({});

function siteIngressModeLabel(site) {
	const labels = { unset: '入口未配置', port: '独立端口', path: '路径', host: '域名前缀', both: '域名前缀（兼容）' };
	return labels[normalizedIngressMode(site)] || labels.unset;
}

function routePrefixForSite(site, routeDomain) {
	if (!site || normalizedIngressMode(site) !== 'host') return '';
	const host = String(site.public_host || '').trim().toLowerCase();
	const domain = String(routeDomain || '').trim().toLowerCase();
	if (!host || !domain || !host.endsWith(`.${domain}`)) return '';
	const prefix = host.slice(0, -(domain.length + 1));
	return prefix.includes('.') ? '' : prefix;
}

function normalizedTargetAuthority(value) {
	let candidate = String(value || '').trim().replaceAll('：', ':');
	if (!candidate) return '';
	if (!candidate.includes('://')) {
		const authority = candidate.split(/[/?#]/, 1)[0];
		candidate = authority.endsWith(':443') ? `https://${candidate}` : `http://${candidate}`;
	}
	try {
		const parsed = new URL(candidate);
		const scheme = parsed.protocol.toLowerCase();
		if (scheme !== 'http:' && scheme !== 'https:') return '';
		const defaultPort = scheme === 'https:' ? '443' : '80';
		return `${scheme}//${parsed.hostname.toLowerCase()}:${parsed.port || defaultPort}`;
	} catch (_) {
		return '';
	}
}

// When a user finishes typing an origin address, complete the missing scheme and
// split an explicit port into the dedicated port field. Saved or manually entered
// ports survive address edits; only an untouched automatic default follows a new
// protocol. Without a scheme, 443 (or no port) uses https, other ports use http.
function autoFillTargetAddress(addressInput, portInput) {
	const raw = String(addressInput && addressInput.value || '').trim().replaceAll('：', ':');
	if (!raw) return;
	if (/\s/.test(raw)) return;
	const explicitScheme = /^[a-z][a-z0-9+.-]*:\/\//i.test(raw);
	// URL normalizes default ports away (http :80, https :443), so read the
	// explicitly typed port from the authority text before parsing.
	const withoutScheme = explicitScheme ? raw.replace(/^[a-z][a-z0-9+.-]*:\/\//i, '') : raw;
	const authority = withoutScheme.split(/[/?#]/, 1)[0];
	const portMatch = authority.match(/^(?:\[[^\]]*\]|[^:]+):(\d+)$/);
	const explicitPort = portMatch ? portMatch[1] : '';
	const currentPort = String(portInput && portInput.value || '').trim();
	const retainedPort = currentPort !== portInput?.dataset?.autofilledPort ? currentPort : '';
	const targetPort = explicitPort || retainedPort;
	try {
		const parsed = new URL(explicitScheme ? raw : `http://${raw}`);
		if (!parsed.hostname) return;
		const scheme = explicitScheme
			? parsed.protocol
			: (targetPort === '443' || !targetPort ? 'https:' : 'http:');
		const hostname = parsed.hostname;
		const path = `${parsed.pathname === '/' ? '' : parsed.pathname}${parsed.search}${parsed.hash}`;
		addressInput.value = `${scheme}//${hostname}${path}`;
		if (portInput) {
			portInput.value = targetPort || (scheme === 'https:' ? '443' : '80');
			if (portInput.dataset) {
				if (targetPort) delete portInput.dataset.autofilledPort;
				else portInput.dataset.autofilledPort = portInput.value;
			}
		}
	} catch (_) {
		// Leave partially typed values untouched until they parse as an address.
	}
}

function renderPanelCertificateStatus(status) {
	if (!status || status.available === false) {
		return '<div class="form-help" style="color:var(--orange)">TLS 数据目录不可写，请检查数据库所在数据目录的权限。</div>';
	}
	if (!status.configured) {
		return '<div class="form-help">尚未申请证书。请先保存域名设置，再点击“申请证书”。</div>';
	}
	const certificateMatchesConfiguredDomain = status.certificate_matches_configured_domain ?? status.certificate_current;
	return `
	  ${status.panel_covered_by_edge_wildcard ? '<div class="form-help" style="color:var(--orange);margin-bottom:10px">⚠ 面板域名位于节点泛域名覆盖范围内；建议改为多级域名，例如 <code>panel.admin.example.com</code>，以隔离面板与节点证书。</div>' : ''}
	  <div class="diag-row"><span class="diag-key">证书域名</span><span class="diag-val">${esc(status.subject || `*.${status.route_domain || ''}`)}</span></div>
	  <div class="diag-row"><span class="diag-key">证书匹配</span><span class="diag-val ${certificateMatchesConfiguredDomain ? 'good' : 'warn'}">${certificateMatchesConfiguredDomain ? '面板域名一致' : '需要申请或更新'}</span></div>
	  <div class="diag-row"><span class="diag-key">站点泛域名</span><span class="diag-val ${status.certificate_matches_route_wildcard ? 'good' : 'warn'}">${status.certificate_matches_route_wildcard ? '已覆盖' : '需要更新'}</span></div>
	  <div class="diag-row"><span class="diag-key">到期时间</span><span class="diag-val">${esc(status.expires_at || '—')}</span></div>
	  <div class="diag-row"><span class="diag-key">证书状态</span><span class="diag-val ${status.certificate_valid ? 'good' : 'warn'}">${status.certificate_valid ? '有效' : '已过期或不可用'}</span></div>
	  <div class="diag-row"><span class="diag-key">自动续签</span><span class="diag-val ${status.auto_renew_enabled ? 'good' : 'warn'}">${status.acme_staging ? '测试环境不会自动续签' : (status.auto_renew_enabled ? '已启用（到期前 30 天）' : '待配置邮箱和 Token')}</span></div>
	  <div class="diag-row"><span class="diag-key">当前监听端口</span><span class="diag-val">${esc(String(status.active_listen_port || '—'))}</span></div>
	  <div class="diag-row"><span class="diag-key">设置监听端口</span><span class="diag-val ${status.listen_port !== status.active_listen_port ? 'warn' : ''}">${esc(String(status.listen_port || '—'))}</span></div>
	  <div class="diag-row"><span class="diag-key">面板 HTTPS</span><span class="diag-val ${status.restart_required ? 'warn' : 'good'}">${status.restart_required ? '等待重启应用' : '已启用'}</span></div>
	`;
}

function panelHTTPSPreview(panelDomain) {
	const port = window.location.port;
	return panelDomain ? `https://${panelDomain}${port && port !== '443' ? `:${port}` : ''}` : '—';
}

function panelDomainFromForm() {
	return document.getElementById('m-panel-domain').value.trim().replace(/^\*\./, '');
}

async function waitForPanelRestart(redirectURL) {
	const deadline = Date.now() + 90000;
	while (Date.now() < deadline) {
		try {
			await fetch(`${redirectURL}/api/auth/check`, { mode: 'no-cors', cache: 'no-store' });
			window.location.replace(redirectURL);
			return;
		} catch (_) {
			await new Promise(resolve => setTimeout(resolve, 1500));
		}
	}
	Toast.error('服务重启超时，请稍后手动打开新的 HTTPS 地址');
}

async function showPanelCertificateModal() {
	let status;
	try {
		status = await API.panelCertificate();
	} catch (error) {
		Toast.error(`无法读取证书状态：${error.message}`);
		return;
	}
	document.getElementById('modal-title').textContent = 'TLS 证书';
	document.getElementById('modal-body').innerHTML = `
	  <div id="m-panel-certificate-status">${renderPanelCertificateStatus(status)}</div>
	  <div class="form-group" style="margin-top:18px">
	    <label>面板访问域名</label>
	    <input type="text" class="form-input" id="m-panel-domain" maxlength="255" value="${esc(status.panel_domain || '')}" placeholder="panel.example.com" autocomplete="off" autocapitalize="none" spellcheck="false">
	    <div class="form-help">填写面板完整域名；面板与节点泛域名必须属于同一注册域但不能相同。</div>
	  </div>
	  <div class="form-group">
	    <label>节点泛域名</label>
	    <input type="text" class="form-input" id="m-wildcard-domain" maxlength="255" value="${esc(status.wildcard_domain || (status.route_domain ? `*.${status.route_domain}` : ''))}" placeholder="*.example.com" autocomplete="off" autocapitalize="none" spellcheck="false">
	    <div class="form-help">Controller 证书会覆盖节点泛域名；多级面板域名会额外加入精确 SAN。请提前将泛域名解析到本机。</div>
	  </div>
	  <div class="form-group">
	    <label>启用后的面板地址</label>
	    <div class="form-help" id="m-panel-address-preview">${esc(panelHTTPSPreview(status.panel_domain || ''))}</div>
	  </div>
	  <div class="form-group">
	    <label>面板监听端口</label>
	    <input type="number" class="form-input" id="m-panel-listen-port" min="1" max="65535" step="1" value="${esc(String(status.listen_port || status.active_listen_port || 9090))}">
	    <div class="form-help">修改端口后需要重启面板；Docker 请使用 host 网络，并在宿主机防火墙中放行新端口。</div>
	  </div>
	  <div class="form-group">
	    <label>ACME 邮箱</label>
	    <input type="email" class="form-input" id="m-acme-email" autocomplete="email" maxlength="254" value="${esc(status.acme_email || '')}" placeholder="admin@example.com">
	    <div class="form-help">邮箱会直接显示在面板中，用于 ACME 账户与证书续签通知。</div>
	  </div>
	  <div class="form-group">
	    <label>DNS 服务商</label>
	    <select class="form-select modal-select" id="m-acme-provider"><option value="cloudflare">Cloudflare DNS</option></select>
	  </div>
	  <div class="form-group">
	    <label>DNS API Token</label>
	    <input type="text" class="form-input mono" id="m-acme-token" autocomplete="off" maxlength="512" value="${esc(status.dns_api_token || '')}" placeholder="Cloudflare DNS API Token">
	    <div class="form-help">Token 会直接显示给已登录管理员；数据库中仍加密保存，并用于证书自动续签。</div>
	  </div>
	  <label style="display:flex;align-items:center;gap:8px;margin-top:10px;color:var(--white-60);font-size:.82rem">
	    <input type="checkbox" id="m-acme-staging" ${status.acme_staging ? 'checked' : ''}>
	    <span>ACME 测试环境</span>
	  </label>
	  <div class="form-help">测试环境只验证 ACME 和 DNS-01，不会替换当前正式证书，也不会启用自动续签。</div>
	`;
	document.getElementById('modal-footer').innerHTML = `
	  <button class="btn-modal" id="m-cert-cancel">关闭</button>
	  ${status.restart_required && ((status.configured && (status.certificate_matches_configured_domain ?? status.certificate_current) && status.certificate_valid) || (!status.configured && status.listen_port !== status.active_listen_port)) ? `<button class="btn-modal primary" id="m-cert-restart">${status.configured ? '启用 HTTPS 并重启' : '重启应用'}</button>` : ''}
	  <button class="btn-modal" id="m-cert-save">保存设置</button>
	  <button class="btn-modal primary" id="m-cert-issue" ${status.available === false || status.issuing || !status.settings_configured ? 'disabled' : ''}>申请证书</button>
	`;
	const refreshPreview = () => {
		const domain = panelDomainFromForm();
		const port = Number(document.getElementById('m-panel-listen-port').value) || 9090;
		document.getElementById('m-panel-address-preview').textContent = domain ? `https://${domain}${port === 443 ? '' : `:${port}`}` : '—';
	};
	document.getElementById('m-panel-domain').addEventListener('input', refreshPreview);
	document.getElementById('m-wildcard-domain').addEventListener('input', refreshPreview);
	document.getElementById('m-panel-listen-port').addEventListener('input', refreshPreview);
	document.getElementById('m-cert-cancel').onclick = closeModal;
	const restartButton = document.getElementById('m-cert-restart');
	if (restartButton) restartButton.onclick = async () => {
		if (!window.confirm('重启会短暂中断面板和所有站点连接，确定现在重启吗？')) return;
		restartButton.disabled = true;
		restartButton.textContent = '正在重启…';
		try {
			const result = await API.restartSystem();
			Toast.success('重启请求已发送，正在等待 HTTPS 服务恢复');
			await waitForPanelRestart(result.redirect_url);
		} catch (error) {
			restartButton.disabled = false;
			restartButton.textContent = '启用 HTTPS 并重启';
			Toast.error(error.message);
		}
	};
	const settingsPayload = () => ({
		panel_domain: document.getElementById('m-panel-domain').value.trim(),
		wildcard_domain: document.getElementById('m-wildcard-domain').value.trim(),
		listen_port: Number(document.getElementById('m-panel-listen-port').value),
	});
	document.getElementById('m-cert-save').onclick = async () => {
		const button = document.getElementById('m-cert-save');
		const payload = settingsPayload();
		if (!payload.panel_domain || !payload.wildcard_domain || !Number.isInteger(payload.listen_port) || payload.listen_port < 1 || payload.listen_port > 65535) {
			Toast.error('请填写面板域名、节点泛域名和有效监听端口');
			return;
		}
		button.disabled = true;
		button.textContent = '保存中…';
		try {
			await API.savePanelSettings(payload);
			Toast.success('面板设置已保存；证书会覆盖面板域名和节点站点泛域名');
			closeModal();
			await showPanelCertificateModal();
		} catch (error) {
			button.disabled = false;
			button.textContent = '保存设置';
			Toast.error(error.message);
		}
	};
	document.getElementById('m-cert-issue').onclick = async () => {
		const button = document.getElementById('m-cert-issue');
		const tokenInput = document.getElementById('m-acme-token');
		const settingsPayloadValue = settingsPayload();
		const savedWildcard = String(status.wildcard_domain || '').toLowerCase().replace(/^\*\./, '');
		const formWildcard = settingsPayloadValue.wildcard_domain.toLowerCase().replace(/^\*\./, '');
		if (settingsPayloadValue.panel_domain.toLowerCase().replace(/\.$/, '') !== String(status.panel_domain || '').toLowerCase().replace(/\.$/, '') || formWildcard !== savedWildcard || settingsPayloadValue.listen_port !== Number(status.listen_port)) {
			Toast.error('请先点击“保存设置”，再申请证书');
			return;
		}
		const payload = {
			email: document.getElementById('m-acme-email').value.trim(),
			dns_provider: document.getElementById('m-acme-provider').value,
			dns_api_token: tokenInput.value.trim(),
			staging: document.getElementById('m-acme-staging').checked,
		};
		if (!payload.email || !payload.dns_api_token) {
			Toast.error('请填写 ACME 邮箱和 DNS API Token');
			return;
		}
		button.disabled = true;
		button.textContent = '申请中…';
		try {
			const updated = await API.requestPanelCertificate(payload);
		Toast.success(updated.staging_tested ? 'ACME 测试成功；正式证书未替换' : (updated.certificate_reused ? '证书与当前面板域名一致，继续使用现有证书' : (updated.restart_required ? '证书已签发，请点击重启按钮' : '证书已签发并热加载')));
			closeModal();
			await showPanelCertificateModal();
		} catch (error) {
			button.disabled = false;
			button.textContent = '申请证书';
			Toast.error(error.message);
		}
	};
	openModal({ closeOnBackdrop: false });
}

async function showSiteIconPackModal(returnToSite = null, reopenSiteModal = false) {
  const existing = siteIconPack && Array.isArray(siteIconPack.icons) ? siteIconPack.icons.length : 0;
  document.getElementById('modal-title').textContent = '导入站点图标包';
  document.getElementById('modal-body').innerHTML = `
    <div class="site-icon-pack-modal-copy">
      <p>上传一个 JSON 图标包，或填写公开的 HTTPS JSON 地址。系统只保存图标名称和 HTTPS 图片地址，不内置任何图标包。</p>
      <p class="form-help">当前${existing ? `已载入 ${existing} 个图标` : '尚未载入图标包'}。导入后点击站点卡片左侧的圆形图标即可按名称搜索并选择。</p>
    </div>
    <div class="form-group">
      <label for="m-icon-pack-file">上传 JSON 文件</label>
      <input class="form-input site-icon-pack-file" id="m-icon-pack-file" type="file" accept="application/json,.json">
    </div>
    <div class="site-icon-pack-divider"><span>或</span></div>
    <div class="form-group">
      <label for="m-icon-pack-url">图标包 HTTPS 地址</label>
      <input class="form-input" id="m-icon-pack-url" type="url" placeholder="https://example.com/icon-pack.json" inputmode="url" autocapitalize="none" autocorrect="off" spellcheck="false">
      <div class="form-help">如果浏览器因跨域策略无法读取地址，请先下载 JSON 文件再上传。</div>
    </div>
    <div class="site-icon-pack-status" id="m-icon-pack-status" role="status" aria-live="polite"></div>
  `;
  document.getElementById('modal-footer').innerHTML = `
    <button class="btn-modal secondary" id="m-icon-pack-cancel">取消</button>
    <button class="btn-modal" id="m-icon-pack-clear" ${existing ? '' : 'disabled'}>清空图标包</button>
    <button class="btn-modal primary" id="m-icon-pack-import">导入</button>
  `;
  openModal({ closeOnBackdrop: false, modalClass: 'site-icon-pack-modal' });

  const fileInput = document.getElementById('m-icon-pack-file');
  const urlInput = document.getElementById('m-icon-pack-url');
  const status = document.getElementById('m-icon-pack-status');
  const importButton = document.getElementById('m-icon-pack-import');
  const setStatus = (message, error = false) => {
    status.textContent = message || '';
    status.classList.toggle('is-error', !!error);
  };
  const readFile = file => new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = () => resolve(String(reader.result || ''));
    reader.onerror = () => reject(new Error('读取图标包文件失败'));
    reader.readAsText(file);
  });
  const loadText = async () => {
    const file = fileInput.files && fileInput.files[0];
    if (file) return { text: await readFile(file), source: '' };
    const source = String(urlInput.value || '').trim();
    if (!source) throw new Error('请选择 JSON 文件或填写图标包地址');
    if (!/^https:\/\//i.test(source)) throw new Error('图标包地址必须使用 HTTPS');
    const response = await fetch(source, { credentials: 'omit', redirect: 'error' });
    if (!response.ok) throw new Error(`读取图标包失败（HTTP ${response.status}）`);
    return { text: await response.text(), source };
  };
  const importPack = async () => {
    importButton.disabled = true;
    setStatus('正在读取并校验图标包…');
    try {
      const loaded = await loadText();
      if (loaded.text.length > 4 * 1024 * 1024) throw new Error('图标包不能超过 4 MiB');
      const parsed = JSON.parse(loaded.text);
      const normalized = normalizeSiteIconPack(parsed);
      if (!normalized.icons.length) throw new Error('图标包中没有可用的 HTTPS 图标');
      const saved = await API.saveSiteIconPack(parsed, loaded.source);
      siteIconPack = normalizeSiteIconPack(saved);
      Toast.success(`已导入 ${siteIconPack.icons.length} 个图标`);
      closeModal();
      await loadSites();
      if (reopenSiteModal) await showSiteModal(returnToSite || undefined);
    } catch (error) {
      importButton.disabled = false;
      setStatus(error && error.message ? error.message : '图标包导入失败', true);
    }
  };
  fileInput.addEventListener('change', () => {
    if (fileInput.files && fileInput.files[0]) urlInput.value = '';
  });
  document.getElementById('m-icon-pack-cancel').onclick = closeModal;
  document.getElementById('m-icon-pack-clear').onclick = async () => {
    const clearButton = document.getElementById('m-icon-pack-clear');
    clearButton.disabled = true;
    try {
      await API.clearSiteIconPack();
      siteIconPack = normalizeSiteIconPack({ icons: [] });
      Toast.success('图标包已清空，已有站点图标仍会保留');
      closeModal();
      await loadSites();
      if (reopenSiteModal) await showSiteModal(returnToSite || undefined);
    } catch (error) {
      clearButton.disabled = false;
      setStatus(error.message || '清空失败', true);
    }
  };
  importButton.onclick = importPack;
}

async function showSiteIconManager(site) {
  if (!site || !site.id) return;
  if (!siteIconPack.icons.length) {
    try {
      siteIconPack = normalizeSiteIconPack(await API.getSiteIconPack());
    } catch (_) {
      siteIconPack = normalizeSiteIconPack({ icons: [] });
    }
  }
  const current = siteIconFor(site);
  document.getElementById('modal-title').textContent = '设置站点图标';
  document.getElementById('modal-body').innerHTML = `
    <div class="site-icon-manager-current">
      <div class="site-icon-manager-current-preview">${renderSiteIcon(site, 'site-icon-large')}</div>
      <div>
        <strong>${esc(site.name)}</strong>
        <p>${current ? `当前图标：${esc(current.name)}` : '当前使用站点首字母'}</p>
      </div>
    </div>
    <div class="site-icon-manager-search">
      <label class="sr-only" for="m-site-icon-search">搜索图标</label>
      <span aria-hidden="true">⌕</span>
      <input type="search" class="form-input" id="m-site-icon-search" placeholder="按图标包名称搜索" autocomplete="off">
    </div>
    <div class="site-icon-results site-icon-manager-results" id="m-site-icon-manager-results" role="listbox" aria-label="可用站点图标"></div>
    <p class="form-help site-icon-manager-help">图标来自你上传的 JSON 图标包。需要更换图标包时，请返回站点页点击“图标包”。</p>
  `;
  document.getElementById('modal-footer').innerHTML = `
    <button class="btn-modal secondary" id="m-site-icon-clear">使用首字母</button>
    <button class="btn-modal secondary" id="m-site-icon-pack">图标包管理</button>
    <button class="btn-modal primary" id="m-site-icon-close">完成</button>
  `;
  openModal({ closeOnBackdrop: true, modalClass: 'site-icon-manager-modal' });

  const searchInput = document.getElementById('m-site-icon-search');
  const results = document.getElementById('m-site-icon-manager-results');
  const close = () => closeModal();
  const render = () => {
    const query = String(searchInput.value || '').trim().toLowerCase();
    const icons = (siteIconPack.icons || []).filter(icon => !query || icon.name.toLowerCase().includes(query)).slice(0, 120);
    results.innerHTML = icons.length ? icons.map(icon => `
      <button type="button" class="site-icon-option ${current && icon.name === current.name && icon.url === current.url ? 'is-selected' : ''}" data-icon-name="${esc(icon.name)}" data-icon-url="${esc(icon.url)}" role="option" aria-selected="${current && icon.name === current.name && icon.url === current.url ? 'true' : 'false'}">
        <span class="site-icon-option-preview">${renderSiteIcon({ name: icon.name, icon_name: icon.name, icon_url: icon.url }, 'site-icon-option-icon')}</span>
        <span class="site-icon-option-name">${esc(icon.name)}</span>
      </button>`).join('') : '<div class="site-icon-empty">暂无可用图标，请先上传图标包</div>';
    results.querySelectorAll('.site-icon-option').forEach(button => {
      button.onclick = async () => {
        button.disabled = true;
        try {
          const updated = await API.updateSiteIcon(site.id, button.dataset.iconName || '', button.dataset.iconUrl || '');
          site.icon_name = updated.icon_name || '';
          site.icon_url = updated.icon_url || '';
          Toast.success('站点图标已更新');
          close();
          await loadSites();
        } catch (error) {
          button.disabled = false;
          Toast.error(error.message || '保存站点图标失败');
        }
      };
    });
    bindSiteIconFallbacks(results);
  };
  searchInput.oninput = render;
  document.getElementById('m-site-icon-clear').onclick = async () => {
    const clearButton = document.getElementById('m-site-icon-clear');
    clearButton.disabled = true;
    try {
      const updated = await API.updateSiteIcon(site.id, '', '');
      site.icon_name = updated.icon_name || '';
      site.icon_url = updated.icon_url || '';
      Toast.success('已恢复站点首字母');
      close();
      await loadSites();
    } catch (error) {
      clearButton.disabled = false;
      Toast.error(error.message || '清除站点图标失败');
    }
  };
  document.getElementById('m-site-icon-pack').onclick = () => showSiteIconPackModal();
  document.getElementById('m-site-icon-close').onclick = close;
  render();
  searchInput.focus();
}

async function showSiteModal(site) {
  const isEdit = !!site;
  const title = isEdit ? '编辑站点' : '添加站点';
	let siteCapabilities;
	try {
		siteCapabilities = normalizeSiteCapabilities(await API.ingressCapabilities());
	} catch (error) {
		Toast.error(`无法读取站点能力：${error.message}`);
		return;
	}
	if (!siteIconPack.icons.length) {
		try {
			siteIconPack = normalizeSiteIconPack(await API.getSiteIconPack());
		} catch (_) {
			// The picker remains usable as an empty state when the optional icon
			// package endpoint is temporarily unavailable.
		}
	}
	const hostOnlyAvailable = siteCapabilities.host_only_available;
	const upstreamHeadersAvailable = siteCapabilities.upstream_headers_available;
	const domainPrefixAvailable = siteCapabilities.domain_prefix_available === true;
	const panelTLSReady = siteCapabilities.panel_tls_enabled === true;
	const canUseHostIngress = hostOnlyAvailable && domainPrefixAvailable && (panelTLSReady || (isEdit && String(site.public_host || '').trim() !== ''));
	const splitTargetAddress = value => {
		const raw = String(value || '').trim();
		if (!raw) return { address: '', port: '' };
		const explicitScheme = /^[a-z][a-z0-9+.-]*:\/\//i.test(raw);
		try {
			const parsed = new URL(explicitScheme ? raw : `http://${raw}`);
			const defaultPort = parsed.protocol === 'https:' ? '443' : '80';
			const hostname = parsed.hostname;
			const path = `${parsed.pathname === '/' ? '' : parsed.pathname}${parsed.search}${parsed.hash}`;
			return {
				address: `${explicitScheme ? `${parsed.protocol}//` : ''}${hostname}${path}`,
				port: parsed.port || defaultPort,
			};
		} catch (_) {
			return { address: raw, port: '' };
		}
	};
	const joinTargetAddress = (address, port) => {
		const rawAddress = String(address || '').trim();
		const rawPort = String(port || '').trim();
		if (!rawAddress) return '';
		const explicitScheme = /^[a-z][a-z0-9+.-]*:\/\//i.test(rawAddress);
		try {
			const parsed = new URL(explicitScheme ? rawAddress : `http://${rawAddress}`);
			parsed.port = rawPort;
			const normalized = parsed.toString().replace(/\/$/, parsed.pathname === '/' && !parsed.search && !parsed.hash ? '' : '/');
			return explicitScheme ? normalized : normalized.replace(/^http:\/\//i, '');
		} catch (_) {
			return rawPort ? `${rawAddress.replace(/:\d+$/, '')}:${rawPort}` : rawAddress;
		}
	};
	const primaryTargetParts = splitTargetAddress(isEdit ? site.target_url : '');
	const hostIngressBlockedHint = !hostOnlyAvailable
		? '当前面板未满足安全的域名前缀转发条件，请先设置 PANEL_BIND_ADDR 或 TRUSTED_PROXY_CIDRS 并重启。'
		: !domainPrefixAvailable
			? '域名前缀通过面板端口转发，不会绑定站点端口，例如 https://123.example.com:9090。请配置 PANEL_ROUTE_DOMAIN。'
			: !panelTLSReady
				? '请先在 TLS 页配置面板域名、泛域名并申请证书，完成后才能启用域名前缀。'
				: '';
  document.getElementById('modal-title').textContent = title;
  document.getElementById('modal-body').innerHTML = `
    <div class="site-config-form">
      <p class="site-config-intro">${isEdit ? '调整站点的回源、访问入口与使用策略。' : '连接一个媒体服务，再为它设置访问入口。'}</p>
      <section class="site-config-section" aria-labelledby="site-section-basics">
        <header class="site-section-heading"><span class="site-section-index" aria-hidden="true">01</span><div><h4 id="site-section-basics">基本资料</h4><p>为站点取一个容易辨认的名称</p></div></header>
        <div class="form-group">
          <label for="m-name">站点名称</label>
          <input type="text" class="form-input" id="m-name" value="${isEdit ? esc(site.name) : ''}" placeholder="如：Emby-US-01" maxlength="100" required>
        </div>
      </section>
      <section class="site-config-section" aria-labelledby="site-section-origins">
        <header class="site-section-heading"><span class="site-section-index" aria-hidden="true">02</span><div><h4 id="site-section-origins">回源线路</h4><p>优先使用主线路，失败时依次切换备用线路</p></div><div class="upstream-lines-buttons"><button type="button" class="console-button upstream-test-all" id="m-test-all-lines">线路测速</button><button type="button" class="console-button upstream-add-line" id="m-add-failover-line"><svg viewBox="0 0 24 24" aria-hidden="true"><path d="M12 5v14M5 12h14"/></svg>添加线路</button></div></header>
        <div class="upstream-lines" id="m-upstream-lines">
          <div class="upstream-line is-primary" data-line="primary">
            <div class="site-origin-meta"><span class="site-origin-role">主线路</span><label class="upstream-line-enabled"><input type="checkbox" checked disabled><span>始终启用</span></label><span class="upstream-line-latency">未测试</span></div>
            <div class="site-origin-fields">
              <label>线路名称<input type="text" class="form-input upstream-line-name" id="m-primary-line-name" value="${esc(isEdit ? (site.primary_line_name || '主线路') : '主线路')}" maxlength="100" aria-label="主线路名称"></label>
              <label>协议、域名与路径<input type="text" class="form-input upstream-line-address" id="m-target-address" value="${esc(primaryTargetParts.address)}" placeholder="https://emby.example.com" inputmode="url" autocapitalize="none" autocorrect="off" spellcheck="false" maxlength="2048" required aria-label="主回源域名或地址"></label>
              <label>端口<input type="number" class="form-input upstream-line-port" id="m-target-port" value="${esc(primaryTargetParts.port)}" placeholder="443" min="1" max="65535" inputmode="numeric" required aria-label="主回源端口"></label>
            </div>
            <input type="hidden" id="m-target">
          </div>
          <div id="m-failover-lines"></div>
        </div>
        <details class="site-field-help"><summary>地址自动补全与备用线路</summary><p>地址离开输入框时自动拆分端口。直接输入域名默认为 https / 443；带 :8096 等其他端口时使用 http，已填写的独立端口会保留。支持 Base URL 路径。最多 7 条备用线路，停用后保留配置；主线路恢复后自动回切。</p></details>
      </section>
      <section class="site-config-section" aria-labelledby="site-section-ingress">
        <header class="site-section-heading"><span class="site-section-index" aria-hidden="true">03</span><div><h4 id="site-section-ingress">访问入口</h4><p>选择客户端访问这个站点的方式</p></div></header>
        <div class="site-config-fields">
          <div class="form-group">
            <label for="m-ingress-mode">入口模式</label>
            <select class="form-select modal-select" id="m-ingress-mode">
              ${isEdit && normalizedIngressMode(site) === 'unset' ? '<option value="unset" disabled>未配置（请选择）</option>' : ''}
              <option value="host" ${canUseHostIngress ? '' : 'disabled'}>域名前缀（推荐${canUseHostIngress ? '' : '，需先配置 TLS 和域名'}）</option>
              <option value="path">路径</option><option value="port">独立端口</option>
            </select>
            <div class="form-help" id="m-ingress-warning"></div>
            ${hostIngressBlockedHint ? `<details class="site-field-help"><summary>域名前缀的使用条件</summary><p>${hostIngressBlockedHint}</p></details>` : ''}
          </div>
          <div class="form-group" id="m-port-group">
            <label for="m-port" id="m-port-label">监听端口</label>
            <input type="number" class="form-input" id="m-port" value="${isEdit && ['port', 'both'].includes(normalizedIngressMode(site)) ? site.listen_port : ''}" placeholder="如：8001" min="1" max="65535" inputmode="numeric">
            <div class="form-help" id="m-port-help"></div>
          </div>
          <div class="form-group" id="m-public-host-group">
            <label for="m-route-prefix">域名前缀</label>
            <input type="text" class="form-input" id="m-route-prefix" value="${isEdit ? esc(routePrefixForSite(site, siteCapabilities.route_domain)) : ''}" placeholder="如：123" autocapitalize="none" autocorrect="off" spellcheck="false" maxlength="63">
            <input type="hidden" id="m-public-host" value="${isEdit ? esc(site.public_host || '') : ''}">
            <div class="form-help">https://前缀.${esc(siteCapabilities.route_domain || 'example.com')}:${esc(String((typeof window !== 'undefined' && window.location && window.location.port) || '9090'))}，通过面板端口访问。</div>
          </div>
          <div class="form-group" id="m-path-prefix-group" hidden>
            <label for="m-path-prefix">路径前缀</label>
            <input type="text" class="form-input" id="m-path-prefix" value="${isEdit ? esc(String(site.path_prefix || '').replace(/^\//, '')) : ''}" placeholder="如：emby" autocapitalize="none" autocorrect="off" spellcheck="false" maxlength="64">
            <div class="form-help">填写一段路径，自动补全为 /emby/。不能使用 api、js、css、_meridian 等系统路径。</div>
          </div>
        </div>
      </section>
      <details class="site-advanced-card site-form-wide">
        <summary class="site-advanced-summary"><span><strong>高级策略</strong><small>客户端身份、播放、缓存与使用限制</small></span><svg viewBox="0 0 24 24" aria-hidden="true"><path d="m6 9 6 6 6-6"/></svg></summary>
        <div class="site-advanced-body">
          <div class="site-form-columns" id="m-site-form-columns">
            <div class="site-form-column site-form-column-policy">
              <h4 class="site-policy-heading">请求与播放</h4>
              <div class="site-config-fields">
                <div class="form-group site-ua-group site-field-ua">
                  <label for="m-ua">UA 模式</label>
                  <select class="form-select modal-select" id="m-ua"><option value="passthrough" ${(!isEdit || site.ua_mode === 'passthrough') ? 'selected' : ''}>透传（保留客户端身份）</option><option value="custom" ${isEdit && uaModeIsCustomLike(site.ua_mode) ? 'selected' : ''}>自定义</option></select>
                  <div class="site-custom-ua-fields" id="m-custom-ua-group" hidden>
                    <label for="m-custom-ua">User-Agent</label><input type="text" class="form-input" id="m-custom-ua" placeholder="User-Agent" maxlength="1024" autocapitalize="none" autocorrect="off" spellcheck="false">
                    <div class="site-config-fields"><div><label for="m-custom-client">Emby Client</label><input type="text" class="form-input" id="m-custom-client" placeholder="Emby Client" maxlength="128" autocapitalize="none" autocorrect="off" spellcheck="false"></div><div><label for="m-custom-version">Emby Version</label><input type="text" class="form-input" id="m-custom-version" placeholder="Emby Version" maxlength="64" autocapitalize="none" autocorrect="off" spellcheck="false"></div></div>
                    <div class="form-help">仅改写 User-Agent、Client 和 Version；Device 与 DeviceId 保持原样。</div>
                  </div>
                </div>
                <div class="form-group site-field-client-ip">
                  <label for="m-client-ip-mode">客户端 IP 透传</label>
                  <select class="form-select modal-select" id="m-client-ip-mode"><option value="both" ${!isEdit || !site.client_ip_mode || site.client_ip_mode === 'both' ? 'selected' : ''}>X-Real-IP 与 X-Forwarded-For</option><option value="real_ip" ${isEdit && site.client_ip_mode === 'real_ip' ? 'selected' : ''}>仅保留 X-Real-IP</option><option value="none" ${isEdit && site.client_ip_mode === 'none' ? 'selected' : ''}>不透传</option></select>
                  <div class="form-help">控制发送给回源的来源字段，不影响日志识别客户端 IP。</div>
                </div>
                <div class="form-group site-field-video">
                  <label for="m-main-video-mode">主视频流策略</label>
                  <select class="form-select modal-select" id="m-main-video-mode"><option value="proxy" ${!isEdit || site.main_video_stream_mode !== 'direct' ? 'selected' : ''}>反代</option><option value="direct" ${isEdit && site.main_video_stream_mode === 'direct' ? 'selected' : ''}>直连</option></select>
                  <details class="site-field-help"><summary>直连范围与限制</summary><p>直连会校验网盘或 CDN 的 302 等最终公网地址，再将 MP4、MKV、MOV、AVI、WebM 及 /Videos/.../stream、original、download、file 等主视频体通过 307 交给播放器；面板、API、PlaybackInfo、HLS / DASH、字幕、图片和必要静态资源仍由 Meridian 反代。自动发现保持启用；localhost、私网、链路本地及回环目标始终拒绝。</p></details>
                </div>
                <div class="form-group site-field-history">
                  <label for="m-watch-history">观看历史记录</label>
                  <select class="form-select modal-select" id="m-watch-history"><option value="off" ${!isEdit || !site.watch_history_enabled ? 'selected' : ''}>关闭</option><option value="on" ${isEdit && site.watch_history_enabled ? 'selected' : ''}>开启</option></select>
                  <div class="form-help">记录后续成功播放，关闭后保留已有历史。</div>
                  <details class="site-field-help"><summary>记录哪些信息</summary><p>保存用户名、设备与原始 PlaySessionId 供管理员排查；令牌仅加密保存且绝不显示或回传。不会保存客户端 IP 或原始请求正文。</p></details>
                </div>
              </div>
            </div>
            <div class="site-form-column-secondary">
              <div class="site-form-column site-form-column-cache">
                <h4 class="site-policy-heading">资源缓存</h4>
                <div class="site-config-fields">
                  <div class="form-group site-field-cache-toggle"><label for="m-asset-cache">缓存图片与静态资源</label><select class="form-select modal-select" id="m-asset-cache"><option value="off" ${!isEdit || !site.asset_cache_enabled ? 'selected' : ''}>关闭</option><option value="on" ${isEdit && site.asset_cache_enabled ? 'selected' : ''}>开启</option></select><details class="site-field-help"><summary>缓存范围</summary><p>仅缓存图片、CSS、JS、字体和 WASM；视频、音频、HLS、DASH、Range 请求、私有响应及带 Set-Cookie 的响应永不缓存。</p></details></div>
                  <div class="form-group cache-limit-group site-field-cache-limits"><div class="cache-limit-grid"><div><label for="m-cache-ttl">缓存时间（小时）</label><input type="number" class="form-input" id="m-cache-ttl" min="1" max="720" value="${isEdit ? Math.max(1, Math.round((site.asset_cache_ttl_sec || 86400) / 3600)) : 24}"></div><div><label for="m-cache-max">容量上限（MB）</label><input type="number" class="form-input" id="m-cache-max" min="1" max="20480" value="${isEdit ? Math.max(1, Math.round((site.asset_cache_max_bytes || 536870912) / 1048576)) : 512}"></div></div></div>
                  <div class="form-group site-field-cache-rules"><label for="m-cache-rules">缓存规则</label><textarea class="form-input" id="m-cache-rules" rows="3" maxlength="4096" spellcheck="false">${esc(isEdit ? (site.asset_cache_rules || '*/file/*\n*/emby/Items/*/Images/*') : '*/file/*\n*/emby/Items/*/Images/*')}</textarea><div class="form-help">每行一条，支持 * 通配符。</div></div>
                </div>
              </div>
              <div class="site-form-column site-form-column-limits">
                <h4 class="site-policy-heading">额度与保号</h4>
                <div class="site-config-fields">
                  <div class="form-group site-field-quota"><label for="m-quota">流量额度（GB）</label><input type="number" class="form-input" id="m-quota" value="${isEdit ? Math.round((site.traffic_quota || 0) / 1073741824) : 0}" placeholder="0" min="0" inputmode="numeric"><div class="form-help">0 为不限。达到额度后拒绝后续新请求；已经开始的媒体传输不会中途截断，少量并发请求可能使最终用量略高于额度。</div></div>
                  <div class="form-group site-field-speed"><label for="m-speed">单连接限速（Mbps）</label><input type="number" class="form-input" id="m-speed" value="${isEdit ? (site.speed_limit || 0) : 0}" placeholder="0" min="0" max="1000000" step="1" inputmode="numeric"><div class="form-help">0 为不限。仅限制 HTTP 响应与 WebSocket 下行，上传不受影响。</div></div>
                  <div class="form-group site-field-retention"><label for="m-account-retention-days">保号周期（天）</label><input type="number" class="form-input" id="m-account-retention-days" value="${isEdit && Number(site.account_retention_days) > 0 ? Number(site.account_retention_days) : ''}" placeholder="留空关闭，如：30" min="1" max="3650" step="1" inputmode="numeric"><div class="form-help">1–3650 天，留空关闭。同一播放身份完成 5 次有效播放状态同步后重置倒计时；剩余不多于 7 天显示提醒，Telegram 日报同步结果。</div></div>
                </div>
              </div>
            </div>
          </div>
          <div class="site-advanced-subsection">
            <div class="site-policy-head"><h4 class="site-policy-heading">主回源固定请求头</h4><button type="button" class="console-button upstream-header-add" id="m-add-upstream-header" ${upstreamHeadersAvailable ? '' : 'disabled'}>添加请求头</button></div>
            <div id="m-upstream-headers"></div>
            <div class="form-help">已有值留空会保持不变；仅发送给主回源，不能与备用线路同时使用。</div>
            <details class="site-field-help"><summary>加密保存与地址变更</summary><p>值使用 UPSTREAM_HEADER_KEY 加密保存且不会回显，只发送给主回源的精确协议、域名和端口；更换主回源的协议、域名或端口后必须重新输入这些值。</p></details>
            ${upstreamHeadersAvailable ? '' : '<div class="form-help site-field-warning">当前部署未配置 UPSTREAM_HEADER_KEY，不能新增、重命名或修改 Header 值；仍可删除旧配置。配置密钥并重启后可恢复编辑。</div>'}
          </div>
        </div>
      </details>
    </div>
  `;

  document.getElementById('modal-footer').innerHTML = `
    <span class="site-config-save-note">地址、入口与策略会一并保存</span>
    <button type="button" class="console-button" id="m-cancel">取消</button>
    <button type="button" class="console-button is-primary" id="m-submit">${isEdit ? '保存' : '创建'}</button>
  `;
	document.getElementById('m-cancel').addEventListener('click', closeModal);

	const failoverLinesContainer = document.getElementById('m-failover-lines');
	let failoverLines = isEdit && Array.isArray(site.failover_lines)
		? site.failover_lines.map(line => ({ name: String(line.name || ''), url: String(line.url || ''), enabled: line.enabled !== false, ...splitTargetAddress(line.url) }))
		: (isEdit && Array.isArray(site.failover_targets)
			? site.failover_targets.map((url, index) => ({ name: `线路${index + 2}`, url: String(url || ''), enabled: true, ...splitTargetAddress(url) }))
			: []);

	function lineActionIcon(kind) {
		if (kind === 'up') return '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="m18 15-6-6-6 6"/></svg>';
		if (kind === 'down') return '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="m6 9 6 6 6-6"/></svg>';
		if (kind === 'remove') return '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M3 6h18M8 6V4h8v2m-9 0 1 14h8l1-14M10 10v6m4-6v6"/></svg>';
		return '';
	}

	function renderFailoverLines() {
		failoverLinesContainer.innerHTML = failoverLines.map((line, index) => `
		  <div class="upstream-line" data-line-index="${index}">
		    <div class="site-origin-meta"><span class="site-origin-role">备用 ${index + 1}</span><label class="upstream-line-enabled"><input type="checkbox" class="upstream-line-toggle" ${line.enabled ? 'checked' : ''}><span>${line.enabled ? '已启用' : '已停用'}</span></label><span class="upstream-line-latency">未测试</span><div class="upstream-line-actions"><button type="button" class="icon-button upstream-line-move-up" title="上移" aria-label="上移" ${index === 0 ? 'disabled' : ''}>${lineActionIcon('up')}</button><button type="button" class="icon-button upstream-line-move-down" title="下移" aria-label="下移" ${index === failoverLines.length - 1 ? 'disabled' : ''}>${lineActionIcon('down')}</button><button type="button" class="icon-button upstream-line-remove" title="删除线路" aria-label="删除线路">${lineActionIcon('remove')}</button></div></div>
		    <div class="site-origin-fields"><label>线路名称<input type="text" class="form-input upstream-line-name" value="${esc(line.name)}" placeholder="线路${index + 2}" maxlength="100" aria-label="备用线路名称"></label><label>协议、域名与路径<input type="text" class="form-input upstream-line-address" value="${esc(line.address)}" placeholder="https://backup.example.com" maxlength="2048" inputmode="url" autocapitalize="none" autocorrect="off" spellcheck="false" aria-label="备用线路域名或地址"></label><label>端口<input type="number" class="form-input upstream-line-port" value="${esc(line.port)}" data-autofilled-port="${esc(line.autofilledPort || '')}" placeholder="443" min="1" max="65535" inputmode="numeric" aria-label="备用线路端口"></label></div>
		  </div>`).join('');

		failoverLinesContainer.querySelectorAll('.upstream-line').forEach(row => {
			const index = Number(row.dataset.lineIndex);
			row.querySelector('.upstream-line-toggle').onchange = event => {
				failoverLines[index].enabled = event.target.checked;
				renderFailoverLines();
			};
			row.querySelector('.upstream-line-name').oninput = event => { failoverLines[index].name = event.target.value; };
			row.querySelector('.upstream-line-address').oninput = event => { failoverLines[index].address = event.target.value; };
			row.querySelector('.upstream-line-address').onchange = event => {
				const portInput = row.querySelector('.upstream-line-port');
				autoFillTargetAddress(event.target, portInput);
				failoverLines[index].address = event.target.value;
				failoverLines[index].port = portInput.value;
				failoverLines[index].autofilledPort = portInput.dataset.autofilledPort || '';
			};
			row.querySelector('.upstream-line-port').oninput = event => {
				delete event.target.dataset.autofilledPort;
				failoverLines[index].port = event.target.value;
				failoverLines[index].autofilledPort = '';
			};
			row.querySelector('.upstream-line-move-up').onclick = () => {
				[failoverLines[index - 1], failoverLines[index]] = [failoverLines[index], failoverLines[index - 1]];
				renderFailoverLines();
			};
			row.querySelector('.upstream-line-move-down').onclick = () => {
				[failoverLines[index + 1], failoverLines[index]] = [failoverLines[index], failoverLines[index + 1]];
				renderFailoverLines();
			};
			row.querySelector('.upstream-line-remove').onclick = () => {
				failoverLines.splice(index, 1);
				renderFailoverLines();
			};
		});
	}

	async function testUpstreamLine(row, addressInput, portInput) {
		const latency = row.querySelector('.upstream-line-latency');
		const targetURL = joinTargetAddress(addressInput.value, portInput.value);
		if (!targetURL) {
			latency.textContent = '请填写地址';
			latency.className = 'upstream-line-latency bad';
			return;
		}
		latency.textContent = '测试中…';
		latency.className = 'upstream-line-latency';
		try {
			const result = await API.testUpstream(targetURL);
			if (result && result.status === 'online') {
				latency.textContent = `${Number(result.latency_ms) || 0} ms`;
				latency.className = `upstream-line-latency ${(Number(result.latency_ms) || 0) < 800 ? 'good' : 'warn'}`;
			} else {
				throw new Error((result && result.error) || '线路不可用');
			}
		} catch (error) {
			latency.textContent = '失败';
			latency.className = 'upstream-line-latency bad';
			latency.title = error.message || '线路测试失败';
		}
	}

	renderFailoverLines();
	document.getElementById('m-target-address').onchange = event => {
		autoFillTargetAddress(event.target, document.getElementById('m-target-port'));
	};
	document.getElementById('m-target-port').oninput = event => {
		delete event.target.dataset.autofilledPort;
	};
	document.getElementById('m-add-failover-line').onclick = () => {
		if (failoverLines.length >= 7) {
			Toast.error('最多添加 7 条备用线路');
			return;
		}
		failoverLines.push({ name: `线路${failoverLines.length + 2}`, url: '', address: '', port: '', enabled: true });
		renderFailoverLines();
		const inputs = failoverLinesContainer.querySelectorAll('.upstream-line-address');
		if (inputs.length) inputs[inputs.length - 1].focus();
	};
	document.getElementById('m-test-all-lines').onclick = async event => {
		const button = event.currentTarget;
		button.disabled = true;
		button.textContent = '测试中…';
		const rows = [...document.querySelectorAll('#m-upstream-lines .upstream-line')].filter(row => {
			if (row.dataset.line === 'primary') return true;
			const index = Number(row.dataset.lineIndex);
			return failoverLines[index] && failoverLines[index].enabled;
		});
		for (const row of rows) {
			await testUpstreamLine(row, row.querySelector('.upstream-line-address'), row.querySelector('.upstream-line-port'));
		}
		button.disabled = false;
		button.textContent = '线路测速';
	};

	const ingressSelect = document.getElementById('m-ingress-mode');
	const publicHostGroup = document.getElementById('m-public-host-group');
	const publicHostInput = document.getElementById('m-public-host');
	const routePrefixInput = document.getElementById('m-route-prefix');
	const pathPrefixGroup = document.getElementById('m-path-prefix-group');
	const pathPrefixInput = document.getElementById('m-path-prefix');
	const portInput = document.getElementById('m-port');
	const portLabel = document.getElementById('m-port-label');
	const portHelp = document.getElementById('m-port-help');
	const ingressWarning = document.getElementById('m-ingress-warning');
	ingressSelect.value = isEdit ? normalizedIngressMode(site) : defaultIngressMode(siteCapabilities);
	function updateIngressFields() {
		const state = ingressFormState(ingressSelect.value);
		publicHostGroup.hidden = !state.showPublicHost;
		publicHostInput.required = state.requirePublicHost;
		routePrefixInput.required = state.requirePublicHost && domainPrefixAvailable;
		routePrefixInput.disabled = !state.showPublicHost;
		pathPrefixGroup.hidden = !state.showPathPrefix;
		pathPrefixInput.required = state.requirePathPrefix;
		pathPrefixInput.disabled = !state.showPathPrefix;
		portInput.required = state.requireListenPort;
		document.getElementById('m-port-group').hidden = !state.requireListenPort;
		portLabel.textContent = state.portLabel;
		portHelp.textContent = state.requireListenPort
			? '独立端口模式需要填写未被占用的监听端口。'
			: '共享入口不会另外开放站点端口。';
		ingressWarning.textContent = state.warning;
	}
	updateIngressFields();
	ingressSelect.addEventListener('change', updateIngressFields);

	const uaSelect = document.getElementById('m-ua');
  const clientIPModeSelect = document.getElementById('m-client-ip-mode');
  const mainVideoModeSelect = document.getElementById('m-main-video-mode');
  const watchHistorySelect = document.getElementById('m-watch-history');
  const accountRetentionDaysInput = document.getElementById('m-account-retention-days');
  const customUAGroup = document.getElementById('m-custom-ua-group');
  const advancedColumns = document.getElementById('m-site-form-columns');
  const customUAInputs = [
    document.getElementById('m-custom-ua'),
    document.getElementById('m-custom-client'),
    document.getElementById('m-custom-version'),
  ];
  const initialUAState = customUAFormState(isEdit ? site.ua_mode : 'passthrough', site);
  uaSelect.value = isEdit && uaModeIsCustomLike(site.ua_mode) ? 'custom' : 'passthrough';
  clientIPModeSelect.value = isEdit && ['both', 'real_ip', 'none'].includes(site.client_ip_mode) ? site.client_ip_mode : 'both';
  mainVideoModeSelect.value = isEdit && site.main_video_stream_mode === 'direct' ? 'direct' : 'proxy';
  watchHistorySelect.value = isEdit && site.watch_history_enabled ? 'on' : 'off';
  customUAInputs[0].value = initialUAState.customUserAgent;
  customUAInputs[1].value = initialUAState.customClient;
  customUAInputs[2].value = initialUAState.customVersion;

  function toggleCustomUAFields() {
    const state = customUAFormState(uaSelect.value);
    customUAGroup.hidden = !state.visible;
    advancedColumns.classList?.toggle('site-form-columns-custom-ua', state.visible);
    customUAInputs.forEach(input => {
      input.required = state.required;
    });
  }
  toggleCustomUAFields();
  uaSelect.addEventListener('change', toggleCustomUAFields);

  const upstreamHeadersContainer = document.getElementById('m-upstream-headers');
  let upstreamHeaders = isEdit && Array.isArray(site.upstream_headers) && site.upstream_headers.length
    ? site.upstream_headers.map(header => ({ name: header.name || '', value: '', configured: !!header.configured }))
    : [{ name: '', value: '', configured: false }];

  function renderUpstreamHeaders() {
    upstreamHeadersContainer.innerHTML = renderUpstreamHeaderRows(upstreamHeaders, upstreamHeadersAvailable);
    upstreamHeadersContainer.querySelectorAll('.m-upstream-header-name').forEach(input => {
      input.oninput = () => { upstreamHeaders[Number(input.dataset.idx)].name = input.value; };
    });
    upstreamHeadersContainer.querySelectorAll('.m-upstream-header-value').forEach(input => {
      input.oninput = () => { upstreamHeaders[Number(input.dataset.idx)].value = input.value; };
    });
    upstreamHeadersContainer.querySelectorAll('.m-upstream-header-remove').forEach(button => {
      button.onclick = () => {
        upstreamHeaders.splice(Number(button.dataset.idx), 1);
        renderUpstreamHeaders();
      };
    });
  }
  renderUpstreamHeaders();

  const addUpstreamHeaderButton = document.getElementById('m-add-upstream-header');
  addUpstreamHeaderButton.onclick = () => {
    if (!upstreamHeadersAvailable) {
      Toast.error('请先配置 UPSTREAM_HEADER_KEY 并重启 Meridian');
      return;
    }
    if (upstreamHeaders.length >= 16) {
      Toast.error('每个站点最多配置 16 个上游请求头');
      return;
    }
    upstreamHeaders.push({ name: '', value: '', configured: false });
    renderUpstreamHeaders();
    const inputs = upstreamHeadersContainer.querySelectorAll('.m-upstream-header-name');
    if (inputs.length) inputs[inputs.length - 1].focus();
  };

  document.getElementById('m-submit').onclick = async () => {
    const uaMode = uaSelect.value;
    const customUAPayload = buildCustomUAPayload(
      uaMode,
      customUAInputs[0].value,
      customUAInputs[1].value,
      customUAInputs[2].value,
    );
		const ingressPayload = buildIngressPayload(
		  ingressSelect.value,
		  document.getElementById('m-port').value,
		  publicHostInput.value,
		  routePrefixInput.value,
		  siteCapabilities.route_domain,
		  pathPrefixInput.value,
		);
		const primaryTargetURL = joinTargetAddress(
			document.getElementById('m-target-address').value,
			document.getElementById('m-target-port').value,
		);
		document.getElementById('m-target').value = primaryTargetURL;
		const accountRetentionRaw = String(accountRetentionDaysInput.value || '').trim();
		const accountRetentionDays = accountRetentionRaw === '' ? 0 : Number(accountRetentionRaw);
		const data = {
	      name: document.getElementById('m-name').value.trim(),
	      icon_name: isEdit ? String(site.icon_name || '').trim() : '',
	      icon_url: isEdit ? String(site.icon_url || '').trim() : '',
	      target_url: primaryTargetURL,
	      primary_line_name: document.getElementById('m-primary-line-name').value.trim() || '主线路',
	      failover_lines: failoverLines.map((line, index) => ({
	        name: String(line.name || '').trim() || `线路${index + 2}`,
	        url: joinTargetAddress(line.address, line.port),
	        enabled: line.enabled !== false,
	      })),
	      failover_targets: failoverLines.filter(line => line.enabled !== false).map(line => joinTargetAddress(line.address, line.port)).filter(Boolean),
      playback_target_url: isEdit ? String(site.playback_target_url || '') : '',
      playback_mode: isEdit ? String(site.playback_mode || 'direct') : 'direct',
		main_video_stream_mode: mainVideoModeSelect.value,
		watch_history_enabled: watchHistorySelect.value === 'on',
		stream_hosts: isEdit ? normalizeStreamHosts(site.stream_hosts) : [],
			...ingressPayload,
		upstream_headers: buildUpstreamHeaderPayload(upstreamHeaders),
      ua_mode: uaMode,
      client_ip_mode: clientIPModeSelect.value,
      ...customUAPayload,
      asset_cache_enabled: document.getElementById('m-asset-cache').value === 'on',
      asset_cache_ttl_sec: parseInt(document.getElementById('m-cache-ttl').value || 24) * 3600,
      asset_cache_max_bytes: parseInt(document.getElementById('m-cache-max').value || 512) * 1048576,
      asset_cache_rules: document.getElementById('m-cache-rules').value.trim(),
      account_retention_days: accountRetentionDays,
      traffic_quota: parseInt(document.getElementById('m-quota').value || 0) * 1073741824,
      speed_limit: parseInt(document.getElementById('m-speed').value || 0),
    };

		const listenPortRequired = ingressFormState(data.ingress_mode).requireListenPort;
		if (data.ingress_mode === 'unset') {
		  Toast.error('请选择当前服务器可用的入口模式');
		  return;
		}
		if (!data.name || !data.target_url || (listenPortRequired && !data.listen_port) || (data.ingress_mode === 'host' && !data.route_prefix && !data.public_host) || (data.ingress_mode === 'path' && !data.path_prefix)) {
	      Toast.error('请填写所有必填项');
	      return;
	    }
	  if (uaMode === 'custom' && (!data.custom_user_agent || !data.custom_client || !data.custom_version)) {
      Toast.error('请完整填写自定义 User-Agent、Client 和 Version');
		return;
	  }
	  if (!Number.isSafeInteger(data.account_retention_days) || (accountRetentionRaw !== '' && (data.account_retention_days < 1 || data.account_retention_days > 3650))) {
		Toast.error('保号周期必须为 1 到 3650 天的整数');
		return;
	  }
	  const invalidHeader = upstreamHeaders.some(header => {
		const name = String(header.name || '').trim();
		const value = String(header.value || '').trim();
		if (!header.configured && !name && !value) return false;
		return !name || (!header.configured && !value);
	  });
		if (invalidHeader) {
			Toast.error('请完整填写新增请求头的名称和值；已有值可留空保持不变');
			return;
		}
		if (!document.getElementById('m-target-port').value || data.failover_lines.some((line, index) => !line.url || !String(failoverLines[index].port || '').trim())) {
			Toast.error('请完整填写每条线路的域名和端口，或删除空线路');
			return;
		}
		if (data.failover_targets.length > 0 && upstreamHeaders.some(header => String(header.name || '').trim())) {
			Toast.error('备用线路不能与主回源固定请求头同时使用');
			return;
		}
		if (isEdit && normalizedTargetAuthority(site.target_url) !== normalizedTargetAuthority(data.target_url)) {
			const retainedSecret = upstreamHeaders.some(header => header.configured && !String(header.value || '').trim());
			if (retainedSecret) {
				Toast.error('主回源的协议、域名或端口已变化，请重新输入每个已配置的固定请求头，或删除对应行');
				return;
			}
		}

    try {
      if (isEdit) {
        await API.updateSite(site.id, data);
        Toast.success('站点已更新');
      } else {
        await API.createSite(data);
        Toast.success('站点已创建');
      }
      closeModal();
      loadSites();
    } catch (e) {
      Toast.error(e.message);
    }
  };

  openModal({ closeOnBackdrop: false, modalClass: 'site-config-modal' });
}

// Global actions
window.toggleSiteAction = async function(id) {
  try {
    const res = await API.toggleSite(id);
    Toast.success(res.enabled ? '站点已启用' : '站点已停用');
    loadSites();
  } catch (e) {
    Toast.error(e.message);
  }
};

window.editSiteAction = async function(id) {
  try {
    const sites = await API.listSites();
    const site = sites.find(s => s.id === id);
    if (site) showSiteModal(site);
  } catch (e) {
    Toast.error(e.message);
  }
};

window.deleteSiteAction = function(id, name) {
  document.getElementById('modal-title').textContent = '确认删除';
  const modalBody = document.getElementById('modal-body');
  modalBody.replaceChildren();
  const message = document.createElement('p');
  message.style.color = 'var(--white-60)';
  message.append('确定要删除站点 ');
  const strong = document.createElement('strong');
  strong.textContent = String(name);
  message.append(strong, ' 吗？此操作不可撤销。');
  modalBody.appendChild(message);
  document.getElementById('modal-footer').innerHTML = `
    <button class="btn-modal secondary" id="delete-cancel">取消</button>
    <button class="btn-modal primary" id="delete-confirm" style="background:var(--red)">删除</button>
  `;
  document.getElementById('delete-cancel').addEventListener('click', closeModal);
  document.getElementById('delete-confirm').addEventListener('click', () => confirmDelete(id));
  openModal({ closeOnBackdrop: true });
};

window.confirmDelete = async function(id) {
  try {
    await API.deleteSite(id);
    Toast.success('站点已删除');
    closeModal();
    loadSites();
  } catch (e) {
    Toast.error(e.message);
  }
};
