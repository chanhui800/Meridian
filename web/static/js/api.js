// Meridian API Client
let meridianTimezoneOffsetMinutes = 480;

function meridianSetTimezoneOffset(value) {
  const numeric = Number(value);
  if (Number.isFinite(numeric) && numeric >= -720 && numeric <= 840) {
    meridianTimezoneOffsetMinutes = Math.trunc(numeric);
  }
  return meridianTimezoneOffsetMinutes;
}

function meridianGetTimezoneOffset() {
  return meridianTimezoneOffsetMinutes;
}

function meridianTimezoneLabel(offset) {
  const value = Number.isFinite(Number(offset)) ? Number(offset) : meridianTimezoneOffsetMinutes;
  const sign = value < 0 ? '-' : '+';
  const absolute = Math.abs(value);
  return `UTC${sign}${String(Math.floor(absolute / 60)).padStart(2, '0')}:${String(absolute % 60).padStart(2, '0')}`;
}

function meridianTimezoneDate(timestamp) {
  const value = Number(timestamp);
  return new Date((Number.isFinite(value) ? value : Date.now()) + meridianTimezoneOffsetMinutes * 60000);
}

function meridianFormatDateTime(timestamp, includeSeconds = true) {
  const date = meridianTimezoneDate(timestamp);
  const pad = value => String(value).padStart(2, '0');
  const base = `${date.getUTCFullYear()}-${pad(date.getUTCMonth() + 1)}-${pad(date.getUTCDate())} ${pad(date.getUTCHours())}:${pad(date.getUTCMinutes())}`;
  return includeSeconds ? `${base}:${pad(date.getUTCSeconds())}` : base;
}

function meridianFormatDate(timestamp) {
  const date = meridianTimezoneDate(timestamp);
  return `${date.getUTCFullYear()}/${date.getUTCMonth() + 1}/${date.getUTCDate()}`;
}

function meridianDateTimeLocalValue(dateOrTimestamp) {
  const timestamp = dateOrTimestamp instanceof Date ? dateOrTimestamp.getTime() : Number(dateOrTimestamp);
  const date = meridianTimezoneDate(Number.isFinite(timestamp) ? timestamp : Date.now());
  const pad = value => String(value).padStart(2, '0');
  return `${date.getUTCFullYear()}-${pad(date.getUTCMonth() + 1)}-${pad(date.getUTCDate())}T${pad(date.getUTCHours())}:${pad(date.getUTCMinutes())}`;
}

function meridianParseDateTimeLocal(value) {
  const match = String(value || '').match(/^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2})$/);
  if (!match) return NaN;
  const timestamp = Date.UTC(Number(match[1]), Number(match[2]) - 1, Number(match[3]), Number(match[4]), Number(match[5]));
  return timestamp - meridianTimezoneOffsetMinutes * 60000;
}

function meridianParseDateTimeText(value) {
  const normalized = String(value || '').trim().replace(' ', 'T');
  const match = normalized.match(/^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2})(?::(\d{2}))?/);
  if (!match) return NaN;
  const timestamp = Date.UTC(Number(match[1]), Number(match[2]) - 1, Number(match[3]), Number(match[4]), Number(match[5]), Number(match[6] || 0));
  return timestamp - meridianTimezoneOffsetMinutes * 60000;
}

function meridianDateOnlyValue(dateOrTimestamp) {
  const timestamp = dateOrTimestamp instanceof Date ? dateOrTimestamp.getTime() : Number(dateOrTimestamp);
  const date = meridianTimezoneDate(Number.isFinite(timestamp) ? timestamp : Date.now());
  const pad = value => String(value).padStart(2, '0');
  return `${date.getUTCFullYear()}-${pad(date.getUTCMonth() + 1)}-${pad(date.getUTCDate())}`;
}

function meridianParseDateOnly(value, endOfDay = false) {
  const match = String(value || '').match(/^(\d{4})-(\d{2})-(\d{2})$/);
  if (!match) return NaN;
  const timestamp = Date.UTC(Number(match[1]), Number(match[2]) - 1, Number(match[3]), endOfDay ? 23 : 0, endOfDay ? 59 : 0, endOfDay ? 59 : 0, endOfDay ? 999 : 0);
  return timestamp - meridianTimezoneOffsetMinutes * 60000;
}

const API = {
  username: '',
  authenticated: false,

  setSession(data) {
    this.username = (data && data.username) || '';
    this.authenticated = true;
  },

  clearSession() {
    this.username = '';
    this.authenticated = false;
  },

  async request(method, path, body) {
    const opts = {
      method,
      credentials: 'same-origin',
      headers: {},
    };
    if (body !== undefined) {
      opts.headers['Content-Type'] = 'application/json';
      opts.body = JSON.stringify(body);
    }

    const res = await fetch(path, opts);
    if (res.status === 401 && path !== '/api/auth/login') {
      await this.logout();
      window.location.reload();
      // The session is gone and the page is navigating to the login screen:
      // stop this request's control flow right here. Parsing or rejecting the
      // stale 401 body would only let callers keep handling a response that
      // is no longer valid (same convention as the dashboard SSE handler).
      return;
    }
    let data;
    try {
      data = await res.json();
    } catch (e) {
      throw new Error(res.statusText || 'Request failed');
    }
    if (!res.ok) {
      throw new Error(data.error || 'Request failed');
    }
    return data;
  },

  // Auth
  checkSetup() { return this.request('GET', '/api/auth/check'); },
  login(username, password) { return this.request('POST', '/api/auth/login', { username, password }); },
  setup(username, password, setupToken) {
    return this.request('POST', '/api/auth/setup', { username, password, setup_token: setupToken });
  },
  getAccount() { return this.request('GET', '/api/account'); },
  updateAccount(data) { return this.request('PUT', '/api/account', data); },

  // Dashboard
  dashboard() { return this.request('GET', '/api/dashboard'); },
  dashboardInsights() { return this.request('GET', '/api/dashboard-insights'); },
  dashboardTrends(siteId, range, customStart, customEnd) {
    const params = new URLSearchParams({ site_id: siteId || 'all', range: range || 'realtime' });
    if ((range || '').toLowerCase() === 'custom') {
      if (customStart) params.set('start', customStart);
      if (customEnd) params.set('end', customEnd);
    }
    return this.request('GET', '/api/dashboard-trends?' + params.toString());
  },
  getSystemSettings() { return this.request('GET', '/api/system-settings'); },
  saveSystemSettings(data) { return this.request('POST', '/api/system-settings', data); },

  // Sites
  ingressCapabilities() { return this.request('GET', '/api/ingress-capabilities'); },
  panelCertificate() { return this.request('GET', '/api/panel-certificate'); },
  savePanelSettings(data) { return this.request('POST', '/api/panel-settings', data); },
  requestPanelCertificate(data) { return this.request('POST', '/api/panel-certificate/issue', data); },
  restartSystem() { return this.request('POST', '/api/system/restart', {}); },
  async exportBackup(password, includeTLS) {
    const res = await fetch('/api/backup/export', {
      method: 'POST',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ password, include_tls: includeTLS === true }),
    });
    if (res.status === 401) {
      await this.logout();
      window.location.reload();
      return;
    }
    if (!res.ok) {
      let message = '创建备份失败';
      try { message = (await res.json()).error || message; } catch (_) {}
      throw new Error(message);
    }
    return {
      blob: await res.blob(),
      disposition: res.headers.get('Content-Disposition') || '',
    };
  },
  async restoreBackup(file, password, confirm) {
    const body = new FormData();
    body.append('backup', file);
    body.append('password', password);
    body.append('confirm', confirm);
    const res = await fetch('/api/backup/restore', {
      method: 'POST',
      credentials: 'same-origin',
      body,
    });
    if (res.status === 401) {
      await this.logout();
      window.location.reload();
      return;
    }
    let data;
    try { data = await res.json(); } catch (_) { throw new Error(res.statusText || '恢复失败'); }
    if (!res.ok) throw new Error(data.error || '恢复失败');
    return data;
  },
  listSites() { return this.request('GET', '/api/sites'); },
  createSite(data) { return this.request('POST', '/api/sites', data); },
  reorderSites(siteIds) { return this.request('PUT', '/api/sites/reorder', { site_ids: siteIds }); },
  updateSite(id, data) { return this.request('PUT', '/api/sites/' + id, data); },
  deleteSite(id) { return this.request('DELETE', '/api/sites/' + id); },
  toggleSite(id) { return this.request('POST', '/api/sites/' + id + '/toggle'); },
  diagSite(id) { return this.request('GET', '/api/sites/' + id + '/diag'); },
  testUpstream(targetURL) { return this.request('POST', '/api/upstream-test', { target_url: targetURL }); },

  // Traffic
  getTraffic(siteId, hours) { return this.request('GET', '/api/traffic/' + siteId + '?hours=' + (hours || 24)); },
  // Live-merged traffic page payload: { snapshot: SiteTraffic, logs: TrafficLog[] }.
  getTrafficSnapshot(siteId, hours) { return this.request('GET', '/api/traffic/' + siteId + '/snapshot?hours=' + (hours || 24)); },

  // Request logs
  getRequestLogs(filters) {
    const params = new URLSearchParams();
    Object.entries(filters || {}).forEach(([key, value]) => {
      if (value !== undefined && value !== null && value !== '') params.set(key, String(value));
    });
    const query = params.toString();
    return this.request('GET', '/api/request-logs' + (query ? '?' + query : ''));
  },
  clearRequestLogs() { return this.request('DELETE', '/api/request-logs'); },

  // Viewing history
  getWatchHistory(filters) {
    const params = new URLSearchParams();
    Object.entries(filters || {}).forEach(([key, value]) => {
      if (value !== undefined && value !== null && value !== '') params.set(key, String(value));
    });
    const query = params.toString();
    return this.request('GET', '/api/watch-history' + (query ? '?' + query : ''));
  },
  getActiveWatchHistory(filters) {
    const params = new URLSearchParams();
    Object.entries(filters || {}).forEach(([key, value]) => {
      if (value !== undefined && value !== null && value !== '') params.set(key, String(value));
    });
    const query = params.toString();
    return this.request('GET', '/api/watch-history/active' + (query ? '?' + query : ''));
  },
  clearWatchHistory(siteId) {
    const params = new URLSearchParams();
    if (siteId !== undefined && siteId !== null && siteId !== '') params.set('site_id', String(siteId));
    const query = params.toString();
    return this.request('DELETE', '/api/watch-history' + (query ? '?' + query : ''));
  },
  deleteWatchHistory(id) {
    const value = Number(id);
    if (!Number.isSafeInteger(value) || value <= 0) return Promise.reject(new Error('观看记录无效'));
    return this.request('DELETE', '/api/watch-history/' + value);
  },
  watchHistoryPosterURL(mediaItemId) {
    const id = Number(mediaItemId);
    if (!Number.isSafeInteger(id) || id <= 0) return '';
    return '/api/watch-history/posters/' + id;
  },
  watchHistoryBackdropURL(mediaItemId) {
    const id = Number(mediaItemId);
    if (!Number.isSafeInteger(id) || id <= 0) return '';
    return '/api/watch-history/backdrops/' + id;
  },
  watchHistoryStillURL(mediaItemId, index) {
    const id = Number(mediaItemId);
    const position = Number(index);
    if (!Number.isSafeInteger(id) || id <= 0 || !Number.isSafeInteger(position) || position < 0 || position >= 12) return '';
    return '/api/watch-history/stills/' + id + '/' + position;
  },
  watchHistoryCastURL(mediaItemId, index) {
    const id = Number(mediaItemId);
    const position = Number(index);
    if (!Number.isSafeInteger(id) || id <= 0 || !Number.isSafeInteger(position) || position < 0 || position >= 20) return '';
    return '/api/watch-history/cast/' + id + '/' + position;
  },

  // TMDB metadata enrichment
  getTMDBSettings() { return this.request('GET', '/api/tmdb-settings'); },
  saveTMDBSettings(data) { return this.request('POST', '/api/tmdb-settings', data); },
  testTMDBSettings(data) { return this.request('POST', '/api/tmdb-settings/test', data || {}); },
  clearTMDBCache(scope) { return this.request('POST', '/api/tmdb-settings/cache/clear', { scope: scope || 'stale' }); },

  // Telegram daily report
  getTelegramReportSettings() { return this.request('GET', '/api/telegram-report'); },
  saveTelegramReportSettings(data) { return this.request('POST', '/api/telegram-report', data); },

  // Asset cache
  getAssetCache() { return this.request('GET', '/api/asset-cache'); },
  clearAssetCache() { return this.request('DELETE', '/api/asset-cache'); },

  // UA Profiles
  getProfiles() { return this.request('GET', '/api/ua-profiles'); },

  // Dynamic discovery
  getDynamicProfiles() { return this.request('GET', '/api/dynamic-profiles'); },
  getDynamicObservations(siteId) {
    return this.request('GET', '/api/sites/' + encodeURIComponent(siteId) + '/dynamic-observations');
  },
  deleteDynamicObservations(siteId) {
    return this.request('DELETE', '/api/sites/' + encodeURIComponent(siteId) + '/dynamic-observations');
  },

  // Distributed nodes
  getNodes() { return this.request('GET', '/api/nodes'); },
  createNode(data) { return this.request('POST', '/api/nodes', data); },
  updateNode(id, data) { return this.request('PUT', '/api/nodes/' + encodeURIComponent(id), data); },
  deleteNode(id) { return this.request('DELETE', '/api/nodes/' + encodeURIComponent(id)); },
  refreshNodeEnrollment(id, controllerURL) {
    return this.request('POST', '/api/nodes/' + encodeURIComponent(id) + '/enrollment', { controller_url: controllerURL });
  },
  saveNodeScheduler(data) { return this.request('PUT', '/api/node-scheduler', data); },
  getSiteNodeSchedules() { return this.request('GET', '/api/node-scheduler/sites'); },
  saveSiteNodeSchedule(id, data) {
    return this.request('PUT', '/api/node-scheduler/sites/' + encodeURIComponent(id), data);
  },

  async logout() {
    this.clearSession();
    try {
      await fetch('/api/auth/logout', {
        method: 'POST',
        credentials: 'same-origin',
      });
    } catch (e) {
      // The local UI can still safely return to its logged-out state.
    }
  }
};

// Shared HTML escaper. It lives in the first-loaded script that every page
// already depends on, so no page can render markup before escaping exists.
// Keep it a function declaration: pages reach it as a global.
function esc(str) {
  return String(str).replace(/[&<>"']/g, char => ({
    '&': '&amp;',
    '<': '&lt;',
    '>': '&gt;',
    '"': '&quot;',
    "'": '&#39;',
  })[char]);
}
