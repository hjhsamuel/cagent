# P9 长任务、取消与恢复

P9 的代码与本地验收已完成。使用真实 MongoDB 8.0.32 临时副本集、真实 ADK/OpenAI SDK 和本地模型 HTTP 服务验证；外部模型及 MCP/A2A 提供方仍需部署环境配置后补验。

## 服务内生命周期

`Application` 在 Run 生命周期中交替执行任务维护和可执行分支，不增加工具 Worker 或执行队列。`taskTracker` 绑定当前 Run 租约及暂停检查点：Track 原子保存原调用/句柄/检查点/等待事件；Get 读取作用域内持久快照；Follow 按版本通知持久快照，同步回调施加背压；Cancel 先写取消意图。HTTP/SSE/Follow 断连不会取消远端任务。

工具是否异步由实际返回的句柄决定。即使远端任务已经完成，仍登记为已接纳任务，再通过同一观察、交付和消费流程取得真实终态。身份始终包含 Scope、RunID、完整 Caller、InvocationID、ParentInvocationID 和 ToolCall ID，不能仅按 Agent 名称匹配。

任务观察每页最多 32 个任务、最多 8 个并发网络操作。提供方支持 Follow 时优先订阅；不支持、断流或超时后查询同一持久句柄。Catalog 把订阅通知当作查询信号，完整查询协调重复/乱序通知，持久游标在下次订阅时恢复。任务确认终态后不再向提供方观察，因此旧进度不会覆盖终态。

网络调用在写锁之外运行，续租和事务提交串行。进度和终态先提交 MongoDB，再通过持久 SSE 重放可见。`tool.waiting/tool.progress/tool.finished` 携带本地 Task ID 和完整调用关联；进度不追加为模型历史。查询失败记录静态安全的 ObservationError 并退避，既不更改远端状态，也不调用 Executor.Execute。一次服务停止会取消并等待观察，不持久化为用户取消。

## 结果接纳与 SDK 推进

结果处理分为两个边界：

1. ObserveTask 原子提交终态 Task 和 pending TaskDelivery。
2. Runtime.Resume 加载原分支检查点，校验完整调用关联，仅合并一个结果。同步 Emit 由 ApplyTask 原子保存工具结果消息、新检查点、AppliedAt、delivery=applied 和事件。Resume 本身不调用模型。
3. 全部依赖到齐的分支才可 Recover；新 Runner 恢复 SDK 会话，并发送包含真实工具响应的历史。已接纳结果只出现一次，已执行调用 ID 保留在去重集合中，模型轮数预算跨暂停保留。

`cagent.adk.tools.v1` 保存原模型/上下文、SDK 日志和状态、实际模型历史、整批句柄、已接纳响应及模型调用计数。兼容 P8 缺少新字段的快照，历史由初始上下文与 SDK 工具事件重建。完成检查点仍为 `adk-go/2.4.0/completed/v1`，恢复该边界只结算 Run，不重新调用模型。

同名 Agent 的不同 Invocation 各有检查点。一个分支接纳全部依赖后，立即停止本轮其他未结束的观察并推进该分支；其他任务下轮从原句柄/游标继续。只有所有未完成分支都在等待依赖时才聚合为 waiting_tool；所有分支完成才释放会话占用。应用层分支调度测试使用运行时替身验证独立路由，真实 ADK 测试另外验证带父调用标识的双任务接纳与新 Runner 恢复。当前启动配置仍使用已有的 ADK Agent 装配。

## 崩溃窗口

| 持久边界 | 恢复行为 |
| --- | --- |
| queued 尚未执行 | 取得租约后首次 Execute |
| 已开始但没有安全检查点 | 记录不确定结果，禁止重新 Execute |
| 整批句柄仅登记了部分 Task | 逐个按原调用唯一键补齐；不重新调用工具 |
| 终态 Task + pending delivery 已提交 | 加载原分支，执行一次结果接纳 |
| ApplyTask 已提交，响应丢失或尚未推进模型 | 已消费任务不再参与扫描，从已更新检查点推进 |
| 完成消息和完成检查点已提交 | 不重发消息、不重调模型，只结算 Run |
| 恢复后再次进入外部生成，但新安全边界尚未提交 | 使用持久 in-flight 屏障记录不确定结果，禁止从旧结果检查点重放 |

进入恢复后的新一轮 SDK 生成前，应用把该分支持久标记为 `cagent.in-flight/v1`。新的任务交接或最终消息会覆盖屏障。若进程在屏障期间中断，下次恢复将 Run 结算为 failed，并在 `run.failed` 事件载荷中保存 `{"reason":"execution_outcome_uncertain","retry_tool":false}`。工具 Execute 的传输错误或返回无法关联的句柄也保留这种不确定语义；超时不是“远端肯定没启动”。

目前提供方边界没有通用的幂等查询/按调用找回丢失句柄能力，所以使用上述保守分支，不能声称外部模型/工具副作用恰好一次。运维需要结合提供方记录人工核对；服务不会猜测一个任务 ID，也不会自动重执行。屏障可能把仅有模型计算、尚未产生工具副作用的中断也标为不确定，这是避免重复副作用的明确取舍。

## 取消和暂停

`Database.CancelTask` 是受认证 Scope 约束的用户命令，不要求持有执行器租约。它用事务保存首次 CancelRequestedAt 并推进 Task/Run 版本，不推断远端终态。Run 持有者随后请求远端 Cancel；即使返回成功，也仍查询真实状态。取消不支持、取消与完成竞争时，保留实际远端结果。

CancelRun 先提交本地终态并撤销生成租约，禁止继续模型输出。服务仍取得维护租约观察已有任务、补齐检查点中的遗漏句柄，迟到结果保存后以 discarded 结算，不写 AppliedAt，也不在终止事件后追加 SSE。没有恢复扫描器的本机应用同样会在显式取消后继续维护；进程重启则由扫描器接管。

input_required/auth_required 是暂停，不是失败。已有 input/authorization API 向原任务补充输入或可信凭据引用，之后 P9 观察器继续跟踪同一任务，终态仍走原子消费流程。202 只说明交互或取消请求被接纳，不说明远端完成。

## HTTP

- `GET /api/v1/tasks/:taskID`：200 返回持久状态、进度、结果、完整调用标识、取消意图、观察错误和消费时间；不返回 Scope、远端句柄、连接引用、原始参数或 SDK 检查点。
- `POST /api/v1/tasks/:taskID/cancel`：202 表示取消意图已持久化，不读取客户端提供的远端任务参数。
- `POST /api/v1/tasks/:taskID/input`：`{"text":"回复"}`。
- `POST /api/v1/tasks/:taskID/authorization`：`{"text":"继续","credential_ref":"已配置引用"}`。

所有接口使用 JWT 作用域；跨租户/用户与不存在一样返回 404。结果 Part 使用 `kind/text/mime_type/uri/data` 白名单，data 为 base64；工具结果包含 `tool_call_id/parts/error`。

## 启动与参数

启动装配单独打开 `mongodb.Recovery` 管理连接，调用 `Application.StartRecovery`。每页扫描 64 个候选，末页后从头开始；扫描不是领取，候选仍须取得原 Run 租约并重读。覆盖非终态 Run 和带未结算 Task 的终态 Run，多实例靠数据库 Fence/CAS 协调。关闭先停止应用中的扫描和运行，再关闭管理连接和普通数据库。

| 配置 | 默认 | P9 消费方 |
| --- | --- | --- |
| CAGENT_TASKS_POLL_INTERVAL | 2s | 正常任务维护周期和持久 Follow 轮询 |
| CAGENT_TASKS_OBSERVATION_TIMEOUT | 30s | 单次订阅、查询和取消请求，不限制远端任务寿命 |
| CAGENT_TASKS_RECONNECT_BACKOFF | 1s | 观察失败退避、运行维护失败重试、启动恢复扫描间隔 |

未新增环境变量。当前全局运行并发/容量策略仍属于 P10；本项限制单 Run 任务观察并发及分页内存。

## 验证

```powershell
$env:CAGENT_TEST_MONGOD = (Resolve-Path .local/mongodb/*/bin/mongod.exe).Path
go test ./... -count=1 -timeout=180s
go test -race ./internal/app ./internal/adapter/adk ./internal/adapter/mongodb ./internal/bootstrap ./internal/transport/httpapi -count=1 -timeout=180s
go vet ./...
go build ./...
```

行为测试覆盖真实双任务 SDK 续接、乱序接纳、重复结果拒绝、提交失败不调用模型、跨调用拒绝、部分登记中断、恢复后再次中断的屏障、两个独立数据库客户端竞争恢复、观察错误/订阅游标/查询回退、暂停补充输入和授权、任务取消不支持、Run 取消与实际成功竞争、持久 Follow 断开、任务 HTTP 身份/DTO 隔离及 SSE 终止水位。

MongoDB 多节点选举/网络分区、生产认证/TLS、真实模型、外部 MCP/A2A 提供方仍未配置验证。官方 MCP SDK 当前没有 tasks API，其观察/取消仍返回 Unsupported，P9 记录观察失败而不会重执行；A2A SDK 不暴露 SSE ID，恢复依靠完整查询，不能宣称提供方事件重放已经可用。
