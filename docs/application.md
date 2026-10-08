# 应用服务与持久事件流（P4）

`internal/app.Application` 直接调用 `*mongodb.Database`，实现会话创建/查询、Run 提交/查询/取消、服务内运行生命周期和恢复接入。P5 已提供可注入的预算化准备器；真实模型、HTTP/SSE 编码与启动装配在 P6/P7 实现。

## 装配与生命周期

通过 `NewService(processContext, db, runtime, Options{LeaseDuration: 30*time.Second, PollInterval: time.Second})` 构造。进程 context 不能使用 HTTP 请求 context；runtime 必须实现 `agent.Runtime`，响应 context 和 Emit 返回的错误，并在返回前停止自己的生成工作。租期至少 3 ms，轮询周期为正且不超过租期的三分之一，生产值还应覆盖数据库事务延迟。

`CreateSession` 生成可信资源 ID，返回数据库时间和版本。`StartRun` 在请求 context 下原子保存输入/Run/会话占用，提交后用服务 context 调度；请求断开不取消已接纳的运行。关闭与接纳竞争时，已持久化但尚未启动的 queued Run 留给恢复入口。幂等键必须由客户端稳定复用；同键同输入返回原 Run 的当前状态，不同输入冲突。没有键不保证重复提交幂等。

本地执行表只去重及发送取消信号。每次进程启动使用独立 owner，真正的跨实例互斥由数据库 AcquireLease 和 Fence 保证。执行前领取租约并重读状态；续期和输出提交在本地串行更新凭证。失去租约立即取消执行 context，后续提交仍受数据库租约/版本限制。取消先提交数据库事实和终止事件，再发送本地取消；远端实例在续租时发现租约撤销，停止生成。取消重试最多八次 CAS，持续竞争时返回可重试的冲突。

`Close(waitContext)` 拒绝新增调度、取消服务 context，并等待活动工作退出；重复调用共享等待信号。正常服务关闭保留非终态运行供恢复，不将其误标为用户取消或模型失败。等待超时后可再次等待，数据库应在退出完成后关闭。Go 无法强制终止不响应 context 的第三方运行时。

## 输出与恢复

首次执行先原子提交 running 和 run.started。`agent.Update` 只允许消息/工具事件；`Message` 非 nil 表示完整 assistant 消息，与事件同事务提交。增量只放 Data，不能逐片写成历史消息。应用生成消息身份，分配序号与时间由数据库完成。正常返回提交 completed，运行错误提交 failed；应用决定运行终态，运行时不能自己发送终止事件。错误原文不进入公共事件。Emit 提供同步持久化背压，错误被记住，即使运行时忽略错误也不能报告成功；返回后的迟到回调被拒绝。

提交失败或响应未知时停止生成并保留持久状态，不盲目产生新的逻辑提交。`DurableEvents.Publish` 接受完整 `store.CommitRunRequest`，调用方可以保留稳定 OperationID/内容重试以读取回执。它替换了原先缺少租约和原子状态边界的 `Publish(Event)` 占位接口。

默认请求准备按页读取原始会话历史（已包含当前输入，不再追加第二份），调用身份默认 AgentID 来自会话、InvocationID 为 Run ID。`Options.Prepare` 可直接注入 P5 的 `NewContextPreparer`，详见 [上下文说明](context.md)；返回的 Run 不得改变、Caller 必须有效。默认路径没有预算控制，仅用于 P4 替身；真实模型装配必须采用预算化准备器，P5 对超预算明确报错。

`RecoverRun(ctx, scope, runID)` 是 P9 扫描器可调用的接入点，返回表示调度已接纳，不表示租约已领取或恢复已完成。queued Run 可以首次 Execute；running/waiting Run 必须配置 `Options.Recover`，否则返回能力不支持，绝不重新 Execute。Recover 回调负责读取原分支检查点并恢复，Prepare 可预先加载检查点。P6/P9 已验证真实 SDK 检查点恢复、长任务等待聚合、原子交付和跨租户扫描循环。终态 Run 的任务由维护租约继续观察，迟到交付被丢弃，详见 [长任务与恢复](tasks.md)。

## 订阅与部署

`NewEventStream(db, pollInterval, pageSize)` 使用 MongoDB 一致快照分页轮询。Follow 从排他游标开始，按顺序同步调用消费者；历史追赶和实时跟随没有不同的数据来源，因此没有本地通知切换窗口，独立进程/客户端提交也可见。最大空闲观察延迟约为轮询周期加数据库读取耗时。

每个订阅只持有一页事件，页大小受 store 上限约束，没有生产者 channel 或无限队列。慢消费者暂停后续读取，不阻塞生产或其他订阅；单条事件大小和总订阅数的容量限制在 P7/P10。HTTP 层必须设置写超时并让回调响应断连，不能以另起 goroutine 的方式掩盖无限阻塞回调。连接断开只取消 Follow，不取消 Run。

`EventPage.Terminal` 与 LastSequence 在同一快照读取；终态且消费到持久水位就退出，已消费终止事件的重连也立即结束。游标过期返回 `store.ErrCursorExpired`，未来游标返回参数错误，消费者错误原样返回。清理在消费页之间发生时会明确报过期，不静默跳过缺失事件。

部署要求与 P3 相同：支持事务的副本集或 mongos；不依赖 change stream。已验证单节点副本集和独立 MongoDB 客户端，未验证多节点故障或生产吞吐。

## 行为验证

测试使用真实临时副本集和 Runtime 接口替身。设置 `CAGENT_TEST_MONGOD`（本仓库 `.local/mongodb/.../bin/mongod.exe`）自动启动隔离测试实例；也可设置 `CAGENT_TEST_MONGODB_URI` 使用已有测试副本集。数据库名使用自动生成的 `cagent_p4_test_*`，测试结束清理本次数据库。未设置测试环境时明确 skip，不视为集成通过。

```powershell
$env:CAGENT_TEST_MONGOD = (Resolve-Path .local/mongodb/*/bin/mongod.exe).Path
go test ./... -count=1
go test -race ./internal/app ./internal/adapter/mongodb ./internal/store ./internal/domain -count=1
go vet ./...
go build ./...
```

覆盖：请求断开、输入不重复、完整消息持久化、异常终态、同键并发/不同输入冲突、作用域隔离、取消幂等与远端取消、迟到输出拒绝、服务关闭、queued 首次恢复/已开始运行的恢复回调、租期跨越、终止游标重连、回执重放、事务冲突不发布、跨客户端追赶和空闲跟随、慢消费者背压、回调失败、订阅取消、未来/过期游标及参数校验。


P6 新增 app.NewOpenAIService 工厂，自动装配预算化准备、真实 ADK 和完成检查点恢复。最终消息、message.completed 事件与检查点同事务提交；恢复已提交输出不重复调用模型。新增集成测试使用真实 ADK、本地 OpenAI 协议服务与 MongoDB，真实外部模型验收仍待配置。详见 [ADK 接入说明](adk.md)。
