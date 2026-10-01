# 上游来源

- 项目：Futureppo/astrbot_plugin_global_status
- 仓库：https://github.com/Futureppo/astrbot_plugin_global_status
- 导入提交：`38822b2e35ad60a12392a7b12d211e748ed76e34`
- 插件版本：1.2.2
- 原始说明和更新记录：`worker/statusmonitor/UPSTREAM_README.md`、
  `worker/statusmonitor/UPSTREAM_CHANGELOG.md`。

`sources.py`、`modern_sources.py`、`betterstack.py`、`datadog_status.py`、
`monitor_state.py` 复用上游算法；`renderer.py`、`translation.py` 仅移除
AstrBot 环境依赖。`main.py` 保留上游采集、状态协调、分批、送达检查点和
只读查询逻辑，平台调度、权限、订阅和 KV 存储接入本项目 Go 服务。
`worker.py` 和 Go 集成是本次移植新增。

后续专用 SOCKS 代理适配：新增 `proxy.py`，`modern_sources.py` 增加显式代理传递，
普通状态源使用 aiohttp-socks；不改变上游事件解析算法。

后续查询进度适配：`sources.py` 的并发采集调度增加进度回调，
`main.py`、`translation.py` 增加阶段/错误通知；解析算法和渲染函数不变。
Go 查询控制器负责立即更新步骤、10 秒无变化刷新、群消息撤回及诊断脱敏。

SVG 图标保留上游 LobeHub MIT 许可全文：
`worker/statusmonitor/assets/icons/LOBEHUB_LICENSE.txt`。
图片中保留上游项目署名。品牌和商标归各自权利人所有。

导入提交没有仓库根目录 LICENSE 文件；本文件仅记录来源和已附带的图标许可，
不为上游 Python 代码另行指定许可证，也不将图标 MIT 许可扩大到整个插件。
