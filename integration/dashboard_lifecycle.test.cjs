const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const path = require('node:path');
const source = fs.readFileSync(path.join(__dirname, '..', 'dashboard_scripts.go'), 'utf8');

function dashboard(rows) {
  const elements = new Map();
  const heads = Array.from({ length: 9 }, () => ({}));
  const context = vm.createContext({
    lastData: { auth_controller: { scheduling_mode: 'native' }, accounts: [], autobans: rows },
    activePage: 'codex', autobanPage: 1, autobanPageSize: 10,
    Date, Number, Math,
    document: { getElementById(id) {
      if (!elements.has(id)) elements.set(id, { closest: () => ({ querySelectorAll: () => heads }) });
      return elements.get(id);
    } },
    esc: value => String(value ?? '').replaceAll('&', '&amp;').replaceAll('<', '&lt;').replaceAll('"', '&quot;'),
    td: (value, cls) => '<td class="' + (cls || '') + '">' + value + '</td>',
    pct: value => value == null ? '-' : value + '%',
    fmt: String,
  });
  for (const name of ['isXAIPool', 'isNativeLifecycle', 'nativeLifecycleRows', 'lifecycleCurrentState', 'lifecycleAuthInvalid', 'lifecycleWorkspaceBlocked', 'lifecycleRateLimited', 'isInvalidAuthBan', 'isWorkspaceDeactivatedBan', 'is429Autoban', 'isPermanentAuthBan', 'duration', 'autobanResetText', 'autobanRemainingText', 'autobanRemainingSortValue', 'sortAutobansByRemaining', 'autobanReleaseRows', 'invalidAuthRows', 'workspaceDeactivatedRows', 'lifecycleStatus', 'accountName', 'renderNativeLifecycleModal', 'renderAutobans']) {
    const start = source.indexOf('function ' + name + '(');
    assert.ok(start >= 0, name + ' exists');
    let extracted = '';
    let compiled;
    for (const line of source.slice(start).split('\n')) {
      extracted += line + '\n';
      try { compiled = new vm.Script('(' + extracted + ')'); break; } catch (_) { /* function continues */ }
    }
    assert.ok(compiled, name + ' parses');
    context[name] = compiled.runInContext(context);
  }
  return { context, elements, run: code => vm.runInContext(code, context) };
}

function row(index, state = 'RATE_LIMITED', extra = {}) {
  const status = { RATE_LIMITED: 429, QUOTA_COOLDOWN: 429, AUTH_INVALID: 401, BILLING_BLOCKED: 402 }[state] || 0;
  return {
    auth_id: index + '.json', auth_index: index, source: index + '@example.test',
    window: String(status), last_status_code: status, seconds_remaining: 120,
    lifecycle: { auth_index: index, version: 1, state, disabled: true, disabled_by_plugin: true, recover_at: Math.floor(Date.now() / 1000) + 120, ...extra },
  };
}

test('19 native 429 accounts populate table and card dialog with pagination', () => {
  const rows = Array.from({ length: 19 }, (_, i) => row('account-' + i));
  const { context, elements, run } = dashboard(rows);
  assert.equal(run('autobanReleaseRows().length'), 19);
  run('renderAutobans(lastData.autobans)');
  assert.equal(elements.get('autoban-scope').textContent, '显示 1-10 / 19 个自动禁用账号');
  assert.equal((elements.get('autobans').innerHTML.match(/<tr>/g) || []).length, 10);
  assert.match(elements.get('autobans').innerHTML, /account-0@example.test/);
  assert.match(elements.get('autobans').innerHTML, /已禁用/);
  context.autobanPage = 2;
  run('renderAutobans(lastData.autobans)');
  assert.equal(elements.get('autoban-scope').textContent, '显示 11-19 / 19 个自动禁用账号');
  run("renderNativeLifecycleModal('autoban-release',{rows:lastData.autobans,pages:2,pageRows:lastData.autobans.slice(10)},2,'管理 429 禁用账号')");
  assert.match(elements.get('autoban-release-list').innerHTML, /account-18@example.test/);
  assert.match(elements.get('autoban-release-list').innerHTML, /data-lifecycle-action="enable"/);
  assert.equal(elements.get('autoban-release-all').hidden, true);
});

test('401/402 cards include native rows even without usage accounts', () => {
  const { run } = dashboard([row('invalid', 'AUTH_INVALID', { recover_at: 0 }), row('billing', 'BILLING_BLOCKED', { recover_at: 0 })]);
  assert.equal(run('invalidAuthRows().length'), 1);
  assert.equal(run('workspaceDeactivatedRows().length'), 1);
  assert.equal(run('autobanReleaseRows().length'), 0);
});

test('historical HTTP codes and blocked states do not flag recovered accounts', () => {
  const { context, run } = dashboard([]);
  for (const [state, predicate, status] of [['RATE_LIMITED', 'lifecycleRateLimited', 429], ['AUTH_INVALID', 'lifecycleAuthInvalid', 401], ['BILLING_BLOCKED', 'lifecycleWorkspaceBlocked', 402]]) {
    context.account = row('recovered', 'HEALTHY', { disabled: false, blocked_state: state, last_http_status: status });
    assert.equal(run(predicate + '(account)'), false);
  }
});

test('unknown reset, pending writes, manual ownership and due recovery are explicit', () => {
  const { context, run } = dashboard([]);
  context.account = row('quota', 'QUOTA_COOLDOWN', { recover_at: 0 });
  assert.equal(run('autobanRemainingText(account)'), '待查询');
  assert.equal(run('autobanResetText(account)'), '等待查询额度恢复时间');
  context.account.lifecycle.disabled = false;
  context.account.lifecycle.pending_action = 'disable';
  assert.equal(run('autobanResetText(account)'), '等待禁用确认');
  assert.match(run('lifecycleStatus(account.lifecycle)'), /待禁用/);
  context.account = row('manual', 'MANUAL_DISABLED', { disabled_by_plugin: false });
  assert.equal(run('autobanResetText(account)'), '需人工复查后启用');
  context.account = row('due', 'RATE_LIMITED', { recover_at: Math.floor(Date.now() / 1000) - 1 });
  assert.equal(run('autobanRemainingText(account)'), '等待启用确认');
});
