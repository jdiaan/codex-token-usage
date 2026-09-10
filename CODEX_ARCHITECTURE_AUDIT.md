# 架构审计与重构边界

审计对象：本仓库原 `0.1.43`，目标 `0.1.44`；宿主依据为 router-for-me/CLIProxyAPI **v7.2.145**。本次为增量改造，不更换插件 ID、SQLite、Dashboard 或构建形式。

## 原架构和 503 根因

`main.go` 实现 C ABI、插件注册、配置、Usage 入口、SQLite 初始化、后台维护、额度请求和 Scheduler；仓库没有独立的 `scheduler.go`。注册返回 `usage_plugin`、`management_api`、`scheduler` 三项能力。

原调用链：

```
CPA 过滤模型/运行时/优先级，构造 Candidates
  → scheduler.pick
  → store.pickAuth → pickAuthOnce
  → 读取 autoban_bans / invalid_auths，清理替换的认证
  → filterCodexSchedulerCandidates
  → pickProtectedAuth 或 pickSchedulerCandidateWithStrategy
  → AuthID / schedulerRejectError
```

旧 ban 只在插件 SQLite 中，CPA 不知道这些限制。插件拿到的子集可能只剩内部禁用账号，而其他健康账号处于不同优先级等候选范围。再次读取库存不能扩大当前 Candidates，最终报 `auth_unavailable` / omitted healthy accounts。`issue12_candidate_pool_test.go` 原测试实际上验证这种拒绝和诊断，并非修复候选池。

`scheduler_rotation.go` 模拟 round-robin/fill-first；`scheduler_affinity.go` 保存进程内会话绑定；`scheduler_state.go` 缓存是否需要查询限制。它们不能完整等价于 CPA 当前的权重、模型及重试语义。

## 原保护、统计与身份

- `account_protection.go` 在选择账号时串行检查 SQLite reservation，并写入预约；额度按 Free/Plus/K12/Team/Pro 区分。全部候选满载会返回 503。
- `usage.handle → recordUsage` 写 Usage 后按账号别名找到最早预约并删除。预约没有 request ID，依赖 TTL 清理丢失完成事件；不能把这一计数说成精确的原生在途统计。
- rolling Token window 基于 `usage_events` 聚合，超过软阈值时降低选择顺序；全部软超限仍可选。它不是上游 token bucket。
- 401/402 写入 `invalid_auths`；连续三次 403 也可能进入旧 invalid；429 写入 `autoban_bans`。旧 429 分类可能只因 reset header 存在就使用长窗口。
- Summary maintenance 会从历史 Usage 和 Probe 重建禁用，再按 reset 或成功请求清理。只换 Scheduler、却继续运行这些维护，会留下相互冲突的状态来源。
- Codex 库存优先使用 `host.auth.list`，并有文件发现/兼容回退；`codex_auth_source.go` 已处理精确索引、runtime-only、相同邮箱多席位及模糊别名拒绝。只读展示仍复用这些能力。
- `quota_activation.go` 已有精确账号校验、窗口存在性、动态时长、一次性确认和持久化 cycle reservation；这些语义保留。primary/secondary 是窗口槽位，不保证分别为 5h/weekly。
- Dashboard 使用 Go 内嵌 HTML/CSS/JavaScript；Summary、费用、模型价格、导出、额度激活、认证导入及 xAI 页面继续保留。

## CPA 接口核实

源码依据：

- [Host Auth API 实现](https://github.com/router-for-me/CLIProxyAPI/blob/v7.2.145/internal/pluginhost/auth_callbacks.go)
- [状态 PATCH](https://github.com/router-for-me/CLIProxyAPI/blob/v7.2.145/internal/api/handlers/management/auth_files_fields.go)
- [Manager.Update](https://github.com/router-for-me/CLIProxyAPI/blob/v7.2.145/sdk/cliproxy/auth/conductor_lifecycle.go)
- [Scheduler 分流](https://github.com/router-for-me/CLIProxyAPI/blob/v7.2.145/sdk/cliproxy/auth/conductor_selection.go)
- [插件类型/生命周期](https://github.com/router-for-me/CLIProxyAPI/blob/v7.2.145/sdk/pluginapi/types.go)

| 接口 | 实际语义与采用方式 |
|---|---|
| `host.auth.list` | 读取宿主库存；修改前验证唯一 auth_index |
| `host.auth.get` | `{auth_index}` → 物理认证完整 JSON；用于指纹和保存后核对 |
| `host.auth.get_runtime` | `{auth_index}` → 当前运行时条目；用于禁用确认 |
| `host.auth.save` | `{name,json}` 写完整文件并重建 Auth；重建时初始 StatusActive，未直接把 JSON disabled 映射到运行时 Disabled。不用于状态切换，原导入功能仍使用它 |
| `PATCH /v0/management/auth-files/status` | `{name,auth_index,disabled}`；查找现有 Auth，更新 Disabled/Status/Metadata，再调用 Manager.Update |

Manager.Update 会更新原生 Scheduler，但 `_ = m.persist(...)` 不把所有磁盘保存错误返回给客户端。因此必须同时核对文件与运行时，不能仅信任 HTTP 200。

状态 API 没有版本条件写入、操作者或禁用所有权标记。插件的 SQLite version 防止旧 Dashboard 操作，并不能替代宿主 CAS。对外部编辑/替换保守暂停；仍存在最后一次读与 PATCH 之间无法彻底消除的外部竞争。

CPA 有 request before/after 和异步 request.complete。它们的 Terminate 结束请求，不提供“这个账号满载，重新选其他账号”的 admission 契约。当前版本的 Home 并发配置由 Home 模式管理，不能冒充普通本地插件保护接口。

## 最终结构

```
Usage / Probe → 脱敏分类 → SQLite durable events
                            ↓
                    Auth Lifecycle Controller
                 精确身份 + 操作意图 + PATCH
                            ↓
                   文件 / Runtime 双重确认
                            ↓
                      CPA 原生 Scheduler
```

新增模块按分类、Host/HTTP 适配、状态存储、Controller、Management 分开。Controller 不选择账号，不把 JSON 上传回宿主，不恢复旧认证内容。

native 的 Codex 调度分支在 SQLite 前返回 `Handled:false`，不设置 DelegateBuiltin；Scheduler capability 仍服务 xAI 和 legacy。API 不可用时继续原生选择，Dashboard 明确显示未配置/同步异常。

native 保留 Token 窗口与告警，不执行插件并发硬限制或 Token 降级；legacy 保留原有保护。Session Affinity 由 CPA 的原生配置决定，插件不会修改 CPA 配置。

状态包括 HEALTHY、AUTH_INVALID、QUOTA_COOLDOWN、RATE_LIMITED、BILLING_BLOCKED、PERMISSION_BLOCKED、MANUAL_DISABLED；同步状态和所有权与业务状态分开。普通 429、模型/未知 403、5xx 不做全局禁用。错误 message 原文不持久化，仅保存白名单错误类型/代码及固定原因。

## 恢复、冲突与兼容限制

- 只有确认由插件禁用、认证指纹和运行时版本未变的额度账号可以自动到期恢复。
- 401 不按时间恢复。重新登录、文件改变、身份变化或外部操作有歧义时暂停；Recheck 不启用，随后显式 Enable。
- 402/账号级 403 的 Recheck 必须明确同意真实模型请求；只读额度成功不能证明模型可用。Probe model 可选择，并检查认证自身的 excluded_models / excluded-models。
- runtime-only、虚拟展开认证、缺少物理文件或身份不唯一的认证不做自动状态写入。
- token 刷新也会改变指纹；无法区分它与外部替换时保守暂停，不盲目保留所有权。
- PATCH 和 SQLite 不能做跨系统事务。重启发现未确认操作已经改变宿主时，不认领禁用，转人工核对；未执行且身份未变的操作可重试。
- Controller 与现有 Probe/一次性激活共享排他 gate。事件先持久化，gate 繁忙时稍后处理，CPA 原生 cooldown 处理即时失败窗口；不承诺已发出的请求被撤回。
- 切换 legacy 不新增 native 禁用，但继续已有插件所有权的恢复；尚未确认的 native 隔离取消并提示核对。
- 旧 release/resolve 的无版本请求返回 409，不能把旧“清 DB”权限悄悄升级成实际启用全部 Auth。携带 auth_index/version 的请求转入新 Controller。
- Summary 读取不修改 Auth，生命周期版本加入缓存 revision。旧表保留供 legacy 回滚；历史证据不转成所有权或待执行任务。

移除插件/降级旧二进制前需显式处理插件拥有的禁用账号，否则 CPA 中的 disabled 会保留。不存在“卸载自动撤销全部禁用”的安全承诺。
