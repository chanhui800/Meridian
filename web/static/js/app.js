(function() {
  'use strict';

  const loginEl = document.getElementById('page-login');
  const shellEl = document.getElementById('app-shell');
  const loginFooterEl = document.getElementById('login-footer');
  const loginButtonEl = document.getElementById('btn-login');
  const setupTokenGroupEl = document.getElementById('setup-token-group');
  const setupTokenInputEl = document.getElementById('inp-setup-token');
  const sidebarToggleEl = document.getElementById('sidebar-toggle');
  const sidebarDrawerCloseEl = document.getElementById('sidebar-drawer-close');
  const sidebarStorageKey = 'meridian-sidebar-expanded';
  let dashboardRefreshTimer = null;
  let appBootstrapped = false;
  let modalBackdropClosable = false;
  let modalPreviousFocus = null;
  let authStatus = {
    needs_setup: false,
    mode: 'single_admin',
    jwt_secret_ephemeral: false,
    setup_token_required: false,
  };

  function storedSidebarExpanded() {
    try {
      if (window.matchMedia && window.matchMedia('(max-width: 768px)').matches) return false;
      return !!(window.localStorage && window.localStorage.getItem(sidebarStorageKey) === 'true');
    } catch (_) {
      return false;
    }
  }

  function setSidebarExpanded(expanded, persist) {
    expanded = !!expanded;
    shellEl.classList.toggle('sidebar-expanded', expanded);
    if (sidebarToggleEl) {
      const label = expanded ? '折叠导航栏' : '展开导航栏';
      sidebarToggleEl.setAttribute('aria-expanded', String(expanded));
      sidebarToggleEl.setAttribute('aria-label', label);
      sidebarToggleEl.title = label;
    }
    if (persist) {
      try {
        if (window.localStorage) window.localStorage.setItem(sidebarStorageKey, String(expanded));
      } catch (_) {}
    }
  }

  setSidebarExpanded(storedSidebarExpanded(), false);
  if (sidebarToggleEl) {
    sidebarToggleEl.addEventListener('click', function() {
      setSidebarExpanded(!shellEl.classList.contains('sidebar-expanded'), true);
    });
  }
  if (sidebarDrawerCloseEl) sidebarDrawerCloseEl.addEventListener('click', () => setSidebarExpanded(false, true));

  const dismissMobileDrawer = () => {
    if (window.matchMedia && window.matchMedia('(max-width: 768px)').matches && shellEl.classList.contains('sidebar-expanded')) {
      setSidebarExpanded(false, true);
    }
  };
  if (typeof document.querySelector === 'function') {
    document.querySelector('.main')?.addEventListener('click', dismissMobileDrawer);
    document.querySelector('.app-header')?.addEventListener('click', event => {
      if (!event.target.closest('#sidebar-toggle')) dismissMobileDrawer();
    });
  }

  document.querySelectorAll('.sidebar a[href^="#"]').forEach(link => {
    link.addEventListener('click', function() {
      if (window.matchMedia && window.matchMedia('(max-width: 768px)').matches) {
        setSidebarExpanded(false, true);
      }
    });
  });

  window.openModal = function(options) {
    modalBackdropClosable = !!(options && options.closeOnBackdrop);
    modalPreviousFocus = document.activeElement;
    const overlay = document.getElementById('modal-overlay');
    const modal = document.getElementById('modal');
    modal.className = 'modal';
    if (options && options.modalClass) modal.classList.add(options.modalClass);
    overlay.scrollTop = 0;
    document.getElementById('modal-body').scrollTop = 0;
    overlay.classList.add('active');
    overlay.setAttribute('aria-hidden', 'false');
    document.body.classList.add('modal-open');
  };

  window.closeModal = function() {
    modalBackdropClosable = false;
    const overlay = document.getElementById('modal-overlay');
    overlay.classList.remove('active');
    overlay.setAttribute('aria-hidden', 'true');
    document.body.classList.remove('modal-open');
    if (modalPreviousFocus && modalPreviousFocus.isConnected) modalPreviousFocus.focus();
    modalPreviousFocus = null;
  };

  document.getElementById('modal-overlay').addEventListener('click', function(e) {
    if (e.target === this && modalBackdropClosable) closeModal();
  });

  document.getElementById('modal-close').addEventListener('click', closeModal);

  document.addEventListener('keydown', function(e) {
    if (e.key === 'Escape' && document.getElementById('modal-overlay').classList.contains('active')) closeModal();
  });

  async function checkAuth() {
    try {
      const res = await API.checkSetup();
      authStatus = Object.assign({}, authStatus, res || {});
      if (res.needs_setup) {
        showSetupMode();
        return;
      }
      if (res.authenticated) {
        API.setSession(res);
        enterApp();
        return;
      }
    } catch (e) {
      // Server not available, just show login
    }

    showLoginMode();
  }

  function renderLoginFooter(isSetup) {
    const lines = [];
    if (authStatus.mode === 'single_admin') {
      lines.push(isSetup
        ? '当前为单管理员模式，请创建唯一的管理员账号。'
        : '当前为单管理员模式。');
    } else {
      lines.push(isSetup
        ? '首次使用，请创建管理员账号。'
        : '请输入管理员账户信息登录。');
    }

    if (authStatus.jwt_secret_ephemeral) {
      lines.push('<span class="login-note warn">当前未固定 JWT_SECRET，服务重启后需要重新登录。</span>');
    }

    return lines.join('');
  }

  function showSetupMode() {
    loginButtonEl.textContent = '注册';
    loginButtonEl.disabled = false;
    loginFooterEl.innerHTML = renderLoginFooter(true);
    loginEl._isSetup = true;
    setupTokenGroupEl.hidden = !authStatus.setup_token_required;
    setupTokenInputEl.required = !!authStatus.setup_token_required;
    document.body.classList.remove('auth-checking');
  }

  function showLoginMode() {
    loginButtonEl.textContent = '登录';
    loginButtonEl.disabled = false;
    loginFooterEl.innerHTML = renderLoginFooter(false);
    loginEl._isSetup = false;
    setupTokenGroupEl.hidden = true;
    setupTokenInputEl.required = false;
    document.body.classList.remove('auth-checking');
  }

  function startDashboardRefresh() {
    if (dashboardRefreshTimer) clearInterval(dashboardRefreshTimer);
    dashboardRefreshTimer = setInterval(() => {
      if (Router.current === 'dashboard') loadDashboardData();
    }, 15000);
  }

  function stopDashboardRefresh() {
    if (!dashboardRefreshTimer) return;
    clearInterval(dashboardRefreshTimer);
    dashboardRefreshTimer = null;
  }

  function teardownAppRuntime() {
    stopDashboardRefresh();
    if (typeof stopDashSSE === 'function') stopDashSSE();
    // Keep cleanup compatible with cached clients that still have the retired
    // traffic page script loaded; the page is no longer registered or linked.
    if (typeof stopTrafficRefresh === 'function') stopTrafficRefresh();
  }

  function loginErrorMessage(error) {
    const message = String(error && error.message || '登录失败');
    if (message.includes('too many login attempts') || message.includes('登录尝试次数过多')) {
      return '登录尝试次数过多，请稍后重试';
    }
    if (message === 'invalid username or password' || message === '用户名或密码错误') {
      return '用户名或密码错误';
    }
    return message;
  }

  document.getElementById('loginForm').addEventListener('submit', async function(e) {
    e.preventDefault();
    const username = document.getElementById('inp-username').value.trim();
    const password = document.getElementById('inp-password').value;
    const setupToken = setupTokenInputEl.value.trim();

    if (!username || !password) {
      Toast.error('请填写用户名和密码');
      return;
    }

    if (loginEl._isSetup && password.length < 12) {
      Toast.error('管理员密码至少需要 12 位');
      return;
    }

    if (loginEl._isSetup && authStatus.setup_token_required && !setupToken) {
      Toast.error('请填写安装时显示或部署环境中设置的初始化令牌');
      return;
    }

    loginButtonEl.disabled = true;
    loginButtonEl.textContent = '处理中...';

    try {
      let res;
      if (loginEl._isSetup) {
        res = await API.setup(username, password, setupToken);
        Toast.success('管理员创建成功');
      } else {
        res = await API.login(username, password);
        Toast.success('欢迎回来, ' + res.username + '!');
      }
      API.setSession(res);
      document.getElementById('inp-password').value = '';
      setupTokenInputEl.value = '';
      enterApp();
    } catch (err) {
      Toast.error(loginErrorMessage(err));
      loginButtonEl.disabled = false;
      loginButtonEl.textContent = loginEl._isSetup ? '注册' : '登录';
    }
  });

  function enterApp() {
    loginEl.classList.add('hidden');
    shellEl.classList.add('active');

    const avatar = document.getElementById('avatar-initial');
    if (avatar) avatar.textContent = (API.username || 'A')[0].toUpperCase();
    const username = document.getElementById('sidebar-username');
    if (username) username.textContent = API.username || '管理员';
    API.ingressCapabilities().then(capabilities => {
      if (!capabilities || !capabilities.app_version) return;
      ['sidebar-version'].forEach(id => {
        const version = document.getElementById(id);
        if (version) version.textContent = capabilities.app_version;
      });
    }).catch(() => {});

    if (!appBootstrapped) {
      Router.register('dashboard', renderDashboard);
      Router.register('sites', renderSites);
      Router.register('request-logs', renderRequestLogs);
      Router.register('telegram-report', renderTelegramReport);
      Router.register('settings-tls', renderTLSSettings);
      Router.register('global-settings', renderGlobalSettings);
      Router.register('backup-restore', renderBackupRestore);
      Router.register('account', renderAccount);
      if (typeof renderDiag === 'function') {
        Router.register('diagnostics', renderDiag);
      } else {
        console.error('renderDiag is not defined; diagnostics page script failed to load');
        Router.register('diagnostics', function() {
          var page = document.getElementById('page-diagnostics');
          if (page) {
            page.innerHTML = '<div class="diag-card diag-card-wide"><div class="diag-empty">诊断页面脚本加载失败，请强制刷新浏览器缓存后重试。</div></div>';
          }
        });
      }
      Router.init();
      loadAppliedSystemSettings();
      appBootstrapped = true;
    }

    Router.resolve();
    startDashboardRefresh();
    document.body.classList.remove('auth-checking');
  }

  async function logoutApp() {
    if (!confirm('确认退出登录？')) return;

    teardownAppRuntime();
    await API.logout();
    loginEl.classList.remove('hidden');
    shellEl.classList.remove('active');
    showLoginMode();
    document.getElementById('inp-password').value = '';
    Toast.info('已退出登录');
  }

  window.logoutMeridian = logoutApp;
  document.getElementById('avatar-btn').addEventListener('click', function() {
    if (window.matchMedia && window.matchMedia('(max-width: 768px)').matches) {
      setSidebarExpanded(false, true);
    }
  });

  checkAuth();
})();
