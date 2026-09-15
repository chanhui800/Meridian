'use strict';

// Frontend half of the dual-stack node publishing feature. These helpers decide
// what the operator is told the node will publish, so the wording and the
// validation both have to match the controller's rules.

const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const vm = require('node:vm');

const root = path.join(__dirname, '..');

function loadNodesSandbox() {
  const sandbox = {
    console, Date, Number, String, Math, Promise, Set, Map, JSON, Array, Object, RegExp,
    setTimeout, clearTimeout, clearInterval, setInterval,
  };
  vm.createContext(sandbox);
  vm.runInContext(fs.readFileSync(path.join(root, 'web/static/js/pages/nodes.js'), 'utf8'), sandbox, { filename: 'nodes.js' });
  return sandbox;
}

const nodes = loadNodesSandbox();
const call = (expression) => vm.runInContext(expression, nodes);

test('dual stack: the address family is derived only from a literal IP', () => {
  assert.equal(call('nodeAddressFamily("203.0.113.10")'), 'v4');
  assert.equal(call('nodeAddressFamily("2001:db8::1")'), 'v6');
  assert.equal(call('nodeAddressFamily("  203.0.113.10  ")'), 'v4');
  // A hostname, a port, a zone or brackets cannot be published as a record.
  assert.equal(call('nodeAddressFamily("node.example.test")'), '');
  assert.equal(call('nodeAddressFamily("203.0.113.10:443")'), '');
  assert.equal(call('nodeAddressFamily("[2001:db8::1]")'), '');
  assert.equal(call('nodeAddressFamily("fe80::1%eth0")'), '');
  assert.equal(call('nodeAddressFamily("999.1.1.1")'), '');
  assert.equal(call('nodeAddressFamily("")'), '');
});

test('dual stack: a family address falls back to the primary slot', () => {
  const cases = [
    // [node literal, expected v4, expected v6]
    ['{"address":"203.0.113.10"}', '203.0.113.10', ''],
    ['{"address":"2001:db8::1"}', '', '2001:db8::1'],
    ['{"address":"203.0.113.10","address_v6":"2001:db8::1"}', '203.0.113.10', '2001:db8::1'],
    ['{"address":"2001:db8::1","address_v4":"203.0.113.10"}', '203.0.113.10', '2001:db8::1'],
    // A wrong-family slot value is ignored rather than published.
    ['{"address_v4":"2001:db8::1"}', '', ''],
    ['{"address_v6":"203.0.113.10"}', '', ''],
  ];
  for (const [node, wantV4, wantV6] of cases) {
    assert.equal(call(`nodeFamilyAddress(${node}, "v4")`), wantV4, `v4 for ${node}`);
    assert.equal(call(`nodeFamilyAddress(${node}, "v6")`), wantV6, `v6 for ${node}`);
  }
});

test('dual stack: the card summary states which records are published', () => {
  const cases = [
    ['{"address":"203.0.113.10"}', '发布 A（IPv4）'],
    ['{"address":"2001:db8::1"}', '发布 AAAA（IPv6）'],
    ['{"address":"203.0.113.10","address_v6":"2001:db8::1"}', '发布 A（IPv4） + AAAA（IPv6）'],
    // A dual-stack node pinned to one family must not claim to publish both.
    ['{"address":"203.0.113.10","address_v6":"2001:db8::1","dns_publish":"v4"}', '发布 A（IPv4）'],
    ['{"address":"203.0.113.10","address_v6":"2001:db8::1","dns_publish":"v6"}', '发布 AAAA（IPv6）'],
    // A pin with no matching address publishes nothing, and says so.
    ['{"address":"203.0.113.10","dns_publish":"v6"}', '不发布 DNS 记录'],
    ['{"address_v4":"203.0.113.10","address_v6":"2001:db8::1","dns_publish":"v4"}', '发布 A（IPv4）'],
    ['{}', '待填写地址'],
  ];
  for (const [node, want] of cases) {
    assert.equal(call(`nodeDNSPublishSummary(${node})`), want, `summary for ${node}`);
  }
});

test('dual stack: the publish mode label is shown on the card', () => {
  assert.equal(call('nodeDNSPublishModeLabel({})'), '自动');
  assert.equal(call('nodeDNSPublishModeLabel({dns_publish:"auto"})'), '自动');
  assert.equal(call('nodeDNSPublishModeLabel({dns_publish:"v4"})'), '仅 IPv4');
  assert.equal(call('nodeDNSPublishModeLabel({dns_publish:"v6"})'), '仅 IPv6');
});

test('dual stack: the form preview names the consequence of each combination', () => {
  const cases = [
    // Both families with auto: the dual-stack default.
    [['203.0.113.10', '2001:db8::1', 'auto'], ['将发布 A（IPv4） + AAAA（IPv6）', '双栈', 'IPv6 优先']],
    // Pinning one family has to warn about the other family's clients.
    [['203.0.113.10', '2001:db8::1', 'v4'], ['将发布 A（IPv4）', '仅 IPv4', '无法连接']],
    [['203.0.113.10', '2001:db8::1', 'v6'], ['将发布 AAAA（IPv6）', '仅 IPv6', '无法连接']],
    // Single stack is stated as a limitation, not as an error.
    [['203.0.113.10', '', 'auto'], ['将发布 A（IPv4）', '纯 IPv6 客户端无法连接']],
    [['', '2001:db8::1', 'auto'], ['将发布 AAAA（IPv6）', '纯 IPv4 客户端无法连接']],
    // Nothing to publish.
    [['', '', 'auto'], ['当前不会发布任何记录']],
    [['203.0.113.10', '', 'v6'], ['当前不会发布任何记录']],
  ];
  for (const [args, needles] of cases) {
    const got = call(`nodeDNSPublishDraftSummary(${args.map(value => JSON.stringify(value)).join(', ')})`);
    for (const needle of needles) {
      assert.ok(got.includes(needle), `${JSON.stringify(args)} -> ${got} must mention ${needle}`);
    }
  }
});

test('dual stack: the form refuses combinations that would publish nothing', () => {
  assert.equal(call('nodeDNSPublishDraftError("203.0.113.10", "2001:db8::1", "auto")'), '');
  assert.equal(call('nodeDNSPublishDraftError("203.0.113.10", "", "v4")'), '');
  assert.equal(call('nodeDNSPublishDraftError("", "2001:db8::1", "v6")'), '');
  assert.match(call('nodeDNSPublishDraftError("", "", "v4")'), /未填写 IPv4/);
  assert.match(call('nodeDNSPublishDraftError("", "", "v6")'), /未填写 IPv6/);
  assert.match(call('nodeDNSPublishDraftError("2001:db8::1", "", "auto")'), /IPv4 地址格式/);
  assert.match(call('nodeDNSPublishDraftError("", "[2001:db8::1]", "auto")'), /IPv6 地址请填字面量/);
});

test('dual stack: an auto-detected address is flagged for review', () => {
  const page = fs.readFileSync(path.join(root, 'web/static/js/pages/nodes.js'), 'utf8');
  // Both provenance values the controller can set must ask for a review, not
  // just the enrollment-inferred one.
  assert.match(page, /node\.address_source === 'enrollment' \|\| node\.address_source === 'detected'/);
  assert.match(page, /自动探测，请核对/);
});
