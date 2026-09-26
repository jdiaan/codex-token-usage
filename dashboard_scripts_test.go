package main

import (
	"strings"
	"testing"
)

func TestDashboardRecentRequestsHaveSeparateTimeColumn(t *testing.T) {
	const header = `<th>模型</th><th>时间</th><th>耗时</th><th>Tokens</th><th>费用</th><th>详情</th>`
	if got := strings.Count(dashboardBody, header); got != 1 {
		t.Fatalf("static recent request tables with time column = %d, want 1", got)
	}
	for _, marker := range []string{
		header,
		`'时间':'Time'`,
		`td(esc(r.time||'-'),'recent-time')`,
		`colspan="6" class="muted">暂无请求记录`,
	} {
		if !strings.Contains(dashboardScripts, marker) {
			t.Fatalf("recent request time column marker %q not found", marker)
		}
	}
	if strings.Contains(dashboardScripts, `esc(who)+' · '+esc(r.time||'-')`) {
		t.Fatal("recent request time is still rendered in the model metadata")
	}
}

func TestDashboardCacheTokensDoesNotDoubleCountOverlappingFields(t *testing.T) {
	markers := []string{
		`const cached=Number(r.cached_tokens||0),read=Number(r.cache_read_tokens||0),creation=Number(r.cache_creation_tokens||0);`,
		`return Math.max(cached-read-creation,0)+read;`,
		`function cacheWriteTokens(r){return Math.max(0,Number(r.cache_creation_tokens||0))}`,
		`cacheInputIncludesDetails(r)?Math.max(input,cache):input+cache`,
		`function cacheRate(r){return ratio(cacheTokens(r),cacheInputTotal(r))}`,
		`provider.includes('claude')||provider.includes('anthropic')`,
	}
	for _, marker := range markers {
		if !strings.Contains(dashboardScripts, marker) {
			t.Fatalf("dashboard cache normalization missing %q", marker)
		}
	}
	if strings.Contains(dashboardScripts, `(r.cached_tokens||0)+(r.cache_read_tokens||0)+(r.cache_creation_tokens||0)`) {
		t.Fatal("dashboard still double-counts overlapping cache fields")
	}
}

func TestDashboardQuotaLayoutSeparatesFreeAndPaidWindows(t *testing.T) {
	for _, marker := range []string{
		"function quotaPlanKind(r)",
		"function quotaDisplayPrefix(r)",
		"function quotaWindowCellForDisplay(r,prefix)",
		"if(quotaPlanKind(r)==='free')return '';",
		"function quotaText(percent,tokens){return pct(percent)}",
		"if(quotaPlanKind(r)==='free')return Number.MAX_SAFE_INTEGER",
	} {
		if !strings.Contains(dashboardScripts, marker) {
			t.Fatalf("quota layout marker %q not found", marker)
		}
	}
	for _, unwanted := range []string{"无本地 Token", "Free 仅单窗口", "尚未探测，待刷新"} {
		if strings.Contains(dashboardScripts, unwanted) {
			t.Fatalf("dashboard still contains removed quota placeholder %q", unwanted)
		}
	}
}

func TestAccountTableLifecycleStatusIsDisplayOnly(t *testing.T) {
	if !strings.Contains(dashboardScripts, "return lifecycleStatusLabel(r.lifecycle);") {
		t.Fatal("account table must render only the lifecycle status label")
	}
	if !strings.Contains(dashboardScripts, "function lifecycleStatus(s)") || !strings.Contains(dashboardScripts, "data-lifecycle-action=\"enable\"") {
		t.Fatal("management dialogs must retain lifecycle controls")
	}
}

func TestNativeLifecycleFailuresAreVisible(t *testing.T) {
	markers := []string{
		"function lifecycleAuthInvalid(r)",
		"function lifecycleRisk(r)",
		"lifecycleAccounts.filter(r=>lifecycleAuthInvalid(r)&&r.lifecycle.disabled).length",
		"最近列表已有 ",
		"请管理员修改服务器私密配置文件并重启 CPA",
		"data-lifecycle-action=\"retry_sync\"",
	}
	for _, marker := range markers {
		if !strings.Contains(dashboardScripts, marker) {
			t.Fatalf("native lifecycle visibility missing %q", marker)
		}
	}
	if !strings.Contains(dashboardStyles, "#lifecycle-banner.lifecycle-danger") {
		t.Fatal("unconfigured lifecycle warning is not visually prominent")
	}
}

func TestRecentRequestsShowLifecycleIdentityFields(t *testing.T) {
	marker := "const who=[r.provider,r.auth_index,r.source]"
	if !strings.Contains(dashboardScripts, marker) {
		t.Fatalf("recent requests do not expose provider/auth_index/source identity: missing %q", marker)
	}
}

func TestPoolTabSwitchReappliesLocale(t *testing.T) {
	start := strings.Index(dashboardScripts, "function switchPage(page){")
	if start < 0 {
		t.Fatal("switchPage function not found")
	}
	end := strings.Index(dashboardScripts[start:], "\nfunction providerStorageKey()")
	if end < 0 {
		t.Fatal("switchPage function end not found")
	}
	switchPage := dashboardScripts[start : start+end]
	renderAt := strings.Index(switchPage, "renderPoolPage(lastData);")
	localeAt := strings.Index(switchPage, "applyLocale();")
	if renderAt < 0 || localeAt < 0 || localeAt < renderAt {
		t.Fatalf("pool tab switch must reapply locale after rendering: %q", switchPage)
	}
}

func TestCodexPoolDataCarriesForbiddenAuths(t *testing.T) {
	if !strings.Contains(dashboardScripts, "forbidden_auths:data.forbidden_auths||[]") {
		t.Fatal("Codex pool data must carry standalone 403 auth records into insights")
	}
}

func TestDashboardExplainsWaitingRuntimeAccountsAndStaleCandidates(t *testing.T) {
	for _, marker := range []string{"waiting_runtime_load", "等待 CPA 加载", "candidate_pool_stale", "CPA 候选缺少"} {
		if !strings.Contains(dashboardScripts, marker) {
			t.Fatalf("dashboard missing Issue #12 diagnostic marker %q", marker)
		}
	}
}

func TestDashboardUsesSimpleLifecycleActions(t *testing.T) {
	start := strings.Index(dashboardScripts, "function lifecycleStatus(")
	end := strings.Index(dashboardScripts[start:], "function renderNativeLifecycleModal(") + start
	status := dashboardScripts[start:end]
	if strings.Contains(status, "Recheck") || strings.Contains(status, "Clear plugin state") || !strings.Contains(status, "retry_sync") {
		t.Fatal("lifecycle still requires manual review controls")
	}
}

func TestInvalidAuthManagementUsesUnfilteredCountsAndPartialDeleteResults(t *testing.T) {
	for _, marker := range []string{
		"const allInvalidRows=",
		"const allWorkspaceRows=",
		"deleteAuthFilesInBatches(names,key)",
		"parseInvalidAuthFileDeleteOutcomes(res,body,batch)",
		"/\\.json$/i.test(name)?name:''",
	} {
		if !strings.Contains(dashboardScripts, marker) {
			t.Fatalf("401 management marker %q not found", marker)
		}
	}
}

func TestAuthFileDeleteUsesBoundedQueryBatches(t *testing.T) {
	for _, marker := range []string{
		"const authFileDeleteBatchSize=25",
		"name='+encodeURIComponent(name)",
		"fetch(authFileDeleteURL(batch),{method:'DELETE'",
		"offset+=authFileDeleteBatchSize",
		"res.status===207",
		"res.status===404",
		"失败原因：",
	} {
		if !strings.Contains(dashboardScripts, marker) {
			t.Fatalf("auth file query delete marker %q not found", marker)
		}
	}
	if strings.Contains(dashboardScripts, "body:JSON.stringify({names:names})") {
		t.Fatal("auth-file DELETE must not send the legacy JSON names body")
	}
	parseStart := strings.Index(dashboardScripts, "function parseInvalidAuthFileDeleteOutcomes")
	parseEnd := strings.Index(dashboardScripts[parseStart:], "\nconst authFileDeleteBatchSize")
	if parseStart < 0 || parseEnd < 0 {
		t.Fatal("auth-file delete outcome parser not found")
	}
	parser := dashboardScripts[parseStart : parseStart+parseEnd]
	partialAt := strings.Index(parser, "res&&res.status===207")
	okAt := strings.Index(parser, "res&&res.ok")
	if partialAt < 0 || okAt < 0 || partialAt > okAt {
		t.Fatal("HTTP 207 must be parsed before the generic 2xx success branch")
	}
}

func TestInvalidAuthReplacementCheckUsesSecondPrecision(t *testing.T) {
	for _, marker := range []string{
		"const recordedSeconds=Math.floor(recorded>1e12?recorded/1000:recorded)",
		"Math.floor(liveMs/1000)>recordedSeconds",
	} {
		if !strings.Contains(dashboardScripts, marker) {
			t.Fatalf("replacement timestamp precision marker %q not found", marker)
		}
	}
	if strings.Contains(dashboardScripts, "liveMs>0&&liveMs>recordedMs") {
		t.Fatal("sub-second host timestamps must not mark an unchanged auth file as replaced")
	}
}

func TestNonStandardAuthImportUIUsesPluginHostSaveFlow(t *testing.T) {
	for _, marker := range []string{
		"账号 JSON 导入",
		"auth-import/preview",
		"auth-import/commit",
		"host.auth.save",
		"无 RT",
	} {
		if !strings.Contains(dashboardBody+dashboardScripts, marker) && !strings.Contains(dashboardBody+dashboardScripts+dashboardStyles, marker) {
			t.Fatalf("auth import UI marker %q not found", marker)
		}
	}
}

func TestInvalidAuthManagementSeparatesSourcesAndResolvesStableIDs(t *testing.T) {
	for _, marker := range []string{
		"invalid-auths/resolve",
		"/v0/management/auth-files/status",
		"auth_source_kind",
		"runtime_only",
		"sameStableAuthIdentity",
		"Object.freeze(selected.map",
		"data-invalid-runtime-disable",
		"file_deleted",
		"file_absent",
		"runtime_disabled",
		"replacement_kept",
		"invalidAuthFileIdentityChanged",
		"invalid_auth_status_code",
		"forbidden_auths",
		"isCredentialStateBan",
		"403 拒绝",
		"renderOpenManagementModals",
		"原本不存在",
		"替换文件已保留",
		"临时禁用",
		"已经解除",
		"不可处理",
	} {
		if !strings.Contains(dashboardScripts, marker) {
			t.Fatalf("401 stable cleanup marker %q not found", marker)
		}
	}
	for _, marker := range []string{"处理所有 401 账号", "处理选中"} {
		if !strings.Contains(dashboardBody, marker) {
			t.Fatalf("401 management UI marker %q not found", marker)
		}
	}
}

func TestQuotaActivationDashboardRequiresPreviewConfirmationAndBilingualCopy(t *testing.T) {
	for _, marker := range []string{
		"一次性启动额度窗口",
		"managementQuotaActivationPreviewApi",
		"managementQuotaActivationRunApi",
		"quotaActivationPreview.confirmation_token",
		"!!preview.force===document.getElementById('quota-activation-force').checked",
		"quota-activation-ack",
		"quotaActivationWindowTransitionHTML(row.before&&row.before.primary,row.after&&row.after.primary)",
		"quota-activation-pagination",
		"quotaActivationPageSize=50",
		"强制恢复模式必须先明确勾选账号。",
		"不保证恰好消耗一个 Token",
		"Primary 上报窗口（前 → 后）",
		"window.limit_window_seconds",
		"window.reset_after_seconds",
		"window.presence==='absent'",
		"'所有上报窗口均已验证':'All reported windows verified'",
		"'已发送但验证未知':'Sent; verification unknown'",
		"'尚未确认活跃窗口已刷新，且安全边界尚未到达':'No active-to-fresh reset or elapsed safe boundary has been established'",
		"'duplicate_cycle':'No active-to-fresh reset or elapsed safe boundary has been established'",
	} {
		if !strings.Contains(dashboardBody+dashboardScripts+dashboardStyles, marker) {
			t.Fatalf("quota activation UI marker %q not found", marker)
		}
	}
	for _, forbidden := range []string{"access_token", "refresh_token", "Authorization: Bearer"} {
		if strings.Contains(dashboardBody, forbidden) {
			t.Fatalf("quota activation HTML contains credential marker %q", forbidden)
		}
	}
}

func TestEnglishLocaleTranslatesDynamicPhrasesBeforeUnits(t *testing.T) {
	for _, marker := range []string{
		"'账号 JSON 导入':'Import account JSON'",
		"'窗口：':'Window: '",
		"Object.entries(i18nEn).sort((left,right)=>right[0].length-left[0].length).forEach(pair=>exact(pair[0],pair[1]))",
		"'部分模型缺价格':'Some model prices missing'",
		"'管理接口':'Management API'",
		"'显示接入点':'Show endpoints'",
	} {
		if !strings.Contains(dashboardScripts, marker) {
			t.Fatalf("dashboard script missing English dynamic-phrase translation marker %q", marker)
		}
	}
}
