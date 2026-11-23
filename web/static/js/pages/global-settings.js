// Global settings pages
let globalSettingsCache = null;
let globalSettingsSection = 'system-ui';
let globalSettingsLoadGeneration = 0;
let globalSettingsModificationRevision = 0;
let globalSettingsEditRevision = 0;
let globalSettingsSaving = false;
let tlsSettingsRenderGeneration = 0;
const globalSettingsDrafts = new Map();
let globalSettingsFormBaseline = { section: '', values: {} };
const globalSettingsFormFields = {
  'system-ui': [
    ['traffic_billing_mode', 'traffic-billing-mode-choice', 'choice'],
    ['traffic_reset_day', 'setting-traffic-reset-day', 'number'],
    ['ui_radius', 'setting-ui-radius', 'number'],
    ['probe_timeout_ms', 'setting-probe-timeout', 'number'],
    ['ping_cache_minutes', 'setting-ping-cache', 'number'],
    ['schedule_timezone_offset', 'setting-schedule-timezone', 'number'],
  ],
  logs: [
    ['log_enabled', 'setting-log-enabled', 'checked'],
    ['log_level', 'log-level-choice', 'choice'],
    ['log_retention_days', 'setting-log-retention', 'number'],
    ['log_batch_size', 'setting-log-batch', 'number'],
    ['log_retry_count', 'setting-log-retries', 'number'],
    ['log_retry_backoff_ms', 'setting-log-backoff', 'number'],
    ['log_write_image', 'setting-write-image', 'checked'],
    ['log_write_playback', 'setting-write-playback', 'checked'],
    ['log_write_metadata', 'setting-write-metadata', 'checked'],
    ['log_write_video', 'setting-write-video', 'checked'],
    ['log_write_subtitle', 'setting-write-subtitle', 'checked'],
    ['log_write_asset', 'setting-write-asset', 'checked'],
    ['log_write_websocket', 'setting-write-websocket', 'checked'],
    ['log_write_api', 'setting-write-api', 'checked'],
    ['log_write_auth', 'setting-write-auth', 'checked'],
    ['log_write_node', 'setting-write-node', 'checked'],
    ['log_write_category', 'setting-write-category', 'checked'],
    ['log_write_status', 'setting-write-status', 'checked'],
    ['log_write_client_ip', 'setting-write-ip', 'checked'],
    ['log_write_ua', 'setting-write-ua', 'checked'],
    ['log_write_upstream_ua', 'setting-write-upstream-ua', 'checked'],
    ['log_write_backend_address', 'setting-write-backend-address', 'checked'],
    ['log_write_timeline', 'setting-write-timeline', 'checked'],
    ['log_display_client_ip', 'setting-display-ip', 'checked'],
    ['log_display_ua', 'setting-display-ua', 'checked'],
    ['log_display_upstream_ua', 'setting-display-upstream-ua', 'checked'],
    ['log_display_backend_address', 'setting-display-backend-address', 'checked'],
    ['log_display_node', 'setting-display-node', 'checked'],
    ['log_display_category', 'setting-display-category', 'checked'],
    ['log_display_status', 'setting-display-status', 'checked'],
    ['log_display_timeline', 'setting-display-timeline', 'checked'],
  ],
};

function applySystemUISettings(settings) {
  if (!settings) return;
  if (typeof meridianSetTimezoneOffset === 'function') meridianSetTimezoneOffset(settings.schedule_timezone_offset);
  const radius = Math.max(0, Math.min(24, Number(settings.ui_radius || 0)));
  document.documentElement.style.setProperty('--ui-radius', `${radius}px`);
}

async function loadAppliedSystemSettings() {
  const generation = ++globalSettingsLoadGeneration;
  const revision = globalSettingsModificationRevision;
  try {
    const settings = await API.getSystemSettings();
    if (settings && generation === globalSettingsLoadGeneration && revision === globalSettingsModificationRevision) {
      globalSettingsCache = settings;
      applySystemUISettings(settings);
    }
  } catch (_) {}
}

function globalSettingsNav(active) {
  const current = id => active === id ? ' aria-current="page"' : '';
  const button = (id, label) => `<button type="button" class="settings-nav-item ${active === id ? 'active' : ''}" data-settings-section="${id}"${current(id)}>${label}</button>`;
  return `<nav class="settings-section-nav settings-category-nav" aria-label="设置分类">
    ${button('system-ui', '基础设置')}${button('logs', '日志')}
    <a href="#settings-tmdb" class="settings-nav-item ${active === 'tmdb' ? 'active' : ''}"${current('tmdb')}>媒体资料</a>
    <a href="#settings-tls" class="settings-nav-item ${active === 'tls' ? 'active' : ''}"${current('tls')}>TLS 与域名</a>
    <a href="#telegram-report" class="settings-nav-item ${active === 'telegram' ? 'active' : ''}"${current('telegram')}>通知</a>
    <a href="#diagnostics" class="settings-nav-item ${active === 'diagnostics' ? 'active' : ''}"${current('diagnostics')}>诊断</a>
    <a href="#backup-restore" class="settings-nav-item ${active === 'backup' ? 'active' : ''}"${current('backup')}>备份与恢复</a>
  </nav>`;
}

function settingsPageFrame(active, title, description, content) {
  return `<div class="console-page settings-console"><header class="console-heading"><div><p class="console-eyebrow">面板管理</p><h1>${esc(title)}</h1><p class="console-lead">${esc(description)}</p></div></header>${globalSettingsNav(active)}<div class="settings-content">${content}</div></div>`;
}

function backupPasswordField(id, label) {
  return `<label class="settings-field"><span>${label}</span><div><input class="form-input" id="${id}" type="password" minlength="12" maxlength="128" autocomplete="new-password" placeholder="至少 12 个字符"></div></label>`;
}

function renderBackupRestore() {
  const page = document.getElementById('page-backup-restore');
  if (!page) return;
  page.innerHTML = settingsPageFrame('backup', '备份与恢复', '下载加密备份，或校验已有文件恢复面板数据。', `<div class="backup-restore-content settings-backup-grid">
    <section class="console-panel settings-panel"><header><h2>创建备份</h2><span class="console-status is-online">密码加密</span></header>
      <p class="settings-panel-help">备份包含站点、账户、流量与请求日志、全局设置及 Telegram 配置；不包含缓存、.env 或部署密钥文件。</p>
      <div class="settings-grid">${backupPasswordField('backup-password', '备份密码')}${backupPasswordField('backup-password-confirm', '确认备份密码')}</div>
      ${settingsCheck('backup-include-tls', '包含 TLS 设置与证书', false, '默认不包含。勾选后才会备份面板域名、监听端口、TLS 开关、证书与 ACME 账户；未勾选时，恢复会保留目标服务器现有 TLS 配置。')}
      <div class="backup-warning">备份中含管理员账户、站点地址及加密凭据，必须妥善保存密码和备份文件。忘记密码后无法恢复。</div>
      <div class="settings-save-bar"><button class="console-button is-primary" type="button" id="backup-download">下载加密备份</button></div>
    </section>
    <section class="console-panel settings-panel backup-restore-danger"><header><h2>从备份恢复</h2><span class="console-status is-warning">恢复后重启</span></header>
      <p class="settings-panel-help">恢复会替换当前数据库并使用备份中的管理员账户；只有备份明确包含 TLS 时才会替换 TLS 数据，否则保留当前服务器的 TLS 设置与证书。文件通过解密、结构检查和 SQLite 完整性检查后，Meridian 才会重启应用。</p>
      <div class="settings-grid">
        <label class="settings-field"><span>Meridian 备份文件</span><div><input class="form-input backup-file-input" id="restore-file" type="file" accept=".mrbak,application/octet-stream"></div></label>
        ${backupPasswordField('restore-password', '备份密码')}
      </div>
      <label class="settings-field backup-confirm-field"><span>确认操作</span><div><input class="form-input" id="restore-confirm" type="text" autocomplete="off" placeholder="请输入：恢复"></div><small>输入“恢复”后才能提交。开始恢复前建议先下载一份当前数据备份。</small></label>
      <div class="backup-warning danger">恢复后当前登录会失效；请使用备份中的管理员用户名和密码重新登录。</div>
      <div class="settings-save-bar"><button class="console-button is-danger" type="button" id="backup-restore-submit">校验并恢复</button></div>
    </section>
  </div>`);
  bindGlobalSettingsNav(page);
  document.getElementById('backup-download').onclick = downloadMeridianBackup;
  document.getElementById('backup-restore-submit').onclick = restoreMeridianBackup;
}

function backupFilename(disposition) {
  const match = String(disposition || '').match(/filename="?([^";]+)"?/i);
  return match ? match[1] : `meridian-backup-${new Date().toISOString().slice(0, 10)}.mrbak`;
}

async function downloadMeridianBackup() {
  const password = document.getElementById('backup-password').value;
  const confirmation = document.getElementById('backup-password-confirm').value;
  if (password.length < 12) return Toast.error('备份密码至少需要 12 个字符');
  if (password !== confirmation) return Toast.error('两次输入的备份密码不一致');
  const button = document.getElementById('backup-download');
  button.disabled = true;
  button.textContent = '正在创建备份…';
  try {
    const result = await API.exportBackup(password, document.getElementById('backup-include-tls').checked);
    if (!result) return;
    const url = URL.createObjectURL(result.blob);
    const link = document.createElement('a');
    link.href = url;
    link.download = backupFilename(result.disposition);
    document.body.appendChild(link);
    link.click();
    link.remove();
    setTimeout(() => URL.revokeObjectURL(url), 1000);
    Toast.success('加密备份已创建，请妥善保存');
  } catch (error) {
    Toast.error(error.message);
  } finally {
    if (button.isConnected) {
      button.disabled = false;
      button.textContent = '下载加密备份';
    }
  }
}

async function restoreMeridianBackup() {
  const fileInput = document.getElementById('restore-file');
  const password = document.getElementById('restore-password').value;
  const confirmation = document.getElementById('restore-confirm').value.trim();
  if (!fileInput.files || !fileInput.files[0]) return Toast.error('请选择 Meridian 备份文件');
  if (password.length < 12) return Toast.error('请输入正确的备份密码');
  if (confirmation !== '恢复') return Toast.error('请输入“恢复”确认操作');
  const button = document.getElementById('backup-restore-submit');
  button.disabled = true;
  button.textContent = '正在校验备份…';
  try {
    const result = await API.restoreBackup(fileInput.files[0], password, confirmation);
    if (!result) return;
    Toast.success(result.message || '恢复已开始，Meridian 正在重启');
    button.textContent = '正在重启…';
    setTimeout(() => window.location.reload(), 5000);
  } catch (error) {
    Toast.error(error.message);
    button.disabled = false;
    button.textContent = '校验并恢复';
  }
}

function settingsNumber(id, label, value, min, max, unit, help) {
  return `<label class="settings-field"><span>${label}</span><div><input class="form-input" id="${id}" type="number" min="${min}" max="${max}" value="${value}"><em>${unit}</em></div>${help ? `<small>${help}</small>` : ''}</label>`;
}

function trafficResetDaySelect(value) {
  const selected = Number(value);
  const options = ['<option value="0">不重置（累计流量）</option>'];
  for (let day = 1; day <= 31; day++) options.push(`<option value="${day}" ${selected === day ? 'selected' : ''}>每月 ${day} 日</option>`);
  if (selected === 0) options[0] = '<option value="0" selected>不重置（累计流量）</option>';
  return `<label class="settings-field"><span>流量周期</span><div><select class="form-select settings-reset-select" id="setting-traffic-reset-day">${options.join('')}</select></div><small>默认每月 1 日；选择不重置后，额度和流量统计使用全部累计值。</small></label>`;
}

function settingsCheck(id, label, checked, help) {
  return `<label class="settings-check"><input id="${id}" type="checkbox" ${checked ? 'checked' : ''}><span class="settings-check-copy"><strong>${label}</strong>${help ? `<small>${help}</small>` : ''}</span></label>`;
}

function settingsRange(id, label, value, min, max, step, unit, help) {
  return `<label class="settings-range"><span>${label}</span><div><input id="${id}" type="range" min="${min}" max="${max}" step="${step}" value="${value}"><output id="${id}-value">${value} ${unit}</output></div>${help ? `<small>${help}</small>` : ''}</label>`;
}

function renderGlobalSettings() {
  const page = document.getElementById('page-global-settings');
  if (!page) return;
  page.innerHTML = settingsPageFrame(globalSettingsSection, '全局设置', '调整统计口径、面板偏好和日志记录方式。', '<div class="console-panel settings-loading">正在读取设置…</div>');
  bindGlobalSettingsNav(page);
  if (globalSettingsCache) paintGlobalSettings(page);
  loadGlobalSettings(page);
}

function bindGlobalSettingsNav(root = document) {
  root.querySelectorAll('[data-settings-section]').forEach(button => {
    button.onclick = () => {
      captureGlobalSettingsDraft();
      globalSettingsSection = button.dataset.settingsSection;
      if (Router.current !== 'global-settings') Router.navigate('global-settings');
      else paintGlobalSettings(document.getElementById('page-global-settings'));
    };
  });
}

async function loadGlobalSettings(page = document.getElementById('page-global-settings')) {
  const generation = ++globalSettingsLoadGeneration;
  const revision = globalSettingsModificationRevision;
  const editRevision = globalSettingsEditRevision;
  try {
    const settings = await API.getSystemSettings();
    if (generation !== globalSettingsLoadGeneration || revision !== globalSettingsModificationRevision || Router.current !== 'global-settings' || !settings || !page) return;
    captureGlobalSettingsDraft();
    globalSettingsCache = settings;
    applySystemUISettings(globalSettingsCache);
    if (editRevision === globalSettingsEditRevision) paintGlobalSettings(page);
  } catch (error) {
    if (generation !== globalSettingsLoadGeneration || revision !== globalSettingsModificationRevision || Router.current !== 'global-settings' || !page) return;
    if (globalSettingsCache) return;
    const content = page.querySelector('.settings-content');
    if (content) content.innerHTML = `<section class="console-panel settings-panel"><p class="settings-load-error" role="alert">读取设置失败：${esc(error.message)}</p><button type="button" class="console-button" id="settings-retry">重新读取</button></section>`;
    const retry = page.querySelector('#settings-retry');
    if (retry) retry.onclick = () => loadGlobalSettings(page);
  }
}

function paintGlobalSettings(page = document.getElementById('page-global-settings')) {
  if (!globalSettingsCache || !page) return;
  const nav = page.querySelector('.settings-section-nav');
  if (nav) nav.outerHTML = globalSettingsNav(globalSettingsSection);
  bindGlobalSettingsNav(page);
  const content = page.querySelector('.settings-content');
  if (!content) return;
  const focusedID = document.activeElement?.id;
  content.innerHTML = globalSettingsSection === 'logs'
    ? renderLogSettingsForm(globalSettingsCache)
    : renderSystemUIForm(globalSettingsCache);
  globalSettingsFormBaseline = { section: globalSettingsSection, values: globalSettingsFormValues(globalSettingsSection, true) };
  const draft = globalSettingsDrafts.get(globalSettingsSection) || {};
  for (const [key, id, type] of globalSettingsFormFields[globalSettingsSection] || []) {
    if (!Object.hasOwn(draft, key)) continue;
    const control = document.getElementById(id);
    if (!control) continue;
    if (type === 'choice') {
      control.querySelectorAll('button').forEach(button => {
        const selected = button.dataset.settingChoice === draft[key];
        button.classList.toggle('active', selected);
        button.setAttribute('aria-pressed', String(selected));
      });
    } else if (type === 'checked') control.checked = draft[key];
    else control.value = draft[key];
  }
  const saveButton = page.querySelector('#settings-save');
  if (saveButton) { saveButton.disabled = globalSettingsSaving; saveButton.onclick = saveGlobalSettings; }
  const rememberEdit = () => { globalSettingsEditRevision += 1; captureGlobalSettingsDraft(); };
  page.querySelectorAll('.settings-content input, .settings-content select, .settings-content textarea').forEach(input => {
    input.addEventListener('input', rememberEdit);
    input.addEventListener('change', rememberEdit);
  });
  page.querySelectorAll('[data-setting-choice]').forEach(button => {
    button.onclick = () => {
      const group = button.closest('.settings-choice');
      group.querySelectorAll('button').forEach(item => {
        item.classList.toggle('active', item === button);
        item.setAttribute('aria-pressed', String(item === button));
      });
      rememberEdit();
    };
  });
  if (focusedID && Router.current === 'global-settings') document.getElementById(focusedID)?.focus?.({ preventScroll: true });
}

function renderSystemUIForm(s) {
  return `<section class="console-panel settings-panel"><header><h2>流量统计</h2><span>全局统计与站点额度</span></header>
    <div class="settings-grid"><div class="settings-field"><span>计费方向</span><div class="settings-choice" id="traffic-billing-mode-choice" role="group" aria-label="流量计费方向"><button type="button" data-setting-choice="outbound" aria-pressed="${s.traffic_billing_mode === 'outbound'}" class="${s.traffic_billing_mode === 'outbound' ? 'active' : ''}">单向（仅下载）</button><button type="button" data-setting-choice="bidirectional" aria-pressed="${s.traffic_billing_mode !== 'outbound'}" class="${s.traffic_billing_mode !== 'outbound' ? 'active' : ''}">双向（下载 + 上传）</button></div><small>单向仅计 VPS 出站；双向累计收发载荷，反代 1 GB 视频约计 2 GB。</small></div>
    ${trafficResetDaySelect(s.traffic_reset_day == null ? 1 : s.traffic_reset_day)}</div>
    <details class="settings-help"><summary>计费与周期说明</summary><p>流量周期可按每月指定日期重置，也可累计不重置。设置为 29、30 或 31 日时，短月自动使用该月最后一天。计费统计只包含经过 Meridian 的代理载荷。</p></details>
  </section>
  <section class="console-panel settings-panel"><header><h2>面板偏好</h2><span>仅调整显示与后台任务</span></header>
    <div class="settings-grid">${settingsNumber('setting-ui-radius', '圆角弧度', s.ui_radius, 0, 24, 'px', '保存后应用到管理面板，不影响代理业务。')}
    ${settingsNumber('setting-schedule-timezone', 'UTC 时区偏移', s.schedule_timezone_offset, -720, 840, '分钟', '480 分钟为北京时间 UTC+8。')}</div>
    <details class="settings-disclosure"><summary><strong>健康探测参数</strong><span>超时与短期缓存</span></summary><div class="settings-grid">${settingsNumber('setting-probe-timeout', 'GET 超时时间', s.probe_timeout_ms, 1000, 180000, 'ms', '限制健康探测等待时间。')}${settingsNumber('setting-ping-cache', 'Ping 缓存时间', s.ping_cache_minutes, 0, 1440, '分钟', '设置为 0 可关闭短期复用。')}</div></details>
  </section>${settingsSaveBar()}`;
}

function renderLogSettingsForm(s) {
  const categories = [
    ['playback', '播放信息与状态同步', s.log_write_playback !== false, '播放地址、会话进度与状态同步。'],
    ['video', '视频与媒体流', s.log_write_video !== false, '主视频、音频、HLS/DASH 清单和媒体分片。'],
    ['image', '图片海报', s.log_write_image === true, '封面、图标与图片资源。'],
    ['metadata', '媒体元数据', s.log_write_metadata === true, '影片、剧集、用户与媒体资料。'],
    ['api', '常规 API', s.log_write_api !== false, '其他普通 API 与状态查询。'],
    ['auth', '用户认证', s.log_write_auth !== false, '登录、认证与 QuickConnect。'],
    ['subtitle', '字幕', s.log_write_subtitle !== false, 'SRT、ASS、VTT 等字幕资源。'],
    ['asset', '静态资源', s.log_write_asset !== false, '脚本、样式、字体与 Web Manifest。'],
    ['websocket', 'WebSocket', s.log_write_websocket !== false, '实时连接请求。'],
  ];
  const fields = [
    ['node', '节点', s.log_write_node, s.log_display_node],
    ['category', '资源类别', s.log_write_category, s.log_display_category],
    ['status', 'HTTP 状态', s.log_write_status, s.log_display_status],
    ['ip', '客户端 IP', s.log_write_client_ip, s.log_display_client_ip],
    ['ua', '客户端 UA', s.log_write_ua, s.log_display_ua],
    ['upstream-ua', '上游 UA', s.log_write_upstream_ua, s.log_display_upstream_ua],
    ['backend-address', '后端地址（仅 authority）', s.log_write_backend_address, s.log_display_backend_address],
    ['timeline', '请求时间线', s.log_write_timeline, s.log_display_timeline],
  ];
  return `<section class="console-panel settings-panel"><header><h2>记录策略</h2><span>写入与查询统一控制</span></header>
    ${settingsCheck('setting-log-enabled', '开启日志写入与日志页显示', s.log_enabled)}
    <div class="settings-group"><span class="settings-label">日志级别</span><div class="settings-choice" id="log-level-choice" role="group" aria-label="日志级别"><button type="button" data-setting-choice="info" aria-pressed="${s.log_level === 'info'}" class="${s.log_level === 'info' ? 'active' : ''}">INFO</button><button type="button" data-setting-choice="error" aria-pressed="${s.log_level === 'error'}" class="${s.log_level === 'error' ? 'active' : ''}">ERROR</button></div></div>
    <details class="settings-disclosure"><summary><strong>保存周期与写入队列</strong><span>落盘、重试与资源占用</span></summary><div class="settings-grid">
      ${settingsNumber('setting-log-retention', '日志保存', s.log_retention_days, 1, 365, '天')}${settingsNumber('setting-log-batch', '单批写入大小', s.log_batch_size, 1, 100, '条')}
      ${settingsNumber('setting-log-retries', '写入重试次数', s.log_retry_count, 0, 10, '次')}${settingsNumber('setting-log-backoff', '重试退避', s.log_retry_backoff_ms, 0, 5000, 'ms')}
    </div></details>
  </section>
  <details class="console-panel settings-panel settings-disclosure"><summary><strong>请求资源类别</strong><span>按需记录播放、媒体和 API 请求</span></summary><p class="settings-panel-help">图片海报和媒体元数据默认不写入；启用后仅影响后续请求。</p><div class="settings-grid">${categories.map(([key, label, enabled, help]) => settingsCheck('setting-write-' + key, label, enabled, help)).join('')}</div></details>
  <div class="settings-two-column"><details class="console-panel settings-panel settings-disclosure"><summary><strong>写入字段</strong><span>仅影响后续新日志</span></summary><p class="settings-panel-help">取消后不会写入该字段；旧日志保持原样。</p><div class="settings-check-list">${fields.map(([key, label, write]) => settingsCheck('setting-write-' + key, label, write !== false)).join('')}</div></details>
  <details class="console-panel settings-panel settings-disclosure"><summary><strong>展示字段</strong><span>仅影响日志页列</span></summary><p class="settings-panel-help">隐藏显示列不会删除已写入的数据。</p><div class="settings-check-list">${fields.map(([key, label, , display]) => settingsCheck('setting-display-' + key, label, display !== false)).join('')}</div></details></div>
  ${settingsSaveBar()}`;
}
function settingsSaveBar() { return '<div class="settings-save-bar"><span class="console-muted">保存后应用到当前面板</span><button class="console-button is-primary" type="button" id="settings-save">保存设置</button></div>'; }

function activeSettingChoice(id, fallback) {
  const active = document.querySelector(`#${id} button.active`);
  return active ? active.dataset.settingChoice : fallback;
}

function numericSetting(id, fallback) {
  const input = document.getElementById(id);
  return input ? Number(input.value) : fallback;
}

function checkedSetting(id, fallback) {
  const input = document.getElementById(id);
  return input ? input.checked : fallback;
}

function globalSettingsFormValues(section = globalSettingsSection, rawNumbers = false) {
  const values = {};
  for (const [key, id, type] of globalSettingsFormFields[section] || []) {
    const control = document.getElementById(id);
    if (!control) continue;
    values[key] = type === 'choice' ? activeSettingChoice(id, globalSettingsCache?.[key])
      : type === 'checked' ? control.checked : rawNumbers ? control.value : Number(control.value);
  }
  return values;
}

function captureGlobalSettingsDraft() {
  if (!globalSettingsCache || Router.current !== 'global-settings' || globalSettingsFormBaseline.section !== globalSettingsSection) return;
  const values = globalSettingsFormValues(globalSettingsSection, true);
  const draft = {};
  for (const [key, value] of Object.entries(values)) {
    if (value !== globalSettingsFormBaseline.values[key]) draft[key] = value;
  }
  if (Object.keys(draft).length) globalSettingsDrafts.set(globalSettingsSection, draft);
  else globalSettingsDrafts.delete(globalSettingsSection);
}

async function saveGlobalSettings() {
  if (globalSettingsSaving || !globalSettingsCache) return;
  captureGlobalSettingsDraft();
  const section = globalSettingsSection;
  const submittedValues = globalSettingsFormValues(section, true);
  const s = globalSettingsFormValues();
  const button = document.getElementById('settings-save');
  if (!button) return;
  globalSettingsSaving = true;
  globalSettingsLoadGeneration += 1;
  globalSettingsModificationRevision += 1;
  button.disabled = true;
  try {
    const settings = await API.saveSystemSettings(s);
    globalSettingsModificationRevision += 1;
    if (!settings) return;
    captureGlobalSettingsDraft();
    const draft = { ...(globalSettingsDrafts.get(section) || {}) };
    for (const key of Object.keys(draft)) if (draft[key] === submittedValues[key]) delete draft[key];
    if (Object.keys(draft).length) globalSettingsDrafts.set(section, draft);
    else globalSettingsDrafts.delete(section);
    globalSettingsCache = settings;
    applySystemUISettings(globalSettingsCache);
    Toast.success('全局设置已保存');
    if (Router.current === 'global-settings') paintGlobalSettings(document.getElementById('page-global-settings'));
  } catch (error) {
    Toast.error('保存失败：' + error.message);
  } finally {
    globalSettingsSaving = false;
    const activeButton = document.getElementById('settings-save');
    if (activeButton) activeButton.disabled = false;
    if (button.isConnected) button.disabled = false;
  }
}

async function renderTLSSettings() {
  const page = document.getElementById('page-settings-tls');
  if (!page) return;
  const generation = ++tlsSettingsRenderGeneration;
  page.innerHTML = settingsPageFrame('tls', 'TLS 与域名', '管理面板地址、HTTPS 证书与自动续签。', '<div class="console-panel settings-loading">正在读取 TLS 配置…</div>');
  bindGlobalSettingsNav(page);
  try {
    let status = await API.panelCertificate();
    if (generation !== tlsSettingsRenderGeneration || Router.current !== 'settings-tls' || !page.isConnected) return;
    page.innerHTML = settingsPageFrame('tls', 'TLS 与域名', '管理面板地址、HTTPS 证书与自动续签。', `
      <section class="console-panel settings-panel tls-overview"><header><h2>证书状态</h2><span class="console-status ${status.configured ? 'is-online' : ''}">${status.configured ? '已配置' : '未配置'}</span></header><div class="global-settings-status" id="p-panel-certificate-status">${renderPanelCertificateStatus(status)}</div></section>
      <section class="console-panel settings-panel"><header><h2>访问地址</h2><span>先保存，再申请证书</span></header><div class="settings-grid">
        <label class="settings-field"><span>面板访问域名</span><div><input type="text" class="form-input" id="p-panel-domain" maxlength="255" value="${esc(status.panel_domain || '')}" placeholder="panel.example.com" autocomplete="off" autocapitalize="none" spellcheck="false"></div><small>填写面板完整域名。</small></label>
        <label class="settings-field"><span>节点泛域名</span><div><input type="text" class="form-input" id="p-wildcard-domain" maxlength="255" value="${esc(status.wildcard_domain || (status.route_domain ? `*.${status.route_domain}` : ''))}" placeholder="*.example.com" autocomplete="off" autocapitalize="none" spellcheck="false"></div><small>证书覆盖节点泛域名及面板精确域名。</small></label>
        <label class="settings-field"><span>面板监听端口</span><div><input type="number" class="form-input" id="p-panel-listen-port" min="1" max="65535" step="1" value="${esc(String(status.listen_port || status.active_listen_port || 9090))}"></div><small>修改后需重启；Docker host 网络与防火墙须放行端口。</small></label>
        <div class="settings-field"><span>启用后的面板地址</span><div class="tls-address-preview console-mono" id="p-panel-address-preview">${esc(panelHTTPSPreview(status.panel_domain || ''))}</div></div>
      </div><div class="settings-save-bar"><button class="console-button" type="button" id="p-cert-save">保存设置</button></div></section>
      <section class="console-panel settings-panel"><header><h2>申请证书</h2><span>${status.acme_staging ? 'ACME 测试环境' : 'Cloudflare DNS-01'}</span></header><div class="settings-grid">
        <label class="settings-field"><span>ACME 邮箱</span><div><input type="email" class="form-input" id="p-acme-email" autocomplete="email" maxlength="254" value="${esc(status.acme_email || '')}" placeholder="admin@example.com"></div></label>
        <label class="settings-field"><span>DNS 服务商</span><div><select class="form-select" id="p-acme-provider"><option value="cloudflare">Cloudflare DNS</option></select></div></label>
        <label class="settings-field settings-field-wide"><span>DNS API Token</span><div><input type="text" class="form-input mono" id="p-acme-token" autocomplete="off" maxlength="512" value="${esc(status.dns_api_token || '')}" placeholder="Cloudflare DNS API Token"></div><small>登录管理员可查看 Token；服务器加密保存并用于自动续签。</small></label>
      </div><details class="settings-disclosure"><summary><strong>测试环境与证书说明</strong><span>正式证书不会在测试中被替换</span></summary><div class="settings-disclosure-body"><label class="settings-check"><input type="checkbox" id="p-acme-staging" ${status.acme_staging ? 'checked' : ''}><span>ACME 测试环境</span></label><p class="settings-panel-help">测试环境只验证 ACME 和 DNS-01，不会替换当前正式证书，也不会启用自动续签。申请前请将节点泛域名解析到本机。</p></div></details>
      <div class="settings-save-bar tls-settings-actions">
        ${status.restart_required && ((status.configured && (status.certificate_matches_configured_domain ?? status.certificate_current) && status.certificate_valid) || (!status.configured && status.listen_port !== status.active_listen_port)) ? `<button class="console-button is-danger" type="button" id="p-cert-restart">${status.configured ? '启用 HTTPS 并重启' : '重启应用'}</button>` : ''}
        <button class="console-button is-primary" type="button" id="p-cert-issue" ${status.available === false || status.issuing || !status.settings_configured ? 'disabled' : ''}>申请证书</button>
      </div></section>`);    bindGlobalSettingsNav(page);
    let formRevision = 0;
    const get = id => document.getElementById(`p-${id}`);
    const refreshPreview = () => {
      const domain = get('panel-domain').value.trim().replace(/^\*\./, '');
      const port = Number(get('panel-listen-port').value) || 9090;
      get('panel-address-preview').textContent = domain ? `https://${domain}${port === 443 ? '' : `:${port}`}` : '—';
    };
    ['panel-domain', 'wildcard-domain', 'panel-listen-port', 'acme-email', 'acme-token', 'acme-staging'].forEach(id => {
      const field = get(id);
      field.addEventListener('input', () => { formRevision++; });
      field.addEventListener('change', () => { formRevision++; });
      if (['panel-domain', 'wildcard-domain', 'panel-listen-port'].includes(id)) field.addEventListener('input', refreshPreview);
    });
    const restartButton = get('cert-restart');
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
        restartButton.textContent = status.configured ? '启用 HTTPS 并重启' : '重启应用';
        Toast.error(error.message);
      }
    };
    const settingsPayload = () => ({
      panel_domain: get('panel-domain').value.trim(),
      wildcard_domain: get('wildcard-domain').value.trim(),
      listen_port: Number(get('panel-listen-port').value),
    });
    get('cert-save').onclick = async () => {
      const button = get('cert-save');
      const payload = settingsPayload();
      if (!payload.panel_domain || !payload.wildcard_domain || !Number.isInteger(payload.listen_port) || payload.listen_port < 1 || payload.listen_port > 65535) {
        Toast.error('请填写面板域名、节点泛域名和有效监听端口');
        return;
      }
      button.disabled = true;
      button.textContent = '保存中…';
      const submittedRevision = formRevision;
      try {
        const savedStatus = await API.savePanelSettings(payload);
        if (generation !== tlsSettingsRenderGeneration || Router.current !== 'settings-tls') return;
        status = savedStatus && typeof savedStatus === 'object' && savedStatus.panel_domain
          && savedStatus.wildcard_domain && Number.isInteger(savedStatus.listen_port)
          ? savedStatus : { ...status, ...payload };
        Toast.success('面板设置已保存；证书会覆盖面板域名和节点站点泛域名');
        if (formRevision === submittedRevision) await renderTLSSettings();
      } catch (error) {
        if (generation !== tlsSettingsRenderGeneration || Router.current !== 'settings-tls') return;
        Toast.error(error.message);
      } finally {
        if (generation === tlsSettingsRenderGeneration && Router.current === 'settings-tls' && page.isConnected && button.isConnected) {
          button.disabled = false;
          button.textContent = '保存设置';
        }
      }
    };
    get('cert-issue').onclick = async () => {
      const button = get('cert-issue');
      const tokenInput = get('acme-token');
      const settingsPayloadValue = settingsPayload();
      const savedWildcard = String(status.wildcard_domain || '').toLowerCase().replace(/^\*\./, '');
      const formWildcard = settingsPayloadValue.wildcard_domain.toLowerCase().replace(/^\*\./, '');
      if (settingsPayloadValue.panel_domain.toLowerCase().replace(/\.$/, '') !== String(status.panel_domain || '').toLowerCase().replace(/\.$/, '') || formWildcard !== savedWildcard || settingsPayloadValue.listen_port !== Number(status.listen_port)) {
        Toast.error('请先点击“保存设置”，再申请证书');
        return;
      }
      const payload = {
        email: get('acme-email').value.trim(),
        dns_provider: get('acme-provider').value,
        dns_api_token: tokenInput.value.trim(),
        staging: get('acme-staging').checked,
      };
      if (!payload.email || !payload.dns_api_token) {
        Toast.error('请填写 ACME 邮箱和 DNS API Token');
        return;
      }
      button.disabled = true;
      button.textContent = '申请中…';
      const submittedRevision = formRevision;
      try {
        const updated = await API.requestPanelCertificate(payload);
        if (generation !== tlsSettingsRenderGeneration || Router.current !== 'settings-tls') return;
        Toast.success(updated.staging_tested ? 'ACME 测试成功；正式证书未替换' : (updated.certificate_reused ? '证书与当前面板域名一致，继续使用现有证书' : (updated.restart_required ? '证书已签发，请点击重启按钮' : '证书已签发并热加载')));
        if (formRevision === submittedRevision) await renderTLSSettings();
      } catch (error) {
        if (generation !== tlsSettingsRenderGeneration || Router.current !== 'settings-tls') return;
        Toast.error(error.message);
      } finally {
        if (generation === tlsSettingsRenderGeneration && Router.current === 'settings-tls' && page.isConnected && button.isConnected) {
          button.disabled = false;
          button.textContent = '申请证书';
        }
      }
    };
  } catch (error) {
    if (generation !== tlsSettingsRenderGeneration || Router.current !== 'settings-tls' || !page.isConnected) return;
    page.innerHTML = settingsPageFrame('tls', 'TLS 与域名', '管理面板地址、HTTPS 证书与自动续签。', `<section class="console-panel settings-panel"><p class="settings-load-error" role="alert">读取 TLS 状态失败：${esc(error.message)}</p><button class="console-button" type="button" id="tls-settings-retry">重新读取</button></section>`);
    bindGlobalSettingsNav(page);
    const retry = page.querySelector('#tls-settings-retry');
    if (retry) retry.onclick = renderTLSSettings;
  }
}
