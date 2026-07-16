# Codex Quota Router

一个只监听本机的 Codex 多账号路由器。Codex 始终连接 `127.0.0.1:4000`，API Key 与实验性 Codex OAuth 账号、排序、分配策略、余额和运行状态通过 Web 页面管理。

## 功能

- 创建、删除、轮换和分页管理多个命名网关 Token；代理请求通过 Token 摘要索引认证。
- 默认拒绝公网 Host；可显式允许一个 HTTPS 反向代理地址，公网管理页面必须使用管理员密码登录。
- 添加任意多个 New API、Sub2API、OpenAI Responses 兼容 API Key 账号，或通过设备码登录实验性 Codex OAuth 账号。
- 用“上移 / 下移”调整账号顺序；数组顺序就是优先级顺序。
- 支持按顺序、轮询、最少使用、套餐重置、余额最多和余额最少六种策略。
- 自动区分额度耗尽、Key 无效、账号受限、普通限流和瞬时网络故障，并只影响对应账号。
- 在每个账号下显示上游声明的支持模型，并可使用账号弹窗中的当前填写内容刷新。
- 每个账号的成功、失败和总请求统计会持久化保存，并可在管理页单独清空。
- 自动读取 New API 的 API Key 额度；使用用户名密码或用户 Access Token 登录后，还会读取活跃订阅的剩余额度、下次重置时间和计费偏好，用于核对实际可用总额度及套餐重置优先；不登录用户账号时可手工填写重置规则。
- Windows 使用 DPAPI 加密配置；Linux 和 macOS 使用 `0700` 目录和 `0600` 配置文件。
- 记录脱敏运行日志，保留启动、路由、切换、余额探测和错误分类；不记录请求/响应正文或任何凭据。
- Windows 和 Linux 运行时显示系统托盘图标，macOS 显示菜单栏图标；菜单可打开管理页、显示版本、跳转 GitHub 或退出服务。

新建 API Key 账号时必须显式选择 `new_api`、`sub2api` 或 `openai_responses`，程序不会按域名猜测上游类型。`auto` 只用于兼容升级前仅保存了 API Key、无法可靠判断平台的旧账号；旧账号在未更换 Key 时可以继续使用 `auto`，填写或更换 Key 时必须完成选择。Codex OAuth 账号固定识别为 `codex_oauth`。

## 路由策略

| 策略 | 行为 |
| --- | --- |
| 按优先级顺序 | 始终选择列表中第一个可用账号；它耗尽、不可用或冷却时再选择下一项。 |
| 轮询使用 | 在可用账号间依次轮换，例如 A → B → C → A。 |
| 最少使用优先 | 从当前可用账号中选择本次程序启动以来已分配请求数最少的账号，连续分配 30 次后重新比较；批次内账号不可用时立即切换。分配计数重启后重新开始，管理页的成功与失败统计单独持久化。 |
| 套餐重置优先 | 优先消耗最早即将清零的套餐额度。Codex OAuth 使用官方用量窗口；登录 New API 的账号使用自动订阅数据，不登录的 API Key 账号使用手工规则；没有可用重置时间的账号排在最后。 |
| 余额最多优先 | 选择已知实际可用额度最高的账号；只有核对后仍为无限的账号才按无限处理，余额相同则选择已分配请求较少的账号。 |
| 余额最少优先 | 优先选择已知实际可用额度最低的有限额度账号；核对后仍为无限的账号排在有限额度之后；余额相同则保持列表顺序。 |

New API 登录模式（用户名 + 密码或用户 Access Token + 用户 ID）从 `/api/subscription/self` 自动读取活跃订阅的 `amount_total`、`amount_used`、`next_reset_time` 和 `billing_preference`，并计算剩余额度。只有计费偏好为 `subscription_first`、`subscription_only` 或上游未返回该字段（按 `subscription_first` 处理）时，才能可靠地优先消耗订阅额度并参与套餐重置排序；`wallet_only` 和 `wallet_first` 不参与。多个有限订阅中，排序时间 `PriorityResetAt` 只取剩余额度大于 0 的最早重置时间；额度错误冷却和状态展示使用独立的 `ResetAt`，取所有有限订阅中最早的恢复时间，其中 `wallet_only` 不使用订阅恢复时间。自动套餐缓存过期或记录的重置时间已经到达时，路由前会重新查询。

API Key（不登录用户账号）可手工选择 `never`、`daily`、`weekly`、`monthly` 或 `custom`。默认时区为 `Asia/Shanghai`：`daily` 在每天 00:00 重置，`weekly` 在每周一 00:00 重置，`monthly` 在每月 1 日 00:00 重置；`custom` 按锚点每 N 小时、天、周或月重置，例如“每 3 个月”和“每 12 个月（1 年）”。切换到登录模式后会忽略并清除手工规则。自动与手工账号统一按实际下一次重置时间升序选择，时间相同则保留账号列表顺序。

两种余额策略都不会把未知余额当作 0。只有全部当前可用账号的账户、订阅与 API Key 可用余额都已验证、新鲜、可读取且单位一致时才会直接比较；瞬时网络、限流或超时失败会在最近一次成功余额仍处于 5 分钟有效期时继续使用该值并显示刷新警告。账号认证失败、仅有 API Key 额度、余额缺失或过期、没有可用的成功值、单位不同，都会明确回退为“最少使用优先”，管理页会显示原因。

任何策略都可能在额度耗尽、认证失败或限流时故障切换账号。路由器会按网关 Token 隔离，在内存中保存已完成 Responses 的完整 transcript；已知 [`previous_response_id`](https://developers.openai.com/api/docs/guides/conversation-state#passing-context-from-the-previous-response) 会优先粘在原账号，必须切号时删除旧 ID，并向新账号回放输入、完整输出、工具调用和加密 reasoning。Provider、Base URL、代理凭据等变化会增加账号 Revision；任何已缓存会话发现所属账号 Revision 不再一致时，都会明确返回 HTTP 409 `session_account_changed`，不会跨账号尝试。状态不会写入磁盘；进程重启、空闲超过 24 小时、缓存淘汰或单条 transcript 超过 32 MiB 后，旧 ID 不能保证无感恢复，客户端仍应具备发送完整上下文的降级能力。`conversation`、`item_reference`、上游文件 ID 等账号或项目绑定资源也不能仅靠 transcript 跨账号复用。

## 自动余额探测

余额探测按 Provider 隔离：`new_api` 和旧 `auto` 使用现有 New API 流程；`openai_responses` 只访问 OpenAI 兼容的 Dashboard Billing 接口；`sub2api` 在尚无已确认额度协议时明确显示“不支持”，不会试探 New API 接口。New API 用户名、密码、用户 Access Token 和用户 ID 不会发送给其他 Provider。

New API 的 Token 标记为“无限”只表示该 API Key 没有单独的 Key 上限，不代表所属用户账号的钱包或订阅无限。路由器按以下方式读取并归一化余额：

1. 尝试 New API 的 `GET /api/usage/token/`，使用账号 API Key 读取该 Key 的剩余额度或“无限”标记。
2. 若配置了实际余额认证，再读取 `GET /api/user/self` 的账户钱包 `quota` 和 `GET /api/subscription/self` 的活跃订阅、剩余额度、下次重置时间及计费偏好。`wallet_only` 只使用钱包，`subscription_only` 只使用订阅，`wallet_first` 计算钱包与订阅合计；`subscription_first` 优先计算订阅，只有活跃订阅都允许 `allow_wallet_overflow` 时才计入钱包，没有活跃订阅时使用钱包。
3. 最终可用额度为 API Key 剩余额度与上述账户可用额度中的较小值；任一侧无限时由另一侧限制，两侧都无限才显示无限。
4. 未配置账号认证时，只展示 API Key 额度，并明确提示未登录 New API 账户、无法核对账户及订阅余额、显示值可能不准确且余额策略可能无法按预期生效。余额策略不会把该值当成完整实际余额。已配置但认证失败时标记为认证错误，提示修正凭据。
5. 若 New API Token 路由不存在或成功响应不符合余额格式，尝试 OpenAI 兼容的 `GET {base_url}/dashboard/billing/subscription` 和 `usage`；未登录账户时，该结果仍只视为未经账户核对的 API Key 余额。
6. 所有方式都不支持时，将余额标记为“不支持”，不会伪造估算值。

管理页账号弹窗中的“套餐余额读取方式”支持：

| 方式 | 用途与限制 |
| --- | --- |
| API Key（不登录用户账号） | 不增加额外凭据；只能查询 API Key 额度及上游可能提供的兼容账单接口，不能核对账户钱包和订阅余额。套餐重置时间由用户手工填写，余额显示可能不准确，余额策略也可能无法按预期生效。 |
| 用户名 + 密码 | 登录对应 New API 站点并自动读取账户钱包、订阅余额、下次重置时间及计费偏好；登录模式不使用手工重置规则。需要 Turnstile、验证码、两步验证或其他交互式校验的站点可能无法自动登录。 |
| 用户 Access Token + 用户 ID | 使用上游生成的用户 Access Token 和正整数用户 ID 自动读取账户钱包、订阅余额、下次重置时间及计费偏好；登录模式不使用手工重置规则，遇到交互式登录限制时优先使用此方式。 |

Passkey 不能直接在本机管理页中完成：WebAuthn 凭据绑定上游站点的 RP ID 和 Origin，`127.0.0.1` 页面不能冒充上游域名。请先在上游站点使用 Passkey 登录，再生成用户 Access Token，并选择“用户 Access Token + 用户 ID”。页面不会提供无法工作的伪 Passkey 按钮。

用户名、密码和用户 Access Token 都只会发往该账号配置的上游。认证秘密与 API Key 一样不会由配置接口回显：认证方式和用户名或用户 ID 未变化时，输入框留空会保留原值；填写新值会替换，勾选清除会删除。切换并保存为“API Key（不登录用户账号）”也会自动清除已保存的用户名、用户 ID 和余额认证秘密。Windows 使用 DPAPI 加密；Linux 和 macOS 依赖 `0700` 配置目录和 `0600` 配置文件。用户名密码登录产生的 Cookie 按账号隔离缓存，避免不同上游账号混用会话。

账号弹窗提供两个互不依赖的查询：“查询当前填写余额”和“刷新支持模型”都直接使用弹窗中的候选配置，不会保存配置。全局“刷新已保存配置余额”只使用服务器已经保存的配置。账号存在未保存编辑时，列表会继续显示服务器旧配置的余额并明确标注，避免把旧状态误认为当前输入的查询结果。

支持模型来自对应上游的 `/v1/models`，页面会在加载已保存配置后和保存配置后自动查询。该列表只代表当前 API Key 能从上游看到的声明模型，不保证每个模型一定可调用，也不是实际推理连通性测试。

当前实际余额计算 API Key 额度、账户钱包 `quota` 和 `/api/subscription/self` 返回的活跃订阅额度。订阅与钱包的合计表示多次请求可累计消费的额度；New API 不会把单次请求拆到多个订阅或“订阅+钱包”共同扣费。上游另行维护且未通过这些接口返回的赠送包或其他额度池仍不会计入。

当前两个网关已只读验证：

| 上游 | 探测方式 |
| --- | --- |
| `http://122.227.250.174:8000/v1` | 根路径 `/api/*` 被另一套 FastAPI 占用，因此使用 `/v1/dashboard/billing/subscription` 和 `/usage`，剩余额度按 `hard_limit_usd - total_usage / 100` 计算。 |
| `https://newapi.shixian.me/v1` | 使用 `/api/usage/token/` 的 `total_available`，并通过 `/api/status` 的 `quota_per_unit` 转换为站点显示额度；配置实际余额认证后再结合用户 `quota`、活跃订阅和计费偏好计算可用值。 |

余额策略使用 5 分钟新鲜度判断；管理页打开且可见时，会在加载后立即刷新，并约每 60 秒刷新一次已启用账号。多个页面同时打开时，后端会合并一分钟内的重复轮询；保存后的刷新和“刷新已保存配置余额”会忽略缓存立即探测。可重试的网络、超时或暂时上游错误只针对失败账号在 5 秒、20 秒后各重试一次，再恢复 60 秒轮询；认证失败和不支持不会快速重试。保存刷新若遇到正在进行的自动刷新，会等待该请求完成后再执行，不会被静默丢弃。

刷新结果按账号显示安全的失败阶段和稳定错误类型，例如 API Key 额度接口、用户登录、用户钱包接口、用户订阅接口、额度换算、超时或限流；不会把上游响应原文、API Key、密码或用户 Access Token 放进页面错误信息。每个账号的单次探测最多等待约 20 秒，并限制为最多 8 个账号并发探测。所有余额请求只发往该账号配置的上游。

这是轮询得到的近实时余额，不是上游主动推送；最终显示速度仍取决于上游账单接口自身的更新延迟。

不同 New API 站点可能把显示额度配置为 USD、CNY 或 Token 数量。程序只比较成功归一化且单位文本一致的余额；USD、CNY、Token 或未知单位不会互相换算，而会安全回退到“最少使用优先”。

## 失败与切换行为

- HTTP 402 或响应内容明确表示额度、余额耗尽：立即将该账号移出正常路由并尝试下一个账号；冷却时间优先采用上游 `Retry-After`，没有有效值时再使用适用的自动订阅 `ResetAt`、Codex OAuth 官方用量窗口或手工重置时间，仍无可用重置时间才按 10 秒、30 秒、5 分钟、15 分钟、30 分钟退避。到期后只允许一个半开请求，成功即自动恢复；余额刷新确认余额恢复时会提前允许半开探测。
- 响应内容明确表示账号已停用、封禁或受限：持久阻止该账号，并尝试下一个账号；普通 403 不会仅凭状态码停用整个账号。
- HTTP 401：立即将账号移出正常路由，并按同一退避阶梯安排单请求半开探测；修改 URL 或 Key 后会立即清除旧状态，探测成功后也会自动恢复。
- 普通 HTTP 429：API Key 账号继续按账号冷却；Codex OAuth 使用 provider 级 10 秒、30 秒、5 分钟、15 分钟、30 分钟退避，并增加 0~25% 正向抖动。有效的上游 `Retry-After` 与退避取较长值，最长 31 天；期间跳过其他 Codex OAuth 账号，但仍允许 API Key 账号兜底。`usage_limit_reached` 等明确额度错误仍按额度耗尽处理并可切换 OAuth 账号。429 不累计网络失败次数。
- 充值、更换 Key 或确认恢复后，仍可点击该账号的“验证并恢复账号”立即验证；自动半开探测和手动验证都只有在真实请求成功后才清除额度或认证不可用状态。因连续请求失败而暂时停用的账号还可点击“立即恢复账号”，直接清除临时停用和失败计数，不发送测试请求。
- 明确的 DNS、连接建立失败会在 3 秒总预算内最多尝试 5 次，全部失败后才累计一次账号失败，并可继续尝试下一个账号。POST 超时、断连或收到真实 HTTP 408/5xx 时可能已经被上游执行，不会自动重放；结果未知的传输错误返回 `upstream_result_unknown`，避免重复文本、工具调用或扣费。GET/HEAD 仍可在网络错误或 HTTP 408/5xx 后尝试下一个账号。
- 网络、HTTP 408 或 5xx 依次冷却 10 秒、30 秒、5 分钟、15 分钟、30 分钟，第三次起在管理页显示“账号请求失败过多，暂时停用”。收到有效且非 408/5xx 的响应、成功请求或用户立即恢复后清零网络失败计数；冷却结束后连续 5 分钟没有再次失败会重新按首次失败处理。同一冷却周期内仍在途的并发请求不会重复增加失败次数。冷却到期后的半开探测同一账号只允许一个请求；若全部候选都只因网络失败冷却，路由器最长约 30 秒进行一次受控探测，其他请求等待探测结果，不直接返回“没有可路由账号”。
- SSE 响应在收到首个有效字节前不会向客户端提交响应头；首字节前失败时仅 GET/HEAD 可以安全切换账号，POST 返回结果未知且不重放。首字节发出后绝不切换账号重放；管理页生成的 Codex provider 配置也将普通请求和流式重试次数设为 0。
- 等待上游响应头最多 1 分钟；响应体连续 2 分钟没有新数据会终止转发并冷却该账号，已开始的响应不会切换账号重放。
- 对 POST，除明确尚未发送的 DNS、连接建立失败外，同一个请求不会对同一账号重放。永久受限、手动停用、配置不完整以及尚未到期的额度、限流或认证冷却不会参与正常路由；冷却到期后只允许一个半开请求。若没有任何可用或可探测账号，返回 HTTP 503。安全故障切换后若没有更多候选，HTTP 错误会透传最后一个上游响应，确定未发送的连接错误返回 HTTP 502。
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

启动后点击托盘或菜单栏图标会显示“打开管理页”“版本”“关于（GitHub）”和“退出”。版本项只显示当前版本；“关于（GitHub）”打开项目仓库。选择“退出”只结束本次运行并保留登录自启动配置，可通过 `router.bat start`、`router.sh start` 或下次登录重新启动。Linux 需要 Session D-Bus 和支持 StatusNotifier/AppIndicator 的桌面环境；条件不满足时程序仍会以纯后台服务运行。

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

旧版双渠道配置会在首次启动时自动迁移为账号列表：通常保持“主渠道、备用渠道”的顺序；旧配置曾强制备用时，迁移后会把备用账号排在前面并停用主账号，保持原先“只用备用”的语义。配置从 v5 首次升级到 v6 前，会把磁盘上的原始受保护内容独占保存为同目录的 `config.dat.v5.bak`；已有备份不会被覆盖，备份失败时升级停止。需要回退时先停止服务，保留当前 v6 文件，把该备份恢复为 `config.dat`，并使用支持 v5 的旧程序版本启动。

## Codex 配置

管理页支持创建、删除和轮换多个命名网关 Token。Token 内容不会出现在列表中；只有点击“复制 Token”“复制本机配置”或“复制公网配置”时才会读取并写入剪贴板。配置必须写入用户级 `~/.codex/config.toml`；Windows 对应 `%USERPROFILE%\.codex\config.toml`。[Codex 官方配置参考](https://developers.openai.com/codex/config-reference/)说明，项目内 `.codex/config.toml` 会忽略 `model_provider` 和 `model_providers`。

```toml
model_provider = "quota_router"

[model_providers.quota_router]
name = "quota-router"
base_url = "http://127.0.0.1:4000/v1"
wire_api = "responses"
experimental_bearer_token = "<管理页生成的本地 Token>"
requires_openai_auth = true
```

网关 Token 只保护 `/v1/*` 代理入口，不是任何上游 Key，但仍应按凭据保护。删除或轮换后旧 Token 立即失效。页面为方便复制使用 `experimental_bearer_token`；Codex 官方更推荐使用 `env_key` 从环境变量读取 provider Token。

### 公网访问

程序始终只监听 `127.0.0.1:4000`，不会直接绑定公网网卡。需要公网访问时：

1. 在本机管理页设置唯一的公网 HTTPS 地址和管理员密码，再启用“允许公网访问”。
2. 使用同机 Caddy、Nginx 或其他受信任反向代理终止 TLS，再转发到 `http://127.0.0.1:4000`。
3. 反向代理必须只接受配置的 Host、保留原始 Host，并把 `X-Forwarded-For` 覆盖为单个真实客户端 IP、把 `X-Forwarded-Proto` 覆盖为 `https`，不能让客户端自行伪造。服务端会拒绝缺失、多段或非 HTTPS 的转发头。
4. 需要公网管理页面时，再启用“允许公网管理”。启用任何公网访问后，本机管理入口也要求管理员密码会话，防止反向代理错误地把公网请求伪装成免登录回环请求；公网 Cookie 额外使用 `Secure`，两类管理会话都使用 `HttpOnly`、`SameSite=Strict`、Host/Origin 校验、CSRF 和登录限流。

不要把 4000 端口直接映射、NAT 或转发到公网。公网地址必须是 HTTPS Origin，不能包含路径、用户名、查询参数或片段。

Caddy 最小示例：

```caddyfile
router.example.com {
    reverse_proxy 127.0.0.1:4000 {
        header_up Host {host}
        header_up X-Forwarded-For {remote_host}
        header_up X-Forwarded-Proto https
    }
}
```

把 `router.example.com` 同时填写到管理页的公网地址中。反向代理配置还应按部署环境增加访问日志脱敏、防火墙和连接数限制。

不要直接使用 Nginx 默认的 `proxy_pass http://127.0.0.1:4000` 配置：它通常会把后端 Host 改成 `127.0.0.1:4000`，且不覆盖 `X-Forwarded-For`，服务端将无法区分真实本机请求与被错误转发的公网请求。至少显式覆盖：

```nginx
location / {
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-For $remote_addr;
    proxy_set_header X-Forwarded-Proto https;
    proxy_pass http://127.0.0.1:4000;
}
```

当前单端口、同机反向代理模型仍把“保留公网 Host、覆盖为单个真实客户端 IP 和 HTTPS 协议”视为部署安全边界；代理若擦除这些来源信息，后端无法从同一个回环 TCP 连接中还原真实来源。服务端会校验这些头，并在启用公网访问后同时给本机管理入口加密码会话，避免错误代理配置直接绕过管理员认证。

### OpenAI/ChatGPT 登录边界

管理页可使用官方 Codex 公共 Client ID 发起设备码登录，取得新的 Access Token、Refresh Token 和 ID Token，然后固定请求 `https://chatgpt.com/backend-api/codex/responses` 与 `/responses/compact`。这套端点可在 OpenAI 官方 Codex 开源客户端中确认，但不是稳定的 OpenAI Platform 公共 API 合约，因此界面统一标记为“实验性 Codex OAuth”，接口或账号权限可能随上游变化。

- 只支持 Responses HTTP/SSE 与 compact；不支持 Chat Completions、WebSocket 或任意协议转换。
- OAuth、用量和 Token 请求只允许代码内固定的 `auth.openai.com` 与 `chatgpt.com` 地址，不新增可携带 OAuth Token 请求任意 URL 的通用管理接口。
- 不读取或导入 `~/.codex/auth.json`，不复用 ChatGPT 网页 Cookie。
- 不加入 TLS 指纹伪装、Cloudflare 绕过或 `codex-tui` 身份混淆；请求使用标准 TLS 和真实的 `codex-quota-router` 标识。
- 建议 Codex OAuth 长期使用稳定、固定的网络出口，不要轮换代理或频繁改变出口地址；这些工程保护不能保证账号不受上游风控影响。
- Access Token、Refresh Token 与 ID Token 只进入服务端配置，不通过管理 API 回显。ID Token 中的邮箱、账号 ID 和套餐只用于展示及构造官方请求头，不作为本地管理员权限依据。
- 套餐用量固定查询 `https://chatgpt.com/backend-api/wham/usage`，缓存 60 秒，不在每次代理请求中查询。

一般 OpenAI Platform 调用仍应使用正式 API Key。参见 [Codex 认证说明](https://developers.openai.com/codex/auth/)。

### 容量边界

- 网关 Token 上限为 20000 个，请求校验使用内存哈希索引，不会逐个扫描 Token。
- 管理页每次最多读取 100 个 Token，并支持名称搜索和分页。
- 请求正文有 128 MiB 全局内存预算，避免大量客户端同时上传大请求耗尽内存。
- Codex OAuth 固定限制为全局最多 4 个、每个真实 `CodexAccountID` 最多 2 个在途请求；全局 2 QPS、burst 4，每个真实账号 1 QPS、burst 2。一次逻辑请求的内部安全网络重试只计一次，SSE 直到响应体关闭才释放并发名额。
- Codex OAuth 本地并发或速率饱和时返回 HTTP 429 和 `Retry-After`，不切换账号，也不增加失败统计、账号冷却或健康失败状态。这些数值是本项目的保守工程保护值，不是 OpenAI 官方阈值。
- 上游连接池已按高并发调整，但“一万人使用”不等于“一万人同时生成”。真实容量仍取决于机器、上游并发限制、平均请求大小、SSE 持续时间和账号额度，公网发布前必须按实际流量压测。
- Codex OAuth 设备登录会话只保存在内存中，15 分钟过期并限制总数量；用量请求按账号合并并发刷新，避免高并发重复请求上游。

## 安全边界

- 管理页和代理只监听 `127.0.0.1`。公网访问只能通过配置的 HTTPS 反向代理 Host。
- 本机管理接口只接受本机 Host；未启用公网访问时保持本机免密码，启用后本机与公网管理都需要密码会话。所有写操作都校验允许的 Origin 和进程启动时随机生成的 CSRF Token。
- 公网响应增加 HSTS、禁止页面嵌入、跨源资源隔离、权限策略和每次页面请求独立的脚本 CSP nonce。
- 管理员密码使用带随机盐的 PBKDF2-SHA256 摘要保存；连续登录失败会触发限流。
- 多个网关 Token 不通过配置与状态接口批量回显，代理请求通过 Token 摘要索引认证。
- 配置与状态接口只返回 `keyConfigured`、`newApiSecretConfigured`、OAuth 登录状态、邮箱和套餐摘要，不会向页面回显完整上游 Key、密码或任何 OAuth Token。
- Codex OAuth 设备会话绑定已经验证的管理员 Cookie；仅在未启用公网访问的本机免密码模式下绑定进程启动时生成的 CSRF Token。设备码由前端按服务器建议间隔轮询，服务端不为每次登录保留长期 goroutine。
- 已保存账号的 Base URL 若改到不同来源（协议、主机或端口变化），必须重新填写 API Key；使用实际余额认证时也必须重新填写余额凭据，或切换为仅 API Key。切换到不使用 New API 余额认证的 Provider 会清除对应账户凭据；旧凭据不会被静默发送到新地址或其他 Provider。
- 本机其他进程仍可能访问本地端口；本工具不是已失陷主机上的隔离边界。
- Windows DPAPI 配置只能由同一 Windows 用户解密，不能直接迁移给另一用户、Linux 或 macOS。
- Linux 和 macOS 配置为权限受限的 JSON 明文，备份时应按敏感凭据处理。
- 日志不包含请求正文或凭据，但会保存路由状态和错误分类；分享日志前仍应先检查内容。
- 当前主网关使用公网 HTTP，Key、余额认证凭据、代码、提示词和响应可能被明文窃听。管理页会强制二次确认，但更安全的方案仍是 HTTPS 上游或可信加密隧道。

## 验证状态

托盘使用 `fyne.io/systray`，前端不使用 CDN。发布前验证项：

- `gofmt`
- `go test ./...`
- `node --test web/tests/frontend.test.mjs`
- `go vet ./...`
- `go test -race ./...`
- 记录单元测试语句覆盖率
- Web JavaScript、HTML/ARIA、Windows PowerShell/CMD 和 Linux Shell 静态检查

`package.ps1` 会在创建任何发布文件前强制检查工作区无修改、`HEAD` 带有精确的 `v<版本>` 标签，并依次执行前端测试与 `go test ./...`；失败时会清理本次目标的半成品。发布产物位于 Git 忽略的 `dist/`，修复版本不得覆盖已经发布过的版本号。

各平台归档汇总到同一发布目录后，再单独生成最终校验和，例如：

```powershell
.\package-checksums.ps1 -Version 0.3.5 -Targets windows-amd64,linux-amd64
```

该步骤会先确认声明的全部目标归档均存在，再一次性写入 `SHA256SUMS.txt`，避免不同构建主机各自生成不完整清单。

macOS 托盘必须在 Darwin 真机验证；WSL 无法完成 Cocoa 编译、LaunchAgent、Intel/Apple 芯片和 Gatekeeper 验证。
