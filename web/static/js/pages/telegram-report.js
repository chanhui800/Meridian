// Telegram scheduled report settings
// Guards the save/test buttons against acting on a form that has not been filled
// from the server yet. It must be cleared on EVERY render: the form is rebuilt
// with template defaults, so a flag left over from a previous visit would let an
// early click submit those defaults and silently overwrite the saved schedule,
// the enabled flag and the warning threshold.
let telegramReportLoaded = false;

function renderTelegramReport() {
  const page = document.getElementById('page-telegram-report');
  if (!page) return;
  telegramReportLoaded = false;
  setTelegramReportButtonsPending(true);
  page.innerHTML = settingsPageFrame('telegram', 'Telegram 通知', '配置请求与流量日报，以及额度预警。', `
    <section class="console-panel settings-panel telegram-settings-panel"><header><h2>通知设置</h2><label class="settings-check telegram-enable"><input type="checkbox" id="telegram-report-enabled"><span>启用通知</span></label></header>
      <div class="telegram-secret-warning" id="telegram-secret-warning" hidden>当前 JWT_SECRET 为临时值，服务重启后将无法解密 Bot Token。请先固定 JWT_SECRET。</div>
      <div class="settings-load-error telegram-load-error" id="telegram-load-error" role="alert" hidden><span id="telegram-error-message"></span><button type="button" class="console-button" id="telegram-retry">重新读取</button></div>
      <fieldset class="settings-form-section"><legend>连接目标</legend><div class="settings-grid telegram-report-form-grid">
        <label class="settings-field telegram-field"><span>Bot Token</span><div><input class="form-input mono" type="text" id="telegram-bot-token" autocomplete="off" placeholder="Telegram Bot Token"></div><small id="telegram-token-state">正在读取…</small></label>
        <label class="settings-field telegram-field"><span>Chat ID</span><div><input class="form-input" type="text" id="telegram-chat-id" autocomplete="off" placeholder="个人、群组或频道 Chat ID"></div><small>机器人需要目标会话的消息发送权限。</small></label>
      </div></fieldset>
      <fieldset class="settings-form-section"><legend>发送计划</legend><div class="settings-grid telegram-report-form-grid">
        <label class="settings-field telegram-field"><span>通知频率</span><div><select class="form-input" id="telegram-frequency"><option value="daily">每天</option><option value="weekly">每周</option></select></div></label>
        <label class="settings-field telegram-field" id="telegram-weekday-field" hidden><span>每周发送日</span><div><select class="form-input" id="telegram-weekday"><option value="1">星期一</option><option value="2">星期二</option><option value="3">星期三</option><option value="4">星期四</option><option value="5">星期五</option><option value="6">星期六</option><option value="0">星期日</option></select></div></label>
        <label class="settings-field telegram-field"><span>发送时间</span><div><input class="form-input" type="time" id="telegram-schedule-time" value="20:00"></div><small>调度时区由基础设置控制，默认 UTC+8。</small></label>
        <label class="settings-field telegram-field"><span>流量预警阈值</span><div><input class="form-input" type="number" id="telegram-traffic-warning" min="0" max="100" step="1" inputmode="numeric" value="80"><em>%</em></div><small>触及阈值立即预警，之后每增加 5% 再提醒；同档位不重复。0 关闭预警。</small></label>
      </div></fieldset>
      <div class="settings-save-bar telegram-report-actions"><button class="console-button telegram-btn" type="button" id="telegram-test" disabled>发送测试日报</button><button class="console-button is-primary telegram-btn" type="button" id="telegram-save" disabled>保存设置</button></div>
    </section>
    <details class="console-panel settings-panel settings-disclosure telegram-report-preview"><summary><strong>日报会包含哪些内容</strong><span>概览、流量、客户端与保号提醒</span></summary><div class="settings-report-sections">
      <div><strong>今日概览</strong><p>请求、视频请求、站点数量与活跃时段。</p></div><div><strong>服务器部署信息</strong><p>节点今日流量、当月用量与剩余额度。</p></div><div><strong>客户端分布</strong><p>访问次数最高的前 5 个客户端及占比。</p></div><div><strong>流量统计</strong><p>当天、七天与当前周期的计费流量。</p></div><div><strong>今日站点热度 TOP 5</strong><p>当日流量最高的站点及请求次数。</p></div><div><strong>流量预警</strong><p>用量触及预警阈值的节点与站点。</p></div><div><strong>保号提醒</strong><p>开启保号站点的完成状态与剩余天数。</p></div><div><strong>阈值预警（单独推送）</strong><p>阈值达到时立即推送，不占用定时日报。</p></div>
    </div></details>`);
  bindGlobalSettingsNav(page);
  document.getElementById('telegram-frequency').onchange = updateTelegramFrequencyFields;
  document.getElementById('telegram-save').onclick = () => submitTelegramReport(false);
  document.getElementById('telegram-test').onclick = () => submitTelegramReport(true);
  document.getElementById('telegram-retry').onclick = loadTelegramReportSettings;
  loadTelegramReportSettings();
}
function updateTelegramFrequencyFields() {
  const weekly = document.getElementById('telegram-frequency').value === 'weekly';
  const field = document.getElementById('telegram-weekday-field');
  const weekday = document.getElementById('telegram-weekday');
  field.hidden = !weekly;
  weekday.disabled = !weekly;
}

// setTelegramReportButtonsPending keeps the save/test buttons disabled until the
// form actually reflects the saved settings. Without it an early click on a
// freshly rendered form either does nothing with no explanation or, worse,
// submits the template defaults over the operator's configuration.
function setTelegramReportButtonsPending(pending) {
  ['telegram-save', 'telegram-test'].forEach(id => {
    const button = document.getElementById(id);
    if (!button) return;
    button.disabled = pending;
    button.title = pending ? '正在读取已保存的设置…' : '';
  });
}

async function loadTelegramReportSettings() {
  const errorPanel = document.getElementById('telegram-load-error');
  const errorMessage = document.getElementById('telegram-error-message');
  try {
    const settings = await API.getTelegramReportSettings();
    if (!settings || Router.current !== 'telegram-report') return;
    document.getElementById('telegram-report-enabled').checked = !!settings.enabled;
    document.getElementById('telegram-bot-token').value = settings.bot_token || '';
    document.getElementById('telegram-chat-id').value = settings.chat_id || '';
    document.getElementById('telegram-frequency').value = settings.frequency || 'daily';
    document.getElementById('telegram-weekday').value = String(Number.isInteger(settings.weekday) ? settings.weekday : 1);
    document.getElementById('telegram-schedule-time').value = settings.schedule_time || '20:00';
    document.getElementById('telegram-traffic-warning').value =
      Number.isInteger(settings.traffic_warning_percent) ? String(settings.traffic_warning_percent) : '80';
    document.getElementById('telegram-token-state').textContent = settings.configured
      ? 'Bot Token 已保存并持续显示'
      : '尚未配置 Bot Token';
    document.getElementById('telegram-secret-warning').hidden = settings.secret_stable !== false;
    if (errorPanel) errorPanel.hidden = true;
    // Only once every field holds the server value may a click be accepted.
    telegramReportLoaded = true;
    updateTelegramFrequencyFields();
    setTelegramReportButtonsPending(false);
  } catch (error) {
    // The form still shows template defaults, so it must stay unsubmittable. The
    // buttons deliberately stay clickable so the operator can retry after the
    // read succeeds; submitTelegramReport explains the refusal.
    telegramReportLoaded = false;
    if (errorPanel) errorPanel.hidden = false;
    if (errorMessage) errorMessage.textContent = '读取失败：' + error.message;
    Toast.error('读取 Telegram 日报设置失败：' + error.message);
  }
}

function telegramTrafficWarningPercent() {
  const raw = document.getElementById('telegram-traffic-warning').value.trim();
  if (!/^\d{1,3}$/.test(raw)) return 80;
  const value = parseInt(raw, 10);
  return value >= 0 && value <= 100 ? value : 80;
}

function telegramReportPayload(action) {
  return {
    enabled: document.getElementById('telegram-report-enabled').checked,
    bot_token: document.getElementById('telegram-bot-token').value.trim(),
    chat_id: document.getElementById('telegram-chat-id').value.trim(),
    frequency: document.getElementById('telegram-frequency').value,
    weekday: Number(document.getElementById('telegram-weekday').value),
    schedule_time: document.getElementById('telegram-schedule-time').value,
    traffic_warning_percent: telegramTrafficWarningPercent(),
    ...(action ? { action } : {}),
  };
}

async function submitTelegramReport(testOnly) {
  if (!telegramReportLoaded) {
    // Never submit a form that has not been filled from the server, and never
    // fail silently: the operator has to know why nothing happened.
    Toast.error('设置尚未读取完成，请稍候再试');
    return;
  }
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
