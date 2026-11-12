# 按 URL 调用远程 MCP

## 范围与边界

受信 WebApp 已掌握 HTTP MCP URL、工具名和参数时，可直接通过 Platform 调用，无需安装连接器、Agent 挂载、工具发现或模型。独立的 `internal/remotemcp` 负责协议、身份注入和资源边界；Server 与 WebApp bridge 只做调用方鉴权和响应适配。`connectorops` 与 `mcp.NewOperationClient` 对 OneID 的拒绝保持不变。

这是部署 owner 审核的可信应用能力：允许调用方自行选择 URL，包括内网服务；不是面向匿名网页的通用代理。不新增 serviceId 注册表、页面审批或 manifest 授权项。安装运行中的可信 WebApp 通过现有会话或后端凭据获得能力；不得把此模式扩展为多租户共享 owner 身份。

## 接口

`POST /api/mcp/invoke`，请求 JSON：

```json
{"url":"https://example.com/mcp","toolName":"lookup","arguments":{"key":"value"},"auth":"sso"}
```

精确接受 `url`、`toolName`、`arguments`、可选 `auth`，拒绝未知字段、大小写变体、重复顶层键、null、尾随 JSON 和用户提供的 headers/token/身份。arguments 必须为对象，其内容作为工具业务输入原样交给 MCP。auth 省略为 sso，仅可显式选择 sso 或 none。URL 不含用户凭据或 fragment；sso 仅允许 HTTPS，none 可 HTTP（包括 loopback）。

成功响应沿用 `{code:0,msg,data}`，`data` 为 `{mcp:CallToolResult}`，保留原生 `content`、`structuredContent`、`isError` 和 SDK 支持的其他结果字段。`isError:true` 表示上游工具报告失败，仍是成功的 MCP 协议响应，不转成 Platform 异常。

失败响应为 `{code:HTTP状态,msg:稳定码,data:{errorCode:稳定码}}`，不返回上游异常、请求 URL、文件路径或凭据。

| errorCode | HTTP | 含义 |
| --- | --- | --- |
| invalid_arguments | 400 | 请求形状、大小、URL 或认证模式非法 |
| mcp_authority_required | 401/403 | 缺失/无效宿主认证，或宿主身份声明不满足要求 |
| sso_identity_unavailable | 401 | 身份文件未供应、为空、不可读或非法 |
| mcp_unavailable | 503 | 核心服务未装配 |
| mcp_connect_failed | 502 | MCP 初始化/协议协商失败 |
| mcp_call_failed | 502 | 上游明确返回 JSON-RPC 错误 |
| mcp_outcome_unknown | 502 | 工具调用后断连等，无法确定远端执行结果 |
| mcp_timeout | 504 | 总截止时间耗尽 |
| mcp_canceled | 408 | 调用方或应用生命周期取消 |
| mcp_result_too_large | 413 | 返回内容超过预算 |
| method_not_allowed | 405 | 非 POST |

超时、取消、超限和未知结果均可能发生在远端操作执行之后，不能据此自动重试写操作。本实现不重试业务调用、不自动回退另一传输、不提供幂等持久化收据。

## 调用方与身份

HTTP 路由即使在普通 API auth 关闭时也强制经过既有 JWT verifier；要求 `scope=app`、非空 subject 和 deviceId/device_id，正文不能选择身份。Desktop 只适配此入口，宿主 JWT 只证明调用权限，不作为远端 ID token。

独立实例不请求 Desktop JWT、不伪造 device。页面入口使用既有 WebApp session、origin 与 CSRF；后端入口使用绑定应用实例/代次/策略的 bootstrap→短期 action 凭据。桥接在调用前后复核 Scope，将请求取消与 Scope/service 退出关联；撤销或停止应用后不能继续使用旧凭据。匿名内容访客不获得 owner 身份。

两端 SDK 统一 `mcp.invoke(input, {signal})`，action 和 capability 均为 `mcp.invoke`。返回 `{mcp:CallToolResult}`；独立实例 transport 继续使用既有 `{ok,action,result|error}` 信封，失败码位于 `error.code`。capability 的 available/granted 只表示入口已装配，不保证 SSO 文件存在或远端授权成功。SDK 源由 Desktop 维护，Platform 通过 `scripts/sync-webapp-sdk.py` 生成并校验独立实例适配副本。

sso 使用 `cfg.SsoIdTokenFile`，每次调用预检、每次出站请求重新读取既有受限身份文件读取器。只在本次 HTTP 请求的 `Authorization: Bearer ...` 内提供；不写配置、进程环境、日志或平台错误响应，不返回给页面。none 不读取身份文件。两种模式都拒绝 HTTP 重定向，避免凭据转发和隐式再次提交操作。

实例默认消费 `<有效 StateDir>/identity/sso-id-token`，通常为 `.state/identity/sso-id-token`，遵循已有只读供应约定。此能力不创建登录会话、不刷新 ID token、不实现多租户凭据；供应方缺失时如实返回 `sso_identity_unavailable`，绝不使用 Platform API token 或 WebApp bridge token 代替。

## 协议与资源

复用仓库固定的 MCP Go SDK v1.6.1 和 `internal/httpclient` 出站网络配置。临时客户端只使用 MCP Streamable HTTP（支持 POST JSON 与 SSE 响应），不启动 stdio，不依赖安装目录，不发现旧式 HTTP+SSE endpoint。初始化后直接 tools/call，不执行 tools/list。

默认总截止时间 30 秒，输入最多 256 KiB，出站响应累计读取预算 1 MiB（含初始化），结果序列化也检查 1 MiB；SDK 重连重试关闭，独立 GET SSE 关闭。请求取消及应用退出传递给协议请求，SDK 清理与取消通知的 detached context 也绑定同一截止时间。会话在调用结束后关闭，不跨调用缓存身份或复用连接器/Agent 会话。

## 验证与交付状态（2026-11-11）

本机可控 HTTP/TLS fixture 已验证：原生结果及 isError 保留、身份更新/缺失、HTTPS 约束、禁止重定向、未知/身份字段拒绝、截止时间和取消、输出限制、断连后未知结果且工具只调用一次。HTTP 适配测试验证强制认证（包括通用 auth 关闭）、宿主声明和响应封装；原宿主测试验证 action/capability、真实后端 action 凭据、bootstrap 不可直接执行及撤销。

共享 SDK 已从 Desktop 源生成并通过同步检查。未访问真实 MCP 服务，未部署；本轮没有在原生 Linux 容器执行真人端到端验收，不能把本机 fixture 测试视作该验收。

最终检查：`go test ./internal/remotemcp ./internal/server ./internal/webappbridge ./internal/app ./internal/mcp ./internal/connectorops` 全部通过；新入口相关用例的 `-race` 检查通过。`CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./cmd/agent-platform` 通过；SDK 同步检查及 Node 页面/后端/取消 fixture 通过。没有执行整个仓库的 `go test ./...`。
