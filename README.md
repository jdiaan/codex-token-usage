# CPA Token Usage

CPA Token Usage is a CLIProxyAPI plugin for Codex account operation dashboards and AI provider usage analytics.

Current version: `0.1.49`

## Features

- Codex account pool dashboard with pagination, saved sorting, quota bars, 7d/month quota estimates, cost estimates, and light/dark compatible UI.
- AI provider pages grouped by CPA endpoint name, separated from Codex OAuth account-pool pricing and quota calculations.
- Native Codex scheduling delegates to CPA; confirmed quota exhaustion is isolated through the CPA Management status API and recovered at reliable reset times.
- Automatic 401/402/account-level 403 isolation, restored after same-account credentials update; timed 429 recovery with a 60-second fallback.
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

### Upgrading to 0.1.49

The default persistent directory is now `$HOME/.cli-proxy-api/data/codex-token-usage`, outside the plugin installation directory. On first use, the plugin snapshots the old `$HOME/.cli-proxy-api/plugins/codex-token-usage/usage.db`, including committed WAL data, checks its integrity, and copies the price cache before publishing the new database. The old files are retained. An existing destination database is authoritative and is never overwritten or merged. Migration errors stop database initialization instead of silently starting with an empty database. The filesystem must support hard links for atomic publication (for example ext4 or NTFS).

`CPA_TOKEN_USAGE_DIR` remains authoritative: explicitly configured directories are not relocated. Set it to an absolute persistent path if the CPA service user or container changes; replacing the binary cannot preserve a home directory or volume that is itself deleted. `CPA_MODEL_PRICE_FILE` still overrides the price cache location.

Before replacing the library, stop CPA and back up `usage.db` together with any `usage.db-wal` and `usage.db-shm` files. Restart CPA, then check Summary's `version` and `db_path` to confirm the loaded build and data location. Preserve the new data directory on future upgrades. For rollback, stop CPA and point `CPA_TOKEN_USAGE_DIR` at the active data directory rather than resuming the stale copy in the old directory.

Old external-change conflicts are migrated automatically when same-account credential evidence is available. The oldest releases sometimes overwrote the only old fingerprint; when neither the saved state nor failure evidence can establish a credential change, use **启用 (Enable)** for that account after signing in again. Do not clear the database to recover accounts. If the old database is already missing, disabled accounts display **禁用来源未知** (`DISABLED_UNKNOWN`) and remain disabled until explicitly enabled.

## Configuration

### Breaking change: private management configuration

Background account status writes now require a server-local private file. The ordinary plugin fields `management_url` / `management_key` and the old `CPA_TOKEN_USAGE_MANAGEMENT_URL` / `CPA_TOKEN_USAGE_MANAGEMENT_KEY` environment variables are ignored. An upgrade with only those old settings pauses automatic account enable/disable operations; statistics and native scheduling continue normally.

Before upgrading, prepare the private file and its permissions as described below. Then replace the plugin, restart the CPA process, check the dashboard configuration status and confirm an actual account-control request. Finally, remove the obsolete fields from CPA's ordinary configuration and remove the old environment variables. The plugin never rewrites the CPA configuration or copies old credentials automatically. Administrators should clean up old plaintext configuration backups and rotate the key if exposure is suspected.

This change concerns background account control only. CPA login and dashboard manual operations retain their existing browser authentication; it does not remove CPA management credentials from the browser.

The plugin is configured under:

```yaml
plugins:
  enabled: true
  configs:
    codex-token-usage:
      enabled: true
      priority: 120
      scheduling_mode: native # native (default) or legacy

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

### Server-private management file

The default file is `$HOME/.cli-proxy-api/secrets/codex-token-usage-management.yaml`, where `$HOME` belongs to the user running CPA. This location is separate from plugin data, installation files and ordinary CPA configuration. Set `CPA_TOKEN_USAGE_MANAGEMENT_CONFIG_FILE` on the CPA process to use another **absolute** path. This environment variable selects a file only; no environment variable supplies the URL or key, and the web/plugin configuration cannot select the path.

Copy [the placeholder example](codex-token-usage-management.example.yaml) into the chosen private location, then edit it locally on the server:

```yaml
management_url: "http://127.0.0.1:8317"
management_key: "REPLACE_WITH_YOUR_PLAINTEXT_MANAGEMENT_KEY"
```

The URL is the CPA root URL. Use loopback when CPA is on the same host; use HTTPS with valid certificates across machines. HTTP redirects are not followed. The key must be the usable plaintext management key, not its password hash. Both fields must be nonempty YAML strings; quote numeric-looking keys. Duplicate or unknown fields, non-string values, multiple YAML documents and invalid addresses are rejected. The file must be a regular file no larger than 64 KiB.

The file is read once on the first successful plugin initialization in each CPA process. A failed read is also cached. **Restart the CPA process after creating, repairing, rotating or editing the file, or changing its path.** Saving web settings, reconfiguring the plugin and polling the dashboard do not reload it. The plugin never creates or edits this file, and there is no fallback to the old configuration sources.

On Linux/macOS, any permission for other users, or group write/execute permission, is rejected. Recommended deployment: an administrator owns the file, the service account has read-only group access (`0640`), and the service cannot write its parent directory. A file readable only by its owner (`0600`/`0400`) is also accepted, but service ownership provides a weaker write boundary. Keep the file outside served directories, repositories, auth-file directories and ordinary configuration exports/backups. On Windows, configure NTFS ACLs explicitly; this version does **not** automatically validate Windows ACLs. These controls reduce configuration-channel exposure; they do not protect a key from a compromised service process or server administrator.

Example Linux installation, assuming the CPA service belongs to the `cpa` group (substitute your actual service group):

```sh
sudo install -d -o root -g cpa -m 0750 /etc/cpa/secrets
sudo install -o root -g cpa -m 0640 codex-token-usage-management.example.yaml /etc/cpa/secrets/codex-token-usage-management.yaml
sudoedit /etc/cpa/secrets/codex-token-usage-management.yaml
```

Set the following in the service's environment and restart it:

```text
CPA_TOKEN_USAGE_MANAGEMENT_CONFIG_FILE=/etc/cpa/secrets/codex-token-usage-management.yaml
```

Docker Compose fragment for an existing CPA service (the source file must already exist; replace the host path and match the container service UID/GID to the file's read permissions):

```yaml
services:
  cpa:
    environment:
      CPA_TOKEN_USAGE_MANAGEMENT_CONFIG_FILE: /run/secrets/codex-token-usage-management.yaml
    volumes:
      - type: bind
        source: /etc/cpa/secrets/codex-token-usage-management.yaml
        target: /run/secrets/codex-token-usage-management.yaml
        read_only: true
        bind:
          create_host_path: false
```

`127.0.0.1` refers to the container itself; use an appropriate service address if CPA is in a different container. Recreate the container after replacing a bind-mounted file so that it sees the new file, then verify configuration and an actual request.

Windows example from an elevated PowerShell, assuming the service identity is `NT SERVICE\CPA` (replace it with the actual identity). Use a new dedicated directory; review any pre-existing explicit ACL entries if reusing one:

```powershell
New-Item -ItemType Directory -Force 'C:\ProgramData\CPA\secrets'
icacls 'C:\ProgramData\CPA\secrets' /inheritance:r /grant:r '*S-1-5-32-544:(OI)(CI)F' '*S-1-5-18:(OI)(CI)F' 'NT SERVICE\CPA:(OI)(CI)RX'
Copy-Item '.\codex-token-usage-management.example.yaml' 'C:\ProgramData\CPA\secrets\codex-token-usage-management.yaml'
icacls 'C:\ProgramData\CPA\secrets\codex-token-usage-management.yaml' /inheritance:r /grant:r '*S-1-5-32-544:F' '*S-1-5-18:F' 'NT SERVICE\CPA:R'
notepad 'C:\ProgramData\CPA\secrets\codex-token-usage-management.yaml'
```

Set `CPA_TOKEN_USAGE_MANAGEMENT_CONFIG_FILE=C:\ProgramData\CPA\secrets\codex-token-usage-management.yaml` in the service launcher and restart CPA. The service identity needs traversal/read access to the directory and read access to the file; only administrators should be able to replace or edit it.

The dashboard distinguishes missing, invalid and locally configured settings, and separately displays request failures. `auth_controller.management_configured` means the cached configuration is valid; it is **not** a connection/authentication health check. `management_config` exposes only `url_source` / `key_source` (`local_file` or `missing`), `missing_fields`, and `error_code` (empty on success). Error codes are `not_initialized`, `invalid_path`, `file_missing`, `file_unreadable`, `invalid_file`, `unsafe_permissions`, `invalid_yaml`, `unknown_field`, `duplicate_field`, `invalid_type`, `missing_fields`, `invalid_url`, and `invalid_key`. API responses and errors never include the private path, file contents or key. No private-file download/edit endpoint is provided.

Missing or invalid configuration leaves statistics and native scheduling working, but the dashboard warns that 401/402/403/429 state writes cannot run. API errors never reactivate the old native candidate filters. Only the dynamic library is included in release archives; the real private file is never packaged.

The controller supports uniquely identified physical Codex OAuth auth files. It sends only `{name, auth_index, disabled}` to `PATCH /v0/management/auth-files/status`, then verifies both the file and runtime state and checks that credential/custom/routing fields were preserved. HTTP 200 alone is insufficient. SQLite stores fingerprints, intent, state versions and sanitized audit evidence. Existing disabled files without plugin history retain unknown ownership; explicit manual disables remain manual.

401/402 (and explicit account-level 403) disable the CPA auth file until same-account credentials update, then automatically enable it. Ordinary metadata changes and token refresh observations never impose a manual-review gate. 429 and explicit traffic limits use returned JSON/header deadlines; multiple exhausted windows use the latest known reset, and a missing window does not erase another valid reset. Without a valid deadline, cooldown lasts 60 seconds. Timer expiry automatically enables plugin-owned isolation. Explicit manual disables remain manual.

Lifecycle polling, dashboard browsing, refreshes and sync actions never query upstream quota or send model probes. Normal requests may record quota observations; cached percentages and concurrent successes do not clear current isolation. Failures bind to credential generations so late old requests cannot disable new credentials. CPA writes and file/runtime read-back failures automatically retry. CPA has no conditional update or operator marker, so an indistinguishable concurrent manual write remains a platform limitation.

Dashboard actions call the existing Management-authenticated endpoint:

```http
POST /v0/management/plugins/codex-token-usage/auth-states/action
Content-Type: application/json

{"auth_index":"exact-index","version":3,"action":"retry_sync"}
```

The dashboard offers **启用 (Enable)** (`enable`) for blocked/disabled accounts and **重试同步** (`retry_sync`) for pending/failed synchronization. Enable supersedes old failures using a durable nanosecond control boundary (`control_since_ns`) and synchronizes CPA without requiring Recheck or sending upstream probes. It reports success only after file and runtime read-back confirms enabled state; this permits scheduling but does not assert that credentials have been validated. New request failures still trigger isolation. Management HTTP 401 explicitly identifies the management key problem, separately from account credential failures.

Use the latest lifecycle version from Summary; stale versions return 409 and the dashboard refreshes before another action. Compatibility `recheck` and `check_and_recover` actions perform the same local reconciliation without upstream requests. Explicit `disable` and `clear` actions remain available through the API; Disable/Clear relinquish automatic recovery ownership. The compact `relogin-required-accounts` response shape is unchanged.

External programs can query confirmed native-mode accounts that are waiting for re-login without fetching the full Summary or changing account state:

```bash
curl -H "Authorization: Bearer <CPA management key>" \
  http://127.0.0.1:8317/v0/management/plugins/codex-token-usage/relogin-required-accounts
```

`GET /v0/management/plugins/codex-token-usage/relogin-required-accounts` returns only plugin-owned, confirmed 401, 402, and account-level 403 disables. It excludes timed 429 cooldowns, manual disables, and pending or failed synchronization. The response contains `generated_at`, `count`, and a stable `accounts` list with `auth_index`, `auth_id`, `name`, `state`, `http_status`, `reason`, `disabled_at`, and `version`. The route returns `409 unsupported_scheduling_mode` in legacy mode and is protected by the same CPA Management Bearer authentication as other plugin routes.

Confirmed automatic disables appear in `autobans`; unconfirmed writes and sync failures appear separately in `auth_controller.pending_accounts`. Each account shows its failure, disable time and either “重新登录后自动恢复” or a cooldown countdown. Healthy, enabled accounts leave the list.

Request records and native lifecycle events commit atomically before auxiliary accounting. Missing identities or temporarily unreadable auth snapshots keep events pending for retry; identity matching never falls back to email. Pending 429 isolation can be upgraded by a later 401, and queued failures are processed before timer recovery. Summary exposes sanitized `auth_controller.events` with disposition counts plus bounded `pending` and `recent` lists (up to 100 each). Dispositions distinguish pending identity/snapshot, pending disable, confirmed disable, stale observations and conflicts. Upgrades retain unfinished events and replay existing failures previously suppressed by erroneous metadata conflicts, except failures superseded by newer login/success and expired cooldowns. Migration is idempotent and preserves explicit manual disables; historical usage errors are not bulk converted into new events.

The old release/resolve routes remain registered. Native callers must supply exact `auth_index` and `version`; unversioned requests return `409 refresh_required` without changing auth. Legacy keeps its original route semantics.

Before uninstalling or downgrading to an old binary, review every plugin-owned disabled account and either explicitly recover it after verification or hand responsibility to an operator using Disable/Clear. Keep the database for audit. Replacing a DLL alone does not undo disabled auth files. Switching to legacy stops new native isolation while continuing previously owned quota recovery.

See [architecture audit](CODEX_ARCHITECTURE_AUDIT.md), [implementation report](CODEX_REFACTOR_REPORT.md), and [manual smoke test](MANUAL_SMOKE_TEST.md).

The auto-disable table and 401/402/429 cards use the same native lifecycle records, including accounts without usage in the selected window. Pending status writes, confirmed plugin disables and manual disables remain visible. The 401/429 card counts include confirmed disables; pending accounts and unresolved event evidence are shown separately. Recovery times and pending recovery are shown explicitly; recovered accounts are removed even if their last historical HTTP status was 429. Native card dialogs use versioned actions, including one-click Check and recover for eligible 429 accounts.

## One-shot quota-window activation

The dashboard action **Activate quota windows once** is independent of the periodic trigger and works while `quota_trigger_enabled` remains `false`:

1. Preview reads quota without model generation and lists every exact Codex auth record separately, including multiple seats with the same email.
2. The default decision requires an enabled, unexpired Codex credential with at least one explicitly reported quota window and every reported window completely fresh. An explicitly `null` window is absent and does not block another valid reported window; omitted presence, zero reported windows, contradictory values, positive usage/tokens, or a countdown shorter than the server-reported duration are not fresh eligibility.
3. Window names are opaque API slots, not duration promises: the UI shows each window's server-reported `limit_window_seconds`, `reset_after_seconds`, presence, usage, and reset time. The dashboard keeps a stable layout by plan: Plus/Pro/Team/K12 retain two window columns, while Free/Trial expose one window and mark the second column as not applicable. Missing or expired observations are shown as pending refresh; an empty slot is never rendered as a fabricated `0.0%` quota. For example, an account may report a seven-day `primary_window` and an explicitly null `secondary_window`.
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
$HOME/.cli-proxy-api/data/codex-token-usage/model_prices.cache
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
codex-token-usage_0.1.49_linux_amd64.zip
codex-token-usage_0.1.49_linux_arm64.zip
codex-token-usage_0.1.49_windows_amd64.zip
codex-token-usage_0.1.49_darwin_amd64.zip
codex-token-usage_0.1.49_darwin_arm64.zip
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
- Native `401`/`402`: isolation requires configured Management access; log in again for the same account to restore it automatically.
- Native `429`: ordinary rate limits and confirmed quota exhaustion both disable the auth through CPA. Unknown quota resets are queried without inventing a recovery time; ordinary 429 without retry metadata uses a 60-second local backoff.
- Provider not visible: confirm the endpoint still exists in CPA config and refresh the dashboard.
- Price missing: check `model_prices.cache` status in the summary JSON and the model price update error if present.

## License

MIT
