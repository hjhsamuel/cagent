# 工具注册与协议适配（P8）

P8 已实现本地工具、MCP/A2A HTTP 客户端、ADK 工具循环、任务句柄持久交接和暂停交互路由。真实 ADK、OpenAI HTTP SDK、本地协议提供方与 MongoDB 单节点副本集已验证。外部模型/MCP/A2A 环境尚未配置，不能将本地提供方夹具算作外部部署验收。

## 配置与授权

`CAGENT_TOOLS_FILE` 为空时不启用工具。设为可信 JSON 文件路径后，bootstrap 在监听前读取、发现并注册；启动失败会释放已打开的连接。示例见 [tools.example.json](tools.example.json)。文件内容不是 HTTP 输入，不支持运行时热更新。

文件顶层字段：

| 字段 | 默认值 | 约束 |
| --- | --- | --- |
| `timeout` | `30s` | 正 Go duration，限制一次执行/观察请求 |
| `max_input_bytes` | `1048576` | 1–4 MiB，JSON 参数字节上限 |
| `max_output_bytes` | `1048576` | 256 字节–8 MiB，领域结果和进度上限 |
| `max_model_calls` | `16` | 1–128，每 Run 的模型循环次数 |
| `connections` | 空 | 明确列出每个完整 tenant/user Scope 的连接授权 |

每个连接包含 `tenant_id/user_id/id/protocol`，协议为 `local/mcp/a2a`。相同 Scope 内连接 ID 和工具名称均不可重复。`tools` 是工具名称白名单，省略或空数组表示允许该可信连接发现的全部工具。`local` 内置无外部副作用的 `echo`；业务本地工具通过 `tool.Entry`、`ExecutorFunc` 注册。

远端连接另有 `url`、可选 `card_path`（A2A，默认 `/.well-known/agent-card.json`）、`credential_ref` 和 `credentials`。例如：

```json
{
  "tenant_id": "example-tenant",
  "user_id": "example-user",
  "id": "research",
  "protocol": "a2a",
  "url": "https://agent.example.invalid",
  "credential_ref": "primary",
  "credentials": {
    "primary": "RESEARCH_AUTHORIZATION",
    "secondary": "RESEARCH_SECONDARY_AUTHORIZATION"
  }
}
```

环境变量内容为完整 Authorization 头（例如 `Bearer ...`）；文件仅保存引用。每个连接的解析器只允许读取自己的引用白名单，并重验完整 Scope。解析在每次请求进行，支持外部轮转；密钥不进入 Descriptor、TaskHandle 或检查点。URL 禁止内嵌凭据、查询和片段，不跟随重定向、不读取 cookie、不自动重试 RPC。A2A Card 的 URL 必须与配置一致；Card 路径相对于配置 URL 的前缀，不会自行切换到 Card 指向的地址。

Catalog 在 List、Resolve、Execute 和任务操作处重验身份，缓存下来的执行器不能跨 Scope 使用。未授权和不存在统一为 NotFound。参数须为 JSON object，并通过已解析 JSON Schema；输入超限、错误关联、空 Outcome、同时有 Result/Task 都被拒绝。业务 `ToolResult.Error` 保留给模型；网络/协议错误阻止后续生成，不被误当作工具成功。

默认超大结果明确失败，不截断。代码装配可注入 `ArtifactWriter`，将完整 JSON 按 Scope 保存并返回小 URI；引用本身也受大小限制，失败标识不会丢失。默认 server 没有 artifact 存储服务，也不自动下载工具给出的 URI。工具代码必须响应 context；本地工具无法被 Go 安全强杀，适配器不会留下后台 goroutine 来制造虚假超时。

共用 `internal/adapter/toolhttp` 只负责 HTTP 安全策略与 Scope 校验，不包含 RPC/SSE 编解码。每个响应（包括订阅流）总量上限为 16 MiB，MCP 另使用 SDK 单事件上限；超限中断本地观察，由 P9 决定后续查询。A2A Card 的其他接口地址不会进入 SDK 的自动传输选择。

## ADK 与任务交接

工具声明参与实际 OpenAI 请求计数，每一轮模型调用前重新检查总预算。无工具时保留原 SSE 文本路径；有工具声明的轮次使用非流式完整响应，收齐工具参数后才允许执行。每个响应至多 64 个工具调用，循环另受 `max_model_calls` 限制。

模型函数名不兼容空格、点或过长名称时，桥接层生成稳定摘要别名；调用远端仍使用原名。普通调用 ID 在同 Run 内不可重复。真实模型输出的未知工具在执行前被拒绝。

即时调用以 assistant 的 `PartToolCall` 持久化；结果以 tool 的 `PartToolResult` 持久化，再送回模型。`agent.Update.MessageRole` 默认 assistant，仅允许 assistant/tool。模型历史中的工具资料不提升为 system。

实际返回 TaskHandle 后，桥接层记录 Scope、Session、Run、Agent/Invocation/ParentInvocation、CallID 和原参数，停止下一次模型调用。ADK v2.4.0 的工具 Context 不支持 `EndInvocation()`；实现使用 BeforeModel 边界截停，并有真实 SDK 测试证明不会额外调用模型。

`cagent.adk.tools.v2` 检查点包含暂停时模型名、准备后的原上下文、SDK 会话快照、所有已返回句柄及安全失败分类，继续读取 v1。默认 `Update.Tasks` 由应用在当前租约/Fence 下调用已有 `Database.TrackTask`：每个任务与检查点及 `tool.waiting` 事件同事务保存，PendingCallIDs 逐项增加。事件返回本地 `task_id/tool_call_id/invocation_id`，不公开远端句柄。也可通过 `ToolOptions.Handoff` 注入负责持久化的回调。`agent.ErrWaiting` 让应用保留 `waiting_tool` 和会话占用，不能提交 completed；混合调用发生失败时先保存已知句柄，再按失败分类结束 Run，恢复时仍维护原任务。

多任务交接不是整批原子事务：每个 TrackTask 原子保存一个任务，SDK Data 同时保存整批已返回句柄。中途故障后，P9 从检查点补齐缺失的 Task 行，不重执行工具；Run 已取消时也可补齐并结算迟到结果。远端已启动但任何句柄尚未落库的窗口仍需提供方幂等/关联查询支持，当前不声称外部副作用恰好一次。

## MCP 支持范围

使用官方 `github.com/modelcontextprotocol/go-sdk v1.8.0`，通过 `mcp.Client`、`ClientSession` 和 `StreamableClientTransport` 连接，固定协商 MCP 2025-11-25。握手、会话头、JSON-RPC、JSON/SSE 解码和关闭均由 SDK 负责；适配层只做授权、分页边界检查和领域结果转换。

支持分页工具发现、普通 tools/call、文本/结构化/资源等结果。禁用 SDK 自动多轮请求、SSE 重连及独立 GET 通知流，不自动重试执行。SDK 未暴露 `execution.taskSupport`，适配器不发送 task 参数；要求 task 的工具由提供方拒绝普通调用。结果须有内容或 structuredContent，无内容的响应（包括仅有 task 的响应）明确失败；文本或结构化内容里的 taskId 不会被猜为任务句柄。

**能力变更：SDK v1.8.0 没有 MCP tasks 客户端 API。** 不再自建 tasks/get/result/cancel；Get、Follow、Cancel 在校验原调用和句柄后返回 Unsupported。旧 MCP 任务句柄不自动迁移、不重执行，需由原提供方或后续 SDK 支持处理。本适配器不创建 MCP 长任务，关闭时交给 SDK 结束会话。

当前装配不启用 stdio、旧 HTTP+SSE、sampling/elicitation、MCP 暂停补充输入或 OAuth 浏览器流程；不为补齐这些能力另写协议客户端。
## A2A 支持范围与交互 API

使用官方 `github.com/a2aproject/a2a-go v0.3.15` 的 `agentcard.Resolver`、`a2aclient.Client` 和 JSON-RPC transport，固定 [A2A 0.3.0 JSON-RPC](https://a2a-protocol.org/v0.3.0/specification/)：读取同一可信端点的 Agent Card，将远程 Agent 暴露为文本工具；message/send 的 Message 变为即时结果，Task 变为句柄，已终态的 Task 也走统一交接。

支持 tasks/get、tasks/cancel、声明 streaming 时的 tasks/resubscribe。订阅通知只触发完整快照查询，不直接拼接可能重复/乱序的 artifact 增量；SDK 不暴露 SSE 事件 ID，因此不生成 Cursor，非空旧游标明确返回 Unsupported；P9 按原任务重新查询并订阅，但不能依赖事件重放。流在终态前结束返回观察错误。终态前组装完整结果并关联原 CallID。UnknownState 仅供内部诊断，观察失败不转成远端 failed。

| A2A 状态 | 领域状态 |
| --- | --- |
| submitted / working | submitted / running |
| input-required / auth-required | input_required / auth_required |
| completed / failed | succeeded / failed |
| canceled / rejected | cancelled / rejected |

暂停交互为 JWT 认证后的本地任务 API：

| 路由 | 请求 | 返回 |
| --- | --- | --- |
| `POST /api/v1/tasks/:taskID/input` | `{"text":"补充内容"}` | 202 表示提供方接纳 |
| `POST /api/v1/tasks/:taskID/authorization` | `{"text":"继续原任务","credential_ref":"secondary"}` | 202 表示提供方接纳 |

应用按认证 Scope 读取本地 Task 和 Run，拒绝终态 Run，查询实际远端暂停状态后调用 TaskInteractor。回复携带原 taskId/contextId；HTTP 无法指定 URL、远端 ID 或 Scope。认证流程只支持由管理配置预先授权的凭据引用，OAuth 获取/用户交互在外部完成；没有引用或不支持交互时明确返回 Unsupported。接纳补充输入不直接触发本地模型，不修改本地已观察终态；P9 在随后确认终态并原子消费后继续模型生成。

不实现 A2A 服务端、gRPC/REST、push notification、客户端 OAuth 浏览器交互或所有提供方专有扩展。不支持的版本/传输/能力明确失败。

## 验证与 P9 接入

行为测试覆盖 Catalog 隔离/捕获执行器越权、Schema、返回互斥、错误结果、artifact、并发和超时；本地 MCP/A2A HTTP 提供方覆盖发现、握手、状态映射、文本不猜句柄、暂停输入/认证、非空旧游标拒绝、取消竞争、错误码、断连、超大响应与禁止重定向。

真实 ADK 测试覆盖即时工具循环、任务暂停和传输失败停止；真实 MongoDB + OpenAI HTTP SDK + ADK 验证即时消息对、双任务句柄及检查点、连续事件与会话占用。HTTP 测试验证 JWT 来源和伪造 Scope/远端句柄拒绝。

全量命令见实施计划。外部冒烟为 `go test ./internal/bootstrap -run TestExternalToolSmoke -count=1 -v`：显式设置 `CAGENT_TEST_TOOLS=1`、`CAGENT_TEST_TOOLS_FILE` 和 `CAGENT_TEST_TOOL_CALL`（JSON 包含 tenant_id/user_id/protocol/name/arguments）。该测试真实执行指定工具一次，可能产生该工具的业务副作用；默认跳过，不输出秘密或结果内容。句柄返回只验证接纳与关联，不验证 P9 生命周期。本次未配置外部提供方，未执行该冒烟。

P9 已实现 TaskTracker、观察错误记录/退避、Task 查询取消 API、恢复扫描、TaskDelivery 原子消费、Runtime.Resume 及 Invocation 分支调度。当前 server 自动观察任务并在结果持久接纳后恢复模型，详见 [长任务与恢复](tasks.md)。MCP SDK 的 tasks 能力限制仍保持明确的 Unsupported，不重执行工具。
