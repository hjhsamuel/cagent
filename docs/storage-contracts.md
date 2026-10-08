# 存储契约（P2）

本文件描述 P2 定义的存储契约；P3 已在 `internal/adapter/mongodb` 实现，并通过真实 MongoDB 8.0.32 单节点副本集的事务、独立客户端竞争和恢复消费测试。具体实现、测试命令与未验证部署范围见 [MongoDB 说明](mongodb.md)。领域及 store 契约类型不带 BSON/JSON 标签，适配层自行定义文档。

调用方直接使用 `*mongodb.Database` 的具体方法。`store` 只保留请求、结果和纯校验，不定义 interface；读取与写入直接操作 MongoDB collection，不增加泛型 CRUD 或仓储转发层。

## 共同约定

- 普通仓储方法必须验证非空可信 Scope；全部读写过滤条件含 tenant_id、user_id 和父资源 ID。无权限作用域内找不到对象返回 ErrNotFound，不泄露其他作用域资源。
- 新建文档 Version=1，请求中的零版本只表示新建/尚不存在。每次事务中发生变化的资源递增一次版本；任何运行相关业务变化也递增 Run.Version，使同一 Run 的并行分支写入通过 CAS 串行化。无变化不递增，版本及序号到 int64 上限报冲突，不能回绕。
- 适配器校验领域身份/关联与 P1.5 状态规则。校验函数只能检查传入的已读对象，不替代数据库原子条件更新。无效输入报 ErrInvalidArgument，版本/占用/旧持有者/幂等内容冲突报 ErrConflict。
- 只有 Database 的运行/任务事务方法、受租约保护的 Database.SaveSnapshot 可写运行数据；旧的 Run/Task Create/Save、Message/Event Append 已删除，没有绕过跨集合事务的通用入口。
- 创建及更新时间、序号、递增后的版本由存储生成，调用方不得伪造。调用方持有独立对象副本；返回的切片/检查点/回执不得与适配器缓存共享可变内存。

## Run、会话占用与租约

Session.ActiveRunID 持久表示会话占用，不是租约：原 Run 未终态时，即使持有者离线或租约过期，也不能启动另一个 Run。创建运行的唯一入口为 Database.StartRun。

Start 按以下顺序在事务内处理：

1. 验证 Run/Input 作用域和关联，要求 queued、Run.Version=0、Input.Role=user、Input.Sequence=0。
2. 对非空幂等键先查询 Scope+SessionID+Key。存在时比较 InputFingerprint，相同返回最初创建的 Run/Input/会话版本回执并标记 Replayed，不重复追加输入；不同输入返回冲突。这个分支先于当前会话版本和活动占用检查。Run 的实时状态通过 Get 查询。
3. 不存在幂等命中时检查会话存在、预期版本相等、ActiveRunID 为空；原子创建 Run、分配输入序号、追加输入、设置 ActiveRunID、递增 Session.Version、保存幂等记录/回执。任一步失败全部回滚。

空键不建立幂等记录，每次调用是新请求；纯空白键拒绝。非空键保持原字节，不 trim。输入摘要与最初 StartRunResult 作为 runs 文档的存储元数据保存，后续 Run 状态变化不改写创建回执。InputFingerprint 是带版本前缀、长度分隔和 nil 标记的 SHA-256 编码，包含所有 Part 字段及顺序；不包含重新生成的 Run/消息 ID、时间、预期版本。未来 StartRun 新增任何影响语义的字段须升级摘要版本。摘要不作为安全标识或公开日志字段。

Database 的租约方法 以数据库时钟控制写入权限：

- Acquire 仅在没有有效持有者或已到期时成功；相同 Owner 在租约有效时再次 Acquire 也冲突，必须 Renew。每次授予递增持久 Fence，Owner 使用每次进程启动唯一 ID。
- Renew 校验 Scope、RunID、Owner、Fence、有效期，保持 Fence；Release 同样检查，只清空持有者/有效期，绝不删除 Fence 计数。到期时刻即失效。时长为正，时间及 Fence 溢出拒绝。
- 业务写入携带 WriteGuard（Lease + 预期 RunVersion），在同一事务内比较租约并实际条件更新租约行的内部写入版本，不能做独立先查后写，也不能用被优化掉的无变化更新。接管、取消与旧写入因此产生数据库写冲突。
- CheckLease 的时间参数在生产必须来自数据库。续期后旧凭证只在其原有效期内可用。Fence 判断独立于 RunVersion；仅有版本号无法证明执行器仍有权限。
- 租约操作不改变会话占用。终态 Run 仅在有活动 Task 或 pending delivery 时允许获取维护租约；维护租约不能继续模型生成、Track 新任务或 Apply 结果。

Cancel 不要求执行器租约：可信 Scope+RunID+预期 RunVersion 原子写 cancelled、终止事件、释放属于此 Run 的会话占用、递增 Fence 并撤销租约。已终态直接返回当前结果，不覆盖成功/失败；并发完成与取消由事务提交顺序决定。Commit 不允许自行写 cancelled。完成/失败的 Commit 也在同事务释放会话占用和撤销旧租约；后续观察需要新的维护租约。

## 提交、序号、分页与事件保留

Database.CommitRun 原子提交 Run 状态、消息、事件及可选检查点。普通 Commit 不能增删检查点中的 PendingCallIDs：新增待完成调用必须 Track，消费必须 Apply；创建无任务检查点时 PendingCallIDs 为空。状态本身未变但输出变化仍递增 Run.Version。完全无变化请求只保存必要幂等回执，不增加业务版本。

每个 Session 有持久的消息序号计数器，每个 Run 有持久的事件序号计数器。事务从 1 起连续分配批次，按照请求切片顺序追加；计数器与内容同事务提交/回滚。消息 ID 在 Scope 内唯一；计数器属于存储文档元数据，不要求领域结构体携带。修改占用或追加消息都改变 Session.Version，其他操作只比较其版本，不做无意义自增。

Commit/Apply 使用稳定 OperationID。mutation_receipts 唯一键为 Scope+RunID+OperationID，保存 MutationKind、规范请求摘要及实际 CommitResult，和业务写入在同事务创建。摘要编码必须版本化、长度分隔并保留原字节和 nil 语义，覆盖全部写入内容及操作种类，排除 Lease、预期版本和存储生成时间/序号；不允许用会替换非法 UTF-8 的序列化合并不同请求。P3 已实现固定结构 BSON 编码、SHA-256 摘要及固定兼容样本测试。

重复请求先查回执：同内容返回原回执中的序号/版本，其他内容报冲突。此时过期租约可以读取原结果，但绝不能产生新写入。没有回执则正常验证全部 Guard/CAS。回执不提前 TTL；其保留期覆盖 Run、交付记录及约定重试窗口。Task.Track 按原调用身份及句柄去重，重试不能用旧检查点覆盖已推进状态；Observe/元数据更新使用版本和 P1.5 无变化规则，不创建重复事件。

SequencePage.After 是排他已消费序号；0 表示从头，Limit 为 1..1000。返回序号升序，NextAfter 为最后一条实际返回记录；空页保持输入游标，不能将 LastSequence 直接当作已消费游标。KeyPage 按 ID 二进制升序，空 After 表示开始，所有资源 ID 保持原值。

EventPage 同一一致读返回 Items、LastSequence 和 PrunedThrough。PrunedThrough 表示已经整体清理的最大连续前缀；after 等于水位可以继续，更旧游标（包括 0）返回可由 errors.Is 判断的 store.ErrCursorExpired；未来游标报参数无效。即使事件全被清理仍保留计数器和水位。适配器不能对单条事件直接使用无协调 TTL：清理必须是连续前缀，并与水位原子提交，否则可能静默丢事件。消息原始历史不删除。

事件必须持久化成功后发布；发布通知丢失可通过事件读取/重放补偿，通知本身不是事实来源。Run 终止事件和状态同事务写入；终止之后不追加事件。迟到任务结果保存在任务及交付记录，客户端通过任务查询获取。分页不是跨多次请求的固定快照，事件实时跟随/有界缓冲仍在 P4 实现。

## 任务、检查点与交付

Checkpoint 唯一键 Scope+RunID+InvocationID，保存完整 Caller、Format、SDK 不透明 Data、PendingCallIDs、Version。Format 必须明确；真实 SDK 可恢复性在 P6 验证。Runtime.Request 已允许携带检查点，Resume 不得静默重新 Execute。

TaskDelivery 唯一键 Scope+TaskID，保存 Run、Caller、ToolCallID、pending/applied/discarded 状态、版本及结算时间。它表示已经启动工具的结果待续接，不是工具执行队列。

| 事务 | 原子写入与约束 |
| --- | --- |
| Track | 新 Task、含原 CallID 的同分支检查点、Run 版本及可选事件；保留旧检查点中其他待完成调用。任务已经终态时同时建立 pending delivery |
| Observe | Guard + TaskVersion 校验后调用 P1.5 ApplyUpdate；实际变化时保存 Task、Run 版本和可选事件，首次终态同时创建唯一 pending delivery。无变化不重复事件；终态 Run 只允许无事件观察 |
| RequestCancel / RecordObservationError | 只改变 P1.5 允许的任务字段、Task/Run 版本；观察失败不会创建终态或交付。数据库时间由适配器传给领域方法 |
| Apply | 校验 Task 终态、未消费、pending delivery、精确 Scope/Run/Caller/CallID、各版本及租约；保存结果已并入的检查点、Task.AppliedAt、delivery=applied、SettledAt、消息/事件、Run 版本及回执 |
| Discard | 仅在 Run 终态时将 pending 交付改为 discarded 并设置 SettledAt/版本及 Run 版本；保留 Task 实际终态，Task.AppliedAt 仍为空，不写检查点/消息/事件 |

Apply 的新检查点必须且只能移除当前 CallID，保留同分支其他待完成调用；新工具调用在随后 Track 事务登记。多个分支不能只按 Agent 名称定位。两种 required 状态和 running 状态不能交付，观察错误不能代替失败结果。

ListUnsettled 包含活动任务及 pending delivery 的终态任务，排除 applied/discarded，不能简单过滤 AppliedAt=nil。终态 Task 没有对应 delivery 代表存储不变量破坏，报错并进入诊断，不能假定已消费。Task/交付唯一索引和 Run CAS 防止并发消费；回执保证响应丢失后的相同 Apply 重试不重复修改检查点。

两步持久化的崩溃语义：

| 崩溃位置 | 恢复行为 |
| --- | --- |
| Start/Track 事务提交前 | 所有写入回滚；同幂等键/原调用身份重试；工具已启动但句柄丢失时须提供方幂等或关联查询，不盲目 Execute |
| 终态观察事务提交前 | 重新观察同一远端句柄，不能重启工具 |
| 终态+pending 提交后，Apply 前 | 扫描发现 pending，取得租约，加载原分支检查点并继续消费 |
| Apply 事务提交前 | 检查点、AppliedAt、交付状态、消息/事件、回执全部回滚，仍可重试 |
| Apply 提交后，响应前 | 读取回执；结果已经并入检查点，禁止再次消费，恢复检查点后继续执行 |
| Cancel 与 Apply 竞争 | Apply 先提交则消费结果保留，随后取消停止生成；Cancel 先提交则撤销 Fence/改变版本，Apply 失败，维护流程 Discard |

外部模型/工具调用不能加入 MongoDB 事务。运行时应先构造并持久化“已接纳工具结果、尚未推进后续模型调用”的检查点，再执行后续步骤。P2 不承诺外部副作用恰好一次；如果所选 SDK 无法分离恢复和继续生成，P6 必须调整恢复协议并明确限制，不能用模拟测试宣称已支持。

## 恢复权限边界

Recovery.Scan 是启动装配仅注入内部恢复循环的独立管理能力，普通 Database 不返回它，HTTP/工具不接收它。P3 通过 OpenRecovery 独立构造连接；部署时使用单独身份与最小数据库权限，Go 类型本身不充当安全沙箱。

扫描无有效租约的活动 Run，以及含活动 Task/pending delivery 的终态 Run。返回每项完整 Scope+RunID，不返回内容或凭据。RecoveryPosition 按 TenantID、UserID、RunID 的二进制字典序排他分页；nil 表示第一页，部分游标拒绝，limit 1..1000。到末尾必须重头扫描，以发现游标之前重新符合条件的记录。

扫描只是发现候选，不能直接写入。必须 Acquire 后按候选的非空 Scope 重新加载 Run、任务和检查点，处理竞争/状态变化。租约续期和进程优雅关闭在 P4/P7，P9 已装配任务恢复循环。

## P3 集成验收要求

以下场景已在本地真实副本集验证；多节点选举/网络分区、mongos、生产认证/TLS 和容量仍待部署验证。详情见 MongoDB 说明。

使用支持事务的 MongoDB replica set 或等价拓扑；不能将独立单机/模拟存储测试计作通过。至少验证：

- 不同用户/租户隔离，同会话双 Start 只有一次成功，同键同输入回执、同键不同输入冲突、跨会话同键互不影响。
- Start/Commit/Track/Observe/Apply 每一步故障注入全部回滚，提交成功但响应丢失重试不新增消息/事件/消费。
- 旧 Fence、过期持有者、同 owner 重取、Renew/Release/Cancel 与写入竞争；Release/到期不释放会话占用。
- 消息/事件唯一序号、回滚不分配持久序号、未来/过期/全部清理游标、一致读与保留水位。
- 同 Run 并行分支 CAS 重试、跨分支结果拒绝、重复终态/消费、取消竞争、终态维护观察不重开事件流。
- 恢复扫描分页、同名跨租户 Run、游标前新增项重扫、pending/applied/discarded 筛选及缺失交付不变量诊断。


P9 补充：Database.CancelTask 接收可信 Scope 和本地 Task ID，不要求执行租约，事务内保留首次取消意图并推进 Task/Run 版本；它不确认远端取消。原 RequestTaskCancel 仍供持有维护租约的执行器使用。ListCheckpoints 按 Scope+RunID+InvocationID 有界分页，普通读取不开放跨作用域扫描。TrackTask 允许终态 Run 从已持久整批检查点补齐已有句柄，但 Events 必须为空，仍要求维护租约、CAS 与原调用关联；不允许因此重新 Execute 或生成模型输出。
