# Codex Quota Router

一个只监听本机的 Codex 多账号 Key 池路由器。Codex 始终连接 `127.0.0.1:4000`，账号、排序、分配策略、余额和运行状态通过 Web 页面管理。

## 功能

- 添加任意多个 OpenAI Responses 兼容账号，每个账号独立配置名称、Base URL、Key 和启用状态。
- 用“上移 / 下移”调整账号顺序；数组顺序就是优先级顺序。
- 支持按顺序、轮询、最少使用、余额最多和余额最少五种策略。
- 自动区分额度耗尽、Key 无效、账号受限、普通限流和瞬时网络故障，并只影响对应账号。
- 在每个账号下显示上游声明的支持模型，并可使用账号弹窗中的当前填写内容刷新。
- 自动读取 New API Token 额度，并可使用用户名密码或用户 Access Token 核对账号钱包余额，显示实际可用额度；保存配置后立即查询，管理页可见时约每 60 秒刷新已启用账号，也可手动立即刷新。
- Windows 使用 DPAPI 加密配置；Linux 和 macOS 使用 `0700` 目录和 `0600` 配置文件。
- 记录脱敏运行日志，保留启动、路由、切换、余额探测和错误分类；不记录请求/响应正文或任何凭据。
- Windows 和 Linux 运行时显示系统托盘图标，macOS 显示菜单栏图标，可打开管理页或退出服务。

## 路由策略

| 策略 | 行为 |
| --- | --- |
| 按优先级顺序 | 始终选择列表中第一个可用账号；它耗尽、不可用或冷却时再选择下一项。 |
| 轮询使用 | 在可用账号间依次轮换，例如 A → B → C → A。 |
| 最少使用优先 | 从当前可用账号中选择本次程序启动以来已分配请求数最少的账号，连续分配 30 次后重新比较；批次内账号不可用时立即切换。计数重启后重新开始。 |
| 余额最多优先 | 选择已知实际可用额度最高的账号；只有核对后仍为无限的账号才按无限处理，余额相同则选择已分配请求较少的账号。 |
| 余额最少优先 | 优先选择已知实际可用额度最低的有限额度账号；核对后仍为无限的账号排在有限额度之后；余额相同则保持列表顺序。 |

两种余额策略都不会把未知余额当作 0。只有全部当前可用账号的实际余额都已验证、新鲜、可读取且单位一致时才会直接比较；瞬时网络、限流或超时失败会在最近一次成功余额仍处于 5 分钟有效期时继续使用该值并显示刷新警告。账号认证失败、只有 Token 额度、余额缺失或过期、没有可用的成功值、单位不同，都会明确回退为“最少使用优先”，管理页会显示原因。

任何策略都可能在额度耗尽、认证失败或限流时故障切换账号；轮询、最少使用和余额策略还会主动切换。Responses API 的 [`previous_response_id`](https://developers.openai.com/api/docs/guides/conversation-state#passing-context-from-the-previous-response) 通常不能跨不共享响应存储的账号继续使用；遇到此问题时应新建 Codex 任务，并避免依赖跨账号延续的响应 ID。

## 自动余额探测

New API 的 Token 标记为“无限”只表示该 API Key 没有单独的 Token 上限，不代表所属用户账号的钱包无限。路由器按以下方式读取并归一化余额：

1. 尝试 New API 的 `GET /api/usage/token/`，使用账号 API Key 读取该 Token 的剩余额度或“无限”标记。
2. 若配置了实际余额认证，再读取 New API 用户账号的 `quota`。有限 Token 的实际可用额度为 `min(Token 剩余额度, 账号 quota)`；无限 Token 的实际可用额度为账号 `quota`。例如 Token 为无限、账号只剩 5 USD，页面会显示 `5 USD（账号余额限制）`。
3. 未配置账号认证时，只展示 Token 额度并明确标记“账号余额未验证”；已配置但认证失败时标记为认证错误，提示修正凭据，不会把未核对的 Token“无限”冒充为真实无限。
4. 若 New API 路由不存在或成功响应不符合余额格式，尝试 OpenAI 兼容的 `GET {base_url}/dashboard/billing/subscription` 和 `usage`。
5. 所有方式都不支持时，将余额标记为“不支持”，不会伪造估算值。

管理页账号弹窗中的“New API 实际余额认证（可选）”支持：

| 方式 | 用途与限制 |
| --- | --- |
| API Key（不登录用户账号） | 不增加额外凭据；查询 Token 额度及上游可能提供的兼容账单接口，但仅凭 New API Token 接口不能证明用户账号余额。 |
| 用户名 + 密码 | 登录对应 New API 站点并读取用户余额。需要 Turnstile、验证码、两步验证或其他交互式校验的站点可能无法自动登录。 |
| 用户 Access Token + 用户 ID | 使用上游生成的用户 Access Token 和正整数用户 ID 读取用户余额；遇到交互式登录限制时优先使用此方式。 |

Passkey 不能直接在本机管理页中完成：WebAuthn 凭据绑定上游站点的 RP ID 和 Origin，`127.0.0.1` 页面不能冒充上游域名。请先在上游站点使用 Passkey 登录，再生成用户 Access Token，并选择“用户 Access Token + 用户 ID”。页面不会提供无法工作的伪 Passkey 按钮。

用户名、密码和用户 Access Token 都只会发往该账号配置的上游。认证秘密与 API Key 一样不会由配置接口回显：认证方式和用户名或用户 ID 未变化时，输入框留空会保留原值；填写新值会替换，勾选清除会删除。切换并保存为“API Key（不登录用户账号）”也会自动清除已保存的用户名、用户 ID 和余额认证秘密。Windows 使用 DPAPI 加密；Linux 和 macOS 依赖 `0700` 配置目录和 `0600` 配置文件。用户名密码登录产生的 Cookie 按账号隔离缓存，避免不同上游账号混用会话。

账号弹窗提供两个互不依赖的查询：“查询当前填写余额”和“刷新支持模型”都直接使用弹窗中的候选配置，不会保存配置。全局“刷新已保存配置余额”只使用服务器已经保存的配置。账号存在未保存编辑时，列表会继续显示服务器旧配置的余额并明确标注，避免把旧状态误认为当前输入的查询结果。

支持模型来自对应上游的 `/v1/models`，页面会在加载已保存配置后和保存配置后自动查询。该列表只代表当前 API Key 能从上游看到的声明模型，不保证每个模型一定可调用，也不是实际推理连通性测试。

当前实际余额只计算 Token 额度与用户 `quota`，暂不计入部分 New API 新版本单独维护的订阅套餐、赠送包或其他额度池。如果上游把可用额度拆分到这些独立池中，页面结果可能低于平台页面展示值。

当前两个网关已只读验证：

| 上游 | 探测方式 |
| --- | --- |
| `http://122.227.250.174:8000/v1` | 根路径 `/api/*` 被另一套 FastAPI 占用，因此使用 `/v1/dashboard/billing/subscription` 和 `/usage`，剩余额度按 `hard_limit_usd - total_usage / 100` 计算。 |
| `https://newapi.shixian.me/v1` | 使用 `/api/usage/token/` 的 `total_available`，并通过 `/api/status` 的 `quota_per_unit` 转换为站点显示额度；配置实际余额认证后再用用户 `quota` 限制可用值。 |

余额策略使用 5 分钟新鲜度判断；管理页打开且可见时，会在加载后立即刷新，并约每 60 秒刷新一次已启用账号。多个页面同时打开时，后端会合并一分钟内的重复轮询；保存后的刷新和“刷新已保存配置余额”会忽略缓存立即探测。可重试的网络、超时或暂时上游错误只针对失败账号在 5 秒、20 秒后各重试一次，再恢复 60 秒轮询；认证失败和不支持不会快速重试。保存刷新若遇到正在进行的自动刷新，会等待该请求完成后再执行，不会被静默丢弃。

刷新结果按账号显示安全的失败阶段和稳定错误类型，例如 Token 额度接口、用户登录、用户余额接口、额度换算、超时或限流；不会把上游响应原文、API Key、密码或用户 Access Token 放进页面错误信息。每个账号的单次探测最多等待约 20 秒，并限制为最多 8 个账号并发探测。所有余额请求只发往该账号配置的上游。

这是轮询得到的近实时余额，不是上游主动推送；最终显示速度仍取决于上游账单接口自身的更新延迟。

不同 New API 站点可能把显示额度配置为 USD、CNY 或 Token 数量。程序只比较成功归一化且单位文本一致的余额；USD、CNY、Token 或未知单位不会互相换算，而会安全回退到“最少使用优先”。

## 失败与切换行为

- HTTP 402 或响应内容明确表示额度、余额耗尽：持久阻止该账号，并尝试下一个账号。
- 响应内容明确表示账号已停用、封禁或受限：持久阻止该账号，并尝试下一个账号；普通 403 不会仅凭状态码停用整个账号。
- 已经成功返回过 2xx 或测试成功的账号，后续返回 401：持久标记为不可用，并尝试下一个账号。
- 从未验证成功的账号首次返回 401：冷却 1 分钟；5 分钟内再次返回 401 才持久标记为 Key 无效，避免偶发认证异常误停用。
- 普通 HTTP 429：优先使用上游 `Retry-After` 指定的时间，缺失或无效时冷却 1 分钟，最长冷却 24 小时；429 不累计网络失败次数。
- 充值、更换 Key 或确认恢复后，可点击该账号的“验证并恢复账号”；服务端会重新查询该账号的 `/v1/models`，取上游返回的第一个模型发送 Responses 请求。只有真实模型请求成功才会清除额度耗尽或 Key 无效等阻止状态；模型查询失败或列表为空时保持原状态，真实验证失败则按认证、额度或普通失败更新阻止或冷却状态。因连续请求失败而暂时停用的账号还可点击“立即恢复账号”，直接清除临时停用和失败计数，不发送测试请求。
- 明确的 DNS、连接建立失败会在 3 秒总预算内最多尝试 5 次，全部失败后才累计一次账号失败，并可继续尝试下一个账号。POST 超时、断连或收到真实 HTTP 5xx 时可能已经被上游执行，不会自动重放；结果未知的传输错误返回 `upstream_result_unknown`，避免重复文本、工具调用或扣费。GET/HEAD 仍可在网络错误或 HTTP 5xx 后尝试下一个账号。
- 网络或 HTTP 5xx 依次冷却 10 秒、30 秒、5 分钟、15 分钟、30 分钟，第三次起在管理页显示“账号请求失败过多，暂时停用”。收到有效非 5xx 响应、成功请求或用户立即恢复后清零网络失败计数；冷却结束后连续 5 分钟没有再次失败会重新按首次失败处理。同一冷却周期内仍在途的并发请求不会重复增加失败次数。冷却到期后的半开探测同一账号只允许一个请求；若全部候选都只因网络失败冷却，路由器最长约 30 秒进行一次受控探测，其他请求等待探测结果，不直接返回“没有可路由账号”。
- 上游已经开始输出 SSE 流后，路由器绝不切换账号重放；管理页生成的 Codex provider 配置也将普通请求和流式重试次数设为 0。
- 等待上游响应头最多 1 分钟；响应体连续 2 分钟没有新数据会终止转发并冷却该账号，已开始的响应不会切换账号重放。
- 除明确尚未发送的 DNS、连接建立失败外，同一个请求不会对同一账号重放。永久阻止、手动停用、配置不完整、429 或认证冷却等硬不可用状态不会被半开探测绕过；若没有任何硬可用或网络软冷却账号，返回 HTTP 503。安全故障切换后若没有更多候选，HTTP 错误会透传最后一个上游响应，确定未发送的连接错误返回 HTTP 502。
- 单个代理请求体上限为 128 MiB，超出时返回 HTTP 413。

## 安装与管理

源码支持 Windows 和 Linux 的 amd64 / x86-64 设备，以及 macOS 的 Intel（amd64）和 Apple 芯片（arm64）设备。仓库当前 `dist` 目录仍只有 Windows 和 Linux 发布包；macOS 包必须在对应的 Darwin 环境构建和验证。

Windows 发布包包含：

```text
codex-quota-router.exe
router.bat
README.md
THIRD_PARTY_NOTICES.md
```

双击 `router.bat` 等同于安装，也可在命令提示符中运行：

```bat
router.bat install
router.bat start
router.bat stop
router.bat status
router.bat uninstall
router.bat purge
```

Linux 发布包包含：

```text
codex-quota-router
router.sh
README.md
THIRD_PARTY_NOTICES.md
```

```sh
sh router.sh install
sh router.sh start
sh router.sh stop
sh router.sh status
sh router.sh uninstall
sh router.sh purge
```

Linux 的安装、启动和健康检查需要系统已有 `curl` 或 `wget`，并需要可用的 `systemd --user` 会话。脚本不会安装系统软件包，也不会自动开启 linger。

macOS 打包时应包含：

```text
codex-quota-router
router.sh
README.md
THIRD_PARTY_NOTICES.md
```

Intel Mac 使用 `darwin-amd64` 包，Apple 芯片 Mac 使用 `darwin-arm64` 包。两种架构的管理命令相同：

```sh
sh router.sh install
sh router.sh start
sh router.sh stop
sh router.sh status
sh router.sh uninstall
sh router.sh purge
```

macOS 脚本会在 `~/Applications` 创建无 Dock 图标的 `.app`，并使用当前用户的 `launchd` 登录项。macOS 构建需要 `CGO_ENABLED=1`、Xcode Command Line Tools 和 Cocoa SDK。当前脚本适合本地未签名安装；正式分发应预先构建完整 `.app`，再完成签名和公证，而不是在目标机器上现场组装。

启动后点击托盘或菜单栏图标会显示“打开管理页”和“退出”。选择“退出”只结束本次运行并保留登录自启动配置，可通过 `router.bat start`、`router.sh start` 或下次登录重新启动。Linux 需要 Session D-Bus 和支持 StatusNotifier/AppIndicator 的桌面环境；条件不满足时程序仍会以纯后台服务运行。

程序固定监听 `127.0.0.1:4000`，安装或直接启动前必须确保该端口未被其他程序占用。安装后的健康检查失败时，脚本会取消本次注册的自动启动；手动 `start` 失败时会停止刚启动的服务或计划任务，避免后台持续重启。

命令含义：

- `install`：复制二进制、注册当前用户登录后自启动、立即启动并打开管理页。
- `start`：启动已安装服务并打开管理页。
- `stop`：停止服务。
- `status`：检查任务或服务是否正在运行，并验证 `http://127.0.0.1:4000/healthz` 的服务身份。
- `uninstall`：删除自启动项和程序，保留账号配置。
- `purge`：在卸载基础上删除默认配置目录。

默认位置：

| 平台 | 程序 | 配置 | 自启动 |
| --- | --- | --- | --- |
| Windows | `%LOCALAPPDATA%\CodexQuotaRouter\bin` | `%APPDATA%\codex-quota-router\config.dat` | 当前用户计划任务 |
| Linux | `~/.local/lib/codex-quota-router` | `${XDG_CONFIG_HOME:-~/.config}/codex-quota-router/config.dat` | `systemd --user` |
| macOS | `~/Applications/Codex Quota Router.app` | `~/Library/Application Support/codex-quota-router/config.dat` | `~/Library/LaunchAgents/com.codex-quota-router.plist` |

## 运行日志

程序使用 Go 标准库写入逐行文本日志，不依赖被安装脚本隐藏的标准输出或错误输出。默认位置：

| 平台 | 当前日志 | 旧日志 |
| --- | --- | --- |
| Windows | `%APPDATA%\codex-quota-router\router.log` | `%APPDATA%\codex-quota-router\router.log.1` |
| Linux | `$XDG_RUNTIME_DIR/codex-quota-router/router.log`；未设置时 `${TMPDIR:-/tmp}/codex-quota-router-<配置引用>/router.log` | 同目录的 `router.log.1` |
| macOS | `${TMPDIR:-/tmp}/codex-quota-router-<配置引用>/router.log` | 同目录的 `router.log.1` |

Windows 使用自定义 `CODEX_QUOTA_ROUTER_CONFIG` 时，日志跟随自定义配置文件放在同一目录。Linux 优先使用每用户的 `XDG_RUNTIME_DIR`；该变量不可用时使用 `os.TempDir()`，macOS 也使用 `os.TempDir()`。Linux fallback 与 macOS 路径都会在项目目录名后加入配置路径的稳定哈希引用，避免多用户共用临时目录时互相冲突。macOS 源码与安装脚本已适配，但仓库当前 `dist` 目录尚未生成 macOS 发布包。

正常启动后的日志写入若让 `router.log` 达到 10 MiB，会用它替换 `router.log.1`，只保留一份旧日志。若轮转暂时失败，程序会保留原日志、写入固定告警并定时重试；当前日志达到 20 MiB 后会写入暂停告警并暂缓追加，轮转恢复后自动继续。端口绑定失败只会安全追加一条固定分类，并受 10 MiB 上限限制，不会轮转正在运行实例的日志。支持 Unix 权限的平台上，日志目录设为 `0700`、日志文件设为 `0600`。临时目录可能被系统在重启或清理时删除，服务进程使用的 `XDG_RUNTIME_DIR` 或 `TMPDIR` 也可能与交互终端不同。Linux 和 macOS 的 `purge` 删除配置但保留临时日志，日志之后由系统清理，也可手动删除对应项目日志目录。

日志只记录版本、平台、请求 ID、路由类别、请求和响应字节数、查询参数是否存在、哈希后的账号与上游引用、策略、HTTP 状态、耗时、流式终止状态和固定错误分类。以下内容不会写入日志：

- 请求、响应、SSE 数据和上游错误正文。
- URL 查询参数、完整 URL、Header、Cookie 和 Trailer 值。
- API Key、网关 Token、CSRF Token、密码、Access Token 和登录会话。
- 原始账号 ID、账号名称、用户名、用户 ID 及具体余额金额。
- 原始网络或文件错误文本。

这里的“开机自启动”准确含义是当前用户登录后启动，不是登录前运行的系统服务。安装成功后访问 [http://127.0.0.1:4000/](http://127.0.0.1:4000/)。

旧版双渠道配置会在首次启动时自动迁移为账号列表：通常保持“主渠道、备用渠道”的顺序；旧配置曾强制备用时，迁移后会把备用账号排在前面并停用主账号，保持原先“只用备用”的语义。

## Codex 配置

管理页不会展示配置内容，点击“复制配置”会直接写入剪贴板。它必须写入用户级 `~/.codex/config.toml`；Windows 对应 `%USERPROFILE%\.codex\config.toml`。[Codex 官方配置参考](https://developers.openai.com/codex/config-reference/)说明，项目内 `.codex/config.toml` 会忽略 `model_provider` 和 `model_providers`。

```toml
model_provider = "quota_router"

[model_providers.quota_router]
name = "quota-router"
base_url = "http://127.0.0.1:4000/v1"
wire_api = "responses"
experimental_bearer_token = "<管理页生成的本地 Token>"
requires_openai_auth = true
```

本地 Token 只保护 `/v1/*` 代理入口，不是任何上游 Key，但仍应按凭据保护。点击“更换 Token”会生成并保存新的随机 Token，旧 Token 立即失效；更换后需要重新复制配置。页面为方便一次复制使用 `experimental_bearer_token`；Codex 官方更推荐使用 `env_key` 从环境变量读取 provider Token。

## 安全边界

- 管理页和代理只监听 `127.0.0.1`，不要改为局域网或公网监听。
- 管理接口只接受本机 Host，拒绝跨站 Origin；写操作需要进程启动时随机生成的 CSRF Token。
- 配置与状态接口只返回 `keyConfigured` 和 `newApiSecretConfigured` 布尔状态，不会向页面回显完整上游 Key、密码或用户 Access Token。
- 已保存账号的 Base URL 若改到不同来源（协议、主机或端口变化），必须重新填写 API Key；使用实际余额认证时也必须重新填写余额凭据，或切换为仅 API Key。旧凭据不会被静默发送到新地址。
- 本机其他进程仍可能访问本地端口；本工具不是已失陷主机上的隔离边界。
- Windows DPAPI 配置只能由同一 Windows 用户解密，不能直接迁移给另一用户、Linux 或 macOS。
- Linux 和 macOS 配置为权限受限的 JSON 明文，备份时应按敏感凭据处理。
- 日志不包含请求正文或凭据，但会保存路由状态和错误分类；分享日志前仍应先检查内容。
- 当前主网关使用公网 HTTP，Key、余额认证凭据、代码、提示词和响应可能被明文窃听。管理页会强制二次确认，但更安全的方案仍是 HTTPS 上游或可信加密隧道。

## 验证状态

托盘使用 `fyne.io/systray`，前端不使用 CDN。发布前验证项：

- `gofmt`
- `go test ./...`
- `go vet ./...`
- `go test -race ./...`
- 记录单元测试语句覆盖率
- Web JavaScript、HTML/ARIA、Windows PowerShell/CMD 和 Linux Shell 静态检查

macOS 托盘必须在 Darwin 真机验证；WSL 无法完成 Cocoa 编译、LaunchAgent、Intel/Apple 芯片和 Gatekeeper 验证。
