function formatAccountCreatedAt(value) {
  const raw = String(value || '').trim();
  if (!raw) return '—';
  const timestamp = typeof meridianParseDateTimeText === 'function' ? meridianParseDateTimeText(raw) : new Date(raw).getTime();
  if (!Number.isFinite(timestamp)) return raw;
  return meridianFormatDateTime(timestamp);
}

function accountErrorMessage(error) {
  const message = String(error && error.message || '保存失败');
  const translations = {
    'current password is incorrect': '当前密码不正确',
    'username must be 1-64 characters': '用户名需要为 1–64 个字符',
    'password must be 12-72 bytes': '新密码需要为 12–72 个字符',
    'no account changes requested': '用户名和密码均未修改',
  };
  return translations[message] || message;
}

function syncAccountIdentity(username) {
  API.username = username;
  const initial = document.getElementById('avatar-initial');
  const label = document.getElementById('sidebar-username');
  if (initial) initial.textContent = (username || 'A')[0].toUpperCase();
  if (label) label.textContent = username || '管理员';
}

function renderAccountSummary(account) {
  const username = document.getElementById('account-current-username');
  const role = document.getElementById('account-role');
  const created = document.getElementById('account-created-at');
  const input = document.getElementById('account-username');
  const avatar = document.getElementById('account-avatar');
  if (username) username.textContent = account.username || '管理员';
  if (role) role.textContent = account.role || '管理员';
  if (created) created.textContent = formatAccountCreatedAt(account.created_at);
  if (input) input.value = account.username || '';
  if (avatar) avatar.textContent = (account.username || 'A')[0].toUpperCase();
}

async function loadAccountPage() {
  try {
    const account = await API.getAccount();
    if (!account || Router.current !== 'account') return;
    renderAccountSummary(account);
  } catch (error) {
    if (Router.current === 'account') Toast.error(accountErrorMessage(error));
  }
}

function renderAccount() {
  const page = document.getElementById('page-account');
  page.innerHTML = `<div class="console-page settings-console account-console"><header class="console-heading"><div><p class="console-eyebrow">登录与安全</p><h1>账户</h1><p class="console-lead">管理当前管理员资料和此设备的登录状态。</p></div></header>
    <section class="console-panel settings-panel settings-account-summary"><div class="settings-account-identity"><span class="settings-account-avatar" id="account-avatar">${esc((API.username || 'A')[0].toUpperCase())}</span><div><strong id="account-current-username">${esc(API.username || '管理员')}</strong><p>单管理员模式</p></div></div><dl class="settings-account-meta"><div><dt>账户类型</dt><dd id="account-role">管理员</dd></div><div><dt>创建时间</dt><dd id="account-created-at">正在读取…</dd></div></dl></section>
    <section class="console-panel settings-panel"><header><h2>账户资料</h2><span>修改时验证当前密码</span></header><form id="account-settings-form" class="settings-account-form">
      <div class="settings-grid"><label class="settings-field"><span>用户名</span><div><input class="form-input" id="account-username" type="text" maxlength="64" autocomplete="username" autocapitalize="none" autocorrect="off" spellcheck="false" required></div></label><label class="settings-field"><span>当前密码</span><div><input class="form-input" id="account-current-password" type="password" maxlength="72" autocomplete="current-password" autocapitalize="none" autocorrect="off" spellcheck="false" required></div></label></div>
      <details class="settings-disclosure"><summary><strong>修改密码</strong><span>不修改时留空</span></summary><div class="settings-grid"><label class="settings-field"><span>新密码</span><div><input class="form-input" id="account-new-password" type="password" maxlength="72" autocomplete="new-password" autocapitalize="none" autocorrect="off" spellcheck="false" placeholder="12–72 个字符"></div><small>密码长度为 12–72 个字符。</small></label><label class="settings-field"><span>确认新密码</span><div><input class="form-input" id="account-confirm-password" type="password" maxlength="72" autocomplete="new-password" autocapitalize="none" autocorrect="off" spellcheck="false" placeholder="再次输入新密码"></div></label></div></details>
      <div class="settings-save-bar"><button class="console-button is-primary" id="account-save" type="submit">保存账户设置</button></div>
    </form></section>
    <section class="console-panel settings-panel settings-account-session"><div><h2>此设备的登录状态</h2><p>退出后需要重新输入管理员凭据。</p></div><button class="console-button is-danger account-logout-button" id="account-logout" type="button"><svg viewBox="0 0 24 24" aria-hidden="true"><path d="M10 17l5-5-5-5M15 12H3M15 4h4a2 2 0 0 1 2 2v12a2 2 0 0 1-2 2h-4"/></svg><span>退出登录</span></button></section>
  </div>`;
  document.getElementById('account-settings-form').addEventListener('submit', async event => {
    event.preventDefault();
    const username = document.getElementById('account-username').value.trim();
    const currentPassword = document.getElementById('account-current-password').value;
    const newPassword = document.getElementById('account-new-password').value;
    const confirmPassword = document.getElementById('account-confirm-password').value;
    const saveButton = document.getElementById('account-save');

    if (!username) return Toast.error('请填写用户名');
    if (!currentPassword) return Toast.error('请输入当前密码');
    if (newPassword && newPassword.length < 12) return Toast.error('新密码至少需要 12 个字符');
    if (newPassword !== confirmPassword) return Toast.error('两次输入的新密码不一致');

    saveButton.disabled = true;
    saveButton.textContent = '正在保存…';
    try {
      const account = await API.updateAccount({
        username,
        current_password: currentPassword,
        new_password: newPassword,
      });
      API.setSession(account);
      syncAccountIdentity(account.username);
      renderAccountSummary(account);
      document.getElementById('account-current-password').value = '';
      document.getElementById('account-new-password').value = '';
      document.getElementById('account-confirm-password').value = '';
      Toast.success('账户设置已保存');
    } catch (error) {
      Toast.error(accountErrorMessage(error));
    } finally {
      saveButton.disabled = false;
      saveButton.textContent = '保存账户设置';
    }
  });

  document.getElementById('account-logout').addEventListener('click', () => {
    if (typeof window.logoutMeridian === 'function') window.logoutMeridian();
  });

  loadAccountPage();
}
