# 部署、凭据与排障

## 构建与启动

使用 go.mod 要求的 Go 1.27.1 工具链，在通过 [整体验收](acceptance.md) 后运行 `go build -o cagent-server ./cmd/server`（Windows 输出名可用 cagent-server.exe）。服务只有这一个入口；工具实际执行在提供方，不部署独立 Worker。

1. 准备支持事务的 MongoDB replica set 或 mongos；standalone 启动会被拒绝。生产账号须有业务集合读写、集合/索引初始化及恢复管理查询能力。认证/TLS 通过驱动 URI 设置，连接信息由秘密管理系统注入，禁止写入仓库。拓扑、schema=1、集合和索引清单见 [MongoDB](mongodb.md)。启动自动幂等初始化索引；不兼容索引/排序规则应先排查并备份，不直接删除生产集合。
2. 依据 [完整配置](configuration.md) 和 [示例](../.env.example) 注入环境变量，或填写当前工作目录的 `.env` 供服务自动加载；已有环境变量优先。示例仍需补齐实际部署配置。设置 MongoDB URI/数据库、监听地址、可写日志路径及关闭宽限期。
3. 在 MongoDB 模型文档中配置 provider/model/base URL/API key 和请求超时；自动摘要依据 LLM 报告的输入用量触发，摘要独立随机选择模型和凭据。真实兼容性须先运行外部冒烟。见 [ADK](adk.md)。
4. 配置 JWT 随机密钥（至少 32 字节），算法固定 HS256；可由可信身份服务签发带 tenant_id/sub/exp 的令牌，或调用 `POST /api/v1/auth/login` 生成随机身份 JWT，无须鉴权或账号密码。详见 [登录与 JWT 签发](http.md#登录与-jwt-签发)。JWT 密钥轮转需协调签发方和实例，当前无多密钥过渡机制。仅通过 TLS 入口传输令牌；服务自身没有 HTTPS 监听配置，应由部署方反向代理终止 TLS。
5. 远端 MCP/A2A 按 [连接文档示例](tools.example.json) 写入 MongoDB `tool_connections`，配置完整租户/用户授权、连接 ID、协议、可信 URL 与工具白名单。`credentials` 的值是环境变量名，环境变量内容为完整 Authorization 头。本地工具自动全量扫描 `local-tools`（可用 `CAGENT_LOCAL_TOOLS_DIR` 覆盖）；部署各工具的 `tool.json` 和可执行文件，业务配置写在 `tool.json.config` 中。仅部署远端工具时保留空本地目录。A2A 任务恢复依赖相同 Scope、连接 ID、远端服务和原模型配置，升级时不要更改这些引用或删除旧授权。
6. 启动二进制，确认 `/healthz` 和 `/readyz` 返回 200，再使用真实 JWT 创建会话/Run 并消费 SSE。路由、DTO、幂等、错误码、暂停补充输入和授权接口见 [HTTP](http.md)。就绪仅证明数据库可达，不证明模型/工具可用。

代理须关闭 SSE 响应缓冲，允许 Authorization 和 Last-Event-ID，空闲超时应大于心跳间隔；客户端持久记录已处理序号，用同一 Run ID 重连。EOF 不代表完成，须看到终止事件或查询 Run 状态。

多实例共享同一数据库与相同身份/工具配置，租约和 Fence 在数据库协调。容量是单实例限制，不是集群配额；配置容量时同时计算实例数及模型提供方限额。无强制粘性会话要求，SSE 通过数据库轮询跨实例跟随。

## 关闭、升级与数据责任

停止前移除流量并发送 SIGINT/SIGTERM，由服务撤销就绪、结束 SSE、等待应用并关闭数据库。预留不少于配置宽限期的终止时间；Windows 使用支持控制台中断或等价优雅停止的进程管理器。直接强杀可能落入“外部执行结果不确定”窗口。

重启后自动扫描未完成 Run 和未结算任务；不要为恢复重复提交新工具操作。安全检查点可续接，无检查点或残留 in-flight 屏障时明确失败。该保守行为不能保证外部副作用恰好一次，见 [任务恢复](tasks.md)。升级前备份并验证恢复，检查检查点格式与新 SDK 兼容性；当前没有自动 schema/检查点迁移器，不承诺任意版本降级。

不启用业务 TTL；不要手动删除活动任务、pending 交付、检查点或幂等回执。事件只能通过原子前缀清理推进水位，不能单独 TTL。原历史保持不删，压缩只产生派生快照。默认没有 artifact 后端，超大工具结果拒绝；自定义存储的授权、持久性和引用保留由部署方负责。完整逐集合责任见 [运行保障](operations.md)。

## 排障

| 现象 | 检查与处理 |
| --- | --- |
| 启动失败或 readyz=503 | 检查数据库 primary、事务拓扑、认证/TLS、索引和排序规则；查看安全错误类别，不要打印 URI/完整配置 |
| 401 | 核对 HS256、密钥、exp/nbf、机器时间、tenant_id/sub；不使用普通身份头代替 JWT |
| 404 | 同时可能表示资源不存在或当前 Scope 无权访问；核对签发身份和本地资源 ID |
| 409 | 活动 Run 占用、幂等输入不一致或版本竞争；相同输入用原幂等键找回，不另起工具任务 |
| 503 overloaded | 查看实例容量和 Retry-After；SSE 可重连，Run 提交保持原幂等键；调整容量前检查提供方限额 |
| SSE EOF/410 | EOF 用最后已消费序号重连；410 表示历史前缀已清理，查询当前状态并重新同步，不假设遗漏事件成功 |
| waiting_tool/observation_error | 查询本地 Task，核对原连接与远端任务；观察失败不代表任务失败，不重复 Execute |
| input_required/auth_required | 使用原本地任务的 input/authorization 路由；只传预授权 credential_ref，不传凭据、URL 或远端句柄 |
| execution_outcome_uncertain | 人工核对远端副作用及原调用关联；不能从旧检查点盲目重放 |
| 模型拒绝输入/大结果失败 | 检查供应商错误和工具输出大小；上下文不进行本地 Token 预算计算或裁剪 |

日志字段已脱敏，日志 Message 必须保持固定文本；默认本地轮转。可配置独立诊断凭据访问 `/debug/metrics` 与 `/debug/traces`，普通 JWT 不适用，未配置时不启用。追踪最多 256 条、重启丢失，没有 OTLP 导出；按 request_id/run_id/invocation_id 关联排障，详见 [日志](logging.md) 和 [观测](operations.md)。
