# Kiro × sub2api 集成 — 代码知识库

> CodeGraph 已索引：`backend/`（1,568 文件 / 54,425 节点 / 172,963 边）
> 查询时传 `projectPath: /Volumes/数据盘/网站/中转站/sub2api/backend`

## 一、整体架构

Kiro 作为 sub2api 的一个**一等平台**（platform=kiro），把 Anthropic Claude API
请求转换成 Kiro IDE 后端（AWS CodeWhisperer/q）协议。从 Rust 项目 kiro.rs 完整移植。

```
客户端 (/v1/messages, Anthropic 格式)
  → gateway_handler.go  (鉴权/分组/账号选择/平台分发)
      ├─ platform==kiro → KiroGatewayService.Forward()   ← Kiro 专属网关
      └─ 其他平台 → 各自 gateway
          ↓
      pkg/kiro (纯协议库, 无 sub2api 依赖)
          ├─ converter.go      Anthropic → Kiro conversationState
          ├─ client.go         端点 URL + 请求头伪装 + socks5
          ├─ token_refresh.go  IdC/Social OAuth 刷新
          ├─ event_stream.go   AWS event-stream 帧解析
          ├─ stream_parser.go  Kiro 流 → Anthropic SSE
          └─ ...
          ↓
      Kiro 后端 https://q.{region}.amazonaws.com/generateAssistantResponse
```

**分层原则**：单账号协议正确性 + 反指纹隔离 = pkg/kiro；多账号调度/failover/计费 = sub2api。

## 二、关键符号（CodeGraph 可直接 node 查）

### 业务层 internal/service/kiro_gateway_service.go
- `KiroGatewayService` (struct) — Kiro 平台网关
- `Forward` (kiro_gateway_service.go:164) — 主入口：转换+发送+流式响应
- `TestConnection` — 账号"测试连接"
- `classifyUpstreamError` — 上游错误分类（429重试/月度配额/上下文满→400）
- `forceRefresh` / `ensureToken` — token 刷新
- `credentialFromAccount` — Account.Credentials(JSONB) → KiroCredential
- `startPingLoop` / `writePing` — SSE 25s 保活（防长思考断连）

### 协议库 internal/pkg/kiro/
- `ConvertRequestV2` (converter.go) — 请求转换 + 返回 tool_name_map
- `convertToolsV2` / `shortenToolName` / `normalizeJSONSchema` (tools.go) — 工具处理
- `NewStreamConverter` / `Run` (stream_parser.go) — 流转换状态机
- `findRealThinkingEnd` / `thinkingSafeEmitLen` (stream_parser.go) — thinking 标签防误判
- `ParseFrame` (event_stream.go) — AWS event-stream 帧解析
- `RefreshToken` / `refreshIdC` / `refreshSocial` (token_refresh.go)
- `EffectiveMachineID` (machineid.go) — 每账号设备指纹派生
- `EffectiveSystemVersion` (types.go) — 每账号稳定 OS 指纹
- `MapModel` / `SupportedModels` (models.go) — 模型映射
- `IsTransientStatus` / `RetryDelay` (client.go) — 瞬态重试(429/408/5xx)
- `IsMonthlyRequestLimit` / `IsBearerTokenInvalid` / `IsContextLengthError` (errors.go)
- `CountInputTokens` (tokencount.go) — 本地 token 估算(count_tokens 端点)
- `FetchUsageLimits` (usage.go) — getUsageLimits 额度查询
- `HandleWebSearch` (websearch.go) — web_search MCP 桥接
- `NewAggregatingWriter` (aggregate.go) — 非流式响应聚合

## 三、移植自 kiro.rs 的核心行为（全部已吸收）

| 行为 | 位置 | kiro.rs 对应 |
|---|---|---|
| event-stream 帧+CRC | event_stream.go | parser/ |
| IdC/Social token 刷新 | token_refresh.go | token_manager.rs |
| 工具名缩短(>63→sha256)+回映射 | tools.go + stream_parser.go | converter.rs |
| Write/Edit 描述后缀 + schema 规范化 | tools.go | converter.rs |
| thinking 注入(enabled/adaptive, opus4.6→adaptive) | converter.go thinkingPrefix | converter.rs |
| thinking 标签防误判(引号包裹/\n\n) | stream_parser.go findRealThinkingEnd | stream.rs |
| 每账号 machineId 指纹 | machineid.go | machine_id.rs |
| 每账号 OS 指纹(darwin/win32 混合) | types.go EffectiveSystemVersion | config.rs(进程级,这里改进为账号级) |
| 客户端版本 0.11.107 | types.go DefaultKiroConfig | config.rs |
| 瞬态重试 429/408/5xx 指数退避 | client.go + Forward doUpstream | provider.rs call_api_with_retry |
| bearer-token 失效→刷新重试同账号 | Forward + errors.go | provider.rs |
| 月度配额 MONTHLY_REQUEST_COUNT | errors.go IsMonthlyRequestLimit | endpoint/mod.rs |
| 上下文满/输入过长→友好400 | classifyUpstreamError | handlers.rs map_provider_error |
| SSE ping 保活 25s | startPingLoop | handlers.rs PING_INTERVAL_SECS |
| 非流式聚合 | aggregate.go | handlers.rs handle_non_stream_request |
| 准确 input_tokens(message_delta) | stream_parser.go finalize | handlers.rs buffered |
| count_tokens 本地估算 | tokencount.go | token.rs |
| 用量额度 getUsageLimits | usage.go | token_manager.rs |
| web_search MCP | websearch.go | websearch.rs |
| profileArn 注入 | client.go / Forward | endpoint/ide.rs |
| socks5/http 代理 | client.go buildHTTPClient | http_client.rs |

**交给 sub2api（未移植 kiro.rs 的，因有更优等价物）**：MultiTokenManager 多账号
调度(priority/balanced)、failover 切换、统计持久化、admin UI。

## 四、面板适配（前端，未被 CodeGraph 索引，仅 backend 入库）
- 创建/编辑账号(EditAccountModal 凭证只在创建/导入, 编辑页只改配置)
- JSON 批量导入 + 代理自动分配（见下"代理分配模型"）
- 平台徽章/图标/配色/筛选/用量展示(KIRO POWER)/模型限制/中文 i18n

### 代理分配模型（防多账号关联封号）

后端：`internal/service/proxy_assignment.go`（ProxyAssignmentPlanner）
+ `account_handler.go` BatchCreate（auto_assign_proxy）

**核心规则**：每个"出口槽位"每平台最多 5 个账号，账号均摊到各槽位。

**出口槽位 = 服务器本机 IP（slot 0）+ 每个真实代理**，三者平等参与均摊：
- **服务器本机 IP 是一等槽位**：可承载 5 个账号、无需代理、直连启用。
  实现：planner 槽位列表前置 `0`；`buildPlatformProxyCounts` 把 proxy_id=NULL
  的账号计入 slot 0（已有直连账号正确占位）。
- 导入时 `Next()` 选当前负载最少的槽位（含本机IP），各槽 cap 5：
  - 返回 `(0,true)` → 账号启用、**不绑代理**（走本机 IP 直连）
  - 返回 `(id>0,true)` → 账号启用、绑该真实代理
  - 返回 `(_,false)` → 所有槽位（含本机IP的5个）全满 → **预存为禁用**
    (schedulable=false)，并返回 `proxy_warning` 提示加代理

**容量公式**：可启用账号数 = (1 + 代理数) × 5。
例：0 代理 → 前 5 个账号用服务器 IP 直连，第 6 个起禁用；1 代理 → 10 个；以此类推。

**为何这么设计**：多账号被 Kiro 风控关联封号的最大信号是"同一出口 IP 挂太多账号"。
均摊 + 每 IP ≤5 把账号摊开；账号级 machineId / OS 指纹隔离（见三）再叠加，
让不同账号在 Kiro 看来像不同设备 + 不同出口。

**编辑页补空缺**：预存禁用的账号，后续加代理 → 编辑页绑代理 → 打开
schedulable 开关即可启用（凭证轮换走重新导入 JSON）。

## 五、常用 CodeGraph 查询

```
# 理解某功能：直接 explore（一次拿全源码）
codegraph_explore "KiroGatewayService Forward ConvertRequestV2 stream_parser"

# 改动影响面（重构前必查）
codegraph_impact ConvertRequestV2
codegraph_callers EffectiveMachineID

# 精确定位重载符号（如 Forward 有5个平台版本）
codegraph_node Forward --file kiro_gateway_service.go

# 调用链
codegraph_callees Forward      # Forward 调用了谁
codegraph_callers credentialFromAccount  # 谁调用了它
```

## 六、部署与验证
- dev 源码：服务器 /opt/sub2api-dev/sub2api（feature/kiro-provider 分支）
- 验证栈：容器 kv-app 端口 28080（独立 PG/Redis，与线上隔离）
- 线上未动：sub2api 18080、kiro.rs 8990、生产库
- 测试：`go test ./internal/pkg/kiro/`（51 用例全过）
- 真实 E2E：流式/非流式/thinking/工具/count_tokens/用量/模型映射/瞬态重试 全验证

## 七、Kiro IDE 版本指纹对齐
A 侧（发往 Kiro 后端的请求）严格模拟 Kiro IDE 真实行为，改动前以解包 extension.js 为依据。
- **基线版本**：0.12.316 / 0.12.333（2026-06-10 解包对比，A 侧逐字一致，纯 minify 重命名差异，零协议改动）。
- **sub2api 默认 KiroVersion**：`internal/pkg/kiro/types.go` 默认 `0.12.301`，env `KIRO_VERSION` 可覆盖；UA 模板 `KiroIDE-{KiroVersion}-{machineId}` 全链路自动跟随。
- **429 重试对齐 IDE**（client.go）：IDE 对话流用 AdaptiveRetryStrategy（解包 it7：maxAttempts=3、节流退避基数 500ms ×5、cap 20s）。sub2api `IsInPlaceRetryStatus` 含 429 → 同号退避重试（`ThrottleRetryDelay`），重试耗尽才切号。
- **获取新版包对比的方法**：`curl https://prod.download.desktop.kiro.dev/stable/metadata-darwin-{arch}-stable.json` 取 `releases[].updateTo.url` 下载 zip（S3 桶其他路径全 403，仅此 metadata 端点可读）。
- 解包资料放本地 `.kiro-decompile/`（已 gitignore，不入库）。详细抓包/解包结论见维护者私有 memory。

