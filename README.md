# CPA Token Usage

CPA Token Usage is a CLIProxyAPI plugin for Codex account operation dashboards and AI provider usage analytics.

Current version: `0.1.46`

## Features

- Codex account pool dashboard with pagination, saved sorting, quota bars, 7d/month quota estimates, cost estimates, and light/dark compatible UI.
- AI provider pages grouped by CPA endpoint name, separated from Codex OAuth account-pool pricing and quota calculations.
- Native Codex scheduling delegates to CPA; confirmed quota exhaustion is isolated through the CPA Management status API and recovered at reliable reset times.
- Durable 401/402/account-level 403 isolation with explicit Recheck and Enable; ambiguous credential replacement never automatically enables an account.
- Suspicious external quota consumption detection for shared or resold accounts.
- Optional periodic Codex quota trigger that sends a tiny real Codex request to refresh/start server-reported quota windows.
- Authenticated Management UI/API workflow for previewing and activating fresh Codex quota windows exactly once per observed account cycle.
- Runtime diagnostics and local alerts are exposed in summary JSON for troubleshooting and plugin-store validation.
- CSV / JSON export support; the dashboard exposes account export buttons and the backend can export accounts, providers, models, and recent requests.
- Built-in price fallbacks plus automatic LiteLLM model price updates.
- Manual Chinese / English language switch saved in the browser.
- xAI account-pool dashboard for xAI OAuth JSON credentials, with xAI-specific 401/403/429 and free-usage-exhausted states.
- xAI accounts are read through CPA `host.auth.list/get/get_runtime` when available, with filesystem fallback for older CPA versions; account rows classify Free, Super, and Heavy tiers from auth metadata.
- Non-standard Codex credential import converts ChatGPT Session, sub2api/account-product, 9router, Codex auth.json, AxonHub, Codex-Manager, and generic nested token JSON through CPA `host.auth.save`, with preview, conflict detection, and no-refresh-token warnings.
- In legacy mode, optional Codex/xAI Session affinity for scheduler requests: the same Session can stay on the same account; without a usable binding, filtered candidates follow CPA `routing.strategy` (`fill-first` or `round-robin`).
- Legacy-only account-protection scheduling for Codex OAuth accounts: per-plan concurrency hard limits and rolling-window Token soft demotion.
- Legacy account-protection and error filtering preserve CPA `fill-first` or `round-robin` selection within the highest-priority candidate tier.
- Configured accounts with no real requests display zero quota even when background health probes have captured quota headers.
- Provider-aware cache read/write normalization keeps OpenAI-compatible and Anthropic-style usage, cache hit rates, and cost estimates consistent.
- Summary cache keys are canonicalized and bounded in memory and SQLite for long-running installations.

## Install Manually

Download the matching release zip, then place the dynamic library under the CLIProxyAPI plugin directory:

```text
plugins/linux/amd64/codex-token-usage.so
plugins/windows/amd64/codex-token-usage.dll
plugins/darwin/arm64/codex-token-usage.dylib
```

Restart CLIProxyAPI after replacing the file.

## Configuration

The plugin is configured under:

```yaml
plugins:
  enabled: true
  configs:
    codex-token-usage:
      enabled: true
      priority: 120
      scheduling_mode: native # native (default) or legacy
      management_url: http://127.0.0.1:8317
      management_key: <your plaintext management key>

      开启定时额度触发（不建议账号多的情况下开启）: false
      触发间隔分钟: 10
      触发模式: probe
      最大并发账号数: 1
      单账号超时秒数: 20
      单账号最小冷却分钟: 10

      同一个Session优先固定到同一个账号: true
      自动更新模型价格表: true
      模型价格更新间隔小时: 6
      模型价格表地址: https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json
      模型价格更新超时秒数: 20

      用量保留天数: 90
      额度触发记录保留天数: 30
      请求明细保留天数: 30

      开启账号保护调度（可能会影响缓存）: false
      Free 并发上限: 2
      Plus 并发上限: 5
      K12 并发上限: 5
      Team 并发上限: 5
      Pro 并发上限: 10
      Free 5 分钟 Token 上限: 2000000
      Plus 5 分钟 Token 上限: 8000000
      K12 5 分钟 Token 上限: 8000000
      Team 5 分钟 Token 上限: 8000000
      Pro 5 分钟 Token 上限: 12000000
      账号保护 Token 窗口秒数: 300
      账号保护预约超时秒数: 900
```

English config keys are also accepted:

```yaml
management_url: http://127.0.0.1:8317
management_key: <your plaintext management key>
quota_trigger_enabled: false
quota_trigger_interval_minutes: 10
quota_trigger_mode: probe
quota_trigger_max_concurrency: 1
quota_trigger_timeout_seconds: 20
quota_trigger_min_account_cooldown_minutes: 10
scheduler_session_affinity_enabled: true
model_price_auto_update_enabled: true
model_price_update_interval_hours: 6
model_price_update_url: https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json
model_price_update_timeout_seconds: 20
usage_retention_days: 90
quota_trigger_retention_days: 30
request_detail_retention_days: 30
account_protection_enabled: false
account_protection_free_concurrency: 2
account_protection_plus_concurrency: 5
account_protection_k12_concurrency: 5
account_protection_team_concurrency: 5
account_protection_pro_concurrency: 10
account_protection_free_token_limit: 2000000
account_protection_plus_token_limit: 8000000
account_protection_k12_token_limit: 8000000
account_protection_team_token_limit: 8000000
account_protection_pro_token_limit: 12000000
account_protection_token_window_seconds: 300
account_protection_reservation_ttl_seconds: 900
```

Quota trigger defaults to off and is not recommended for large account pools. `probe` sends a real minimal Codex model request and can consume tokens. In native mode its failures use the same classifier as normal Usage: explicit 401, 402, account/workspace 403 and 429 request auth isolation; unknown/model 403 and transient failures remain with CPA cooldown/retry. Successful responses reporting an exhausted quota window also request isolation. A terminal `refresh_token_invalidated` exposed on an exact CPA runtime auth is also isolated even when refresh failed before CPA emitted a Usage event. Success does not automatically clear isolation. Disabled accounts are checked through the state controller without temporarily enabling them. Legacy retains its existing probe, auto-ban and successful-probe recovery behavior. The old Chinese trigger key and `quota` mode remain accepted.

## Native scheduling and account state

`scheduling_mode` defaults to `native`; invalid values reject configuration. Native returns control to CPA before old Codex filters, reservations or selectors run. Configure Priority, Weight, fallback, excluded models and Codex Session Affinity in CPA. The plugin does not rewrite CPA routing configuration. The old Session Affinity key continues to apply to legacy Codex and existing xAI behavior.

Token windows and warnings remain available in native mode. Plugin concurrency hard limits and Token soft demotion are **not executed**. Select `legacy` to retain those older scheduling behaviors. xAI behavior is unchanged.

Set the `management_url` and `management_key` plugin fields shown above, or set these environment variables on the CPA process, to enable status writes:

```text
CPA_TOKEN_USAGE_MANAGEMENT_URL=http://127.0.0.1:8317
CPA_TOKEN_USAGE_MANAGEMENT_KEY=<your plaintext management key>
```

The URL is the CPA root URL. Environment variables override the corresponding plugin fields. The key must be plaintext; a configuration password hash is not a usable substitute. CPA 7.2.145 does not provide a masked secret field for plugin configuration, so `management_key` is stored visibly in the CPA YAML and should only be used where access to that file and Management UI is trusted. Prefer the environment variable when the config must not contain the key. Missing or invalid configuration leaves statistics and native scheduling working, but the dashboard shows a prominent warning that 401/402/403/429 state writes cannot run. API errors never reactivate the old native candidate filters.

The controller supports uniquely identified physical Codex OAuth auth files. It sends only `{name, auth_index, disabled}` to `PATCH /v0/management/auth-files/status`, then verifies both the file and runtime state and checks that credential/custom/routing fields were preserved. HTTP 200 alone is insufficient. SQLite stores fingerprints, intent, state versions and sanitized audit evidence. Existing disabled files are treated as manually disabled unless a completed plugin operation proves ownership.

Only unchanged, plugin-owned quota or rate-limit isolation is automatically enabled when its recovery time is reached. Ordinary 429 uses the server retry/reset time, or a 60-second local backoff if none is provided; the local backoff is not a quota reset estimate. Multiple exhausted windows use their latest reset. Recovery is lazy: the lifecycle controller, dashboard browsing and page refreshes never query upstream quota. Unknown reset times wait for an explicit **Check and recover / 检查并恢复** action. Normal requests still record returned quota headers, but successful in-flight requests and cached percentages never clear isolation. 401 never has timer recovery. 402 and account-level 403 require manual review. External changes pause control, and Recheck never implicitly enables an account. If an operator enables a manually disabled account outside the plugin, the lifecycle state stops reporting `MANUAL_DISABLED` but remains paused until reviewed. CPA has no conditional update or operator marker, so a narrow concurrent external-write race remains possible.

Dashboard actions call the existing Management-authenticated endpoint:

```http
POST /v0/management/plugins/codex-token-usage/auth-states/action
Content-Type: application/json

{"auth_index":"exact-index","version":3,"action":"recheck"}
```

Actions are `check_and_recover`, `recheck`, `enable`, `disable`, and `clear`; use the latest lifecycle version from Summary. A stale version returns 409. `check_and_recover` is available only for unchanged plugin-owned 429 disables: it makes one quota request and enables only when all applicable windows explicitly have available quota, then verifies physical and runtime status. Exhaustion updates a reliable recovery time; incomplete quota, errors or new pending failures keep the account disabled. A 401 during this check upgrades the account to authentication-invalid with no recovery timer. Recheck remains check-only, and HTTP 200 with exhausted or unknown quota is not a successful quota recovery check. Billing/permission Recheck requires `confirm_model_probe: true` and can include `probe_model` (default `gpt-5.5`); this sends a real request and respects auth-file model exclusions. Other Rechecks use read-only quota requests. A successful check is valid for five minutes for explicit Enable. Disable withdraws automatic recovery ownership. Clear stops plugin management without enabling the account and retains audit history.

Request records and native lifecycle events commit atomically before auxiliary accounting. Missing identities or temporarily unreadable auth snapshots keep events pending for retry; identity matching never falls back to email. Pending 429 isolation can be upgraded by a later 401, and queued failures are processed before timer recovery. Summary exposes sanitized `auth_controller.events` with disposition counts plus bounded `pending` and `recent` lists (up to 100 each). Dispositions distinguish pending identity/snapshot, pending disable, confirmed disable, stale observations and conflicts. Upgrades retain unfinished events without replaying historical request failures as new disables.

The old release/resolve routes remain registered. Native callers must supply exact `auth_index` and `version`; unversioned requests return `409 refresh_required` without changing auth. Legacy keeps its original route semantics.

Before uninstalling or downgrading to an old binary, review every plugin-owned disabled account and either explicitly recover it after verification or hand responsibility to an operator using Disable/Clear. Keep the database for audit. Replacing a DLL alone does not undo disabled auth files. Switching to legacy stops new native isolation while continuing previously owned quota recovery.

See [architecture audit](CODEX_ARCHITECTURE_AUDIT.md), [implementation report](CODEX_REFACTOR_REPORT.md), and [manual smoke test](MANUAL_SMOKE_TEST.md).

The auto-disable table and 401/402/429 cards use the same native lifecycle records, including accounts without usage in the selected window. Pending status writes, confirmed plugin disables and manual disables remain visible. The 401/429 card counts include confirmed disables; pending accounts and unresolved event evidence are shown separately. Recovery times and pending recovery are shown explicitly; recovered accounts are removed even if their last historical HTTP status was 429. Native card dialogs use versioned actions, including one-click Check and recover for eligible 429 accounts.

## One-shot quota-window activation

The dashboard action **Activate quota windows once** is independent of the periodic trigger and works while `quota_trigger_enabled` remains `false`:

1. Preview reads quota without model generation and lists every exact Codex auth record separately, including multiple seats with the same email.
2. The default decision requires an enabled, unexpired Codex credential with at least one explicitly reported quota window and every reported window completely fresh. An explicitly `null` window is absent and does not block another valid reported window; omitted presence, zero reported windows, contradictory values, positive usage/tokens, or a countdown shorter than the server-reported duration are not fresh eligibility.
3. Window names are opaque API slots, not duration promises: the UI shows each window's server-reported `limit_window_seconds`, `reset_after_seconds`, presence, usage, and reset time. For example, an account may report a seven-day `primary_window` and an explicitly null `secondary_window`.
4. After explicit acknowledgement, a confirmed run revalidates the exact auth identity and quota, reserves a stable cycle key, and sends one fixed compact Codex request per selected account. A fresh full-duration window's moving `reset_at` is not part of that key. A later fresh cycle becomes distinct either after a prior safe window boundary has passed or after durable valid observations show the guarded cycle active and then show every reported window full/fresh again. The active-to-fresh policy intentionally accepts one authoritative fresh server read after prior active evidence so event-driven or manual resets need not wait for a scheduled boundary. That refresh observation is persisted once on the predecessor, making its successor key stable across preview, run revalidation, restart, and definite pre-send retry. Repeated fresh reads cannot mint another successor; the successor must itself be observed active before a later fresh read can create another generation. An ambiguous send without active-to-fresh evidence or a safe elapsed boundary stays blocked rather than risking a duplicate.
5. The result reports `verified`, `partial`, `failed_before_send`, `sent_unknown`, or an explainable skip. Verification requires positive usage/tokens or a full-duration-to-shorter-countdown transition for every reported target window. Reset-time movement alone is not evidence, explicitly absent windows do not force `partial`, and ambiguous or partially verified sends are never retried automatically.

The request is the smallest fixed request currently used by this plugin; it is a real request and can consume a small amount of quota. The API does not enforce an exact one-token output, so this feature makes no exact-token-cost claim. Force recovery mode bypasses only the fresh-window decision and requires explicit auth indexes; it never bypasses disabled, expired, provider, identity, credential, unknown-presence, zero-window, contradictory-quota, or quota-read safety checks.

Management routes (all protected by CPA Management authentication) are:

```text
POST /v0/management/plugins/codex-token-usage/quota-activation/preview
GET  /v0/management/plugins/codex-token-usage/quota-activation/preview?id=<preview-id>
POST /v0/management/plugins/codex-token-usage/quota-activation/run
GET  /v0/management/plugins/codex-token-usage/quota-activation/run?id=<run-id>
```

Example preview body:

```json
{"force": false, "auth_indexes": []}
```

A completed preview returns a short-lived one-time confirmation token. Pass that token, the preview ID, and an explicit subset of preview-eligible auth indexes to the run endpoint. Preview/run state and cycle reservations are persisted in the plugin SQLite database. Credentials, internal auth IDs/file names, authorization headers, cookies, and raw upstream bodies are not returned; credentials, headers, cookies, and raw bodies are not persisted. A periodic trigger round and a one-shot run share an exclusion gate and cannot dispatch concurrently.

Stopping the activation feature stops future requests but cannot undo a successful upstream request or its quota-window effect. Before removing the plugin, also complete the account-state ownership handoff described above.

## Model Price Table

The plugin includes a small built-in fallback price table. By default it also downloads and refreshes the full LiteLLM-style model price table from:

```text
https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json
```

The downloaded file is stored under the current CPA user's data directory:

```text
$HOME/.cli-proxy-api/plugins/codex-token-usage/model_prices.cache
```

The file is about 1.5 MB and is not bundled into release zips, so plugin binaries stay smaller and prices can be refreshed without rebuilding the plugin.

To override the location, set:

```bash
CPA_MODEL_PRICE_FILE=/path/to/model_prices.json
```

`CPA_TOKEN_USAGE_DIR` overrides the shared plugin data directory used by both `usage.db` and, unless `CPA_MODEL_PRICE_FILE` is set, `model_prices.cache`. The cache content is JSON, but the non-JSON extension prevents CPA from treating it as an authentication file. Existing plugin-owned `model_prices.json` files are migrated automatically. `CPA_CONFIG_PATH` (or the legacy `CPA_CONFIG_FILE`) overrides the CPA config path. Otherwise the plugin follows CPA's `-config` / `--config` process argument. Without either override, the canonical `$HOME/.cli-proxy-api/config.yaml` is preferred when present, followed by an existing `$HOME/config.yaml` or `config.yaml` in the process working directory; the final fallback remains `$HOME/.cli-proxy-api/config.yaml`.

## Data Safety

- Access tokens, refresh tokens, id tokens, and API keys are not written to summary JSON, UI, alert output, or exports.
- Exported account labels that look like API keys are masked as `sk-****abcd`.
- Local alert data is generated inside summary/export responses only; this version does not send webhooks.
- Auth JSON is read for identity, quota access and conflict detection. Native status operations ask CPA to persist only disabled/enabled state, with field-preservation checks; the existing explicitly requested import workflow remains separate. Tokens are used in memory and never included in lifecycle logs, Summary or exports.

## Build

```bash
CGO_ENABLED=1 go test ./...
go test -race ./...
go vet ./...
python3 integration/run_cpa_native.py # Go >=1.26; pinned CPA v7.2.145
./build.sh
./package-release.sh dist
```

Release assets are named in the CLIProxyAPI plugin store format:

```text
codex-token-usage_0.1.46_linux_amd64.zip
codex-token-usage_0.1.46_linux_arm64.zip
codex-token-usage_0.1.46_windows_amd64.zip
codex-token-usage_0.1.46_darwin_amd64.zip
codex-token-usage_0.1.46_darwin_arm64.zip
checksums.txt
```

## Plugin Store Checklist

- Build and upload all required OS / architecture zip files.
- Include `checksums.txt`.
- Add screenshots for the Codex account pool, AI provider overview, and a selected AI endpoint page.
- Document default-off quota trigger behavior and real-probe token cost risk.
- Confirm `go test ./...` passes before publishing.

## Common Issues

- `未注册 / 未生效`: confirm the file is under the correct plugin directory and restart CLIProxyAPI.
- Native `401`: isolation requires configured Management access; verify credentials with Recheck, then explicitly Enable. Replacement alone does not restore an ambiguous account.
- Native `429`: ordinary rate limits and confirmed quota exhaustion both disable the auth through CPA. Unknown quota resets are queried without inventing a recovery time; ordinary 429 without retry metadata uses a 60-second local backoff.
- Provider not visible: confirm the endpoint still exists in CPA config and refresh the dashboard.
- Price missing: check `model_prices.cache` status in the summary JSON and the model price update error if present.

## License

MIT
