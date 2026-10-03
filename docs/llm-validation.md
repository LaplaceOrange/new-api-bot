# LLM 对话实现与验证

日期：2026-10-03。默认关闭，不内置人格、模型名称或模型密钥。

## 实现证据

- **E1 — 入口及运行**：`internal/bot/service.go` 的 `HandleGateway`、`process`；`internal/qq/chat.go` 的 `ChatContent`、`ReplyOrigin`；`internal/bot/llm.go` 的 `handleChat`、`runLLMDispatcher`、`runLLMJob`。独立有界工作队列，同一会话串行；两个群事件共享 LLM 消息去重键；消息原始时间限制被动回复期限。
- **E2 — 配置及人格**：`internal/llm/config.go` 的 `DefaultConfig`、`LoadEnvironment`、`Set`；`internal/bot/llm_config.go` 的 `handleLLMConfig`、`updateLLMConfig`。人格默认空，管理员热更新；部署默认及加密命令覆盖，密钥只允许单聊设置，不回显。
- **E3 — 模型、搜索及业务工具**：`internal/llm/client.go`、`internal/llm/search.go`；`internal/bot/llm_tools.go` 的 `generateLLM`、`executeLLMBusinessTool`。四种搜索后端，无自动回退；15个只读业务工具，严格参数和当前身份/权限检查，不通过命令路由执行写操作。
- **E4 — 存储与恢复**：`internal/store/llm.go` 的 `AdmitLLMJob`、`LLMSession`、`PutLLMSession`、`RecoverLLMJobs`、`PruneLLM`。准入去重、限额、容量及历史版本在事务内处理；正文、任务、答案和配置覆盖加密；未知调用/发送结果不自动重试。
- **E5 — 隐私及输出**：`internal/bot/llm_tools.go` 的业务上下文标记与传播；`internal/bot/llm.go` 的 `llmTurn`、`llmSavedResult`、`llmSafeText`、`sendLLMResult`。群历史排除业务轮次，单聊业务历史按绑定和权限隔离，缓存答案重新检查管理员权限；保留实际来源、转义 QQ 控制标签，递增被动回复序号。

## 本地覆盖矩阵

| 范围 | 验证入口 |
| --- | --- |
| 命令/@入口、普通文本忽略、代码/换行/help 原样处理 | `TestChatContent`、`TestLLMGatewayAtAndIgnoredMessages`、`TestLLMEntryPreservesCodeAndHelpSuffix` |
| 绑定、群开关、管理员、只读管理员及命令禁用 | `TestLLMAdmissionRequiresBindingAndExplicitGroupEnable`、`TestLLMConfigPersonaHotUpdateSecretsAndReadonly`、`TestLLMToolAuthorizationValidationAndNoWrites` |
| 人格空值、多行、热更新、配置重启保留、损坏配置恢复 | `TestEnvironmentConfig`、`TestLLMDeploymentConfig`、`TestLLMHelpConfigRecoveryAndPersistence` |
| 群共享与单聊隔离、业务隐私、绑定更改、管理员权限撤销 | `TestLLMGroupHistorySharedAndBusinessExcluded`、`TestLLMPrivateHistoryIsolatedAndBindingScoped`、`TestLLMBusinessPrivacyTaintSurvivesFollowups`、`TestLLMAdminHistoryNotReusedAfterPrivilegeRemoval` |
| 原子限额/容量/去重、滚动窗口、每日限额、并发准入 | `TestLLMAdmissionAtomicLimitsAndDedupe`、`TestLLMConcurrentAdmission` |
| 历史裁剪、过期、清空版本、群关闭、崩溃恢复 | `TestLLMHistoryResetExpiryAndPrune`、`TestLLMRecoveryPersistsAndGroupCancellation`、`TestLLMResetRevocationAndShutdownDuringModel` |
| 任务恢复不重复付费、缓存权限、消息时效、群事件别名 | `TestLLMReadyResultResumesWithoutModelCall`、`TestLLMReadyAdminResultRechecksPrivilege`、`TestLLMExpiredInputAndEventAliases`、`TestReplyOriginCannotExtendPassiveWindow` |
| Chat Completions 协议、工具循环/预算、非法参数、401/429、空/超大响应、取消/超时 | `TestCompletionProtocol`、`TestCompletionFailuresAreBoundedAndSanitized`、`TestCompletionCancellation`、`TestLLMToolBudgetResetAndDisableDoNotRegenerate`、`TestLLMTimeoutKeepsQuotaAndNoHistory` |
| Tavily/SearXNG/SerpApi Bing/原生模型搜索及异常 | `TestSearchBackends`、`TestSearchUnavailableAndInvalid` |
| 搜索实际来源、搜索失败说明、禁止转发业务上下文 | `TestLLMSearchCitationsErrorsAndPrivacy`、`TestLLMHistoryTrimAndSourcesSurviveTruncation` |
| 15个业务工具、日志/邮箱/订阅 URL 凭据不暴露、无业务写操作、过期厂商快照 | `TestLLMAllBusinessToolsAndRedaction`、`TestLLMVendorSnapshotMarksUnknownAndStale` |
| 慢模型不阻塞业务命令、同会话串行、回复序号/长度/部分发送失败 | `TestLLMDispatcherDoesNotBlockGatewayAndSerializesSessions`、`TestLLMReplySequencesPartialFailureAndCap` |
| 模型输出不能直接触发 QQ @全体等控制标记 | `TestLLMOutputDoesNotExecuteQQControlMarkup` |

所有 LLM/搜索 HTTP 验证使用本地模拟服务；业务数据及 QQ 发送使用测试替身。

## 执行记录

**E6 — 最终 Go 复验全部通过**（Go 1.23.4，Windows/amd64）：

| 命令 | 结果 |
| --- | --- |
| `go test ./... -count=1 -timeout=180s` | 退出0；机器人包63.237s |
| `go test -race ./... -count=1 -timeout=240s` | 退出0；机器人包72.633s，无竞态报告 |
| `go vet ./...` | 退出0 |
| `go build ./...` | 退出0 |

本机日志位于 `.codex/llm-evidence/go-test-final.txt`、`go-race-final.txt`，退出状态记录在 `go-results.json`。测试环境将 `VENDOR_STATUS_TEST_PYTHON` 指向既有 Python 测试虚拟环境。

**E7 — Python 回归已执行**：

- `python -m pytest internal/vendorstatus/worker -q`：**145 passed, 7 skipped**；另有2条 Windows `curl_cffi` selector 回退提示，不是测试失败。
- `python -m ruff check internal/vendorstatus/worker`：通过。
- `python -m ruff format --check internal/vendorstatus/worker`：31个文件格式通过。

## 未执行及边界

- **未执行**真实模型调用、真实 Tavily/SerpApi/SearXNG/原生搜索及真实 QQ 发送联调，需要部署凭据与实际 QQ 机器人事件。
- `GROUP_MESSAGE_CREATE` 全量事件可能不保留机器人 @标记；不能据此猜测用户目标。自然 @使用 `GROUP_AT_MESSAGE_CREATE`，无可靠目标信息时使用 `/chat`。
- 厂商工具返回带采集时间的已有监控快照，不即时采集，不将未知/过期状态描述为正常。
- 首版不包含图片/语音、流式回复、任意 URL 网页抓取、代码执行、业务写工具及自动跨搜索后端回退。
