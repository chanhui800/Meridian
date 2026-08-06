const Router = {
  routes: Object.create(null),
  current: null,
  initialized: false,
  pageMeta: {
    dashboard: ['仪表盘', '反代服务运行概览'],
    sites: ['站点管理', '管理入口、回源与站点策略'],
    'request-logs': ['日志记录', '检索客户端请求与视频流记录'],
    traffic: ['流量统计', '查看各站点流量使用趋势'],
    diagnostics: ['故障诊断', '检查入口、回源与运行状态'],
  },

  register(path, handler) {
    this.routes[path] = handler;
  },

  navigate(path) {
    location.hash = path;
  },

  resolve() {
    const hash = location.hash.slice(1) || 'dashboard';
    const previous = this.current;

    if (previous === 'dashboard' && hash !== 'dashboard' && typeof stopDashSSE === 'function') {
      stopDashSSE();
    }
    if (previous === 'traffic' && hash !== 'traffic' && typeof stopTrafficRefresh === 'function') {
      stopTrafficRefresh();
    }
    if (previous === 'request-logs' && hash !== 'request-logs' && typeof stopRequestLogRefresh === 'function') {
      stopRequestLogRefresh();
    }

    this.current = hash;

    let activeNav = null;
    document.querySelectorAll('.topnav-link').forEach(link => {
      const active = link.dataset.page === hash;
      link.classList.toggle('active', active);
      if (typeof link.setAttribute === 'function' && typeof link.removeAttribute === 'function') {
        if (active) link.setAttribute('aria-current', 'page');
        else link.removeAttribute('aria-current');
      }
      if (active) activeNav = link;
    });
    document.querySelectorAll('.mobile-tab').forEach(tab => {
      const active = tab.dataset.page === hash;
      tab.classList.toggle('active', active);
      if (typeof tab.setAttribute === 'function' && typeof tab.removeAttribute === 'function') {
        if (active) tab.setAttribute('aria-current', 'page');
        else tab.removeAttribute('aria-current');
      }
    });

    const meta = this.pageMeta[hash] || [hash, ''];
    const title = document.getElementById('app-page-title');
    const subtitle = document.getElementById('app-page-subtitle');
    const icon = document.getElementById('app-page-icon');
    if (title) title.textContent = meta[0];
    if (subtitle) subtitle.textContent = meta[1];
    document.title = `${meta[0]} — Meridian`;
    if (icon && activeNav && typeof activeNav.querySelector === 'function') {
      const svg = activeNav.querySelector('svg');
      if (svg) icon.innerHTML = svg.outerHTML;
    }

    document.querySelectorAll('.page').forEach(page => page.classList.remove('active'));
    const target = document.getElementById('page-' + hash);
    if (target) target.classList.add('active');

    const handler = this.routes[hash];
    if (handler) handler();
  },

  init() {
    if (this.initialized) return;
    window.addEventListener('hashchange', () => this.resolve());
    this.initialized = true;
  }
};
