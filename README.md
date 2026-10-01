# QQ × New API 机器人后端

一个面向 [QQ 机器人 API v2](https://bot.q.qq.com/wiki/develop/api-v2/) 与 [New API 管理接口](https://docs.newapi.ai/zh/docs/api) 的低内存 Go 后端。机器人在群聊中完成邮箱验证码绑定、直接加额度签到、用量查询、额度提醒、订阅管理、RSS/Atom 更新推送和管理员审计。

## 推荐 AI 中转站

需要便捷、稳定的 AI API 中转服务，可以访问 **[FSY AI（ai.fsykk.cn）](https://ai.fsykk.cn)**。支持多种主流模型，可用于 API 调用、开发测试及日常使用。

## 功能

- QQ 群聊 @ 消息，使用 WebSocket Gateway 接收事件；仅处理并回复以 `/` 开头的指令消息。
- QQ Access Token 自动刷新、心跳、Resume、断线重连与消息去重。
- 一个 QQ 主身份与一个 New API 用户 ID 的双向唯一绑定。
- SMTP 邮箱验证码，每个 QQ 身份和目标账户每小时默认最多发送两封。
- 按自然日、自然周或自然月限制签到次数，并按昨日用量与随机上限动态增加绑定用户额度。
- 管理员增加/扣除额度、管理用户订阅、查看绑定和解除绑定。
- 管理员按群发放拼手气额度红包，绑定账户限领一次，领取额度直接入账；全部领完后自动汇总数量、总额度与用时。
- 查询个人、指定用户或全站用户用量，查看调用记录和站点已启用模型。
- 在指定群内发送低额度提醒；管理员可查看全站用户与模型用量报表。
- 支持 QQ 2026-08-10 新增的入群申请事件与审批接口；可按群开启 New API 邮箱/用户 ID 自动核验。
- 支持 QQ 官方群成员禁言状态查询、定时禁言和解除禁言接口。
- 低资源监测 Codex 重置信号，并按群发起可恢复的用量补偿抽奖。
- 完整接入全球厂商状态监控：20 个官方状态源、自定义 Statuspage、历史补报、按群重试、采集健康通知、五主题 PNG 和可缓存的 AI 双语翻译。
- bbolt 单文件持久化、AES-256-GCM 敏感数据加密、JSON 结构化日志。
- `/healthz` 和 `/readyz` 健康检查。

## 指令

| 指令 | 场景 | 说明 |
| --- | --- | --- |
| `/bind <邮箱或用户ID>` | 群聊 | 向 New API 账户邮箱发送绑定验证码 |
| `/bind verify <6位验证码>` | 群聊 | 在当前群完成双向唯一绑定 |
| `/bind status` | 群聊 | 查看当前绑定的 New API 用户信息 |
| `/unbind` | 群聊 | 解除当前 QQ 身份的 New API 绑定 |
| `/checkin` | 群聊 | 签到并直接增加绑定账户额度 |
| `/checkin status` | 群聊 | 查看当前周期签到状态 |
| `/checkin reset` | 管理员 | 重置当前周期所有用户的签到状态，使其可以再次签到 |
| `/hongbao new <总额度> <数量> [分组限制 ...]` | 管理员群聊 | 发放当前群的拼手气额度红包，可指定多个允许领取的 New API 分组，不要求管理员个人绑定 |
| `/hongbao stop` | 管理员群聊 | 停止当前群红包，禁止后续领取，不扣回已发放额度 |
| `/hongbao` | 已绑定用户群聊 | 领取当前群红包，显示领取额度、剩余个数与剩余额度 |
| `/me` | 群聊 | 查看绑定账户及额度 |
| `/usage [today\|7d\|month]` | 已绑定用户 | 查看自己的请求数、成功/失败数、Token、消耗额度、余额及常用模型，默认今天 |
| `/usage <用户ID或@用户> <时间长度>` | 管理员 | 查看指定用户用量 |
| `/usage <时间长度> all` | 已绑定用户 | 查看该时间段全站总请求次数、总 Token、总消耗额度和活跃用户数 |
| `/usage <时间长度> <前N名>` | 已绑定用户 | 查看按消耗额度排序的前 N 名用户，例如 `/usage 7d 10` |
| `/usage chart <时间长度>` | 已绑定用户 | 生成自己的每日额度折线及模型用量占比 PNG 图表 |
| `/usage chart <时间长度> <@群成员或用户ID>` | 管理员 | 生成指定已绑定群成员或 New API 用户的用量图表 |
| `/usage chart <时间长度> all` | 已绑定用户 | 汇总当前群内已被机器人识别且已绑定成员的用量图表 |
| `/logs [数量]` | 已绑定用户 | 查看自己的最近调用记录，默认 10 条、最多 20 条 |
| `/logs <用户ID或@用户> [数量]` | 管理员 | 查看指定用户的最近调用记录 |
| `/models [用户ID或@用户]` | 用户/管理员 | 查看用户分组可用模型；目标用户查询仅管理员可用 |
| `/notify quota <额度>` | 已绑定用户 | 在当前群设置低额度提醒 |
| `/notify quota off` | 已绑定用户 | 关闭自己的低额度提醒 |
| `/notify daily on\|off` | 已绑定用户 | 开启或关闭每日用量摘要 |
| `/notify status` | 已绑定用户 | 查看自己的额度提醒状态 |
| `/bot status` | 已绑定用户 | 诊断 Gateway、QQ Token、New API 及当前群状态 |
| `/whoami` | 任意 | 查看可写入管理员名单的 OpenID |
| `/vendor_status` | 任意群聊/单聊 | 立即查询全部启用厂商并发送最新状态总览 PNG，无需绑定 |
| `/vendor_subscribe on` | 管理员群聊 | 持久化开启当前群的厂商异常、更新、恢复及采集健康通知，无需绑定 |
| `/vendor_subscribe off` | 管理员群聊 | 持久化关闭当前群的厂商自动推送，无需绑定 |
| `/vendor_config` | 管理员 | 查看当前有效厂商配置，密钥不回显，无需绑定 |
| `/vendor_config <key> <value>` | 管理员 | 持久化修改任一适用配置并热加载；只读管理员不能修改 |
| `/vendor_config help` | 管理员 | 查看全部配置键、参数和自定义源管理用法 |
| `/vendor_config reset <key\|all>` | 管理员 | 清除命令覆盖，恢复部署默认；不会清除事故送达状态 |
| `/help` | 任意 | 按当前用户权限查看一级命令及说明 |
| `/enable list`、`/disable list` | 任意 | 查看明确启用或禁用的命令关键词 |
| `/enable "<关键词>"` | 管理员 | 恢复包含指定关键词的命令 |
| `/disable "<关键词>"` | 管理员 | 静默忽略包含指定关键词的命令，并从 `/help` 隐藏匹配项 |
| `/credit add <用户ID或@用户> <额度>` | 管理员 | 增加用户额度 |
| `/credit sub <用户ID或@用户> <额度>` | 管理员 | 在余额不会变成负数时扣除用户额度 |
| `/credit show <用户ID或@用户>` | 管理员 | 查询用户额度 |
| `/plan view` | 已绑定用户 | 查看自己的全部订阅，按创建时间从新到旧排列 |
| `/plan view <用户ID或@用户>` | 管理员 | 查看目标用户的全部订阅 |
| `/plan add <套餐ID> <用户ID或@用户>` | 管理员 | 给目标用户添加订阅并返回订阅编号 |
| `/plan sub <订阅编号> <用户ID或@用户>` | 管理员 | 验证订阅归属后立即取消订阅 |
| `/admin bindings [页码]` | 管理员 | 分页查看绑定 |
| `/admin unbind <用户ID或@用户>` | 管理员 | 解除绑定 |
| `/admin report [时间长度]` | 管理员 | 查看全站用户及模型用量摘要，默认最近 24 小时 |
| `/admin report export [时间长度]` | 管理员 | 生成并发送 UTF-8 CSV 全站报表 |
| `/admin checkin` | 管理员 | 查看当天签到人数、已发放总额度和动态签到规则 |
| `/welcome on\|off` | 管理员 | 开启或关闭当前群的新成员欢迎；欢迎消息会实际 @ 新成员 |
| `/welcome set <欢迎语>` | 管理员 | 设置当前群欢迎语并自动开启 |
| `/join on\|off\|status` | 管理员 | 按群开启、关闭或查看 New API 账户入群自动审批 |
| `/join limit <QQ等级数>` | 管理员 | 设置自动审批最低 QQ 用户等级；`0` 表示不限制 |
| `/join check "<匹配字符串>"` | 管理员 | 要求申请内容包含指定字符串；`""` 表示不限制 |
| `/mute <@成员或member_openid> <时长>` | 管理员 | 禁言普通群成员，时长支持 `10m`、`2h`、`3d`，最长 30 天 |
| `/mute off <@成员或member_openid>` | 管理员 | 解除指定普通群成员禁言 |
| `/mute status` | 管理员 | 查看全员禁言模式和当前成员禁言列表 |
| `/recall [消息ID]` | 管理员 | 回复机器人两分钟内的消息进行撤回；消息 ID 可作回退 |
| `/admin user status <用户ID或@用户>` | 管理员 | 查看用户状态、角色和分组 |
| `/admin user enable <用户ID或@用户>` | 管理员 | 启用用户 |
| `/admin user disable <用户ID或@用户>` | 管理员 | 生成一次性确认码，确认后禁用用户 |
| `/admin user reset2fa <用户ID或@用户>` | 管理员 | 二次确认后重置用户 2FA |
| `/admin user resetpasskey <用户ID或@用户>` | 管理员 | 二次确认后重置用户 Passkey |
| `/confirm <一次性操作码>` | 管理员 | 确认五分钟内的敏感管理操作 |
| `/benefit <面额> <数量> <有效期(h)> <违者封禁时间(day)>` | 管理员 | @全体成员并批量发放一人限领一个的福利兑换码；自动检测多领、封禁并到期解封 |
| `/reset check` | 任意 | 查看当前群状态：未知、可能重置、即将重置或确认重置（抽奖进行中） |
| `/reset last` | 任意 | 查看 Codex Reset timeline API 最新重置事件、验证状态和时间窗口，不改变当前群状态或活动 |
| `/reset join` | 已绑定用户 | 参加当前群有效期内正在进行的重置补偿抽奖 |
| `/reset new` | 管理员 | 按当前群 `/reset set` 配置手动开启一轮新的重置补偿活动 |
| `/reset stop` | 管理员 | 停止当前活动，不抽取用户、不发放补偿额度 |
| `/reset end` | 管理员 | 提前截止当前活动，并立即抽取用户、发放补偿额度 |
| `/reset set duration <时长>` | 管理员 | 设置下一轮活动有效期，默认 `5h` |
| `/reset set winners <人数>` | 管理员 | 设置下一轮抽取人数，默认 `5` |
| `/reset set lookback <时长>` | 管理员 | 设置获奖者在活动开始前的用量补偿回溯时间，默认 `24h` |
| `/reset proxy <代理链接或off>` | 管理员 | 设置仅用于访问 Codex Reset timeline API 的 HTTP/SOCKS5 代理，凭据加密保存 |

帮助按层级展示，并始终过滤无权限、功能关闭或命中禁用关键词的命令：`/help` 只列出一级命令，普通用户不显示管理员专用命令，管理员显示其可用的全部一级命令，只读管理员只显示查询类操作。在命令末尾添加 `help` 可查看当前用法和直属下一级命令，例如 `/checkin help`、`/admin help`、`/admin user help`、`/reset set help`。每级帮助末尾会提示如何查看下一级；叶子命令显示详细用法。参数不构成命令层级，帮助查询无需绑定，也不会执行对应操作。

除 `/help`、各级命令的 `help` 查询、`/whoami`、`/bind`、`/vendor_status`、`/reset check`、`/reset last`、`/enable list`、`/disable list` 以及管理员的 `/vendor_config`、`/vendor_subscribe`、`/enable`、`/disable`、`/checkin reset`、`/hongbao new`、`/hongbao stop` 管理操作外，所有指令都要求执行者已经绑定。管理员指令还要求执行者命中 `QQ_ADMIN_OPENIDS`。

### 额度红包

- 管理员使用 `/hongbao new 10 5` 发放总额度为 `10`、数量为 `5` 的拼手气红包，用户使用 `/hongbao` 领取。仅支持群聊，红包按群独立，每群同一时间只允许一轮未结束的红包。
- 管理员可使用 `/hongbao stop` 停止当前群红包，无需个人绑定；普通用户和只读管理员不可执行。停止状态持久化保存，后续领取会被拒绝，不扣回已发放额度，也不会发送“全部领取”的总结。停止后可发放新一轮红包，但若有额度发放结果待确认，则仍保留原记录并阻止新发放，直至管理员完成核查；停止操作不会取消已经提交的额度请求。
- 可以在数量后追加多个 New API 分组名称，以空格分隔，例如 `/hongbao new 10 10 gpt-cheap gpt-smart` 仅允许 `gpt-cheap` 或 `gpt-smart` 分组领取。分组按名称精确匹配（区分大小写），重复名称会去重；领取时从 New API 核验账户的当前分组，分组限制随红包持久化保存。省略分组参数时不限制；不另设分组数量上限，仍遵循通用的 `4096` 字节指令长度限制。
- 金额使用 New API 站点显示额度，必须能够精确换算为整数 quota；总金额受 `CREDIT_MAX_PER_COMMAND` 限制，红包数量为 `1` 至 `10000`，每份至少为 `1 quota`。
- 每个 New API 账户每轮限领一次；随机分配使用整数 quota 的二倍均值法，最后一份领取全部剩余额度，确保总额度守恒。额度由管理员通过 New API 管理接口直接发放，不扣除管理员账户余额。
- 领取成功后显示领取额度、剩余红包个数及剩余额度。全部成功入账后，机器人另发总结，包含红包总数、发放总额度和自创建至最后一份入账的用时（秒）。
- 无权限创建或停止红包（含只读管理员）、分组不匹配时的无权限领取提示及重复领取的提示，会在发送后固定 `30` 秒尝试撤回机器人提示和对应用户指令；不受 `CHECKIN_AUTO_RECALL_AFTER` 配置影响。正常发放、停止成功、成功领取及红包总结不自动撤回。需要 QQ 客户端支持群消息撤回，接口拒绝或权限不足时仅记录失败日志。
- 红包及领取预留记录持久化保存，重启后继续有效。明确失败的额度请求会释放预留，允许重试；超时或其他结果不明确的请求保留该份红包，禁止重复入账，需要管理员核查 New API 后人工处理。剩余个数及额度不包含已预留的份额。
- 总结发送失败后由后台任务重试（使用 `BENEFIT_CHECK_INTERVAL` 周期，不要求启用福利兑换码功能）。成功记录已保存时不会重复发送；QQ 已接收总结但本地保存状态失败的极端情况下，重试可能重复通知。

命令关键词状态持久化保存在 bbolt。示例：管理员执行 `/disable "bind view"` 后，标准化内容中包含 `bind view` 的指令会被静默忽略，`/help` 中匹配该关键词的行也会隐藏；执行 `/enable "bind view"` 即可恢复。匹配不区分英文大小写，并会合并连续空白字符。`/enable` 和 `/disable` 管理指令本身始终可执行，避免规则将管理入口锁死。

所有以“用户ID”为目标的管理指令均可在群聊中使用 `@群成员` 代替数字 New API 用户 ID；机器人会读取该群成员已经建立的绑定。`/bind <邮箱或用户ID>` 是例外，只接受邮箱或数字 New API 用户 ID，不能使用 `@群成员`。

用量时间长度支持 `30m`、`24h`、`7d`、`4w`、`today`、`week` 和 `month` 等格式，最长查询 31 天。`/usage 7d all` 只返回最近 7 天的全站汇总；`/usage 7d 10` 返回按消耗额度从高到低排列的前 10 名用户。排行榜数量范围为 1 到 100，`10`、`top10`、`前10名` 三种写法均可。全站汇总和排行榜对所有已绑定用户开放。

## RSS/Atom 群订阅

每个 QQ 群独立订阅；不需要绑定 New API。支持 RSS 2.0、RSS 1.0、
Atom 1.0，以及 RSSHub 输出的完整 HTTP/HTTPS 订阅地址；不部署 RSSHub、
不接受仅有路由的地址。与厂商状态告警中的 RSS 采集相互独立。

| 命令 | 权限 | 功能 |
| --- | --- | --- |
| `/rss add <URL>` | Bot 管理员 | 验证源、建立基线并返回稳定编号；首次不推历史文章 |
| `/rss list` | 所有群成员 | 查看当前群订阅和启停状态 |
| `/rss remove <编号>` | Bot 管理员 | 删除订阅及待发送文章 |
| `/rss pause <编号>` | Bot 管理员 | 暂停抓取与推送，清除待发送文章 |
| `/rss resume <编号>` | Bot 管理员 | 成功重新抓取建立基线后恢复，不补发暂停期间文章 |
| `/rss interval <时长>` | Bot 管理员 | 设置本群检查间隔，范围 `1m`–`24h`，例如 `5m`、`1h` |
| `/rss status` | 所有群成员 | 查看间隔、最近检查、采集/推送错误及待发送数量 |
| `/rss check <编号>` | 管理员（含只读） | 验证连通性和解析，不更新基线、不推送文章 |

私聊指令会提示在群内操作；所有修改仅作用于当前群。只读管理员不能添加、
删除、启停或修改间隔。同群同一地址重复添加返回原编号，删除后的编号不复用；
不同群订阅相同地址允许有不同基线。`/rss help` 提供分级帮助，
`/disable rss` 和 `/enable rss` 按现有机制控制命令入口，不改变后台订阅。

部署选项：`RSS_ENABLED=true`、`RSS_POLL_INTERVAL=5m`、
`RSS_HTTP_TIMEOUT=20s`（正数，最大 `2m`）、`RSS_PROXY_URL=`。
群间隔覆盖持久化保存，优先于部署默认值。`RSS_ENABLED=false` 只停止后台
抓取与推送，仍可管理、查询和检查订阅；重新开启需修改部署配置并重启。
群主动消息需要机器人具备相应 QQ 权限与配额，发送失败保留待发送记录。

`RSS_PROXY_URL` 统一作用于 RSS 抓取，可填写带认证的 HTTP、HTTPS、
SOCKS5 或 SOCKS5H 地址；空值沿用环境代理，`off` 强制直连。
Go 标准库的 SOCKS5/SOCKS5H 均由代理解析目标域名。专用代理失败不会自动
退回直连，不复用 `/vendor_config proxy` 或 `/reset proxy`；
QQ、New API 等连接不受影响。该配置只从环境读取，不写入数据库，
错误回复、日志和审计不输出代理凭据或订阅 URL 参数。

每篇新文章单独发送来源、标题、纯文本摘要和原文链接。标题最多 200 字、
摘要最多 300 字（截断时附省略号）；HTML、脚本、样式和内联 @ 标签会移除，
缺失摘要或链接时省略。原文链接保留一般查询参数，但移除认证凭据、
token、API key、签名等敏感参数。没有关键词筛选、图片卡片或私聊订阅。

后台顺序抓取，每个调度轮对相同地址复用一次抓取结果；支持 ETag、
Last-Modified 条件请求。单次响应最多 2 MiB、最多 10,000 篇文章、
最多 5 次重定向。网络/解析失败不会推进基线，恢复抓取后继续识别新文章；
文章标识依次采用 GUID/Atom ID、原文链接、标题与发布时间哈希，
同标识内容修改不会重复推送。

每群每轮最多发送 10 篇，文章间隔至少 2 秒，有发布时间的文章按时间升序。
剩余与失败文章留到下一轮，群间失败相互隔离；bbolt 持久化基线和待发送内容，
重启继续未完成投递。暂停/删除完成后不再发送其待投递文章，恢复失败保持暂停。
采用至少一次投递：QQ 已接收但数据库提交前异常退出时，存在小概率重复。
订阅源仅暴露有限历史时，停机期间已从源中移除的文章无法补回；已见标识保留
至删除订阅或恢复时重新建立基线，长期高频源会增加数据库体积。

自动化覆盖：`internal/rss` 验证解析、内容清理、请求限制及代理认证；
`internal/store/rss_test.go` 验证基线、群隔离、重启恢复和失效投递；
`internal/bot/rss_test.go` 验证权限、帮助、命令、调度、重试、限速及停止；
`internal/config/rss_test.go` 验证部署选项。真实 QQ 推送需在部署环境联调。

## 全球厂商状态监控

移植自 `Futureppo/astrbot_plugin_global_status` **1.2.2**，固定源提交
`38822b2e35ad60a12392a7b12d211e748ed76e34`。Go 服务负责 QQ 命令、权限、
生命周期和 bbolt；Python 核心负责官方来源解析、事件协调、翻译和图片渲染。
Python 源码和 SVG 图标已嵌入 Go 二进制，**不需要安装 AstrBot 或另行下载插件**。

### 启用与部署

Docker 镜像已包含 Python 3.12、Pillow、aiohttp、curl_cffi、时区数据和 Noto CJK
中文字体，重新构建镜像即可。运行本机 Go 二进制时需要 Python **3.11+**：

```powershell
python -m venv .venv
.\.venv\Scripts\python.exe -m pip install -r internal/vendorstatus/requirements.txt
# .env 中填入这个解释器的绝对路径：
# VENDOR_STATUS_PYTHON=D:\code\Github\new-api-bot\.venv\Scripts\python.exe
```

Linux：

```bash
python3 -m venv .venv
.venv/bin/python -m pip install -r internal/vendorstatus/requirements.txt
# VENDOR_STATUS_PYTHON 填写 .venv/bin/python 的绝对路径。
# 系统还需安装中文字体，例如 Debian 的 fonts-noto-cjk。
```

默认每 **5 分钟**轮询，自动监控开启，但初始群白名单为空，不主动向任何群发送。
管理员在目标群执行 `/vendor_subscribe on` 即可订阅；查询使用 `/vendor_status`，
无需绑定 New API 账户，群聊和单聊均支持。
订阅参数仅接受 `on` / `off`，不接受中文参数；只读管理员不能修改订阅。
所有命令继续遵循 `/enable`、`/disable` 关键词控制。

也可配置 `VENDOR_STATUS_GROUP_WHITELIST=GROUP_OPENID_1,GROUP_OPENID_2`。
命令开启/关闭的覆盖设置与状态、逐目标送达记录、翻译缓存一起存入原有 bbolt，
**关闭环境白名单中的群后，重启不会重新订阅**。订阅变更会在每次实际投递前再次检查。
`VENDOR_STATUS_ENABLED=false` 仅关闭后台监控，查询仍可用，订阅仍可保存。

### 用命令配置全部选项

管理员可直接在群聊/单聊中使用 `/vendor_config`，无需修改 `.env`：

```text
/vendor_config help
/vendor_config show
/vendor_config card_theme liquid_glass
/vendor_config display_language zh-CN
/vendor_config poll_interval_seconds 5m
/vendor_config notify_maintenance on
/vendor_config history_lookback_hours 48
/vendor_config sources.openai off
/vendor_config custom add "我的状态页" https://status.example.com
/vendor_config custom set "我的状态页" enabled off
/vendor_config enabled off
/vendor_config enabled on
/vendor_config reset card_theme
```

**配置全局生效**，不是仅对执行命令的群生效。每个键都支持
`/vendor_config <key>` 查询和 `/vendor_config <key> <value>` 修改；布尔值可用
`true/false`、`on/off`、`1/0`。含空格的名称或路径使用引号，`""` 表示空值。
秒数配置支持整数秒或 Go duration（例如 `300`、`5m`）。
厂商功能仅支持英文命令、子命令、配置键和枚举值，不保留中文别名。
回复说明及自定义源名称等自由文本仍可使用中文；图片语言依旧可设为 `zh-CN`。

命令覆盖保存到原 bbolt 数据库，优先级为 **命令覆盖 > 环境变量 > JSON > 内置默认**。
设置后新查询采用新配置，后台正在进行的轮询会取消并按新配置重新调度；
`enabled` 支持即时启停，即使启动时设为 `false` 也可用命令开启，无需重启。
已进行的查询沿用原快照，已发出的通知不会因关闭配置被撤回。
只读管理员可以查询和查看帮助，不能修改或重置。

`/vendor_config reset <key>` 清除指定覆盖，恢复本次启动加载的部署配置；
`reset all` 清除所有命令设置和群订阅覆盖，但不清除事故、送达或翻译缓存。
整体替换/清空 `group_whitelist` 会同时清除旧的单群订阅覆盖，确保实际目标与列表一致。

全部原配置、20 个来源开关、自定义字段及新增运行参数的逐项命令表见
**`docs/vendor-status-config-commands.md`**。其中 `platform_type` 和 `platform_id`
对应固定 QQ 官方平台与当前 AppID，只支持查询；不能用状态插件命令切换宿主身份。
原 `translation_provider_id` 命令键映射到本项目的翻译模型名称。

### 状态源、主题和高级配置

保留全部 20 个来源：OpenAI、OpenRouter、Claude/Anthropic、Google Vertex AI/Gemini、
Gemini Developer API/AI Studio、Groq、Cohere、Moonshot/Kimi、MiniMax、Fireworks、
Novita、xAI、DeepSeek、Cursor、Cerebras、AWS、Azure、GitHub、Vercel、Cloudflare。
支持 Statuspage、Google Cloud、AI Studio、FlashDuty、Better Stack、Datadog 和 RSS/Atom。

复制 `vendor-status.example.json` 到 `data/vendor-status.json`，设置
`VENDOR_STATUS_CONFIG_PATH=/data/vendor-status.json`（Docker）或本机绝对路径。
可在 `sources` 中单独停用来源，并添加自定义 Statuspage：

```json
{
  "sources": {"openai": false},
  "custom_statuspage_sources": [
    {"name": "我的服务", "base_url": "https://status.example.com", "enabled": true}
  ]
}
```

JSON 在默认配置上增量覆盖；已明确设置的 `VENDOR_STATUS_*` 环境变量优先于 JSON，
数据库中的命令覆盖优先于两者。
若需用 JSON 控制语言、主题等设置，应从 `.env` 移除对应环境变量行。
沿用上游配置字段名；旧 JSON 的 `platform_type`、`platform_id`、`translation_provider_id`
不参与部署配置。命令可查询固定平台信息、用 `translation_provider_id` 设置翻译模型；
本项目 JSON 的模型配置使用 `translation.model`。
当前项目只有 QQ 官方平台，白名单必须为 `group_openid`，不接受 AstrBot UMO。

- 图片主题：`paper`（纸质公报）、`midnight`（午夜蓝图）、`porcelain`（青瓷云笺）、
  `terminal`（荧光终端）、`liquid_glass`（液态玻璃）。
- 显示语言：`bilingual`、`zh-CN`、`en-US`；图片时间精确到秒并标注 UTC 偏移。
- 翻译：单独配置 `VENDOR_STATUS_TRANSLATION_BASE_URL`（包含 `/v1`）、
  `VENDOR_STATUS_TRANSLATION_API_KEY` 和 `VENDOR_STATUS_TRANSLATION_MODEL`。
  不使用管理令牌调用模型；翻译失败或未配置模型时继续显示原文，缓存最多 2000 条，
  翻译变化不会触发新告警。命令设置的模型密钥使用 `BOT_DATA_KEY` 加密保存，
  不混入事故状态或翻译缓存；配置查询、成功回复和审计都不回显密钥。
  `translation.api_key` 仅允许管理员在机器人单聊中设置。
- 代理：可用 `/vendor_config proxy "socks5h://username:password@host:1080"`
  设置厂商状态专用代理，支持 `socks5://`（本地 DNS）和 `socks5h://`（代理 DNS），
  可包含认证；`/vendor_config proxy off` 关闭。命令设置加密保存，不回显凭据；
  带密码时建议私聊设置。仅影响手动查询及后台厂商采集（含 DeepSeek/AI Studio），
  不改变 QQ、New API 或翻译请求，也不使用 `/reset proxy`。
  未设置专用代理时沿用原有环境代理；新增依赖 `aiohttp-socks`，Docker 需重新构建。
- 字体：可通过 `VENDOR_STATUS_FONT_PATH` 指定中文字体绝对路径；默认自动寻找
  微软雅黑、Noto CJK 或苹方。Docker 已包含中文字体。

### 通知和可靠性

#### 手动查询进度

管理员可选择三种显示模式，设置全局生效、持久化保存，并从下一次查询开始应用：

```text
/vendor_config progress_mode detailed
/vendor_config progress_mode simple
/vendor_config progress_mode off
```

- `detailed`（默认）：显示具体步骤，每次步骤变化立即更新。
- `simple`：只显示“正在生成中……”，每 10 秒刷新，不显示具体步骤。
- `off`：不发送进度消息，生成完成后直接发送图片。

使用 `/vendor_config progress_mode` 查看，`/vendor_config reset progress_mode` 恢复部署默认。
三种模式均保留脱敏错误提示，`off` 不是静默丢弃异常。环境变量为
`VENDOR_STATUS_PROGRESS_MODE`，JSON 配置键为 `progress_mode`。

详细模式下，执行 `/vendor_status` 后立即显示进度。进入新的步骤时立即发送新进度消息，
随后撤回上一条；如果同一步骤持续 10 秒未变化，则重新发送当前进度并撤回旧消息。
文案示例：`正在收集 OpenAI 信息……（已完成 3/20）`、`正在翻译事件信息……`、
`正在生成状态总览图片……`、`正在发送状态总览图片……`。

图片发送成功或查询失败后清理最后一条进度。部分来源采集失败、翻译失败会即时显示
脱敏错误，不会阻止其余来源生成总览；超时、渲染或上传失败会发送具体的脱敏原因。
错误回复使用独立短超时，避免生成任务超时后连错误消息也发不出去。

自动撤回仅在群聊执行；私聊保留进度。每条进度及最终图片使用独立回复序号，
并为最终结果预留回复预算。长任务或频繁步骤更新可能转为主动消息，仍需要 QQ
相应发送权限和配额。新进度发送失败时保留旧进度；撤回失败会记录脱敏日志并重试。
这些进度不会修改事故去重、群订阅或自动告警送达状态。

保留首次存量告警选项、维护开关、去重、官方内容更新通知、明确恢复通知、
0–168 小时历史补报、连续采集异常阈值和逐目标冷却。
事件从 Feed 消失不视为恢复；结构异常或部分请求失败不会误报正常。
查询不会改变自动告警去重和送达状态。

每图最多 **5 条**，每轮每源/目标最多 **20 条**；未发送部分在后续轮次继续。
发送前等待 Go 将基线提交到 bbolt，每批成功后立即提交送达记录；失败群单独重试。
每次 QQ 投递最多等待 **30 秒**；整个 worker 默认 **10 分钟**，停机时取消后台 worker。
QQ 已接收消息但响应超时、或发送成功后检查点写入前进程崩溃，仍可能重复一次。
损坏的送达状态不会被静默丢弃并重新群发，而是让该轮失败并记录错误。
主动消息仍受 QQ 官方 API 权限、配额和群主动消息设置限制。

完整功能覆盖、验证命令和未验证项见 `docs/vendor-status-port.md`；
配置命令逐项映射见 `docs/vendor-status-config-commands.md`；上游出处和
图标许可见 `internal/vendorstatus/THIRD_PARTY.md`。

## 准备 QQ 机器人

1. 在 QQ 开放平台创建机器人，记录 AppID 和 AppSecret/ClientSecret。
2. 开通群聊消息能力，并允许 `GROUP_MESSAGE_CREATE`（兼容旧名 `GROUP_AT_MESSAGE_CREATE`）及 `GROUP_MEMBER_ADD` 对应事件。
3. 如需入群自动审批，将机器人设置为目标群管理员，并确保开放平台向 Gateway 投递 `GROUP_JOIN_REQUEST`；该事件与群/C2C 消息使用同一个 `GROUP_AND_C2C_EVENT (1<<25)` Intent。
4. 群聊中需要 @ 机器人后发送指令；官方事件会自动移除消息开头的机器人 @ 前缀。
5. QQ API v2 不提供数字 QQ 号。启动机器人后执行 `/whoami`，将输出的 OpenID 写入 `QQ_ADMIN_OPENIDS`。

管理员名单格式示例：

```dotenv
QQ_ADMIN_OPENIDS=union:ABCDEF,user:123456,member:GROUP_OPENID:MEMBER_OPENID
# 只读管理员（可选）：只能执行查询/报表类管理员命令，不能修改机器人、QQ 或 New API 状态。
QQ_READONLY_ADMIN_OPENIDS=member:GROUP_OPENID:READ_ONLY_MEMBER_OPENID
```

优先使用 `union:` 标识。纯群聊事件没有 union_openid 时，使用 `member:<group_openid>:<member_openid>`。

## 准备 New API

1. 在 New API 管理员账户的个人设置中生成“系统访问令牌”。这不是模型调用使用的 `sk-` API 令牌。
2. 将令牌写入 `NEWAPI_ADMIN_TOKEN`。
3. 将该管理员的数字用户 ID 写入 `NEWAPI_ADMIN_USER_ID`。
4. 确认管理员令牌具有用户查询和额度管理权限。
5. 确认待绑定用户已经在 New API 账户中绑定邮箱。

服务使用以下 New API 接口：

- `GET /api/status`
- `GET /api/user/{id}`
- `GET /api/user/`
- `GET /api/user/search`
- `POST /api/user/manage`
- `GET /api/data/users`
- `GET /api/data`
- `GET /api/log/`
- `GET /api/channel/models_enabled`
- `GET /api/user/models?group={group}`
- `POST /api/user/manage`（enable、disable、额度调整）
- `DELETE /api/user/{id}/2fa`
- `DELETE /api/user/{id}/reset_passkey`
- `GET /api/subscription/admin/users/{id}/subscriptions`
- `POST /api/subscription/admin/users/{id}/subscriptions`
- `POST /api/subscription/admin/user_subscriptions/{id}/invalidate`

所有受保护请求都会同时携带：

```text
Authorization: Bearer <NEWAPI_ADMIN_TOKEN>
New-Api-User: <NEWAPI_ADMIN_USER_ID>
```

## 配置

复制模板：

```bash
cp .env.example .env
```

Windows PowerShell：

```powershell
Copy-Item .env.example .env
```

`.env.example` 中每个配置项前都有中文注释，注明用途、格式、默认值和敏感性。程序启动时会自动读取工作目录下的 `.env`，但操作系统中已经存在的环境变量具有更高优先级。

### 生成 BOT_DATA_KEY

Linux、macOS 或安装了 OpenSSL 的环境：

```bash
openssl rand -base64 32
```

Windows PowerShell：

```powershell
$bytes = New-Object byte[] 32
[Security.Cryptography.RandomNumberGenerator]::Fill($bytes)
[Convert]::ToBase64String($bytes)
```

`BOT_DATA_KEY` 用于验证码 HMAC 和兼容数据保护，生产环境必须妥善备份该值。

### SMTP 加密模式

- `starttls`：先建立普通 SMTP 连接，再升级 TLS，常见端口为 587。
- `tls`：连接建立时直接使用 TLS，常见端口为 465。
- `none`：明文 SMTP，仅适用于可信内网测试环境。

### 签到与额度换算

签到奖励按 `CHECKIN_TIMEZONE` 的昨日自然日计算：`min(max(round1(昨日显示用量 × rand(1.0, 3.0)), 1), randInt(5, 10))`。随机倍数以 0.1 为步进并包含 1.0 和 3.0；上限为包含 5 和 10 的随机整数。所有计算使用显示额度，最终再换算为 New API 的整数 quota。`CHECKIN_CREDIT` 仅为旧版部署兼容项，不再影响签到结果。

`CREDIT_MAX_PER_COMMAND` 和 `/credit add` 使用 New API 页面显示的额度单位。服务读取 `/api/status` 中的 `quota_per_unit` 进行精确有理数换算，不使用浮点数。

例如 `quota_per_unit=500000` 时：

- 显示额度 `1` 对应原始 quota `500000`。
- 显示额度 `0.01` 对应原始 quota `5000`。
- 如果换算结果不是整数 quota，指令会被拒绝。

### 升级通知

容器收到 `SIGTERM` 或 `SIGINT` 时，会先向已识别的 QQ 群发送“机器人正在更新！”，并把待完成通知持久化到 bbolt。新进程连接 QQ Gateway 后发送“机器人更新完毕！”并清除记录；首次启动和异常退出不会误发更新完成通知。`docker-compose.yml` 默认提供 45 秒停止宽限期。

### 额度提醒

- 用户在希望接收提醒的群内执行 `/notify quota <额度>`，阈值使用站点显示额度。
- 后台默认每 10 分钟检查一次，可通过 `NOTIFY_CHECK_INTERVAL` 调整。
- 余额首次低于或等于阈值时，机器人会在设置提醒的群内发送一次通知。
- 提醒后不会重复刷屏；账户充值并重新高于阈值后会自动恢复监控，下次再次低于阈值时重新提醒。
- `/notify quota off` 会删除提醒配置；用户解绑时也会自动删除对应提醒。
- `/notify daily on` 会在 `NOTIFY_DAILY_TIME` 发送当天请求、Token、额度和余额摘要；同群消息会合并，并受 `NOTIFY_GROUP_COOLDOWN` 控制。

### 群欢迎、状态和报表

- `/welcome on` 会在 QQ `GROUP_MEMBER_ADD` 事件到达时发送主动群消息，并使用 QQ 当前的 `<qqbot-at-user id="..." />` 文本协议实际 @ 新成员；`/welcome set` 可为每个群保存独立欢迎语。机器人日志会记录群成员事件、欢迎设置状态和发送失败原因，便于排查 QQ 平台未投递事件或主动消息配额问题。
- `/bot status` 会优先查询 QQ 的群内机器人状态和群基础信息。相关接口未获得开放权限时，仍会返回 Gateway、Access Token 和 New API 连通状态。
- `/usage chart 7d`、`/usage chart 7d @某成员` 和 `/usage chart 7d all` 将 PNG 上传到当前群；`all` 仅统计当前群内已被机器人识别且已绑定 New API 的成员。`/admin report export 7d` 将 CSV 文件上传到当前群。需要 QQ 机器人具备群文件/富媒体接口权限。
- `/recall` 仅撤回机器人自己发送且不超过两分钟的消息。
- 禁用用户、重置 2FA 和重置 Passkey 使用 `/confirm <code>` 文本确认，并写入本地审计记录。

### 入群审批和群禁言

- QQ 官方在 2026-08-10 新增 `GROUP_JOIN_REQUEST`、入群申请审批和群禁言接口，并将所有 HTTP API 域名统一为 `api.bot.qq.com`；本项目已使用统一域名。
- 自动审批默认对所有群关闭。管理员需在目标群执行 `/join on`，关闭时使用 `/join off`。
- 开启后，机器人从验证消息或管理员问答答案中查找完整邮箱或正整数 New API 用户 ID；账户存在且状态正常时才调用 QQ `approve`。不匹配、账户禁用、申请人为机器人、QQ 返回 `risk_tips` 或接口查询失败时均保留为人工审核。
- `/join check "内部用户"` 会额外要求验证消息或任一管理员问答答案包含大小写敏感的字面字符串 `内部用户`；使用 `/join check ""` 清除该限制。
- `/join limit 20` 会额外要求 QQ 入群申请事件中的 `qq_level` 或 `level` 至少为 20；使用 `/join limit 0` 清除该限制。当前 QQ 官方 `GROUP_JOIN_REQUEST` 文档未承诺提供用户 QQ 等级，因此阈值大于 0 且事件缺少等级字段时，机器人会保留该申请等待人工审核，不会猜测等级或自动放行。
- 自动审批不会建立 QQ 与 New API 的绑定关系；入群后仍需执行 `/bind` 完成邮箱验证。
- 入群申请事件和审批接口都要求机器人是目标群管理员。事件按 `group_openid`、`member_openid` 和 `join_request_id` 去重，避免重复审批。
- `/mute` 使用 QQ `/v2/groups/{group_openid}/restrict_chat_setting` 接口，只能操作普通成员，不能禁言群主、管理员或机器人；QQ 返回的权限或参数错误会直接回复执行者。
- 新增的 Markdown 参数 `force_verify_image_resource` 仅影响 Markdown 图片资源转存；当前机器人发送文本和上传文件，不受该参数影响。

### 群福利兑换码

- 管理员使用 `/benefit 1 20 24 7` 可生成 20 个面额为 1、有效期 24 小时的兑换码，并在群内先 @全体成员，再逐行发送兑换码。
- 每个用户限领一个。后台按照 `BENEFIT_CHECK_INTERVAL` 查询 New API 的充值日志，并通过活动对应的兑换码 ID 判断领取次数。
- 同一用户在活动有效期内兑换两个或以上活动兑换码时，机器人会在发放群公布用户 ID、违反规则和封禁天数，并调用 New API 禁用用户。
- 封禁记录持久化在 bbolt；达到解封时间后自动重新启用用户并发送群消息，机器人重启不会丢失封禁计划。
- 兑换码在本地数据库中使用 AES-256-GCM 加密保存；日志不输出完整兑换码。

### Codex 重置监测与补偿

- 首次在某个群执行 `/reset check`、`/reset join` 或管理员设置命令时，该群会自动登记为重置通知群。`/reset last` 只查询 `https://codex-reset.com/api/timeline` 的最新重置事件，不登记群、不改变状态，也不会创建活动。没有登记群时后台不会发起监测请求。
- `/reset last` 展示 API 最新重置事件的公告时间、机器人状态、API 验证状态、摘要和官方时间窗口；该查询不受 `RESET_SIGNAL_MAX_AGE` 限制。
- 后台默认每 `3m` 请求一次 Codex Reset timeline API，只处理最近 `24h` 的全局 reset 事件。单次响应限制为 `512 KiB`，使用一个受限 HTTP 连接池，不运行浏览器、Node 或其他常驻进程。
- 状态分为：未知、可能重置、即将重置、确认重置（抽奖进行中）。API 的 pending、hinted 和仍有效的官方时间窗口映射为候选状态；只有 `confirmed` 或 `reset_observed` 才会启动活动。`rejected`、`unchanged`、`unverified` 或 `expired` 会将对应候选恢复为未知并发送更正通知，已经开始或正在结算的活动不会被自动取消。
- timeline 事件以稳定事件 ID 去重；同一事件从可能重置升级为即将重置或确认重置时只通知一次对应变化。部署或重启后不会把超出 `RESET_SIGNAL_MAX_AGE` 的历史确认事件补建为新活动。
- 活动默认持续 `5h`，随机抽取最多 `5` 名参与者。每名获奖者获得从活动开始前 `24h` 到活动开始时刻的实际消耗额度，活动进行期间产生的用量不计入补偿；参与人数不足时抽取全部参与者，消耗为零时不调用额度写入接口。
- 管理员执行 `/reset new` 可立即手动开启活动；活动会读取执行时该群通过 `/reset set duration`、`winners` 和 `lookback` 保存的配置。已有活动正在进行或结算时不会重复创建。
- 管理员执行 `/reset stop` 会原子停止仍在进行的活动、取消后续结算并将群状态恢复为未知，不抽取用户也不发放额度；已经进入结算阶段的活动不会被强行停止。
- 管理员执行 `/reset end` 会立即关闭参加入口并唤醒持久化结算流程，按原活动配置抽取用户和发放额度；服务异常重启后仍会继续未完成的结算。
- 中奖名单、补偿额度和逐人发放状态会先写入 bbolt。服务重启不会重新抽取；额度写入超时等结果不确定的情况会标记待确认，不会自动重复加额。
- 重置信号、活动开始和活动结束通知使用持久化 outbox；正文与分块边界会冻结保存，每发送一块就持久化游标，QQ 暂时发送失败或服务重启后会从未完成分块继续退避重试。
- QQ 主动群消息接口没有可查询的幂等键，因此若消息已被 QQ 接收、但进程在写入发送游标前异常退出，该分块存在极小概率重复。服务采用至少一次投递以避免通知静默丢失。
- `/reset proxy http://user:password@host:port` 与 `/reset proxy socks5://user:password@host:port` 均受支持；用户名或密码中的特殊字符需使用 URL 编码。代理只用于访问 Codex Reset timeline API，QQ 和 New API 始终使用各自现有连接。代理完整地址使用 `BOT_DATA_KEY` 加密保存，回复与日志不显示密码。
- 可通过 `RESET_ENABLED`、`RESET_POLL_INTERVAL`、`RESET_HTTP_TIMEOUT`、`RESET_SIGNAL_MAX_AGE`、`RESET_DEFAULT_DURATION`、`RESET_DEFAULT_WINNERS` 和 `RESET_DEFAULT_LOOKBACK` 调整全局默认值。管理员的群内设置只影响后续新活动。

## Docker Compose 部署

1. 创建并填写 `.env`。
2. 创建数据目录：

```bash
mkdir -p data
```

3. 构建并启动：

```bash
docker compose up -d --build
```

4. 查看日志：

```bash
docker compose logs -f bot
```

5. 检查状态：

```bash
curl http://127.0.0.1:18080/healthz
curl http://127.0.0.1:18080/readyz
```

数据库保存在宿主机 `./data/bot.db`。升级或迁移前同时备份数据库和 `BOT_DATA_KEY`。

## 直接运行

要求 Go 1.23 或更新的兼容版本：

```bash
go build -trimpath -ldflags="-s -w" -o bin/new-api-bot ./cmd/bot
./bin/new-api-bot
```

Windows PowerShell：

```powershell
go build -trimpath -ldflags="-s -w" -o bin/new-api-bot.exe ./cmd/bot
./bin/new-api-bot.exe
```

## 绑定流程

1. 用户在群内 @ 机器人并发送：`/bind user@example.com` 或 `/bind 123`。
2. 机器人通过管理员接口确认用户存在、已启用且有邮箱。
3. 机器人通过 SMTP 向账户邮箱发送六位验证码。
4. 用户在同一群内 @ 机器人并发送：`/bind verify 123456`。
5. 验证通过后写入双向唯一绑定。

验证码只以 HMAC 摘要保存。连续输错达到 `BIND_CODE_MAX_ATTEMPTS` 后，本次绑定请求立即失效。

## 签到一致性

- 签到同时按 QQ 主身份、New API 用户 ID 和周期键去重。
- 签到查询 `CHECKIN_TIMEZONE` 下昨日 `[00:00, 今日00:00)` 的用户总用量，换算为显示额度后按 `min(max(round1(昨日显示用量 × rand(1.0,3.0)), 1), randInt(5,10))` 计算奖励。
- 签到通过 New API `add_quota` 操作直接增加绑定用户额度，不创建兑换码。
- 已完成签到时重复执行只返回本周期已签到，不会再次增加额度。
- 明确的 New API 请求失败会撤销本地待处理记录，用户可稍后重试。
- 请求在等待响应头时超时，机器人会将签到标记为“待确认”并禁止本周期重试，避免 New API 已完成加额而响应丢失时重复发放。管理员核对到账情况后再处理。
- 群聊中的签到、`/help` 和无效命令（未知指令、`/help` 格式错误、指令过长）回复会在 `CHECKIN_AUTO_RECALL_AFTER`（默认 `30s`）后自动撤回，同时一并撤回触发回复的指令消息；设为 `0s` 可禁用。未知指令无需绑定即可收到提示。QQ 官方 API 仅支持撤回群消息且限发送后 2 分钟内，因此私聊回复不受影响。

## 健康检查

Docker Compose 默认仅在服务器本机的 `127.0.0.1:18080` 暴露健康检查端口，可通过 `HEALTH_HOST_PORT` 调整。

### `GET /healthz`

检查进程和 bbolt 数据库是否可用。数据库正常时返回 HTTP 200。

### `GET /readyz`

检查：

- bbolt 数据库；
- QQ Gateway 连接；
- QQ Access Token；
- New API `/api/status`。

任一检查失败时返回 HTTP 503，并在 JSON 中说明失败项目。响应不会包含凭据。

## 日志与数据保护

- 日志使用单行 JSON，方便 Docker、Loki 或其他日志系统采集。
- 不记录管理员 Token、QQ AppSecret、SMTP 密码、验证码和完整邮箱。
- bbolt 数据库默认权限为 `0600`，数据目录默认权限为 `0700`。
- 管理员加额度、解绑、绑定和签到操作会写入本地审计桶；为限制数据库长期增长，仅保留最近 10,000 条审计记录。
- 过期验证码、关联码、管理员确认码、邮件限流记录和机器人消息撤回索引由后台每小时清理。

## 测试

运行全部测试：

```bash
go test ./...
```

运行竞争检测：

```bash
go test -race ./...
```

构建所有包：

```bash
go build ./...
```

厂商状态核心及嵌入式 worker 端到端测试（仅访问本地 Mock，不向 QQ 发消息）：

```powershell
.\.venv\Scripts\python.exe -m pip install -r internal/vendorstatus/requirements-dev.txt
.\.venv\Scripts\python.exe -m pytest internal/vendorstatus/worker -q
$env:VENDOR_STATUS_TEST_PYTHON = (Resolve-Path .\.venv\Scripts\python.exe).Path
go test ./...
```

Linux 使用 `.venv/bin/python`；设置
`VENDOR_STATUS_TEST_PYTHON="$(pwd)/.venv/bin/python"` 后运行 Go 测试。
未配置测试解释器时仅跳过跨进程 Python 集成测试，协议、存储、命令等 Go 测试仍运行。
Python 在线状态接口测试默认跳过，显式设置 `GLOBAL_STATUS_LIVE=1` 才访问厂商官方端点。

只读检查公开测试实例：

```bash
curl https://ai.fsykk.cn/api/status
```

真实额度写入测试必须使用专用管理员测试凭据；自动化测试默认使用本地 Mock，不修改公开实例数据。

## 资源控制

Docker Compose 默认设置：

```dotenv
GOMEMLIMIT=64MiB
GOGC=50
GOMAXPROCS=2
```

服务使用两个命令工作协程、长度 64 的有界内存队列、最多四个每主机 HTTP 连接，并将用量图表生成限制为单任务执行；第二个图表请求会立即返回忙碌提示，不占用另一个 worker 等待。非 `/` 消息在进入队列和 bbolt 去重前直接忽略，超过 4096 字节的指令只保留必要元数据并回复长度错误。QQ HTTP/WebSocket 响应限制为 1 MiB，New API 响应限制为 8 MiB，并使用流式受限解码降低峰值内存。

全球厂商状态任务共用一个可取消的单任务槽位。Python worker 仅在查询/轮询时启动，
完成即退出，不常驻等待；但图片渲染会增加瞬时内存，**`GOMEMLIMIT` 只约束 Go，
不包含 Python 子进程**。需要保持原有纯 Go 运行资源占用时，可关闭
`VENDOR_STATUS_ENABLED`，并避免执行厂商状态查询；新 Docker 镜像因包含 Python 和
中文字体而比原 distroless 镜像大。

Gateway 序号最多每秒持久化一次，并在断线时强制保存；健康连接建立后会重置重连退避。待处理事件会使用 `BOT_DATA_KEY` 加密，并与去重状态在同一个 bbolt 事务中写入持久化收件箱；内存队列满时事件仍可落盘并由后台调度，进程异常退出后也会恢复处理。持久化待处理事件固定上限为 512 条；达到上限时 Gateway 才会退避重连，避免无界占用磁盘或内存。

## 停止与备份

收到 SIGINT 或 SIGTERM 后，服务会：

1. 停止 QQ Gateway 接收新事件。
2. 最多等待 30 秒完成已经入队的命令，超时后取消剩余上游请求。
3. 关闭健康检查 HTTP 服务。
4. 同步并关闭 bbolt 数据库。

Docker Compose 为该流程配置了 45 秒的 `stop_grace_period`。

备份时复制：

- `data/bot.db`
- 部署环境中的 `BOT_DATA_KEY`
- `.env` 中的服务凭据

不要把真实 `.env`、数据库或日志中的敏感信息提交到 Git 仓库。
