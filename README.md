# cagent

完整模块实施计划、验收要求及跨会话进度见 [实施计划与进度](docs/implementation-plan.md)。当前 P1–P9 及 P10.1/P10.2 的代码与本地验收已完成。P10.3 已补齐整体链路、提交断连故障测试及交付检查脚本；复现步骤和外部验收门槛见 [整体验收](docs/acceptance.md)，部署、凭据配置及排障见 [部署交付](docs/deployment.md)。外部模型和工具提供方、生产部署环境仍待验证，项目未标记全部验收完成。

使用 Go 构建的多用户 Agent 服务，采用 Gin + SSE、MongoDB 和 Google ADK，并支持 MCP、A2A、持久化异步工具任务及上下文压缩。

当前包含领域校验、公共错误、配置、日志、状态规则、存储契约及 MongoDB 仓储/事务/租约/恢复管理实现，已实现应用执行链路、跨实例持久事件轮询和动态 OpenAI 兼容模型客户端。启动入口加载完整配置、连接 MongoDB、装配预算化模型服务并监听 JWT 认证的 HTTP，支持优雅关闭。依赖 MongoDB Go v2.9.1、ADK v2.4.0、OpenAI Go v3.66.0、Gin v1.11.0 和 JWT v5.3.1；MCP/A2A 使用官方 Go SDK：modelcontextprotocol/go-sdk v1.8.0、a2aproject/a2a-go v0.3.15；MCP tasks 暂不支持，A2A 保留长任务能力。

## 模块

| 目录 | 职责 |
| --- | --- |
| `cmd/server` | Web 服务与 Agent 运行时入口 |
| `internal/bootstrap` | 配置依赖装配、真实监听、就绪与优雅关闭 |
| `internal/transport/httpapi` | Gin 路由、JWT 身份、DTO、错误与 SSE |
| `internal/config` | 默认配置、环境变量加载、类型解析与集中校验 |
| `internal/observability` | 容量闸门、固定类别指标、有界本地追踪、日志脱敏与轮转 |
| `internal/domain` | 用户作用域、会话、消息、运行、事件、任务、上下文快照 |
| `internal/apperrors` | 稳定错误类别、字段路径、安全说明及底层原因包装 |
| `internal/app` | 会话/Run 用例、运行生命周期与取消、持久事件发布/重放/跟随 |
| `internal/agent` | Agent/Subagent 执行、长任务跟踪与恢复接口 |
| `internal/contextengine` | 已实现上下文组装、工具配对、摘要覆盖、Token 预算与 P10.1 有界压缩策略 |
| `internal/tool` | 作用域注册、发现、参数/结果/权限边界和任务观察接口 |
| `internal/adapter/mcp`、`a2a`、`toolhttp` | 官方 MCP/A2A SDK 适配、HTTP 安全策略与 A2A 已有任务观察 |
| `internal/adapter/adk` | 预算化模型执行、工具桥接及 SDK 检查点 |
| `internal/store` | 存储请求/结果与纯校验；具体方法位于 MongoDB 适配器 |
| `internal/adapter/mongodb` | 已实现仓储、索引、事务、租约、幂等回执及恢复管理 |

P2 定义的 Run/输入/会话占用原子提交、租约 Fence、幂等回执、序号分页、检查点和交付事务已由 P3 实现。真实 MongoDB 8.0.32 单节点副本集测试覆盖独立客户端竞争、回滚、租约到期、未知提交结果和恢复消费。详见 [存储契约](docs/storage-contracts.md)及 [MongoDB 适配与集成测试](docs/mongodb.md)；多节点部署故障尚未验证。

已实现的公共组件包括领域校验、公共错误、配置、日志和领域状态规则，并附中文注释与行为测试。Run/Task 支持状态合法性及迁移校验；Task 区分暂停、取消意图和观察错误，保护已确认终态，重复快照不产生无变化更新。错误支持 `errors.Is/As`，关联 ID 在日志输出位置通过 WithFields 显式添加；P10.2 已接入日志字段脱敏、容量控制、指标和本地追踪，详见[运行保障与保留策略](docs/operations.md)。

P4 装配、生命周期、恢复接入与事件背压详见 [应用服务与持久事件流](docs/application.md)。真实副本集行为测试验证请求断开、幂等并发、跨实例取消与事件传播、续租、恢复接入、游标及慢消费者；P4 测试使用运行时替身；P6 已补充真实 ADK、本地模型协议及 MongoDB 集成，P9 已补充工具任务恢复，详见 [长任务说明](docs/tasks.md)。

P5 已实现预算化上下文准备：保留固定约束、完整工具对与未完成调用，超预算在调用运行时前明确失败；通过 `app.NewContextPreparer` 接入。详见 [上下文基础能力](docs/context.md)。实际模型计数在 P6 验证，P10.1 已接入摘要生成、原文保护、预算回退与并发快照保存。

## 本地检查

配置规则和全部环境变量见 [配置约定](docs/configuration.md)，示例见 [`.env.example`](.env.example)。加载优先级为默认值 < 已设置的环境变量，显式空值不会回退；完整配置要求显式设置 MongoDB URI、模型提供方和模型名。程序不自动读取 `.env`；实际启动还必须设置 JWT 密钥（固定 HS256），详见 [HTTP 与启动装配](docs/http.md)。

日志使用 Logrus v1.10.2 和 Lumberjack v2.2.1：启动时用 `observability.Init` 配置已有默认实例，业务直接调用 `logrus.Info/WithFields`，多个 goroutine 共用。默认 info 文本日志写入 `/app/logs/cagent.log`，50 MiB 轮转、保留 3 个 gzip 备份；通过 CAGENT_LOG_LEVEL/PATH/SIZE/ROLL 调整，部署时设置适合本机的路径。退出时调用 `observability.Close`。详情见 [日志约定](docs/logging.md)。

使用满足 `go.mod` 的 Go 工具链：

```sh
go test ./...
go build ./...
go vet ./...
go run ./cmd/server
```

运行前请按配置文档设置环境；真实数据库测试需设置 CAGENT_TEST_MONGOD 或专用测试 URI，未配置时会明确跳过。架构、接口语义、路由规划及后续实现顺序见 [架构文档](docs/architecture.md)。

异步任务指工具调用返回的持续执行任务（类似 A2A Task）。Agent 或 Subagent 根据实际返回的任务句柄进入长任务处理流程，跟踪进度并在结束后恢复原调用。项目不设置独立 Worker 入口或任务执行队列。



P6 的配置、预算估算、流式输出、完成检查点恢复及验收边界见 [ADK 与 OpenAI 兼容执行](docs/adk.md)。模型名、API 地址和密钥均可配置。

工具配置见 [P8 工具说明](docs/tools.md)：设置 `CAGENT_TOOLS_FILE` 指向可信清单后启用。已支持即时结果回传、长任务持久交接/持续观察、原子结果接纳和恢复模型生成，详见 [P9 长任务与恢复](docs/tasks.md)。
