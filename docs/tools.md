# 工具注册与协议适配（P8）

P8 已实现本地工具、MCP/A2A HTTP 客户端、ADK 工具循环、任务句柄持久交接和暂停交互路由。真实 ADK、OpenAI HTTP SDK、本地协议提供方与 MongoDB 单节点副本集已验证。外部模型/MCP/A2A 环境尚未配置，不能将本地提供方夹具算作外部部署验收。

## 配置与授权

启动时从 MongoDB `tool_connections` 集合读取 MCP/A2A 远端连接，并独立全量扫描 `local-tools`。不再读取 `CAGENT_TOOLS_FILE`。两种来源都在 HTTP 监听前加载，修改后需重启；读取、发现或注册失败会阻止启动，并释放已打开的连接。

本地工具对所有有效租户/用户身份可见，调用仍传入实际 Scope。远端连接仅对文档中指定的 `tenant_id/user_id` 可见。共享本地工具名称不能与任何远端工具名称冲突；同一 Scope 内远端连接 ID 和工具名称也不能重复。

| 环境变量 | 默认值 | 约束 |
| --- | --- | --- |
| `CAGENT_LOCAL_TOOLS_DIR` | `local-tools` | 非空目录路径，相对路径以进程工作目录为基准 |
| `CAGENT_TOOLS_TIMEOUT` | `30s` | 正 Go duration，限制一次执行/观察请求 |
| `CAGENT_TOOLS_MAX_INPUT_BYTES` | `1048576` | 1 字节–4 MiB，JSON 参数上限 |
| `CAGENT_TOOLS_MAX_OUTPUT_BYTES` | `1048576` | 256 字节–8 MiB，结果和进度上限 |
| `CAGENT_TOOLS_MAX_MODEL_CALLS` | `16` | 1–128，每 Run 的模型循环次数 |

远端连接文档包含 `tenant_id/user_id/id/protocol/url`，协议为 `mcp/a2a`。`tools` 是远端工具名称白名单，省略或空数组表示加载该连接发现的全部工具。本地工具不使用连接文档或工具白名单。远端连接示例见 [tools.example.json](tools.example.json)，文件是供写入 MongoDB 的文档数组，不由服务直接读取。

## 本地业务工具

所有 local tool 放在同一个根目录下，每个工具占用一个一级子目录。服务启动时读取 `tool.json` 注册配置及检查可执行文件，发现阶段不会执行工具。每次调用启动独立进程，新增工具只需部署目录并重启服务，无需修改或重新编译服务。

```text
local-tools/
  echo/
    tool.json
    bin/echo.exe
  read_skill/
    tool.json
    bin/read_skill.exe
    skills/code-review/SKILL.md
```

Windows 使用 `.exe`，其他平台使用有执行权限的文件，例如 `bin/read_skill`。配置中的 `executable: "bin/read_skill"` 会在 Windows 上自动补 `.exe`。工具目录名必须与配置中的名称一致，由 1–64 个英文字母、数字、下划线或连字符组成。不使用 shell 拼接命令，模型参数只能通过 stdin 传入，不能选择可执行文件或修改固定命令参数。

本地工具的业务配置直接写入自己的 `tool.json` 的可选 `config` 对象。例如 `read_skill`：

```json
{
  "name": "read_skill",
  "description": "读取技能的完整 SKILL.md",
  "executable": "bin/read_skill",
  "config": {"skills_dir": "skills", "max_skill_bytes": 262144},
  "input_schema": {
    "type": "object",
    "properties": {"name": {"type": "string"}},
    "required": ["name"],
    "additionalProperties": false
  }
}
```

`config` 必须是 JSON object，省略时传入空对象；仅将当前工具的配置传给子进程，不进入模型参数。工具的工作目录为自己的目录，因此相对 `skills_dir` 以 `local-tools/read_skill` 为基准。全量扫描所有一级工具目录，忽略根目录下普通文件和隐藏目录。目录缺失、缺少注册文件或可执行文件会导致启动失败；没有本地工具的部署须提供空目录。

每个工具只需一个纯 JSON 注册文件 `tool.json`，不需要 Markdown 说明文档，文件最大 256 KiB。必要字段如下：

| 字段 | 含义 |
| --- | --- |
| `name` | 工具名，与目录名相同 |
| `description` | 提供给模型的工具说明 |
| `executable` | 本工具目录内的相对可执行文件路径，使用 `/` 分隔 |
| `input_schema` | 业务入参的 JSON Schema，根类型必须为 `object` |

`protocol_version` 可选，省略或 0 时使用当前协议 `1`；`args` 可选，为管理员固定的启动参数数组。维护字段均可选，不进入模型参数或子进程请求：

| 字段 | 含义 |
| --- | --- |
| `version` | 工具版本，例如 `1.0.0`，可带 prerelease/build 后缀 |
| `created_at` | RFC3339 创建时间，带时区 |
| `updated_at` | RFC3339 更新时间，带时区 |
| `author` | 作者或维护团队 |
| `license` | 许可证标识 |
| `tags` | 用于维护分类的字符串数组 |

最小注册示例：

```json
{
  "name": "echo",
  "description": "返回输入文本",
  "executable": "bin/echo",
  "input_schema": {
    "type": "object",
    "properties": {"text": {"type": "string"}},
    "required": ["text"],
    "additionalProperties": false
  }
}
```

服务拒绝未知字段、已填写但无效的时间/版本/Schema、缺失可执行文件及越界可执行路径。工具目录不能是符号链接，可执行文件的符号链接也不能越出工具目录。含维护字段的示例见 [read_skill/tool.json](../local-tools/read_skill/tool.json)。业务配置的具体校验由对应程序负责；例如缺失技能目录会成为 `read_skill` 的业务错误。

### 进程协议 v1

stdin 只包含一个 JSON 请求。`arguments` 是经过 Catalog Schema 校验的模型参数，`config` 是可信管理配置；其余字段由服务填充：

```json
{
  "protocol_version": 1,
  "name": "read_skill",
  "call_id": "call-1",
  "scope": {"tenant_id": "example-tenant", "user_id": "example-user"},
  "session_id": "session-1",
  "run_id": "run-1",
  "caller": {"agent_id": "agent-1", "invocation_id": "invocation-1"},
  "arguments": {"name": "code-review"},
  "config": {"skills_dir": "skills", "max_skill_bytes": 262144}
}
```

可选字段还有 `idempotency_key`、`caller.parent_invocation_id`。工具涉及数据库或业务 API 时应按 `scope` 过滤数据。stdout 必须仅包含一个 UTF-8 JSON 响应，日志写 stderr：

```json
{"protocol_version":1,"text":"完整工具结果"}
```

可选 `error` 表示业务错误，允许同时保留 `text`。业务错误应以退出码 0 返回，以便模型处理；非零退出码、错误协议版本、非 JSON 或多份响应均为执行失败。工具不能指定 CallID，服务始终关联原调用。协议 v1 只支持即时结果，不创建长任务句柄。

执行时间受顶层 `timeout` 控制，取消或超时时终止本次工具进程。stdout 总量受 `max_output_bytes` 限制，stderr 上限 32 KiB，超限即终止并失败；结果还会经过 Catalog 的完整结果大小校验。管道等待额外最多 1 秒，工具应负责派生子进程的生命周期。失败时不回显 stderr、服务路径或凭据，不自动重试执行。

程序仅继承 PATH、系统目录、临时目录和语言环境变量；不会自动继承服务模型密钥或数据库凭据。工具仍使用服务的操作系统账号和权限运行，部署目录及配置应由管理员维护；此执行方式不提供操作系统沙箱。

### read_skill 和新增工具

目录组织为 `<skills_dir>/<name>/SKILL.md`。模型调用示例：

```json
{"name":"code-review"}
```

`read_skill` 返回完整 UTF-8 文本，包含 frontmatter 和正文；允许 `team/code-review` 等嵌套相对目录名。拒绝绝对路径、`..`、Windows 反斜杠路径和备用数据流路径。使用 `os.Root` 在文件打开阶段约束路径和符号链接，不能读取根目录以外的文件。只读取普通文件，按上限完整读取或明确失败，不截断技能。`max_skill_bytes` 缺省或 0 为 256 KiB，显式值在 1 字节至 4 MiB 内。结果仍需满足 Catalog 的 `max_output_bytes`。自动加载的工具共享注册配置和技能目录；用户数据访问应由业务程序依据请求 Scope 过滤。Catalog 将每个解析出的执行器绑定到当时的 Scope，禁止缓存执行器跨用户调用。

找不到技能、配置无效、文件不可访问、文件过大或不是 UTF-8 时返回响应的 `error`，映射为 `ToolResult.Error`，让模型可以修正选择。技能内容作为工具结果返回，不自动提升为系统提示，也不执行其中的脚本。

在 Windows 上构建仓库自带工具：

```powershell
./scripts/build-local-tools.ps1
```

其他平台可在仓库根目录运行 `go build -buildvcs=false -o ./local-tools/read_skill/bin/read_skill ./local-tools/read_skill`，同理构建 echo。二进制不纳入 Git，部署时将目标平台的二进制和 `tool.json` 放在一起。Go 工具可用 `pkg/localtool.Serve` 实现协议入口，其他语言按相同 JSON 协议实现即可。

新增工具的步骤是：创建工具目录，编写可执行程序和 `tool.json`，部署后重启服务即可自动加载。配置声明的版本应与部署程序匹配；服务不会运行程序来查询版本。旧清单中的 `local.config.<工具名>` 应迁移到对应工具的 `tool.json.config`。本地业务程序应依据请求中的实际 Scope 处理用户数据；工具配置和技能目录由部署者共享维护。

## 远端连接配置

管理员将远端连接写入 MongoDB `tool_connections` 集合。服务创建 `(tenant_id, user_id, id)` 唯一索引，启动时按这些字段稳定排序读取。空集合允许只加载本地工具，数据库读取错误会阻止启动。文档另有可选 `card_path`（A2A，默认 `/.well-known/agent-card.json`）、`credential_ref` 和 `credentials`。例如：

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

环境变量内容为完整 Authorization 头（例如 `Bearer ...`）；MongoDB 文档仅保存引用。每个连接的解析器只允许读取自己的引用白名单，并重验完整 Scope。解析在每次请求进行，支持外部轮转；密钥不进入 Descriptor、TaskHandle 或检查点。URL 禁止内嵌凭据、查询和片段，不跟随重定向、不读取 cookie、不自动重试 RPC。A2A Card 的 URL 必须与配置一致；Card 路径相对于配置 URL 的前缀，不会自行切换到 Card 指向的地址。

Catalog 在 List、Resolve、Execute 和任务操作处重验身份，缓存下来的执行器不能跨 Scope 使用。未授权和不存在统一为 NotFound。参数须为 JSON object，并通过已解析 JSON Schema；输入超限、错误关联、空 Outcome、同时有 Result/Task 都被拒绝。业务 `ToolResult.Error` 保留给模型；网络/协议错误阻止后续生成，不被误当作工具成功。

默认超大结果明确失败，不截断。代码装配可注入 `ArtifactWriter`，将完整 JSON 按 Scope 保存并返回小 URI；引用本身也受大小限制，失败标识不会丢失。默认 server 没有 artifact 存储服务，也不自动下载工具给出的 URI。远端工具必须响应 context；可执行本地工具在取消或超时时由进程适配器终止。

共用 `internal/adapter/toolhttp` 只负责 HTTP 安全策略与 Scope 校验，不包含 RPC/SSE 编解码。每个响应（包括订阅流）总量上限为 16 MiB，MCP 另使用 SDK 单事件上限；超限中断本地观察，由 P9 决定后续查询。A2A Card 的其他接口地址不会进入 SDK 的自动传输选择。

## ADK 与任务交接

工具声明随实际 OpenAI 请求发送，不计算 Token 预算。无工具时保留原 SSE 文本路径；有工具声明的轮次使用非流式完整响应，收齐工具参数后才允许执行。每个响应至多 64 个工具调用，循环另受 `max_model_calls` 限制。

模型函数名不兼容空格、点或过长名称时，桥接层生成稳定摘要别名；调用远端仍使用原名。普通调用 ID 在同 Run 内不可重复。真实模型输出的未知工具在执行前被拒绝。

即时调用以 assistant 的 `PartToolCall` 持久化；结果以 tool 的 `PartToolResult` 持久化，再送回模型。`agent.Update.MessageRole` 默认 assistant，仅允许 assistant/tool。模型历史中的工具资料不提升为 system。

实际返回 TaskHandle 后，桥接层记录 Scope、Session、Run、Agent/Invocation/ParentInvocation、CallID 和原参数，停止下一次模型调用。ADK v2.4.0 的工具 Context 不支持 `EndInvocation()`；实现使用 BeforeModel 边界截停，并有真实 SDK 测试证明不会额外调用模型。

`cagent.tools/v1` 检查点包含暂停时模型名、领域 LLM 交互消息、整批工具调用/句柄、调用次数及安全失败分类，不包含 SDK 状态。旧 v2 从 LLM 历史迁移，恢复不重放 SDK 事件。默认 `Update.Tasks` 由应用在当前租约/Fence 下调用已有 `Database.TrackTask`：每个任务与检查点及 `tool.waiting` 事件同事务保存，PendingCallIDs 逐项增加。事件返回本地 `task_id/tool_call_id/invocation_id`，不公开远端句柄。也可通过 `ToolOptions.Handoff` 注入负责持久化的回调。`agent.ErrWaiting` 让应用保留 `waiting_tool` 和会话占用，不能提交 completed；混合调用发生失败时先保存已知句柄，再按失败分类结束 Run，恢复时仍维护原任务。

多任务交接不是整批原子事务：每个 TrackTask 原子保存一个任务，应用恢复记录同时保存整批调用和已返回句柄。中途故障后，P9 从记录补齐缺失的 Task 行，不重执行工具；Run 已取消时也可补齐并结算迟到结果。远端已启动但任何句柄尚未落库的窗口仍需提供方幂等/关联查询支持，当前不声称外部副作用恰好一次。

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

全量命令见实施计划。外部冒烟为 `go test ./internal/bootstrap -run TestExternalToolSmoke -count=1 -v`：显式设置 `CAGENT_TEST_TOOLS=1`、`CAGENT_TEST_TOOLS_MONGODB_URI`、`CAGENT_TEST_TOOLS_MONGODB_DATABASE` 和 `CAGENT_TEST_TOOL_CALL`（JSON 包含 tenant_id/user_id/protocol/name/arguments）。该测试真实执行指定工具一次，可能产生该工具的业务副作用；默认跳过，不输出秘密或结果内容。句柄返回只验证接纳与关联，不验证 P9 生命周期。本次未配置外部提供方，未执行该冒烟。

P9 已实现 TaskTracker、观察错误记录/退避、Task 查询取消 API、恢复扫描、TaskDelivery 原子消费、Runtime.Resume 及 Invocation 分支调度。当前 server 自动观察任务并在结果持久接纳后恢复模型，详见 [长任务与恢复](tasks.md)。MCP SDK 的 tasks 能力限制仍保持明确的 Unsupported，不重执行工具。
