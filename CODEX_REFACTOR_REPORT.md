# codex-token-usage 0.1.44 重构报告

本次在现有 0.1.43 上增量实现原生调度与账号状态管理分离。插件名称、C ABI、SQLite、内嵌 Dashboard 和原有功能保留；未修改 CPA 生产源码。

## 实现结果

- `scheduling_mode` 默认 `native`，支持 `legacy`，非法值拒绝加载配置。Codex native 在访问旧过滤、reservation、选择器之前返回 `Handled:false`，不指定 DelegateBuiltin。xAI 和 legacy 继续使用已有 Scheduler capability。
- 新增独立分类器、身份/Management 客户端、持久化状态、恢复 Worker 与 Management 操作模块。Usage 和 Probe 进入统一分类；普通 429、模型/未知 403、5xx 和网络错误不做全局禁用。
- 明确 401、402、账号/Workspace 403 与额度耗尽 429 使用状态 PATCH。调用前持久化意图并检查唯一身份；调用后核对物理 JSON、运行时及非 disabled 字段的内容指纹。HTTP 200、单边成功和超时不直接等于完成。
- 七种业务状态与实际 disabled、插件所有权、暂停、同步状态分开保存。人工禁用不自动恢复；外部启用、凭证替换或不明修改暂停控制。迟到失败不能认领外部启用后的账号。
- Worker 启动立即 reconcile，失败事件唤醒，每 30 秒补充检查；可注入 Clock，操作串行，与额度触发/一次性激活共用 gate。重配置和退出等待旧 Worker 停止；失败指数退避最多五分钟。
- 已确认拥有的到期额度隔离自动解除。动态窗口保留原语义，支持 `resets_at` / `resets_in_seconds`；多个耗尽窗口取最后 reset，缺失 reset 使用只读额度复查。401 无定时恢复，402/权限恢复要求明确真实模型复查。
- Dashboard 显示账号状态、禁用所有权、恢复时间、原因及同步异常；native 明示仅保留 Token 统计/告警，不执行插件并发硬限制和 Token 降级。原 Usage、费用、Quota、导出、认证导入、一次性激活和 xAI 保留。

## 配置、数据库与接口

控制器只读取环境变量 `CPA_TOKEN_USAGE_MANAGEMENT_URL` 和 `CPA_TOKEN_USAGE_MANAGEMENT_KEY`。缺少配置时继续统计与原生调度，报告控制器未配置；不读取密钥哈希充当密码，不用 host.auth.save 切换状态。

增量新增 `auth_lifecycle_states`、`auth_lifecycle_operations`、`auth_lifecycle_events`、`auth_model_issues`、`auth_lifecycle_legacy_evidence`。事件重试字段也支持幂等补迁移。旧 Usage、价格、quota activation、xAI 及限制表均保留；旧禁用仅导入为历史证据，不产生所有权或启动禁用任务。native 隔离旧 Codex 历史回填和自动清理路径。状态审计 revision 参与 Summary 缓存失效。

新增 `POST /v0/management/plugins/codex-token-usage/auth-states/action`，沿用 CPA Management 鉴权；需要精确 `auth_index` 和当前 `version`，支持 recheck/enable/disable/clear。Recheck 不启用；Enable 对失效或冲突账号要求五分钟内成功复查；Disable 撤回自动恢复所有权；Clear 只清理管理责任并保留审计。旧 release/resolve 路由在 native 转交控制器，无版本请求返回 409。

凭证仅在请求内存中使用。状态数据只保存 SHA-256 指纹及固定/白名单错误内容，Management 密钥、Token、完整认证和原始响应不进入新增状态日志、Summary 或导出。

## 实际验证

本地使用 Windows amd64、Go 1.27.1、CGO_ENABLED=1 和便携 Zig 0.13 C 编译器。最初 CGO=0/缺少 GCC 的环境失败已解决；原项目完整测试基线通过。

| 检查 | 结果 |
|---|---|
| 最终 `go test ./...` | 通过，6.427s |
| 最终 race 全套测试 | 通过，6.755s |
| `go vet ./...` | 通过 |
| `go build -trimpath -buildmode=c-shared -o outputs/codex-token-usage.dll .` | 通过 |
| `python integration/run_cpa_native.py` | 通过 |
| `git diff --check` | 通过 |

Windows 的 Zig race 链接需要系统 SDK 的 `synchronization.lib`，实际命令为 `go test -race "-ldflags=-extldflags=<temporary synchronization.lib>" ./...`。未改变生产代码规避 race 检查。CI 保留常规 Linux test/race/vet，增加 shared build 和 Go 1.26 的 CPA 固定版本集成任务；现有多平台发布流程保留。本地只构建验证了 Windows DLL，未声称本地完成所有平台构建。

CPA 集成脚本下载 v7.2.145 到临时目录，注入测试文件，使用本插件实际协议响应驱动真实 Manager/Scheduler。验证高优先级 A/B 隔离后 C/D fallback、恢复，以及原生 round-robin/weighted/fill-first；同时运行上游 Weight、Priority、Session Affinity、excluded-models 与状态 PATCH 对应测试。未部署服务或使用真实凭证。

新增测试覆盖分类、动态/未知 reset、重启恢复、人工所有权、凭证变化、同邮箱隔离、字段保留、HTTP 200 仅运行时成功、超时后成功、外部启用后的迟到失败、版本冲突、复查与启用分离、模型排除、旧状态迁移/维护隔离和 Summary 缓存失效。Dashboard JavaScript 通过 Node 语法与转义渲染检查。原 legacy 并发、TTL、Token、Quota activation、价格、导入、xAI 等现有回归套件保留并通过。

构建产物：`outputs/codex-token-usage.dll`，17,506,304 字节。SHA-256：

```text
5E71CA9C13FC202F54C271562B020576492164F69DBE78A5E006F0E73B4A24AE
```

## 已知边界与运行交接

- 只自动写入可唯一识别的物理 Codex OAuth 认证。runtime-only、虚拟认证、丢失文件或重复身份停止自动写入；库存/物理快照暂时不可用时事件延后处理。
- CPA 没有 CAS 或操作者标记，无法彻底消除外部操作恰好发生在最终回读与 PATCH 之间的竞争。跨 SQLite/CPA 的崩溃窗口采用保守暂停，可能需要人工核对，不冒认禁用所有权。
- 自动解除插件禁用不保证所有模型可用。Token 自动刷新也可能触发保守身份冲突；需要 Recheck 后明确 Enable。真实模型复查只能验证所选模型。
- 新事件和审计表目前保留历史记录，尚未配置自动清理策略；长期高失败率部署需监测数据库大小。不要删除所有权记录来处理空间问题。
- 本次没有访问真实 CPA 服务。真实插件加载、Management 鉴权、实际文件/运行时同步和重启验证按 [MANUAL_SMOKE_TEST.md](MANUAL_SMOKE_TEST.md) 执行，无需耗尽额度或等待长窗口。
- 切换 legacy 后继续已有插件所有权的恢复责任；移除插件或降级旧 DLL 前必须核对并恢复或移交禁用账号。只替换 DLL 不构成完整回滚。

架构及上游源码依据见 [CODEX_ARCHITECTURE_AUDIT.md](CODEX_ARCHITECTURE_AUDIT.md)，部署和 API 用法见 [README.md](README.md)。
