'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const vm = require('node:vm');

const STATIC_JS = path.join(__dirname, '..', 'web', 'static', 'js');
const SOURCE = fs.readFileSync(path.join(STATIC_JS, 'pages', 'telegram-report.js'), 'utf8');

// The page script is a classic script that only touches document.getElementById
// inside the two payload helpers, so a stub document is enough to exercise them.
function loadTelegramHelpers(values) {
  const sandbox = {
    window: {},
    document: {
      getElementById(id) {
        if (!(id in values)) throw new Error('unexpected element id: ' + id);
        return values[id];
      },
    },
  };
  vm.createContext(sandbox);
  vm.runInContext(SOURCE, sandbox, { filename: 'pages/telegram-report.js' });
  return sandbox;
}

function baseValues(overrides) {
  return Object.assign(
    {
      'telegram-report-enabled': { checked: true },
      'telegram-bot-token': { value: '  123:abc  ' },
      'telegram-chat-id': { value: ' -100123 ' },
      'telegram-frequency': { value: 'daily' },
      'telegram-weekday': { value: '1' },
      'telegram-schedule-time': { value: '20:00' },
      'telegram-traffic-warning': { value: '80' },
    },
    overrides,
  );
}

test('telegram payload always sends the traffic warning threshold', () => {
  const sandbox = loadTelegramHelpers(baseValues({ 'telegram-traffic-warning': { value: '65' } }));
  const payload = vm.runInContext('telegramReportPayload("")', sandbox);

  assert.equal(payload.traffic_warning_percent, 65);
  assert.equal(payload.bot_token, '123:abc');
  assert.equal(payload.chat_id, '-100123');
  assert.equal(payload.schedule_time, '20:00');
  assert.equal(payload.action, undefined);
});

test('telegram payload carries the test action through', () => {
  const sandbox = loadTelegramHelpers(baseValues({}));
  const payload = vm.runInContext('telegramReportPayload("test")', sandbox);
  assert.equal(payload.action, 'test');
  assert.equal(payload.traffic_warning_percent, 80);
});

test('telegram threshold accepts 0 as the explicit off switch', () => {
  const sandbox = loadTelegramHelpers(baseValues({ 'telegram-traffic-warning': { value: '0' } }));
  const payload = vm.runInContext('telegramReportPayload("")', sandbox);
  assert.equal(payload.traffic_warning_percent, 0);
});

test('telegram threshold falls back to the default for junk input', () => {
  for (const raw of ['', '  ', 'abc', '101', '1000', '-5', '8.5', '80%']) {
    const sandbox = loadTelegramHelpers(baseValues({ 'telegram-traffic-warning': { value: raw } }));
    const payload = vm.runInContext('telegramReportPayload("")', sandbox);
    assert.equal(payload.traffic_warning_percent, 80, `raw ${JSON.stringify(raw)} should fall back to 80`);
  }
});

test('telegram threshold keeps the boundary values 1 and 100', () => {
  for (const raw of ['1', '100']) {
    const sandbox = loadTelegramHelpers(baseValues({ 'telegram-traffic-warning': { value: raw } }));
    const payload = vm.runInContext('telegramReportPayload("")', sandbox);
    assert.equal(payload.traffic_warning_percent, Number(raw));
  }
});

test('telegram settings page renders the threshold control and hydrates it', () => {
  assert.match(SOURCE, /id="telegram-traffic-warning"/);
  assert.match(SOURCE, /type="number"[^>]*id="telegram-traffic-warning"/);
  assert.match(SOURCE, /min="0" max="100"/);
  assert.match(SOURCE, /流量预警阈值/);
  assert.match(
    SOURCE,
    /getElementById\('telegram-traffic-warning'\)\.value =\s*Number\.isInteger\(settings\.traffic_warning_percent\) \? String\(settings\.traffic_warning_percent\) : '80'/,
  );
  // The field must sit in the same form grid as the schedule controls rather
  // than floating outside it, which would break the two-column rhythm.
  assert.match(SOURCE, /telegram-field[\s\S]*?id="telegram-traffic-warning"[\s\S]*?telegram-report-actions/);
});

test('telegram page previews the report sections the builder actually emits', () => {
  for (const section of [
    '今日概览',
    '服务器部署信息',
    '客户端分布',
    '流量统计',
    '今日节点热度 TOP 5',
    '流量预警',
    '保号提醒',
  ]) {
    assert.ok(SOURCE.includes(section), `preview must describe the ${section} section`);
  }
});
