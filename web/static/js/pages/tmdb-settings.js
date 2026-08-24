// TMDB metadata enrichment settings
let tmdbSettingsLoadGeneration = 0;
let tmdbSettingsCache = null;
let tmdbSettingsCacheRefreshTimer = null;

function normalizeTMDBSettings(value) {
  const raw = value && typeof value === 'object' ? value : {};
  // Accept both the current direct response and the wrapped shape returned by
  // older panel builds, so a stale API proxy cannot silently turn real cache
  // statistics into the default 0/0 display.
  const settings = raw.settings && typeof raw.settings === 'object' ? raw.settings : raw;
  const cacheEntriesValue = settings.cache_entries ?? settings.cacheEntries;
  const cacheSizeValue = settings.cache_size_bytes ?? settings.cacheSizeBytes;
  const language = ['zh-CN', 'zh-TW', 'en-US'].includes(settings.language) ? settings.language : 'zh-CN';
  const retention = Number(settings.history_retention_days);
  const configured = settings.configured === true || settings.token_configured === true;
  const credentialState = ['unconfigured', 'ready', 'invalid', 'unknown'].includes(settings.credential_state)
    ? settings.credential_state
    : (configured ? 'unknown' : 'unconfigured');
  return {
    enabled: settings.enabled === true,
    configured,
    secretStable: settings.secret_stable !== false,
    language,
    historyRetentionDays: Number.isSafeInteger(retention) && retention >= 1 && retention <= 3650 ? retention : 90,
    cacheEntries: Number.isSafeInteger(Number(cacheEntriesValue)) && Number(cacheEntriesValue) >= 0 ? Number(cacheEntriesValue) : 0,
    cacheSizeBytes: Number.isFinite(Number(cacheSizeValue)) && Number(cacheSizeValue) >= 0 ? Number(cacheSizeValue) : 0,
    credentialState,
    lastErrorCode: String(settings.last_error_code || ''),
    lastTestedAtMS: Number(settings.last_tested_at_ms) || 0,
  };
}

function tmdbFormatCacheSize(value) {
  let bytes = Math.max(0, Number(value) || 0);
  if (bytes < 1024) return `${bytes} B`;
  const units = ['KB', 'MB', 'GB'];
  let unit = -1;
  while (bytes >= 1024 && unit < units.length - 1) {
    bytes /= 1024;
    unit += 1;
  }
  return `${bytes >= 100 ? bytes.toFixed(0) : bytes.toFixed(1)} ${units[unit]}`;
}

function buildTMDBSettingsPayload(token, language, historyRetentionDays, enabled, clearToken) {
  const retention = Number(historyRetentionDays);
  const payload = {
    enabled: clearToken === true ? false : enabled === true,
    language: ['zh-CN', 'zh-TW', 'en-US'].includes(language) ? language : 'zh-CN',
    history_retention_days: Number.isSafeInteger(retention) ? retention : 0,
  };
  const trimmedToken = String(token || '').trim();
  if (trimmedToken && clearToken !== true) payload.token = trimmedToken;
  if (clearToken === true) payload.clear_token = true;
  return payload;
}

function tmdbSettingsStatusHTML(settings) {
  let statusClass = 'is-unconfigured';
  let title = '尚未配置 TMDB Token';
  let description = '本地观看历史仍可正常记录；配置后才会异步补全海报和影片资料。';
  if (!settings.secretStable) {
    statusClass = 'is-error';
    title = '当前密钥为临时值';
    description = '请先配置持久 JWT_SECRET，否则 Meridian 不会保存 TMDB Token。';
  } else if (settings.configured && !settings.enabled) {
    statusClass = 'is-disabled';
    title = 'TMDB 资料补全已关闭';
    description = 'Token 已安全保存；开启后才会处理待补全的观看记录。';
  } else if (settings.configured && settings.credentialState === 'invalid') {
    statusClass = 'is-error';
    title = 'TMDB Token 无效';
    description = '请更换 Read Access Token 并重新测试连接。';
  } else if (settings.configured && settings.enabled) {
    statusClass = 'is-configured';
    title = settings.credentialState === 'ready' ? 'TMDB 资料补全正常' : 'TMDB 资料补全已开启';
    description = settings.credentialState === 'ready'
      ? 'Meridian 会在后台补全新观看记录的影片信息；Token 不会在面板中回显。'
      : 'Token 已配置，建议测试连接以确认可用性。';
  }
  return `<div class="tmdb-settings-status ${statusClass}" id="tmdb-settings-status" role="status" aria-live="polite"><span aria-hidden="true"></span><div><strong>${title}</strong><small>${description}</small></div></div>`;
}

function renderTMDBSettingsForm(rawSettings) {
  const settings = normalizeTMDBSettings(rawSettings);
  return `<section class="settings-panel tmdb-settings-panel">
    <header><span>TMDB</span><h2>媒体资料补全</h2><b>${settings.enabled ? '已开启' : '可选'}</b></header>
    <div class="tmdb-brand-row">
      <img src="/tmdb-logo.svg" alt="The Movie Database (TMDB)">
      <p class="settings-panel-help">TMDB 只用于后台补全观看历史的海报、影片名称与剧集资料；不参与代理、播放和站点可用性判断。</p>
    </div>
    ${tmdbSettingsStatusHTML(settings)}
    <div class="settings-grid tmdb-settings-grid">
      <label class="settings-field"><span>自动资料补全</span><div><select class="form-select" id="tmdb-enabled"><option value="off" ${!settings.enabled ? 'selected' : ''}>关闭</option><option value="on" ${settings.enabled ? 'selected' : ''}>开启</option></select></div><small>关闭后仍会记录本地观看历史，但不会向 TMDB 发起资料查询。</small></label>
      <label class="settings-field tmdb-token-field"><span>TMDB API Read Access Token</span><div><input class="form-input" id="tmdb-api-token" type="password" autocomplete="new-password" autocapitalize="none" autocorrect="off" spellcheck="false" placeholder="${settings.configured ? '已配置，留空保持不变' : '粘贴 TMDB Read Access Token'}"></div><small>Token 仅加密保存在服务器中，读取设置时不会返回到浏览器。可前往 <a href="https://www.themoviedb.org/settings/api" target="_blank" rel="noopener noreferrer">TMDB API 设置</a>申请。</small></label>
      <label class="settings-field"><span>资料语言</span><div><select class="form-select" id="tmdb-language"><option value="zh-CN" ${settings.language === 'zh-CN' ? 'selected' : ''}>简体中文（zh-CN）</option><option value="zh-TW" ${settings.language === 'zh-TW' ? 'selected' : ''}>繁體中文（zh-TW）</option><option value="en-US" ${settings.language === 'en-US' ? 'selected' : ''}>English（en-US）</option></select></div><small>匹配不到本地语言资料时，由后端保留已有本地标题作为回退。</small></label>
      <label class="settings-field"><span>观看历史保留时间</span><div><input class="form-input" id="tmdb-history-retention" type="number" min="1" max="3650" step="1" inputmode="numeric" value="${settings.historyRetentionDays}"><em>天</em></div><small>仅清理过期观看会话；不会删除站点、请求日志或流量统计。</small></label>
    </div>
    <div class="settings-save-bar tmdb-settings-actions">
      ${settings.configured ? '<button type="button" class="telegram-btn danger secondary" id="tmdb-remove-token">移除 Token</button>' : ''}
      <button type="button" class="telegram-btn" id="tmdb-test">测试连接</button>
      <button type="button" class="telegram-btn primary" id="tmdb-save">保存设置</button>
    </div>
    <section class="tmdb-cache-panel" aria-label="TMDB 缓存管理">
      <header><div><span>TMDB CACHE</span><h3>缓存管理</h3></div><b id="tmdb-cache-summary">${settings.cacheEntries} 条 · ${esc(tmdbFormatCacheSize(settings.cacheSizeBytes))}</b></header>
      <p>观看历史保留时间会联动清理过期和未引用的 TMDB 资料缓存；手动清理会同时失效历史记录中的复制资料并排队后台重新补全。删除单条历史时，只有没有其它记录引用的缓存才会同步删除。</p>
      <div class="tmdb-cache-actions"><button type="button" class="telegram-btn" id="tmdb-cache-clean">清理过期与未引用</button><button type="button" class="telegram-btn danger secondary" id="tmdb-cache-clear-all">清空全部缓存</button></div>
    </section>
    <p class="tmdb-attribution">This product uses the TMDB API but is not endorsed or certified by TMDB. <a href="https://www.themoviedb.org/" target="_blank" rel="noopener noreferrer">The Movie Database</a></p>
  </section>`;
}

function tmdbSettingsCurrentPayload(clearToken) {
  return buildTMDBSettingsPayload(
    document.getElementById('tmdb-api-token')?.value,
    document.getElementById('tmdb-language')?.value,
    Number(document.getElementById('tmdb-history-retention')?.value),
    document.getElementById('tmdb-enabled')?.value === 'on',
    clearToken,
  );
}

function validateTMDBSettingsPayload(payload) {
  if (!Number.isSafeInteger(payload.history_retention_days) || payload.history_retention_days < 1 || payload.history_retention_days > 3650) {
    return '观看历史保留时间必须为 1 到 3650 天的整数';
  }
  if (payload.enabled && !payload.token && !(tmdbSettingsCache && tmdbSettingsCache.configured)) {
    return '开启 TMDB 资料补全前请先填写 Read Access Token';
  }
  if (payload.token && tmdbSettingsCache && !tmdbSettingsCache.secretStable) {
    return '当前 JWT_SECRET 为临时值，无法安全保存 TMDB Token';
  }
  return '';
}

function tmdbTestPayload() {
  const token = String(document.getElementById('tmdb-api-token')?.value || '').trim();
  return token ? { token } : {};
}

function setTMDBActionState(button, busy, busyText, normalText) {
  if (!button) return;
  button.disabled = !!busy;
  button.textContent = busy ? busyText : normalText;
}

async function saveTMDBSettings() {
  const payload = tmdbSettingsCurrentPayload(false);
  const validationError = validateTMDBSettingsPayload(payload);
  if (validationError) return Toast.error(validationError);
  const button = document.getElementById('tmdb-save');
  setTMDBActionState(button, true, '正在保存…', '保存设置');
  try {
    const updated = await API.saveTMDBSettings(payload);
    if (!updated) return;
    const tokenInput = document.getElementById('tmdb-api-token');
    if (tokenInput) tokenInput.value = '';
    Toast.success('TMDB 设置已保存');
    const page = document.getElementById('page-settings-tmdb');
    if (page) await loadTMDBSettings(page);
  } catch (error) {
    Toast.error(error.message || '保存 TMDB 设置失败');
  } finally {
    if (button && button.isConnected) setTMDBActionState(button, false, '', '保存设置');
  }
}

async function testTMDBConnection() {
  const payload = tmdbTestPayload();
  if (!payload.token && !(tmdbSettingsCache && tmdbSettingsCache.configured)) {
    return Toast.error('请先填写 TMDB Read Access Token');
  }
  const button = document.getElementById('tmdb-test');
  const status = document.getElementById('tmdb-settings-status');
  setTMDBActionState(button, true, '正在测试…', '测试连接');
  if (status) status.setAttribute('aria-busy', 'true');
  try {
    const result = await API.testTMDBSettings(payload);
    if (!result) return;
    if (result.connected !== true) throw new Error(result.message || 'TMDB 连接测试失败');
    Toast.success('TMDB 连接正常');
    if (status) {
      status.classList.remove('is-unconfigured');
      status.classList.add('is-configured');
      status.querySelector('strong').textContent = 'TMDB 连接正常';
      status.querySelector('small').textContent = 'Token 可用，媒体资料补全服务连接正常。';
    }
    if (result.settings) tmdbSettingsCache = normalizeTMDBSettings(result.settings);
  } catch (error) {
    Toast.error(error.message || 'TMDB 连接测试失败');
    if (status) {
      status.classList.remove('is-configured');
      status.classList.add('is-error');
      status.querySelector('strong').textContent = 'TMDB 连接失败';
      status.querySelector('small').textContent = error.message || '请检查 Token 和服务器网络。';
    }
  } finally {
    if (status) status.removeAttribute('aria-busy');
    if (button && button.isConnected) setTMDBActionState(button, false, '', '测试连接');
  }
}

async function removeTMDBToken() {
  if (!confirm('确认移除 TMDB Token？本地观看历史不会被删除，但媒体资料补全会停止。')) return;
  const payload = tmdbSettingsCurrentPayload(true);
  const validationError = validateTMDBSettingsPayload(payload);
  if (validationError) return Toast.error(validationError);
  const button = document.getElementById('tmdb-remove-token');
  setTMDBActionState(button, true, '正在移除…', '移除 Token');
  try {
    const updated = await API.saveTMDBSettings(payload);
    if (!updated) return;
    Toast.success('TMDB Token 已移除');
    const page = document.getElementById('page-settings-tmdb');
    if (page) await loadTMDBSettings(page);
  } catch (error) {
    Toast.error(error.message || '移除 TMDB Token 失败');
  } finally {
    if (button && button.isConnected) setTMDBActionState(button, false, '', '移除 Token');
  }
}

async function clearTMDBCache(scope) {
  const all = scope === 'all';
  const prompt = all
    ? '确认清空全部 TMDB 缓存？观看历史记录不会删除，资料会在后台重新补全。'
    : '确认清理已过期和未被观看历史引用的 TMDB 缓存？过期记录的资料会在后台重新补全。';
  if (!confirm(prompt)) return;
  const button = document.getElementById(all ? 'tmdb-cache-clear-all' : 'tmdb-cache-clean');
  setTMDBActionState(button, true, '正在清理…', all ? '清空全部缓存' : '清理过期与未引用');
  try {
    const result = await API.clearTMDBCache(all ? 'all' : 'stale');
    Toast.success(all ? 'TMDB 缓存已清空' : 'TMDB 缓存已清理');
    if (result && result.settings) paintTMDBSettings(result.settings);
    else {
      const page = document.getElementById('page-settings-tmdb');
      if (page) await loadTMDBSettings(page);
    }
  } catch (error) {
    Toast.error(error.message || '清理 TMDB 缓存失败');
  } finally {
    if (button && button.isConnected) setTMDBActionState(button, false, '', all ? '清空全部缓存' : '清理过期与未引用');
  }
}

function bindTMDBSettingsActions(page) {
  page.querySelector('#tmdb-save').onclick = saveTMDBSettings;
  page.querySelector('#tmdb-test').onclick = testTMDBConnection;
  const remove = page.querySelector('#tmdb-remove-token');
  if (remove) remove.onclick = removeTMDBToken;
  const clean = page.querySelector('#tmdb-cache-clean');
  if (clean) clean.onclick = () => clearTMDBCache('stale');
  const clearAll = page.querySelector('#tmdb-cache-clear-all');
  if (clearAll) clearAll.onclick = () => clearTMDBCache('all');
}

function paintTMDBSettings(settings) {
  const page = document.getElementById('page-settings-tmdb');
  if (!page || Router.current !== 'settings-tmdb') return;
  const content = page.querySelector('.settings-content');
  if (!content) return;
  tmdbSettingsCache = normalizeTMDBSettings(settings);
  content.innerHTML = renderTMDBSettingsForm(tmdbSettingsCache);
  bindTMDBSettingsActions(page);
  startTMDBCacheRefresh(page);
}

function stopTMDBCacheRefresh() {
  if (tmdbSettingsCacheRefreshTimer !== null) {
    clearInterval(tmdbSettingsCacheRefreshTimer);
    tmdbSettingsCacheRefreshTimer = null;
  }
}

function startTMDBCacheRefresh(page) {
  stopTMDBCacheRefresh();
  const refresh = async () => {
    if (Router.current !== 'settings-tmdb' || !page.isConnected) {
      stopTMDBCacheRefresh();
      return;
    }
    try {
      const settings = await API.getTMDBSettings();
      if (Router.current !== 'settings-tmdb' || !page.isConnected || !settings) return;
      const normalized = normalizeTMDBSettings(settings);
      tmdbSettingsCache = normalized;
      const summary = page.querySelector('#tmdb-cache-summary');
      if (summary) summary.textContent = `${normalized.cacheEntries} 条 · ${tmdbFormatCacheSize(normalized.cacheSizeBytes)}`;
    } catch (_) {
      // A transient refresh failure must not replace the form or interrupt edits.
    }
  };
  void refresh();
  tmdbSettingsCacheRefreshTimer = setInterval(refresh, 15000);
}

async function loadTMDBSettings(page) {
  const generation = ++tmdbSettingsLoadGeneration;
  try {
    const settings = await API.getTMDBSettings();
    if (generation !== tmdbSettingsLoadGeneration || Router.current !== 'settings-tmdb' || !page.isConnected || !settings) return;
    paintTMDBSettings(settings);
  } catch (error) {
    if (generation !== tmdbSettingsLoadGeneration || Router.current !== 'settings-tmdb' || !page.isConnected) return;
    const content = page.querySelector('.settings-content');
    if (content) content.innerHTML = `<section class="settings-panel"><div class="settings-loading request-log-error">读取 TMDB 设置失败：${esc(error.message || '请求失败')}</div><div class="settings-save-bar"><button type="button" class="telegram-btn" id="tmdb-settings-retry">重新加载</button></div></section>`;
    const retry = page.querySelector('#tmdb-settings-retry');
    if (retry) retry.onclick = () => renderTMDBSettings();
  }
}

function renderTMDBSettings() {
  const page = document.getElementById('page-settings-tmdb');
  if (!page) return;
  stopTMDBCacheRefresh();
  tmdbSettingsCache = null;
  page.innerHTML = `<div class="settings-layout fade-up">${globalSettingsNav('tmdb')}<main class="settings-content"><section class="settings-panel"><div class="settings-loading">正在读取 TMDB 设置…</div></section></main></div>`;
  bindGlobalSettingsNav(page);
  loadTMDBSettings(page);
}
