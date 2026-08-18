// Telegram scheduled report settings
let telegramReportLoaded = false;

function renderTelegramReport() {
  const page = document.getElementById('page-telegram-report');
  if (!page) return;
  page.innerHTML = `
    <div class="settings-layout fade-up">
      ${globalSettingsNav('telegram')}
      <main class="settings-content"><div class="telegram-report-layout">
      <section class="telegram-report-card fade-up">
        <div class="telegram-report-card-head">
          <div>
            <h2>Telegram 定时日报</h2>
            <p>每天或每周自动汇总 Meridian 的请求与流量数据。</p>
          </div>
          <label class="telegram-switch">
            <input type="checkbox" id="telegram-report-enabled">
            <span aria-hidden="true"></span>
            <strong>启用通知</strong>
          </label>
        </div>

        <div class="telegram-secret-warning" id="telegram-secret-warning" hidden>
          当前 JWT_SECRET 为临时值，服务重启后将无法解密 Bot Token。请先固定 JWT_SECRET。
        </div>

        <div class="telegram-report-form-grid">
          <label class="telegram-field telegram-field-wide">
            <span>Bot Token</span>
            <input class="form-input mono" type="text" id="telegram-bot-token" autocomplete="off" placeholder="Telegram Bot Token">
            <small id="telegram-token-state">尚未配置</small>
          </label>
          <label class="telegram-field telegram-field-wide">
            <span>Chat ID</span>
            <input class="form-input" type="text" id="telegram-chat-id" autocomplete="off" placeholder="个人、群组或频道 Chat ID">
            <small>机器人需要拥有向目标会话发送消息的权限。</small>
          </label>
          <label class="telegram-field">
            <span>通知频率</span>
            <select class="form-input" id="telegram-frequency">
              <option value="daily">每天</option>
              <option value="weekly">每周</option>
            </select>
            <small>每天会在设定时间发送；选择每周后，再按指定星期发送。</small>
          </label>
          <label class="telegram-field" id="telegram-weekday-field" hidden>
            <span>每周发送日</span>
            <select class="form-input" id="telegram-weekday">
              <option value="1">星期一</option><option value="2">星期二</option>
              <option value="3">星期三</option><option value="4">星期四</option>
              <option value="5">星期五</option><option value="6">星期六</option>
              <option value="0">星期日</option>
            </select>
          </label>
          <label class="telegram-field">
            <span>发送时间</span>
            <input class="form-input" type="time" id="telegram-schedule-time" value="20:00">
            <small>默认使用北京时间（UTC+8），可在“全局设置 → 系统 UI”中调整调度时区。</small>
          </label>
        </div>

        <div class="telegram-report-actions">
          <button class="telegram-btn secondary" type="button" id="telegram-test">发送测试日报</button>
          <button class="telegram-btn primary" type="button" id="telegram-save">保存设置</button>
        </div>
      </section>

      <aside class="telegram-report-card telegram-report-preview fade-up stagger-1">
        <div class="telegram-report-card-head compact">
          <div><h2>日报内容</h2><p>通知会自动包含以下统计。</p></div>
        </div>
        <div class="telegram-report-items">
          <div><span>01</span><p><strong>当日概览</strong><small>请求总数、独立客户端、视频请求、站点数量</small></p></div>
          <div><span>02</span><p><strong>流量统计</strong><small>今日、近 7 日、近 30 日与历史累计</small></p></div>
          <div><span>03</span><p><strong>请求量排行</strong><small>当日请求次数最高的前 5 个站点</small></p></div>
          <div><span>04</span><p><strong>流量排行</strong><small>当日流量使用最高的前 5 个站点</small></p></div>
          <div><span>05</span><p><strong>客户端分布</strong><small>访问次数最高的前 5 个客户端标识</small></p></div>
        </div>
      </aside></div></main>
    </div>`;

  bindGlobalSettingsNav(page);
  document.getElementById('telegram-frequency').onchange = updateTelegramFrequencyFields;
  document.getElementById('telegram-save').onclick = () => submitTelegramReport(false);
  document.getElementById('telegram-test').onclick = () => submitTelegramReport(true);
  loadTelegramReportSettings();
}

function updateTelegramFrequencyFields() {
  const weekly = document.getElementById('telegram-frequency').value === 'weekly';
  const field = document.getElementById('telegram-weekday-field');
  const weekday = document.getElementById('telegram-weekday');
  field.hidden = !weekly;
  weekday.disabled = !weekly;
}

async function loadTelegramReportSettings() {
  try {
    const settings = await API.getTelegramReportSettings();
    if (!settings || Router.current !== 'telegram-report') return;
    telegramReportLoaded = true;
    document.getElementById('telegram-report-enabled').checked = !!settings.enabled;
    document.getElementById('telegram-bot-token').value = settings.bot_token || '';
    document.getElementById('telegram-chat-id').value = settings.chat_id || '';
    document.getElementById('telegram-frequency').value = settings.frequency || 'daily';
    document.getElementById('telegram-weekday').value = String(Number.isInteger(settings.weekday) ? settings.weekday : 1);
    document.getElementById('telegram-schedule-time').value = settings.schedule_time || '20:00';
    document.getElementById('telegram-token-state').textContent = settings.configured
      ? 'Bot Token 已保存并持续显示'
      : '尚未配置 Bot Token';
    document.getElementById('telegram-secret-warning').hidden = settings.secret_stable !== false;
    updateTelegramFrequencyFields();
  } catch (error) {
    Toast.error('读取 Telegram 日报设置失败：' + error.message);
  }
}

function telegramReportPayload(action) {
  return {
    enabled: document.getElementById('telegram-report-enabled').checked,
    bot_token: document.getElementById('telegram-bot-token').value.trim(),
    chat_id: document.getElementById('telegram-chat-id').value.trim(),
    frequency: document.getElementById('telegram-frequency').value,
    weekday: Number(document.getElementById('telegram-weekday').value),
    schedule_time: document.getElementById('telegram-schedule-time').value,
    ...(action ? { action } : {}),
  };
}

async function submitTelegramReport(testOnly) {
  if (!telegramReportLoaded) return;
  const button = document.getElementById(testOnly ? 'telegram-test' : 'telegram-save');
  const original = button.textContent;
  button.disabled = true;
  button.textContent = testOnly ? '正在发送…' : '正在保存…';
  try {
    const result = await API.saveTelegramReportSettings(telegramReportPayload(testOnly ? 'test' : ''));
    if (testOnly) {
      Toast.success('测试日报已发送');
    } else {
      telegramReportLoaded = false;
      Toast.success('Telegram 日报设置已保存');
      await loadTelegramReportSettings();
    }
    return result;
  } catch (error) {
    Toast.error((testOnly ? '测试发送失败：' : '保存失败：') + error.message);
  } finally {
    button.disabled = false;
    button.textContent = original;
  }
}
