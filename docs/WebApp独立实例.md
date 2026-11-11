# WebApp 独立实例

`--webapp-host=single` 是 standalone 的单应用装配档位，支持纯前端及宿主提供的 Node 24 后端。Desktop 安装、升级和卸载包，Platform 读取 `AP_RUNTIME_DIR/webapp.json`，不提供单应用安装命令。Compose 多应用托管保持独立。

## 启动和版本

```sh
AP_RUNTIME_DIR=/absolute/instance/runtime \
AP_RUNTIME_REGISTRIES_DIR=/absolute/shared/registries \
agent-platform --runtime-mode=standalone --webapp-host=single \
  --config-dir=/absolute/instance/config \
  --listen=127.0.0.1:0 \
  --ready-file=/absolute/instance/run/ready.json \
  --webapp-node=/absolute/desktop/bin/node \
  --parent-pid=12345
```

Windows 传 `node.exe`。Mac/Windows 的 `--webapp-node` 必须为绝对路径；启动探测 `--version`，只接受 24 主版本。纯前端可不提供 Node。Linux 使用镜像锁定的 `/opt/node/bin/node`、supervisor 和 Landlock，不降级为直接执行。`--port` 与 `--listen` 互斥；`--listen` 支持端口 0。API 和应用使用不同 origin；应用端口从 `webapps.frontend` 范围选择。Linux 容器使用 `--listen=0.0.0.0:8080` 时应用监听也使用 `0.0.0.0`，必须分别发布 API 与应用端口。

`agent-platform --version`（或 `version`）仅输出版本号，不初始化配置或 builtin。实际版本来自根 `VERSION`，不会因支持 single 而自动提升；包的 `minPlatformVersion` 必须不高于该输出，包含预发布版本的比较遵循 SemVer。

空配置根可使用内置默认配置；Desktop 不需生成 runtime YAML。single 自动启用 WebApp，关闭自动化、记忆整理、Mailbox、渠道/反向网关、KBX 库调度、终端、LSP 和实时语音，不创建对应运行数据目录。Agent/Chat、工具循环、HITL 和规划继续使用既有 Runtime。被裁模块的 HTTP/主 WS 路由被拒绝，对应工具也从有效配置排除。

监听和应用健康成功后原子写入：

```json
{"pid":12345,"apiUrl":"http://127.0.0.1:53121","appUrl":"http://127.0.0.1:20200","platformVersion":"0.5.46"}
```

取得实例锁并完成装配后移除旧 ready 文件；正常退出移除当前文件。硬崩溃可能留下旧文件，宿主必须同时验证进程仍活着、pid 匹配和健康检查。启动失败退出非零；不得只凭日志中的 ready 字样判断。`--parent-pid` 每秒检测父进程；Windows 固定持有父进程句柄，父进程退出后走正常关闭流程。

## Owner 与一次性 ticket（Desktop 对接契约）

独立实例沿用普通 RS256 owner verifier。默认在拿到实例锁后为本次启动生成密钥，私钥只在内存中；`<runtime>/.state/webapp-owner/owner.json` 是仅供宿主本地读取的凭证文件：

```json
{"token":"<owner JWT>","subject":"webapp-owner","expiresAt":1797000000000}
```

token 有效期 30 天，每次启动轮换；Desktop 每次启动重新读文件，不能复用上次启动的 token。`.state` 对应用后端和普通 Agent 工具均禁止读取。凭证不写入 ready.json、应用配置或页面。若部署配置显式使用 JWKS，则沿用部署自己的 issuer、audience、expiry 和唯一 owner，不生成本地 owner 文件。

1. 等待 ready 与 `/healthz` 后，读取 owner 文件。
2. `POST <apiUrl>/api/admin/webapps/<appId>/launch-ticket`，请求头 `Authorization: Bearer <token>`、`Content-Type: application/json`，请求体 `{"returnPath":"/"}`。
3. 响应为 `{"code":0,"msg":"success","data":{"action":"http://127.0.0.1:20200/__desktop/session/exchange","ticket":"wlt_…","expiresAt":1794410000000}}`（`msg` 仅作说明，不作程序判断）。
4. WebView **顶层 POST** 到 `data.action`，Content-Type 为 `application/x-www-form-urlencoded`，正文只有一个字段 `ticket=<编码后的票据>`，`Origin` 必须等于 `apiUrl`。不能把票据放 URL、iframe 或普通 GET 中。
5. 成功为 303 + HttpOnly 会话 Cookie，再跳转 `returnPath`。票据一分钟有效且只消费一次。旧代次的票据与会话不能跨重启使用。

页面继续使用 `/__desktop/bridge.js` 的 Platform SDK 适配，assistant/skill/artifact 经现有 Runtime/Catalog/ChatResource。它不授予 Desktop 原生能力。Copilot Agent 必须来自包内 `agents/`；Skill 必须来自包内 `skills-center/` 或可解析的 Platform builtin。

## 空闲回收

`GET <apiUrl>/api/webapp/status` 要求同一 owner Bearer，沿用 `{code,msg,data}`：

```json
{"lastRequestAt":1794410000000,"activeRequests":0,"activeRuns":0,"awaitingRuns":0,"idle":true}
```

`lastRequestAt` 为毫秒时间戳，跟踪 API、SDK/页面/业务代理请求的进入与结束。状态轮询及 health 不刷新时间；主 WS 业务帧计入请求，单纯保持连接不计入。`activeRuns` 包括运行中和等待人工输入的活动 Run；`awaitingRuns` 还计入 Run 已结束后的规划确认。Desktop 仅在 `idle=true` 且距 `lastRequestAt` 至少 30 分钟时回收；Platform 不自行执行该 30 分钟策略。

## 文件与进程边界

包内文件为只读事实源。single 禁止 Agent/Skill/Registry 管理写入，跳过技能包自动迁移和源目录清理；普通文件/Bash 工具把 Agent、Skill、连接器包和共享 registries 放入 readonly roots，审批不能放宽。管理记录在 `.state/webapps/<id>/`，运行锁在 `.lock/webapps/instance.lock`；Windows 使用 LockFileEx。

后端只读 `webapp.json`、`frontend/`、`backend/` 及本代用户配置，只能写 `webapp-data/` 和本代临时目录。应用根包含凭证和 Chat 数据，绝不整体授予后端读取。Node 使用 `--permission --no-addons`，不放行 child_process、worker 或原生 addon；禁止 manifest 环境变量覆盖运行时隔离配置。

Mac/Windows 使用控制管道看门狗：Platform 死亡使管道 EOF，Mac 回收进程组，Windows 在启动 Node 前加入 kill-on-close Job，回收启动器及后代。代次文件锁在看门狗退出前保持；新实例不接管旧 PID。Windows launcher 可以再使用其自己的 Job。Linux 继续由 C supervisor 管理后代；协议升级为 v4，`--run-single` 对 Landlock 按包内条目授权，Compose 的 `--run` 保持原布局。最低 ABI4，增强能力按真实探测报告。

Linux 的出站 TCP 端口继续取 owner policy 的 `connectPorts`；single 的包内 Agent/Skill/connector scope 不可修改，仅允许 owner 调整端口。保存位置为 `.state/webapps/<id>/policy.json`，重启保留；`GET /api/admin/webapps/<id>` 读取 policy，`PUT …/<id>/policy` 沿用 revision 校验。当前 Desktop 共享 manifest **没有网络端口字段**，因此不能声称已由 manifest 自动授予端口。Mac/Windows 的 Node 权限模型不限制 TCP，诊断明确返回 `webapp_network_isolation_unavailable`。

## 验证

常规回归：

```sh
go test ./internal/app ./internal/config ./internal/catalog ./internal/webapp ./internal/webapphost ./internal/webappgateway ./internal/webappbridge ./internal/server ./internal/adminsource ./internal/accesspolicy ./internal/runtime/runstate ./cmd/agent-platform
python3 scripts/sync-webapp-contracts.py --check
python3 scripts/test-webapp-single.py --platform /absolute/agent-platform --node /absolute/desktop/bin/node
```

验收脚本仅创建临时实例和本机 mock 模型，测试 Node 权限、ticket/页面、SDK 包内 Agent/Skill 对话、共享模型配置不变、Platform 强杀后的回收与重启、父进程死亡与回收，不访问真实模型或生产数据。日志和实例留在打印出的临时目录供复查。

2026-11-11：macOS arm64 开发态 Electron Node 24 已完成真实启动、按项拒绝、owner ticket、页面访问、包内 Agent/Skill 的 mock 模型 SDK 对话、Platform 强杀后的回收重启、父进程退出、共享 registries 不变验证；Windows amd64 与 Linux amd64 的 Go 入口交叉构建通过，supervisor 经 Linux GCC `-Wall -Wextra -Werror -static` 编译通过。Docker Desktop amd64 仿真返回协议 v4、abi 0、errno 38，按预期拒绝降级。Windows 实机、打包后的 Mac，以及原生 Linux amd64 的新版 Landlock、生命周期验收仍需目标环境执行，不能沿用旧版 Compose 验收作为本次通过证据。
