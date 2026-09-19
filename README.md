# CPA Token Usage

CLIProxyAPI（CPA）的用量统计与账号管理插件，提供 Codex、xAI 账号看板及 AI 服务商用量分析。

当前版本：`0.1.49`。原生调度接口测试基于 CPA `7.2.145`。

注意：当前正在修改原项目，有很多原项目功能没有测试过可能会有各种问题
当前实现：
- 账号额度到达上限后，将相关账号通过CPA自动禁用，到解禁时期时自动解封
- 只在ubuntu上测试过代码

## 主要功能

- 按账号、服务商和模型统计请求数、Token、缓存命中率及估算费用，支持 CSV / JSON 导出。
- 展示 Codex 额度窗口、恢复时间、账号异常及疑似外部额度消耗。
- 自动禁用异常 Codex 账号，并在凭据更新或冷却结束后恢复。
- 支持定时额度触发、一次性启动额度窗口和非标准 Codex 凭据导入。
- 提供 xAI 账号看板、模型价格自动更新、中英文界面及深浅色主题。

## 安装与启用

1. 下载与系统、架构对应的发布压缩包。
2. 停止 CPA，将动态库放入对应插件目录。例如：

   ```text
   plugins/linux/amd64/codex-token-usage.so
   plugins/windows/amd64/codex-token-usage.dll
   plugins/darwin/arm64/codex-token-usage.dylib
   ```

3. 在 CPA 配置中启用插件：

   ```yaml
   plugins:
     enabled: true
     configs:
       codex-token-usage:
         enabled: true
         priority: 120
         scheduling_mode: native
   ```

4. 如需自动启停账号，按下文配置服务器私密文件。
5. 启动 CPA，在管理页面打开 Token Usage，确认插件版本、数据目录和账号控制状态。

## 账号控制的私密配置

**后台账号控制只从服务器本地文件读取管理地址和密钥。** 普通插件配置中的 `management_url`、`management_key`，以及旧环境变量 `CPA_TOKEN_USAGE_MANAGEMENT_URL`、`CPA_TOKEN_USAGE_MANAGEMENT_KEY` 均已停用。

未配置或配置无效时，自动启停暂停，统计与原生调度继续工作。此改动只涉及后台凭据；CPA 登录和网页手动操作仍沿用原有认证方式。

### 文件位置与内容

默认位置如下，`$HOME` 指运行 CPA 的用户主目录：

```text
$HOME/.cli-proxy-api/secrets/codex-token-usage-management.yaml
```

也可在 CPA 进程环境中指定其他**绝对路径**：

```text
CPA_TOKEN_USAGE_MANAGEMENT_CONFIG_FILE=/etc/cpa/secrets/codex-token-usage-management.yaml
```

参考[配置示例](codex-token-usage-management.example.yaml)，在服务器上创建文件：

```yaml
management_url: "http://127.0.0.1:8317"
management_key: "替换为实际管理密钥"
```

- 地址填写 CPA 根地址。同机访问使用回环地址，跨机器建议使用 HTTPS；客户端不跟随重定向。
- 密钥必须是可用的原始密钥，不能填写密码哈希。
- 两项均为非空字符串；建议加引号。文件不超过 64 KiB，重复字段、未知字段、非字符串值和多份 YAML 文档均会被拒绝。
- 文件在进程首次初始化插件时读取一次，读取失败也会缓存。**创建、修改、修复文件或更换路径后，必须重启 CPA。** 网页保存设置和插件重新配置不会重新读取。
- 插件不自动创建、迁移或修改私密文件，也不回退到旧配置。

页面显示“未配置”“配置无效”或“已配置”。“已配置”只表示本地配置有效，连接和认证是否成功以实际请求结果为准。状态接口不返回私密路径、文件原文或密钥。

### 文件权限

Linux/macOS 推荐管理员持有文件，服务账号通过所属组只读，文件权限为 `0640`，父目录不允许服务账号写入。`0600`、`0400` 也可使用，但应注意文件所有者的修改权限。其他用户有任何权限，或所属组有写入、执行权限时，插件会拒绝加载。

**通过 SFTP 上传或编辑器新建的文件可能是 `0644`，会触发“私密配置文件权限过宽”。** 此时整个文件未被加载，即使已经填写密钥，页面也可能同时提示缺少 `management_url / management_key`，应先修复权限。

如果文件属于运行 CPA 的用户，请以该用户登录服务器后执行；使用自定义路径时替换下面的文件路径：

```sh
chmod 600 "$HOME/.cli-proxy-api/secrets/codex-token-usage-management.yaml"
ls -l "$HOME/.cli-proxy-api/secrets/codex-token-usage-management.yaml"
```

权限应显示为 `-rw-------`，所有者应为运行 CPA 的用户。**修改权限后必须重启 CPA 进程**，仅刷新网页或保存插件设置无效，因为首次读取失败的结果也会缓存。若文件所有者与 CPA 运行用户不同，应按下面的管理员持有、服务组只读方式设置，避免改为 `0600` 后服务无法读取。

下面以服务组 `cpa` 为例，请替换为实际组名：

```sh
sudo install -d -o root -g cpa -m 0750 /etc/cpa/secrets
sudo install -o root -g cpa -m 0640 codex-token-usage-management.example.yaml /etc/cpa/secrets/codex-token-usage-management.yaml
sudoedit /etc/cpa/secrets/codex-token-usage-management.yaml
```

然后在服务环境中设置 `CPA_TOKEN_USAGE_MANAGEMENT_CONFIG_FILE` 并重启 CPA。

Windows 可将文件放在 `C:\ProgramData\CPA\secrets\codex-token-usage-management.yaml`，并在服务启动环境中指定此绝对路径。通过 NTFS 权限关闭不必要的继承，仅允许管理员修改，CPA 服务账号读取文件及访问父目录。**当前版本不自动检查 Windows ACL。**

私密文件应放在网页目录、账号认证目录、代码仓库和普通配置导出范围之外。文件权限不能防止已取得服务进程或管理员权限的人读取密钥。

### Docker 部署

先在宿主机创建私密文件，再为现有 CPA 服务增加只读挂载：

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

确保容器内服务的 UID/GID 有读取权限。容器中的 `127.0.0.1` 指容器自身；连接其他容器时应使用对应服务地址。替换宿主机挂载文件后，重新创建容器，使其读取新文件。

## 调度与账号状态

| 模式 | 行为 |
| --- | --- |
| `native`（默认） | Codex 调度交给 CPA，插件负责统计、告警和账号状态管理。优先级、权重、回退、模型排除及会话亲和在 CPA 中配置。 |
| `legacy` | 使用旧版插件调度，可启用并发限制、Token 软降级和会话亲和。 |

`native` 不执行插件的并发硬限制和 Token 软降级。旧会话亲和配置仍适用于 `legacy` Codex 和现有 xAI 调度。

### 原生模式的自动启停

| 情况 | 处理方式 |
| --- | --- |
| 401、402、明确的账号或工作区级 403 | 自动禁用；检测到同一账号凭据更新后恢复。 |
| 429、明确的额度耗尽 | 禁用并按服务端恢复时间冷却；多个耗尽窗口取最晚时间，无有效时间时冷却 60 秒。 |
| 模型权限问题、无法分类的 403、网络错误或 5xx | 不据此禁用整个账号，由 CPA 继续处理冷却或重试。 |
| 人工禁用 | 保持人工管理，不自动启用。 |
| 已禁用但缺少插件历史记录 | 显示“禁用来源未知”，需人工确认后启用。 |

插件通过 CPA 管理接口修改启停状态，并核对账号文件和运行时结果。写入或核对失败会重试；未确认的操作显示为待同步，不计作已完成禁用。

网页提供“启用”和“重试同步”。启用后恢复调度，但不代表凭据已验证；后续请求仍可能触发禁用。普通浏览、账号状态轮询和重试同步不会发送模型探测请求，也不会主动查询上游额度。

自动恢复只处理插件负责的禁用。切换到 `legacy` 后不再新增原生禁用，但会继续处理此前由插件负责的额度冷却恢复。

## 常用设置

以下选项写在 CPA 配置的 `plugins.configs.codex-token-usage` 下，也可在插件设置页面修改。示例为默认值；未填写的选项使用默认值。

```yaml
开启定时额度触发（不建议账号多的情况下开启）: false
触发间隔分钟: 10
触发模式: probe
最大并发账号数: 1
单账号超时秒数: 20
单账号最小冷却分钟: 10

自动更新模型价格表: true
模型价格更新间隔小时: 6
模型价格更新超时秒数: 20

用量保留天数: 90
额度触发记录保留天数: 30
请求明细保留天数: 30
```

模型价格默认每 6 小时从 [LiteLLM 价格表](https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json)更新，也可通过“模型价格表地址”指定来源。插件内置少量回退价格；页面费用为估算值。

### 旧版账号保护

以下设置用于 `legacy` Codex 调度，默认关闭账号保护：

```yaml
同一个Session优先固定到同一个账号: true
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

普通设置仍支持原有英文键名，例如 `quota_trigger_enabled`、`model_price_auto_update_enabled`、`account_protection_enabled`。

### 额度触发与一次性启动

定时额度触发默认关闭。开启后会定期发送少量真实 Codex 请求，可能消耗额度，账号较多时不建议开启。

“一次性启动额度窗口”与定时触发独立：先读取额度并预览，确认后才向所选账号发送真实请求。同一已识别周期会防止重复发送；结果不明确或只验证了部分窗口时，不自动重发。默认只接受已启用、凭据有效、已上报窗口均为全新状态的账号。强制模式仅放宽窗口新鲜度条件，不绕过账号、身份和额度读取检查。

额度窗口时长以服务端上报为准，不固定为 5 小时或 7 天；未上报或过期的数据不视为剩余额度充足。一次性启动不保证只消耗一个 Token，停止功能也无法撤回已发出的请求。

## 数据目录与升级

默认数据目录为运行 CPA 用户的：

```text
$HOME/.cli-proxy-api/data/codex-token-usage/
├── usage.db
└── model_prices.cache
```

`usage.db` 保存用量和账号管理状态；价格缓存使用 JSON 内容，但以 `.cache` 为扩展名，避免被 CPA 误识别为账号认证文件。

| 环境变量 | 用途 |
| --- | --- |
| `CPA_TOKEN_USAGE_DIR` | 指定持久化数据目录，建议使用绝对路径。 |
| `CPA_MODEL_PRICE_FILE` | 单独指定模型价格文件位置。 |
| `CPA_TOKEN_USAGE_MANAGEMENT_CONFIG_FILE` | 指定私密配置文件的绝对路径。 |
| `CPA_CONFIG_PATH` | 指定插件读取的 CPA 主配置位置；兼容旧名称 `CPA_CONFIG_FILE`。 |

CPA 主配置查找顺序：`CPA_CONFIG_PATH` → `CPA_CONFIG_FILE` → 进程 `-config` / `--config` 参数 → 工作目录中的 `config.yaml` → `$HOME/.cli-proxy-api/config.yaml` → `$HOME/config.yaml`。未找到已有文件时，默认使用 `$HOME/.cli-proxy-api/config.yaml`。

### 升级步骤

1. 准备私密配置文件及读取权限；旧网页配置和旧凭据环境变量不会自动迁移。
2. 停止 CPA，备份 `usage.db` 及存在的 `usage.db-wal`、`usage.db-shm` 文件。
3. 替换动态库并启动 CPA，核对汇总接口的 `version`、`db_path`，确认账号控制配置及实际请求结果。
4. 删除 CPA 普通配置中的旧管理地址、密钥和旧凭据环境变量；检查历史明文备份，有泄露疑虑时轮换密钥。

从旧版升级时，插件会将默认旧目录 `$HOME/.cli-proxy-api/plugins/codex-token-usage` 中的数据库和价格缓存迁移到新数据目录，并保留旧文件。已有目标数据库不会被覆盖或合并；指定了 `CPA_TOKEN_USAGE_DIR` 时不自动迁移目录。迁移失败会阻止数据库初始化，文件系统需支持硬链接。

容器或服务账号变更时，须保留原数据卷或指定持久化目录。卸载、回滚前逐一核对插件禁用的账号，决定恢复或交由人工管理。**更换动态库不会自动解除禁用，不要通过删除数据库恢复账号。** 回滚时让 `CPA_TOKEN_USAGE_DIR` 指向当前数据目录，避免重新使用旧副本。

## 管理接口

所有接口均使用 CPA 管理认证，路径前缀为 `/v0/management/plugins/codex-token-usage`。

| 方法与路径 | 用途 |
| --- | --- |
| `GET /summary` | 用量、账号状态和运行诊断。 |
| `GET /export` | 导出账号、服务商、模型或请求统计。 |
| `GET /relogin-required-accounts` | 查询已确认由插件禁用、等待重新登录的 401、402 和账号级 403 账号。 |
| `POST /auth-states/action` | 执行启用或重试同步等账号操作。 |
| `POST /quota-activation/preview` | 创建额度启动预览。 |
| `GET /quota-activation/preview?id=...` | 查询预览结果。 |
| `POST /quota-activation/run` | 提交已确认的额度启动请求。 |
| `GET /quota-activation/run?id=...` | 查询执行结果。 |

账号操作示例：

```json
{"auth_index":"目标账号索引","version":3,"action":"retry_sync"}
```

`version` 必须取自最新汇总结果；版本过期返回 HTTP 409。`enable` 表示启用，`retry_sync` 表示重试同步。旧接口在原生模式下同样要求准确的账号索引与版本。

待重新登录接口返回 `generated_at`、`count` 和 `accounts`，排除人工禁用、429 冷却及未确认同步的账号；`legacy` 模式返回 `409 unsupported_scheduling_mode`。

额度启动需先获取预览及一次性确认令牌，再提交预览内允许启动的账号。账号操作、导出和诊断不会返回账号访问令牌、刷新令牌或管理密钥；本地告警仅出现在汇总或导出结果中，不发送外部通知。

## 常见问题

| 问题 | 检查方法 |
| --- | --- |
| 插件未注册或未生效 | 检查动态库的系统、架构和目录，重启 CPA。 |
| 自动启停显示未配置或配置无效 | 检查私密文件路径、字段和权限；修复后重启 CPA。 |
| 已填写密钥，仍提示“私密配置文件权限过宽”及缺少字段 | 文件因权限过宽被拒绝读取，常见原因是上传后权限为 `0644`。按[文件权限](#文件权限)修复，并重启 CPA 进程。 |
| 管理接口返回 401 | 检查对应请求使用的 CPA 管理密钥。后台控制报错时修改私密文件并重启；这与上游账号凭据的 401 不同。 |
| 账号重新登录后仍禁用 | 检查是否同一账号、凭据是否更新及状态是否同步；缺少历史记录时可人工确认后启用。 |
| 429 后何时恢复 | 有有效恢复时间时按时间恢复，否则冷却 60 秒；同步失败会显示原因并重试。 |
| 列表显示“已启用”后又被禁用 | 启用仅恢复调度，新的账号请求失败仍会触发禁用。 |
| 服务商未显示或模型缺少价格 | 检查 CPA 中对应服务商配置、模型价格更新状态和缓存文件。 |

## 构建与验证

需要 Go 1.21 或更新版本、支持 CGO 的 C 编译器；前端测试需要 Node.js。构建与打包脚本使用 Bash，Windows 可使用相应终端环境。

```sh
export CGO_ENABLED=1
go test ./...
go test -race ./...
go vet ./...
node --test integration/dashboard_lifecycle.test.cjs
bash ./build.sh
bash ./package-release.sh dist
```

CPA 原生接口兼容性测试需要 Python 3、Go 1.26 或更新版本及网络连接，测试固定版本 CPA `7.2.145`：

```sh
python3 integration/run_cpa_native.py
```

发布包按 `codex-token-usage_版本_系统_架构.zip` 命名，支持 Linux amd64/arm64、Windows amd64、macOS amd64/arm64，并生成 `checksums.txt`。压缩包仅包含动态库，不包含私密配置、数据库或下载的价格表。

真实环境验证见[手动验证说明](MANUAL_SMOKE_TEST.md)。[架构审查](CODEX_ARCHITECTURE_AUDIT.md)和[重构报告](CODEX_REFACTOR_REPORT.md)记录历史设计，当前行为以代码和本说明为准。

## 许可证

[MIT 许可证](LICENSE)
