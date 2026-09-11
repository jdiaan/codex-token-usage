const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const path = require('node:path');
const source = fs.readFileSync(path.join(__dirname, '..', 'dashboard_scripts.go'), 'utf8').replace(/\r\n/g, '\n');

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
  assert.doesNotMatch(elements.get('autoban-release-list').innerHTML, /Recheck|Clear plugin state|check_and_recover/);
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
  assert.equal(run('autobanRemainingText(account)'), '正在恢复');
  assert.equal(run('autobanResetText(account)'), '默认冷却 1 分钟');
  context.account.lifecycle.disabled = false;
  context.account.lifecycle.pending_action = 'disable';
  assert.equal(run('autobanResetText(account)'), '正在同步');
  assert.match(run('lifecycleStatus(account.lifecycle)'), /正在同步/);
  context.account = row('manual', 'MANUAL_DISABLED', { disabled_by_plugin: false, manual_disabled: true });
  assert.equal(run('autobanResetText(account)'), '人工停用');
  context.account = row('due', 'RATE_LIMITED', { recover_at: Math.floor(Date.now() / 1000) - 1 });
  assert.equal(run('autobanRemainingText(account)'), '正在恢复');
});

test('normal account states have no review controls and only sync failures offer retry', () => {
  const {context,run}=dashboard([]);
  for(const state of ['HEALTHY','AUTH_INVALID','BILLING_BLOCKED','QUOTA_COOLDOWN','RATE_LIMITED']){
    context.account=row('a',state);
    const html=run('lifecycleStatus(account.lifecycle)');
    assert.doesNotMatch(html,/data-lifecycle-action|Recheck|Enable|Disable|check_and_recover/);
  }
  context.account=row('a','AUTH_INVALID',{disabled:false,pending_action:'disable',sync_error:'retrying'});
  assert.match(run('lifecycleStatus(account.lifecycle)'),/data-lifecycle-action="retry_sync"/);
});

test('pending evidence is visible without counting it as a confirmed disable or querying quota', () => {
  const {context,elements,run}=dashboard([row('pending','AUTH_INVALID',{disabled:false,pending_action:'disable'})]);
  let queries=0;
  context.fetch=()=>{queries++;throw Error('render must not request quota')};
  context.lastData.auth_controller.events={pending:[{auth_index:'<unmatched>',status:401,outcome:'pending_identity',detail:'auth identity missing or ambiguous'}]};
  for(let i=0;i<3;i++)run("renderNativeLifecycleModal('invalid-auth',{rows:lastData.autobans,pages:1,pageRows:lastData.autobans},1,'管理 401 失效账号')");
  assert.equal(elements.get('invalid-auth-summary').textContent,'已禁用 0 个 · 正在同步 1 个');
  assert.doesNotMatch(elements.get('invalid-auth-status').textContent,/<unmatched>|manual control|需复查/);
  assert.match(elements.get('invalid-auth-status').textContent,/自动恢复，无需额外操作/);
  assert.equal(queries,0);
});

test('retry-sync click sends one versioned action and refreshes after success or failure', async () => {
  for(const outcome of ['enabled','still_disabled','error']){
    const {context}=dashboard([row('a')]);
    const button={dataset:{authIndex:'a',version:'1',lifecycleAction:'retry_sync'},disabled:false};
    const requests=[];
    let refreshes=0,handler;
    context.window={confirm:()=>true,alert:()=>{}};
    context.document.addEventListener=(_event,fn)=>{handler=fn};
    context.quotaActivationJSON=async(url,options)=>{requests.push({url,body:JSON.parse(options.body)});if(outcome==='error')throw Error('failed');return {account:{disabled:outcome!=='enabled'}}};
    context.load=async()=>{refreshes++};
    const start=source.indexOf("document.addEventListener('click',async event=>{\n  const button=event.target.closest('[data-lifecycle-action]')");
    assert.ok(start>=0);
    vm.runInContext(source.slice(start,source.indexOf('function accountStatus(',start)),context);
    await handler({target:{closest:()=>button}});
    assert.equal(requests.length,1);
    assert.equal(requests[0].body.action,'retry_sync');
    assert.equal(requests[0].body.auth_index,'a');
    assert.equal(requests[0].body.version,1);
    assert.equal(refreshes,1);
    assert.equal(button.disabled,false);
  }
});
