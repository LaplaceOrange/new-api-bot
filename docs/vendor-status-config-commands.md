# 厂商状态：全部配置命令映射

## 通用入口

```text
/vendor_config help
/vendor_config show
/vendor_config show <key>
/vendor_config <key>
/vendor_config <key> <value>
/vendor_config set <key> <value>
/vendor_config reset <key|all>
```

仅提供英文入口 `/vendor_config`，支持 `help`、`show`、`get`、`set`、`reset`，
不保留中文命令或中文子命令别名。
配置命令均仅限 Bot 管理员，无需绑定 New API；只读管理员可查看，不可修改或重置。
每个修改都是全局配置，影响所有查询与订阅群，不是当前群单独的主题设置。

布尔值：`true/false`、`on/off`、`1/0`。
秒数：整数秒或 Go duration（例如 `300`、`5m`）。
字符串含空格须用引号，Windows 路径保留反斜杠；`""` 是空字符串。
主题/语言值使用规范英文标识，不接受中文枚举别名；中文说明和自由文本名称仍保留。

## 原插件 18 个顶层选项

每行都有对应命令；固定平台项明确只读，不伪装成可以切换到未实现的适配器。

| 原配置键 | 命令示例 | 可用值 / 语义 |
| --- | --- | --- |
| `enabled` | `/vendor_config enabled on` | 全局自动监控即时启停；关不影响手动查询 |
| `platform_type` | `/vendor_config platform_type` | 查询固定 `qq_official`；宿主没有 OneBot 适配器 |
| `platform_id` | `/vendor_config platform_id` | 查询当前 `QQ_APP_ID`；修改 QQ 身份仍须修改宿主配置并重启 |
| `group_whitelist` | `/vendor_config group_whitelist ["GROUP1","GROUP2"]` | QQ group_openid 数组；不接受 UMO |
| `poll_interval_seconds` | `/vendor_config poll_interval_seconds 5m` | 60–86400 秒；也可填整数 `300` |
| `notify_maintenance` | `/vendor_config notify_maintenance on` | 是否将计划维护列入推送 |
| `history_lookback_hours` | `/vendor_config history_lookback_hours 48` | 0–168 小时，0 关闭补报 |
| `notify_source_failures` | `/vendor_config notify_source_failures off` | 是否通知采集异常/恢复 |
| `source_failure_threshold` | `/vendor_config source_failure_threshold 3` | 连续不完整采集阈值，1–100 |
| `source_failure_cooldown_seconds` | `/vendor_config source_failure_cooldown_seconds 1h` | 60–86400 秒，逐来源/目标冷却 |
| `notify_existing_on_first_startup` | `/vendor_config notify_existing_on_first_startup off` | 来源首次成功采集时是否通知存量；不倒回已有初始化状态 |
| `display_language` | `/vendor_config display_language zh-CN` | `bilingual`（双语）、`zh-CN`（简体中文）、`en-US`（英文） |
| `card_theme` | `/vendor_config card_theme liquid_glass` | 见下方五主题 |
| `timezone` | `/vendor_config timezone Asia/Shanghai` | 有效 IANA 时区；`inherit`、`default` 或 `""` 跟随宿主 CHECKIN_TIMEZONE |
| `enable_ai_translation` | `/vendor_config enable_ai_translation on` | 启用模型翻译；缺少连接信息时仍显示原文 |
| `translation_provider_id` | `/vendor_config translation_provider_id my-translator` | 原键映射本项目 `translation.model`，不再是 AstrBot provider 实例 ID |
| `sources` | `/vendor_config sources.openai off` | 20 个来源逐个开关；也可整体设置 JSON |
| `custom_statuspage_sources` | `/vendor_config custom add "我的服务" https://status.example.com` | 添加、删除、开关、改名、改 URL、JSON 整体替换 |

### 五种主题

| 中文值 | 规范值 | 示例 |
| --- | --- | --- |
| 纸质公报 | `paper` | `/vendor_config card_theme paper` |
| 午夜蓝图 | `midnight` | `/vendor_config card_theme midnight` |
| 青瓷云笺 | `porcelain` | `/vendor_config card_theme porcelain` |
| 荧光终端 | `terminal` | `/vendor_config card_theme terminal` |
| 液态玻璃 | `liquid_glass` | `/vendor_config card_theme liquid_glass` |

### 群白名单的每种操作

```text
/vendor_config group_whitelist
/vendor_config group_whitelist add GROUP_OPENID
/vendor_config group_whitelist remove GROUP_OPENID
/vendor_config group_whitelist ["GROUP1","GROUP2"]
/vendor_config group_whitelist clear
/vendor_config reset group_whitelist
```

`/vendor_subscribe on|off` 用于管理员在当前群直接订阅/退订。
整体替换、增删或清空白名单时，保存的是变更后的完整有效列表，并清除旧单群覆盖；
因此不会出现“列表已清空，但旧订阅群仍发送”的情况。
`reset group_whitelist` 清除命令白名单与单群覆盖，恢复启动时的部署白名单。

### 全部 20 个内置来源开关

语法一：`/vendor_config sources.<id> on|off`。
语法二：`/vendor_config sources <id> on|off`。

| 来源 | 完整关闭命令 |
| --- | --- |
| OpenAI | `/vendor_config sources.openai off` |
| OpenRouter | `/vendor_config sources.openrouter off` |
| Claude / Anthropic | `/vendor_config sources.claude off` |
| Google Vertex AI / Gemini | `/vendor_config sources.google_vertex_gemini off` |
| Gemini Developer API / AI Studio | `/vendor_config sources.gemini_developer off` |
| Groq | `/vendor_config sources.groq off` |
| Cohere | `/vendor_config sources.cohere off` |
| Moonshot / Kimi | `/vendor_config sources.moonshot off` |
| MiniMax | `/vendor_config sources.minimax off` |
| Fireworks AI | `/vendor_config sources.fireworks off` |
| Novita AI | `/vendor_config sources.novita off` |
| xAI | `/vendor_config sources.xai off` |
| DeepSeek | `/vendor_config sources.deepseek off` |
| Cursor | `/vendor_config sources.cursor off` |
| Cerebras | `/vendor_config sources.cerebras off` |
| AWS | `/vendor_config sources.aws off` |
| Azure | `/vendor_config sources.azure off` |
| GitHub | `/vendor_config sources.github off` |
| Vercel | `/vendor_config sources.vercel off` |
| Cloudflare | `/vendor_config sources.cloudflare off` |

```text
/vendor_config sources
/vendor_config sources.openai
/vendor_config sources {"openai":false,"claude":false}
/vendor_config reset sources.openai
/vendor_config reset sources
```

整体替换 `sources` 会清除此前的各个 `sources.<ID>` 命令覆盖；
JSON 中未填写的来源遵循上游默认启用规则。
`reset sources` 同时清除整个对象和所有逐来源覆盖，恢复部署配置。

### 自定义源：三个字段全部有命令

`custom` 是 `custom_statuspage_sources` 的别名；名称精确匹配，带空格须加引号。

```text
/vendor_config custom
/vendor_config custom add "我的服务" https://status.example.com
/vendor_config custom add "暂停采集" https://other-status.example.com off
/vendor_config custom set "我的服务" name "新服务名"
/vendor_config custom set "新服务名" base_url https://new-status.example.com
/vendor_config custom set "新服务名" enabled off
/vendor_config custom enable "新服务名"
/vendor_config custom disable "新服务名"
/vendor_config custom remove "新服务名"
/vendor_config custom clear
/vendor_config custom [{"name":"批量服务","base_url":"https://status.example.com","enabled":true}]
/vendor_config reset custom_statuspage_sources
```

字段对应：`name` ↔ 改名，`base_url` ↔ URL，`enabled` ↔ 开关。
批量 JSON 省略 `enabled` 时默认 true。空名称、重复名称、无效 URL 或不存在的目标会拒绝，
不会部分保存。采集仍沿用上游按 URL 去重；不同名称但同 URL 不会重复采集。

## 移植新增参数也全部有命令

| 配置键 | 命令示例 | 说明 |
| --- | --- | --- |
| `python` | `/vendor_config python "C:\Python\python.exe"` | Python 可执行文件；请使用存在的解释器，不校验机器上的安装状态 |
| `proxy` | `/vendor_config proxy "socks5h://username:password@host:1080"` | 厂商采集专用代理；支持 socks5/socks5h、用户名密码、IPv6；加密保存，查询不回显；`off` 关闭，`reset proxy` 恢复部署值 |
| `http_timeout_seconds` | `/vendor_config http_timeout_seconds 15s` | 单个状态 HTTP 超时，1–120 秒 |
| `worker_timeout_seconds` | `/vendor_config worker_timeout_seconds 10m` | 查询/轮询总时限，30–3600 秒 |
| `font_path` | `/vendor_config font_path "C:\Fonts\font.ttf"` | 字体路径；`""` 或 `auto` 恢复自动查找 |
| `translation.base_url` | `/vendor_config translation.base_url https://model.example.com/v1` | 翻译聊天 API 根地址，包含 `/v1` |
| `translation.api_key` | `/vendor_config translation.api_key "MODEL_API_KEY"` | **仅机器人单聊**可设置；AES-GCM 加密存储、不回显 |
| `translation.model` | `/vendor_config translation.model my-translator` | 翻译模型；与原 `translation_provider_id` 命令对应同一项 |

翻译三项可以逐项设置。未完整设置时不调用模型，按上游规则显示原文，不阻断告警。
查询 API Key 只显示“已设置/未设置”；清空用 `""`，恢复部署密钥用
`/vendor_config reset translation.api_key`。密钥不进入配置查询、成功回复或审计正文。

代理支持 JSON `proxy`、环境变量 `VENDOR_STATUS_PROXY` 和命令覆盖。
`socks5` 在本地解析目标 DNS；`socks5h` 在代理解析，适合目标域名本地不可解析的场景。
用户名/密码含 `@`、`:`、`#`、`%` 时应百分号编码（例如 `p@ss:word` → `p%40ss%3Aword`）。
代理只用于状态页查询和后台监控，不改变 QQ、New API、翻译 API 的网络路径。
命令可在管理员群聊设置，但带凭据建议私聊，以避免原始指令暴露密码。
本机重新安装 `internal/vendorstatus/requirements.txt`，Docker 重新构建镜像后使用。

## 优先级、生效与重置

1. 启动时读取：内置默认 → JSON → 环境变量。
2. 每次查询/轮询读取：以上部署配置 → bbolt 命令覆盖 → 单群订阅覆盖。
3. 配置修改取消当前后台轮询并唤醒调度器；新轮次按新配置执行，停用后调度器休眠。
4. 已启动的手动查询使用自身快照；未来查询使用新设置。QQ 已发送的消息不回滚。
5. `reset <key>` 删除该键的命令覆盖，恢复本次启动读取的部署值；
   `reset all` 同时清除单群订阅覆盖，但保留事故、去重/送达记录和翻译缓存。
6. `enabled` 启停不需要重启；启动时 false 也有休眠调度器，可即时用命令启用。

保存失败、非法参数和权限拒绝不会修改设置。配置仍受 `/enable`、`/disable` 命令关键词管理。
前述命令均是全局管理员操作，不赋予普通群成员修改机器人进程或发送目标的权限。

## 覆盖与证据

### 英文命令约定

本项目实际入口为 `/vendor_status`、`/vendor_subscribe on|off` 和 `/vendor_config`。
已移除中文根命令、子命令、配置键别名和主题/语言枚举别名。
中文回复、自定义源名称及路径等自由文本不受影响。上游说明归档
`UPSTREAM_README.md` 中的命令仅用于保留出处，不代表本项目提供这些入口。

E7：`internal/bot/vendor_english_test.go` 验证旧中文命令不能执行、英文帮助不再展示
中文入口、自由文本保留及规范配置存储兼容；`options_test.go` 验证关键字/示例仅用英文。
本轮全量 Go、race、vet、Windows/Linux 构建通过，Python 136 passed / 7 live skipped；
日志与实际构建 SHA256 在 `.codex/vendor-english-evidence/`。

- E1：`internal/vendorstatus/worker/statusmonitor/_conf_schema.json`，18 个顶层配置和 20 个来源。
- E2：`internal/vendorstatus/options.go::Options/FindOption`，统一配置/帮助注册表；
  `TestCommandRegistryCoversEveryUpstreamConfiguration` 对照原 schema，防止漏项。
- E3：`internal/bot/vendor_config.go`，参数、查询、设置、CRUD、重置、权限和脱敏；
  `vendor_config_test.go` 覆盖所有可写标量、20 个来源、三个自定义字段及非法参数。
- E4：`internal/store/vendor_runtime.go`、`internal/bot/vendor_runtime.go`，
  bbolt 命令覆盖和 AES-GCM 密钥；存储重开、白名单清理与并发更新测试。
- E5：`internal/bot/vendor_status.go::runVendorStatusWorker`，
  可唤醒休眠调度、启停取消；热启停测试与原有关闭测试。

### 2026-10-01 本地实际验证

- [已验证 E6] 全量 `go test -count=1 ./...`、`go test -race -count=1 ./...`、
  `go vet ./...` 和 `go build ./...` 成功，包含真实 Python worker 的本地 Mock 集成。
- [已验证 E6] Windows 和 Linux amd64 二进制已重新构建；
  Python **136 passed, 7 skipped**，Ruff lint/format 和 diff check 通过。
- E6 具体日志：`.codex/vendor-config-evidence/go-tests.txt`、
  `go-race-fresh-cache.txt`、`python-tests.txt`、`build-sha256.txt`。
- 首次 race 在共享 Go 缓存下出现标准库找不到的报错；确认相关源码实际存在后，
  使用项目内独立 `GOCACHE` 复测成功。旧失败输出保留在 `go-race.txt`，
  不作为成功证据。

未验证项：真实 QQ 命令投递、管理员私聊权限、部署机器上解释器/字体路径是否存在。
本地测试使用 Fake QQ 和 bbolt，不能代替真实平台验证。
