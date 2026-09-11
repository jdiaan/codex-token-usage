# 真实 CPA 最小验证

自动测试使用 Mock 和 CPA v7.2.145 源码，不需要耗尽额度。

1. 停止 CPA，备份原数据目录（包含 `usage.db` 及存在的 WAL/SHM 文件），只替换动态库。重启后核对 Summary 的 `version=0.1.49` 和 `db_path`；默认应迁移到 `$HOME/.cli-proxy-api/data/codex-token-usage/usage.db`，旧库仍保留。设置过 `CPA_TOKEN_USAGE_DIR` 时应继续使用指定目录。
2. 配置 `CPA_TOKEN_USAGE_MANAGEMENT_URL`、`CPA_TOKEN_USAGE_MANAGEMENT_KEY`，确认 Dashboard 为 native、管理 API 已配置，原 Usage/Quota/xAI 页面正常；用量、自动禁用归属、人工禁用、恢复时间在升级后保持。
3. 在 CPA 管理页手动禁用一个测试账号，确认插件显示人工停用；重启后不会自动启用。在插件账号表点击“启用 (Enable)”，确认出现明确结果，CPA 文件与运行时均恢复，不需要 Recheck。
4. 对已有 401 测试账号重新登录，确认同账号凭据更新后自动恢复。若旧版已丢失凭据变化证据，逐账号点击“启用”，确认退出当前 401 待处理列表。启用仅恢复调度，不等于凭据已验证。
5. 在测试环境使用错误的管理密钥，确认启用失败明确提示 Management 401 和 management_key；修正后重试同步。分别验证账号表和管理弹窗中的按钮，以及过期页面的版本冲突提示。
6. 再次停止并启动 CPA，确认使用新库且新增用量未被旧库覆盖。缺失旧库的测试实例应显示“禁用来源未知”，不应自动批量启用。

不要为了验证而消耗真实额度。Quota 到期、401、402、403、429、崩溃窗口和重启恢复已由 Fake Clock/Mock 覆盖。

回滚：先逐一核对插件禁用记录；决定恢复的账号通过 Enable 处理，决定继续禁用的账号通过 CPA 人工停用接管。停止 CPA 并备份当前数据库，再更换二进制；设置 `CPA_TOKEN_USAGE_DIR` 指向当前的新数据目录，避免误读保留的旧库。不能直接删除数据库来解禁。
