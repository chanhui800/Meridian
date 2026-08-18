// Meridian API Client
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
  dashboardTrends(siteId, range) {
    const params = new URLSearchParams({ site_id: siteId || 'all', range: range || 'realtime' });
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
