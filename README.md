# QQ × New API 机器人后端

一个面向 [QQ 机器人 API v2](https://bot.q.qq.com/wiki/develop/api-v2/) 与 [New API 管理接口](https://docs.newapi.ai/zh/docs/api) 的低内存 Go 后端。机器人在群聊中完成邮箱验证码绑定、直接加额度签到、额度管理和管理员审计。

## 推荐 AI 中转站

需要便捷、稳定的 AI API 中转服务，可以访问 **[FSY AI（ai.fsykk.cn）](https://ai.fsykk.cn)**。支持多种主流模型，可用于 API 调用、开发测试及日常使用。

## 功能

- QQ 群聊 @ 消息，使用 WebSocket Gateway 接收事件；仅处理并回复以 `/` 开头的指令消息。
- QQ Access Token 自动刷新、心跳、Resume、断线重连与消息去重。
- 一个 QQ 主身份与一个 New API 用户 ID 的双向唯一绑定。
- SMTP 邮箱验证码，每个 QQ 身份和目标账户每小时默认最多发送两封。
- 按自然日、自然周或自然月直接给已绑定 New API 用户增加签到额度。
- 管理员增加额度、查询额度、查看绑定和解除绑定。
- bbolt 单文件持久化、AES-256-GCM 敏感数据加密、JSON 结构化日志。
- `/healthz` 和 `/readyz` 健康检查。

## 指令

| 指令 | 场景 | 说明 |
| --- | --- | --- |
| `/bind <邮箱或用户ID>` | 群聊 | 向 New API 账户邮箱发送绑定验证码 |
| `/bind vertify <6位验证码>` | 群聊 | 在当前群完成双向唯一绑定 |
| `/bind status` | 群聊 | 查看当前绑定的 New API 用户信息 |
| `/unbind` | 群聊 | 解除当前 QQ 身份的 New API 绑定 |
| `/checkin` | 群聊 | 签到并直接增加绑定账户额度 |
| `/checkin status` | 群聊 | 查看当前周期签到状态 |
| `/me` | 群聊 | 查看绑定账户及额度 |
| `/whoami` | 任意 | 查看可写入管理员名单的 OpenID |
| `/help` | 任意 | 查看指令说明 |
| `/credit add <用户ID或@用户> <额度>` | 管理员 | 增加用户额度 |
| `/credit sub <用户ID或@用户> <额度>` | 管理员 | 在余额不会变成负数时扣除用户额度 |
| `/credit show <用户ID或@用户>` | 管理员 | 查询用户额度 |
| `/admin bindings [页码]` | 管理员 | 分页查看绑定 |
| `/admin unbind <用户ID>` | 管理员 | 解除绑定 |

除 `/help`、`/whoami` 和 `/bind` 外，所有指令都要求执行者已经绑定。管理员指令还要求执行者命中 `QQ_ADMIN_OPENIDS`。

## 准备 QQ 机器人

1. 在 QQ 开放平台创建机器人，记录 AppID 和 AppSecret/ClientSecret。
2. 开通群聊消息能力，并允许 `GROUP_MESSAGE_CREATE`（兼容旧名 `GROUP_AT_MESSAGE_CREATE`）对应事件。
3. 群聊中需要 @ 机器人后发送指令；官方事件会自动移除消息开头的机器人 @ 前缀。
4. QQ API v2 不提供数字 QQ 号。启动机器人后执行 `/whoami`，将输出的 OpenID 写入 `QQ_ADMIN_OPENIDS`。

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
- `GET /api/user/search`
- `POST /api/user/manage`

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
4. 用户在同一群内 @ 机器人并发送：`/bind vertify 123456`。
5. 验证通过后写入双向唯一绑定。

验证码只以 HMAC 摘要保存。连续输错达到 `BIND_CODE_MAX_ATTEMPTS` 后，本次绑定请求立即失效。

## 签到一致性

- 签到同时按 QQ 主身份、New API 用户 ID 和周期键去重。
- 签到通过 New API `add_quota` 操作直接增加绑定用户额度，不创建兑换码。
- 已完成签到时重复执行只返回本周期已签到，不会再次增加额度。
- 已知的 New API 请求失败会撤销本地待处理记录，用户可稍后重试。

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
- 管理员加额度、解绑、绑定和签到操作会写入本地审计桶。

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
```

服务使用两个命令工作协程、长度 64 的有界队列、最多两个每主机空闲 HTTP 连接，并限制单个 HTTP/WebSocket 消息为 1 MiB。可根据实际消息量调整 Go 运行时参数，但不建议无界增加协程或队列。

## 停止与备份

收到 SIGINT 或 SIGTERM 后，服务会：

1. 停止 QQ Gateway 接收新事件。
2. 等待已经入队的命令处理完成。
3. 关闭健康检查 HTTP 服务。
4. 同步并关闭 bbolt 数据库。

备份时复制：

- `data/bot.db`
- 部署环境中的 `BOT_DATA_KEY`
- `.env` 中的服务凭据

不要把真实 `.env`、数据库或日志中的敏感信息提交到 Git 仓库。
