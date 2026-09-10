# 真实 CPA 最小验证

自动测试使用 Mock 和 CPA v7.2.145 源码，不需要耗尽额度。

1. 在 CPA 进程环境配置 `CPA_TOKEN_USAGE_MANAGEMENT_URL`、`CPA_TOKEN_USAGE_MANAGEMENT_KEY`，载入新插件，确认 Dashboard 为 native、管理 API 已配置，原 Usage/Quota/xAI 页面正常。
2. 选一个允许暂时停用的测试账号，在新账号状态面板点击 Disable。确认 CPA 管理页及该认证 JSON 都是 disabled，其他健康账号仍可请求；重启 CPA 后再次核对。
3. 对该账号 Recheck，再显式 Enable，确认文件与运行时均恢复。手工禁用一个账号后重启，确认插件没有自动启用。

不要为了验证而消耗真实额度。Quota 到期、401、402、403、429、崩溃窗口和重启恢复已由 Fake Clock/Mock 覆盖。

回滚：优先设置 `scheduling_mode: legacy`。若必须更换旧二进制，先逐一核对插件禁用记录；决定恢复的账号通过 Recheck/Enable 处理，决定继续禁用的账号通过人工 Disable 接管。完成后再停插件，不能直接删除数据库来解禁。
