# 全球厂商状态：移植覆盖与验证

## 范围

- [已验证 E1] 上游：`Futureppo/astrbot_plugin_global_status` 1.2.2，
  提交 `38822b2e35ad60a12392a7b12d211e748ed76e34`。
  来源说明：`internal/vendorstatus/THIRD_PARTY.md`。
- Go 管理宿主功能；源解析、协调和渲染复用 Python 核心，无 AstrBot 依赖。
- 本项目仅支持 QQ 官方平台；多适配器、平台实例选择及 UMO 路由不是本项目功能，
  替换为原生 QQ `group_openid`。上游 `/sid` 为 AstrBot 自带命令，非该插件功能，
  可使用本项目已有 `/whoami` 中的 `member:<group_openid>:<member_openid>` 获取群标识。
- AstrBot 默认对话模型替换为单独配置的聊天模型 API，不复用 New API 管理令牌。

## 功能覆盖矩阵

| 上游功能 | 移植实现 | 测试证据 |
| --- | --- | --- |
| 20 个内置源与启用开关 | E2：`worker/statusmonitor/sources.py`，`vendorstatus/config.go` | `test_sources.py`、`test_betterstack.py`、`test_datadog_status.py`、Go 配置测试 |
| Statuspage 与自定义源 | E2：`sources.py`，`DecodeConfig` | 来源构建验证、Mock HTTP 端到端 |
| Google Cloud / AI Studio | E2：`sources.py`、`modern_sources.py` | `test_modern_sources.py`、真实数据快照 fixtures |
| DeepSeek FlashDuty 与 RSS/Atom | E2：`modern_sources.py`、`sources.py` | 历史/当前合并、格式漂移、明确恢复、维护测试 |
| Novita Better Stack / OpenRouter Datadog | E2：`betterstack.py`、`datadog_status.py` | 时间线、组件、维护、部分历史失败、结构异常测试 |
| 首次存量/静默基线 | E3：`main.py::_run_cycle` | `test_main.py` |
| 严重度/服务/正文变化去重 | E3：`sources.py::Issue.fingerprint`、`monitor_state.py` | 来源、协调、翻译不影响指纹测试 |
| 消失不恢复、保留未确认故障 | E3：`monitor_state.py::reconcile_source` | `test_monitor_state.py`、`test_source_reliability.py` |
| 明确恢复和历史补报 | E3：`monitor_state.py` | 补报窗口、迁移基线、重启、过期和容量测试 |
| 每图 5 条/每轮 20 条及队列界限 | E3：`main.py::_event_batches`、`monitor_state.py` | `test_delivery_bounds.py` |
| 多目标隔离、失败重试、送达检查点 | E4：`bridge.go::consumePackets`、`main.py::_run_cycle` | Python 中断恢复；Go 本地端到端重新启动 worker 后只重试失败目标 |
| 写盘失败阻止发送 | E4：同步 checkpoint/ack 协议 | Python/Go 存储失败测试 |
| 采集健康阈值、冷却及恢复 | E3：`plan_health_notices`、`mark_health_delivered` | `test_monitor_integration.py`、`test_monitor_state.py` |
| 查询不改变告警状态 | E4：`vendor_status`、Go 协议拒绝查询写状态 | Python/Go 只读查询和 PNG 端到端 |
| 双语/中文/英文、五种主题 | E5：`renderer.py` | `test_renderer.py`、五主题实际 PNG 渲染 |
| 本地 SVG、中文字体、秒级时区 | E5：`renderer.py`、Docker Noto CJK | SVG、厂商图标、时区、长正文布局测试 |
| AI 翻译批次、缓存、失败降级 | E6：`translation.py`、`worker.py::ChatProvider` | 缓存、模型选择、指纹隔离、专用凭据测试 |
| worker 最小凭据范围 | E6：`bridge.go::workerEnvironment` | 不继承 QQ/SMTP/New API 管理/数据库密钥，仅保留网络和运行时环境测试 |
| 英文查询命令、群聊及单聊 | E7：`bot/vendor_status.go`、`qq/client.go` | `/vendor_status` 未绑定调用、两场景图片上传及回复 ID 测试 |
| 管理员群订阅、精确参数、幂等 | E7：`handleVendorSubscription` | 普通用户/私聊/只读管理员拒绝、非法参数无修改、重复操作测试 |
| 重启保持订阅、取消环境白名单 | E7：`store/vendor_status.go` | 关闭/重新打开 bbolt，覆盖默认白名单测试 |
| 取消后停止投递 | E7：每次 `Send` 前重新核验订阅 | 轮询中取消订阅测试 |
| 自动轮询、优雅停机 | E8：`runVendorStatusWorker`、Go Service 生命周期 | 取消进行中的 worker、单任务槽 deadline 测试 |
| 原机器人命令启停、权限和消息撤回记录 | E7：`service.go`、`sendVendorImage` | 命令静默禁用、只读控制与原有全量 Go 回归 |
| 全部配置对应命令、热修改和重置 | E11：`bot/vendor_config.go`、`vendorstatus/options.go` | 原 schema 18 项覆盖、20 来源开关、自定义字段、全部新增参数；详见配置命令文档 |
| 运行时配置持久化、密钥加密、热启停 | E11：`store/vendor_runtime.go`、`bot/vendor_runtime.go` | 数据库重开、密钥不回显、并发不丢更新、白名单完整替换、启停取消测试 |
| Docker、本机运行、配置迁移 | E8：Dockerfile、`.env.example`、JSON 示例、README | 配置默认/覆盖/错误验证，Linux 静态交叉构建；镜像实测见下方待验证项 |

`worker/statusmonitor/` 为 `internal/vendorstatus/worker/statusmonitor/` 的简写。
纯函数源码仅进行格式整理；宿主相关 import、字体路径、命令和持久化接入详见出处说明。

## 可复现验证

全部配置项的命令表见 `docs/vendor-status-config-commands.md`。

```powershell
python -m venv .venv
.\.venv\Scripts\python.exe -m pip install -r internal/vendorstatus/requirements-dev.txt
.\.venv\Scripts\python.exe -m pytest internal/vendorstatus/worker -q
.\.venv\Scripts\python.exe -m ruff check internal/vendorstatus/worker
.\.venv\Scripts\python.exe -m ruff format --check internal/vendorstatus/worker
$env:VENDOR_STATUS_TEST_PYTHON = (Resolve-Path .\.venv\Scripts\python.exe).Path
go test ./...
go vet ./...
go build ./...
```

端到端测试 `TestEmbeddedPythonWorkerEndToEnd` 使用本地 `httptest` 状态服务器，
真实运行二进制内嵌 Python 模块，验证 PNG、检查点、失败目标独立重试、只读查询、
明确恢复和恢复去重。测试不会投递 QQ 消息，也不会调用翻译模型。

### 2026-10-01 本地实际结果

- [已验证 E9] `go test -count=1 ./...`、`go test -race ./...`、
  `go vet ./...`、`go build ./...` 全部成功；Go 测试包含真实 Python 子进程集成。
- [已验证 E9] Python：**136 passed, 7 skipped**；7 项跳过均为显式 opt-in 在线测试。
  Ruff lint、format check 与 `git diff --check` 通过。
- [已验证 E9] 已构建 Windows 可执行文件
  `bin/new-api-bot-vendor-status.exe` 和 `CGO_ENABLED=0` 的 Linux amd64
  `bin/new-api-bot-vendor-status-linux`，实际 SHA256 保存在本地构建证据。
- [已验证 E9] 五主题告警和 20 来源总览 PNG 已实际渲染并查看，中文、双语、
  来源图标与完整列表可见。图片使用演示数据，不是在线厂商状态。
- [已验证 E10] 对五个采集/协调模块进行 AST 比较，与上游提交完全一致；
  渲染器除字体查找外，其余 **27 个函数** AST 一致。

本地具体证据在 `.codex/vendor-status-evidence/`：
`go-tests.txt`、`go-race.txt`、`python-tests.txt`、`build-sha256.txt`、
`upstream-ast-parity.txt`、`themes-contact.png`、`overview-paper.png`。
该目录是本地任务证据，不进入 Docker 镜像；可按上述命令独立复现。

## 待验证 / 运维 Backlog

- [未验证] 真实 QQ 群/单聊图片、主动推送权限及配额：需要部署凭据；
  当前只通过本地协议、HTTP Mock 和客户端上传测试验证。
- [未验证] Docker 镜像实测：本机 Docker daemon 未运行；
  Dockerfile 已更新，但不能以配置存在代替成功构建。
- [未验证] 厂商官方在线接口持续可达性：默认测试的 7 项在线用例跳过；
  显式设置 `GLOBAL_STATUS_LIVE=1` 可运行，格式变动或网络异常会触发采集健康告警。
- [未执行] GitHub Actions 远端运行：已添加工作流，尚未提交/推送触发。
- [已知语义] QQ 已接收但响应超时，或外部发送成功后本地检查点尚未落盘时崩溃，
  仍可能产生一次重复通知；这是外部消息发送与本地数据库不能原子提交的边界。
- [已知资源差异] Python 图片 worker 不受 `GOMEMLIMIT` 约束；
  worker 按任务启动/退出，Docker 含字体及 Python，非原 distroless 大小。

本文件明确区分功能实现、自动化 Mock 验证与真实平台验证，不将待验证项视作已通过。
