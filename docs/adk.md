# ADK 与 OpenAI 兼容执行（P6）

P6 的代码、真实 ADK SDK 行为测试、本地 HTTP 协议测试及 MongoDB 集成已实现。外部模型尚未配置凭据，真实模型冒烟验收待执行；本地协议服务不代表提供方已验证。P7 已接 HTTP 启动；P8 已实现工具执行及句柄交接，详见 [工具说明](tools.md)。P9 已实现任务持续观察、原子接纳和新 Runner 续接，详见 [长任务与恢复](tasks.md)。

## 依赖与装配

锁定 `google.golang.org/adk/v2 v2.4.0`、`google.golang.org/genai v1.71.0`、`github.com/openai/openai-go/v3 v3.66.0`。GenAI 仅用作 ADK 消息类型，本实现不调用 Gemini。

通过 `app.NewOpenAIService(parent, db, cfg, system, lifecycle)` 组合 P4 生命周期、P5 上下文准备、ADK Runner/LLMAgent 和 OpenAI 客户端。`parent` 是服务生命周期，`system` 必须来自可信配置；数据库应已连接并完成索引初始化。构造过程不会调用模型。租约与轮询仍使用 `app.Options`；该工厂不允许覆盖已装配的 Prepare/Recover 回调。测试或后续提供方可通过 `NewADKService` 注入 ADK Runtime。

ADK 自带 OpenAI 适配采用 Responses，本项目按用户要求使用 Chat Completions 兼容接口，通过官方 OpenAI Go SDK 实现 ADK `model.LLM`。请求路径为配置的 API 前缀加 `/chat/completions`；不要把完整方法路径填入 BaseURL。请求支持标准文本 SSE 和非流式文本，执行链路使用 SSE。参考 [Chat Completions 官方接口](https://developers.openai.com/api/reference/go/resources/chat/subresources/completions/methods/create)。

## 动态模型配置

生产启动时从 MongoDB models 集合读取模型、地址、供应商分类、加权加密 API key 列表及模型选项。创建会话时用户选择模型 _id；未指定则随机选择，没有默认模型。主对话模型与加权抽取的 API key 持久绑定到会话，子分支和摘要独立随机选择。Provider 仅分类，调用统一使用 OpenAI 兼容接口，模型文档不含 protocol 字段；主对话后续请求使用已绑定 key，子分支恢复使用其检查点绑定，深度思考参数由文档配置。AES-GCM 密钥通过版本化环境 keyring 注入，旧明文模型环境变量不再读取。完整结构、工具和轮换步骤见 [MongoDB 模型配置](models.md)。配置是启动时的只读快照，更换配置需重启实例。测试仍可直接构造 Agent 快照。

兼容端必须支持文本 Chat Completions SSE 和 `stream_options.include_usage`。主对话与摘要不计算 Token 预算，也不发送 `max_tokens` 或 `max_completion_tokens`。不同厂商的非标准扩展不自动兼容。禁止 HTTP 重定向，避免凭据跟随到另一地址；SDK 自动重试关闭，避免提交结果不确定时重复生成。错误的公开文本为稳定说明；原始 SDK 原因只供内部 `errors.Is/As` 诊断，不可直接打印。

P5 准备与 ADK BeforeModel 使用相同消息映射，系统指令和历史只发送一次；工具历史保持结构化角色和参数，超过 float64 精确范围的整数保持原文。工具 CallID 按原 Run 与局部 ID 映射，领域任务路由不变。

输入不进行本地 Token 计数、窗口校验或裁剪。模型响应中的 usage 保留为事件元数据，prompt_tokens 同事务随完整 assistant 消息持久化。下一轮准备时，最新响应的输入用量达到主模型 `config.window_tokens` 的 80%（可通过 `CAGENT_CONTEXT_COMPRESSION_THRESHOLD_PERCENT` 配置比例）才尝试自动摘要；缺失用量不估算、不累加或沿用旧响应用量。近期轮数只决定摘要范围。

## 输出、隔离与故障边界

每次 Execute 创建独立 SDK 内存会话，MongoDB 是持久数据的唯一真相。租户与用户的作用域在应用层读取、消息校验和检查点身份中保留；SDK 对象不会在并发 Run 间复用。领域 AgentID、InvocationID、ParentInvocationID 与 SDK InvocationID 分别写入事件 JSON，避免混淆调用链。

文本增量作为 `message.delta` 事件持久化，不追加为历史消息。收到正常 `stop`、完整读取响应并完成 SDK 工作流后，发出一次 `message.completed`；完整 assistant 消息、完成检查点、事件及回执在同一数据库事务提交。客户端应把 completed 看作最终内容，不能再次拼到 delta 后。随后应用提交 `run.completed`。没有正常结束、空回复、截断、拒绝或流错误均不会伪造完成消息。P8 开启工具后使用完整非流式响应，调用/结果成对持久化；返回任务句柄则登记并保持 waiting_tool。文本累计限制为 8 MiB，整体容量治理仍属 P10。

如果最终消息事务提交后、Run 终态提交前服务退出，`RecoverRun` 读取完成检查点，校验版本、作用域、调用身份和模型，恢复 SDK 会话后只结算 Run，不重复调用模型或发布消息。没有完成检查点的中途执行不能通过重新 Execute 冒充恢复，会明确拒绝。取消沿服务 Run context 传递到 SDK 与 HTTP；已持久化的部分事件可重放，取消或故障后不将部分文本写成完整历史。

## 检查点与真实 SDK 恢复验证

格式固定为 `adk-go/2.4.0/completed/v1`。载荷包含 SDK 事件、最终状态、最终事件 ID、模型及领域身份，不包含客户端配置或 API 密钥。SDK 会话可能包含业务内容，应沿用数据库访问控制。恢复时先重放事件，再应用最终状态；不序列化 goroutine 或迭代器。未知格式、错误身份、有待完成调用的检查点拒绝恢复。

`checkpoint_test.go` 使用真实 ADK Runner、长运行函数工具和会话服务：触发暂停，将实际事件/状态序列化，创建全新服务与 Runner，提交匹配的工具结果，验证继续生成且工具没有重执行。模型响应由测试替身提供，但暂停、序列化、恢复控制流由真实 SDK 执行。依据 [ADK v2.4.0 Runner 实现](https://github.com/google/adk-go/blob/v2.4.0/runner/run_node.go) 核对恢复边界。

该探针只确认 SDK 能力；P9 另以生产 `Runtime.Resume` 和真实 MongoDB 验证 TaskDelivery 原子接纳、双任务续接与重启。应用按 Invocation 调度已有分支，SDK 测试验证精确 Caller 关联；SDK 升级必须重新验证格式与恢复行为。多模态模型输入仍未实现。

## 验证方式

本地测试覆盖动态 URL/模型/密钥、不发送 Token 限额、长输入、SSE/usage、429 不重试、超时/取消、截断/拒绝/工具输出、禁止重定向、错误脱敏、并发隔离及整数精度。应用集成使用真实临时 MongoDB 副本集，覆盖输入到持久输出、幂等重放、完成检查点恢复、失败无完整消息和无检查点。

```powershell
$env:CAGENT_TEST_MONGOD = (Resolve-Path .local/mongodb/*/bin/mongod.exe).Path
go test ./... -count=1
go test -race ./internal/adapter/adk ./internal/app ./internal/contextengine ./internal/config ./internal/adapter/mongodb ./internal/domain -count=1
go vet ./...
go build ./...
```

真实冒烟需先在 MongoDB 中创建模型文档，并在环境中设置数据库连接、模型选择器及加密 keyring，再执行：

```powershell
$env:CAGENT_TEST_MODEL = '1'
go test ./internal/adapter/adk -run '^TestRealOpenAISmoke$' -count=1 -v
```

此测试会实际调用模型并可能计费，使用配置的请求超时，只检查最终回复和 usage，不打印提示词、回复、地址或密钥。未显式启用时跳过。当前尚无真实模型凭据，因此该项验收未通过也未声称通过；无需在聊天中提供密钥。
