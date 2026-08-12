# QQ × New API 机器人后端

一个面向 [QQ 机器人 API v2](https://bot.q.qq.com/wiki/develop/api-v2/) 与 [New API 管理接口](https://docs.newapi.ai/zh/docs/api) 的低内存 Go 后端。机器人在群聊中完成邮箱验证码绑定、直接加额度签到、用量查询、额度提醒、订阅管理和管理员审计。

## 推荐 AI 中转站

需要便捷、稳定的 AI API 中转服务，可以访问 **[FSY AI（ai.fsykk.cn）](https://ai.fsykk.cn)**。支持多种主流模型，可用于 API 调用、开发测试及日常使用。

## 功能

- QQ 群聊 @ 消息，使用 WebSocket Gateway 接收事件；仅处理并回复以 `/` 开头的指令消息。
- QQ Access Token 自动刷新、心跳、Resume、断线重连与消息去重。
- 一个 QQ 主身份与一个 New API 用户 ID 的双向唯一绑定。
- SMTP 邮箱验证码，每个 QQ 身份和目标账户每小时默认最多发送两封。
- 按自然日、自然周或自然月直接给已绑定 New API 用户增加签到额度。
- 管理员增加/扣除额度、管理用户订阅、查看绑定和解除绑定。
- 查询个人、指定用户或全站用户用量，查看调用记录和站点已启用模型。
- 在指定群内发送低额度提醒；管理员可查看全站用户与模型用量报表。
- 支持 QQ 2026-08-10 新增的入群申请事件与审批接口；可按群开启 New API 邮箱/用户 ID 自动核验。
- 支持 QQ 官方群成员禁言状态查询、定时禁言和解除禁言接口。
- 低资源监测 Codex 重置信号，并按群发起可恢复的用量补偿抽奖。
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
| `/help` | 任意 | 查看指令说明 |
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
| `/admin checkin` | 管理员 | 查看当天签到人数、已发放总额度和当前单次发放额度 |
| `/admin checkin edit <发放额度>` | 管理员 | 立即更新后续签到的单次发放额度，并持久化保存 |
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
| `/reset join` | 已绑定用户 | 参加当前群有效期内正在进行的重置补偿抽奖 |
| `/reset set duration <时长>` | 管理员 | 设置下一轮活动有效期，默认 `5h` |
| `/reset set winners <人数>` | 管理员 | 设置下一轮抽取人数，默认 `5` |
| `/reset set lookback <时长>` | 管理员 | 设置获奖者用量补偿回溯时间，默认 `24h` |
| `/reset proxy <代理链接或off>` | 管理员 | 设置仅用于 X 检测的 HTTP/SOCKS5 代理，凭据加密保存 |

除 `/help`、`/whoami`、`/bind`、`/reset check`、`/enable list`、`/disable list` 以及管理员的 `/enable`、`/disable` 管理操作外，所有指令都要求执行者已经绑定。管理员指令还要求执行者命中 `QQ_ADMIN_OPENIDS`。

命令关键词状态持久化保存在 bbolt。示例：管理员执行 `/disable "bind view"` 后，标准化内容中包含 `bind view` 的指令会被静默忽略，`/help` 中匹配该关键词的行也会隐藏；执行 `/enable "bind view"` 即可恢复。匹配不区分英文大小写，并会合并连续空白字符。`/enable` 和 `/disable` 管理指令本身始终可执行，避免规则将管理入口锁死。

所有以“用户ID”为目标的管理指令均可在群聊中使用 `@群成员` 代替数字 New API 用户 ID；机器人会读取该群成员已经建立的绑定。`/bind <邮箱或用户ID>` 是例外，只接受邮箱或数字 New API 用户 ID，不能使用 `@群成员`。

用量时间长度支持 `30m`、`24h`、`7d`、`4w`、`today`、`week` 和 `month` 等格式，最长查询 31 天。`/usage 7d all` 只返回最近 7 天的全站汇总；`/usage 7d 10` 返回按消耗额度从高到低排列的前 10 名用户。排行榜数量范围为 1 到 100，`10`、`top10`、`前10名` 三种写法均可。全站汇总和排行榜对所有已绑定用户开放。

## 准备 QQ 机器人

1. 在 QQ 开放平台创建机器人，记录 AppID 和 AppSecret/ClientSecret。
2. 开通群聊消息能力，并允许 `GROUP_MESSAGE_CREATE`（兼容旧名 `GROUP_AT_MESSAGE_CREATE`）及 `GROUP_MEMBER_ADD` 对应事件。
3. 如需入群自动审批，将机器人设置为目标群管理员，并确保开放平台向 Gateway 投递 `GROUP_JOIN_REQUEST`；该事件与群/C2C 消息使用同一个 `GROUP_AND_C2C_EVENT (1<<25)` Intent。
4. 群聊中需要 @ 机器人后发送指令；官方事件会自动移除消息开头的机器人 @ 前缀。
5. QQ API v2 不提供数字 QQ 号。启动机器人后执行 `/whoami`，将输出的 OpenID 写入 `QQ_ADMIN_OPENIDS`。

管理员名单格式示例：

```dotenv
QQ_ADMIN_OPENIDS=union:ABCDEF,user:123456,member:GROUP_OPENID:MEMBER_OPENID
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

### 额度换算

`CHECKIN_CREDIT`、`CREDIT_MAX_PER_COMMAND` 和 `/credit add` 使用 New API 页面显示的额度单位。服务读取 `/api/status` 中的 `quota_per_unit` 进行精确有理数换算，不使用浮点数。

例如 `quota_per_unit=500000` 时：

- 显示额度 `1` 对应原始 quota `500000`。
- 显示额度 `0.01` 对应原始 quota `5000`。
- 如果换算结果不是整数 quota，指令会被拒绝。

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

- 首次在某个群执行 `/reset check`、`/reset join` 或管理员设置命令时，该群会自动登记为重置通知群。没有登记群时后台不会发起监测请求。
- 后台默认每 `3m` 顺序检查 `X @thsottiaux`、`X @OpenAI`、`X @OpenAIDevs`、`codexreset.org` 和 OpenAI Status，只处理最近 `24h` 的相关信号。网络响应有严格大小限制，不运行浏览器、Node 或其他常驻进程。
- 状态分为：未知、可能重置、即将重置、确认重置（抽奖进行中）。可能或即将重置信号过期后恢复未知；确认信号为每群开启一轮活动，活动结算后立即恢复未知。
- 活动默认持续 `5h`，随机抽取最多 `5` 名参与者。每名获奖者获得该活动结束前近 `24h` 的实际消耗额度；参与人数不足时抽取全部参与者，消耗为零时不调用额度写入接口。
- 中奖名单、补偿额度和逐人发放状态会先写入 bbolt。服务重启不会重新抽取；额度写入超时等结果不确定的情况会标记待确认，不会自动重复加额。
- 重置信号、活动开始和活动结束通知使用持久化 outbox；正文与分块边界会冻结保存，每发送一块就持久化游标，QQ 暂时发送失败或服务重启后会从未完成分块继续退避重试。
- QQ 主动群消息接口没有可查询的幂等键，因此若消息已被 QQ 接收、但进程在写入发送游标前异常退出，该分块存在极小概率重复。服务采用至少一次投递以避免通知静默丢失。
- `/reset proxy http://user:password@host:port` 与 `/reset proxy socks5://user:password@host:port` 均受支持；用户名或密码中的特殊字符需使用 URL 编码。代理只用于访问 X，OpenAI Status、聚合站、QQ 和 New API 始终直连。代理完整地址使用 `BOT_DATA_KEY` 加密保存，回复与日志不显示密码。
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
- 签到通过 New API `add_quota` 操作直接增加绑定用户额度，不创建兑换码。
- 已完成签到时重复执行只返回本周期已签到，不会再次增加额度。
- 明确的 New API 请求失败会撤销本地待处理记录，用户可稍后重试。
- 请求在等待响应头时超时，机器人会将签到标记为“待确认”并禁止本周期重试，避免 New API 已完成加额而响应丢失时重复发放。管理员核对到账情况后再处理。

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
