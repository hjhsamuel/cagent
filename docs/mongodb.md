# MongoDB 适配与集成测试

P3 已实现 `internal/adapter/mongodb`，使用官方 `go.mongodb.org/mongo-driver/v2 v2.9.1`。它实现 P2 的全部普通仓储、运行/任务事务、租约及独立恢复管理入口。启动服务尚未装配此适配器；`cmd/server` 仍只记录骨架生命周期。

存储操作直接调用 `*mongodb.Database`，例如 `CreateSession`、`StartRun`、`GetTask`、`ApplyTask`。`store` 仅保存请求/结果和纯校验，无 interface；`Database` 的方法直接操作 collection，不通过泛型 read/write/load、仓储转发对象或通用 CRUD 层。公共辅助函数仅负责事务生命周期、BSON 编解码和业务规则。

## 连接与装配

```go
db, err := mongodb.Open(ctx, mongodb.Options{
    URI:      cfg.MongoDB.URI,
    Database: cfg.MongoDB.Database,
    Timeout:  15 * time.Second,
})
if err != nil {
    return err
}
// 在所有业务 goroutine 停止后关闭；正式启动/关闭装配在 P7。
defer db.Close(context.Background())

// scope 来自可信身份，taskID 是请求的任务 ID。
task, err := db.GetTask(ctx, scope, taskID)
if err != nil {
    return err
}
_ = task
```

Open 验证选项、连接与认证、事务拓扑，然后幂等创建固定名称索引和内部时钟锚点。只接受支持会话的 replica set 或 mongos；standalone 返回 ErrUnsupported，不降级为非事务操作。测试实际使用 MongoDB Community 8.0.32 单节点副本集；其他版本、分片拓扑和生产认证/TLS 尚未进行环境验证。

普通读取使用 primary + majority，事务采用 snapshot 读关注与 majority 写关注。Timeout 必须显式为正，约束驱动操作与完整事务；调用方更早的 context 截止时间优先。驱动在限定时间内重试可重试事务/提交；回调没有发布事件、模型或工具副作用。只有返回成功的持久结果可以发布。

不存在映射为 ErrNotFound，唯一索引冲突映射为 ErrConflict，非法选项映射为 ErrInvalidArgument，不支持拓扑/排序规则映射为 ErrUnsupported。其他数据库错误展示固定安全文本，原因仍可通过 errors.Is/As 检查；context 取消/超时身份保留。不得将显式提取的驱动诊断直接输出给外部用户。

OpenRecovery 使用单独连接和配置，只交给内部恢复/保留流程。它提供 Scope 完整的候选扫描及 PruneEvents，不创建索引，须先由初始化流程 Open 完成集合准备。生产应使用单独数据库身份、最小权限和装配隔离；MongoDB RBAC 是集合级授权，业务的租户/用户隔离仍依赖可信 Scope 与适配器条件，不能把 Go 类型视为安全沙箱。

初始化需要 listCollections、createIndex、对 clock 的写权限及普通业务集合权限；正常业务涉及多集合事务读写。恢复扫描读取 runs/run_leases，事件保留流程还需更新 runs、删除 events。不要在服务运行期间更改集合模式或排序规则。

## 文档、索引和事务

每个集合在 `internal/storage/schema` 中具备独立的 BSON 映射结构体，适配层按集合使用对应类型进行读写和载荷转换，不再使用统一的 Document。业务集合继续使用 schema=1 的显式 BSON 信封，分别保留本集合需要的 tenant_id/user_id/id、查询关联字段、版本、序号/保留水位、未结算任务计数及私有 data 子文档。旧共用信封的无关字段读取时忽略，重新写入时移除；既有载荷无需迁移。data 的 v1 编码采用领域字段的小写名称，不给领域结构加 BSON/JSON 标签，也不向 HTTP 暴露数据库文档。时间由服务端生成，以 BSON UTC 毫秒存储；nil 和空切片、工具字节内容保持区别。后续变更载荷格式需要 schema 迁移。

所有资源的唯一/查询索引均含 tenant_id、user_id。使用 simple 二进制比较；Open/OpenRecovery 拒绝已有的非 simple 默认排序规则和 view，避免不区分大小写的匹配合并不同用户。

| 集合 | 索引与主要元数据 |
| --- | --- |
| tool_connections | Scope+id 唯一；管理员维护的 protocol/url/card_path/凭据环境引用/工具白名单，启动时通过 ListToolConnections 读取 |
| sessions | Scope+id 唯一；消息计数器、data.ActiveRunID |
| runs | Scope+id 唯一；Scope+session_id+非空 idempotency_key 条件唯一；status 查询、初始回执、事件计数/水位、unsettled 数量 |
| messages | Scope+id 唯一；Scope+session_id+sequence 唯一 |
| events | Scope+内部 id 唯一；Scope+run_id+sequence 唯一 |
| tasks | Scope+id 唯一；Scope+run_id+invocation_id+call_id 唯一；远端句柄查询；run_id+unsettled+id 查询 |
| agent_checkpoints | Scope+内部 id 唯一；Scope+run_id+invocation_id 唯一；版本与原调用分支 |
| task_deliveries | Scope+task_id（信封 id）唯一；run_id+status+id 查询 |
| context_snapshots | Scope+内部 id 唯一；Scope+session_id+version 唯一；保留派生快照版本 |
| run_leases | Scope+run_id（信封 id）唯一；Owner、Fence、ExpiresAt、Revision |
| mutation_receipts | Scope+内部 id 唯一；Scope+run_id+operation_id（call_id 索引字段）唯一 |
| clock | 固定 `_id=clock`，仅作服务端 `$$NOW` 聚合的锚点，不包含租户资源 |

内部复合 ID 对长度编码后的字段求摘要，避免资源 ID 内含分隔符产生碰撞。固定索引名称和参数支持重复 Open；不自动删除或修改不兼容旧索引，初始化失败即返回错误。

具体原子边界见 [存储契约](storage-contracts.md)。实现要点：

- Start 先查询幂等回执，再检查会话版本/占用；先写会话，竞争方经事务重试重新发现幂等结果。Run、输入、序号、占用和初始 lease 一起提交。
- 租约所有权用服务器 `$$NOW` 在条件更新中检查，实际递增 Revision，令接管/续期/取消和旧写入产生写冲突。Fence 永不重置。MongoDB 时间精度为毫秒，租期至少 1 ms；Release/到期不清除会话占用。
- Commit/Apply 的摘要使用固定结构、版本前缀及 BSON 长度编码，保留原字节；排除租约/CAS、存储生成时间和序号，固定兼容样本测试防止格式漂移。回执在同事务保存，旧凭证只能重放已有结果，不能新写。
- Run 转终态时保证匹配的终止事件位于最后；调用方未提供时由适配器生成。禁止普通进度/任务事务伪造终止事件。终态后远端结果只写 Task/Delivery，不重开事件流。
- Track/Observe 首次终态与 pending delivery 原子提交；Apply 原子写检查点、AppliedAt、交付结算、消息/事件、Run 版本及回执；Discard 不把结果误标为已消费。
- tasks.unsettled 和 runs.unsettled 是同事务维护的查询投影，applied/discarded 均排除；发现待交付终态缺少 delivery 会报存储不变量错误，不静默跳过。
- 所有事务序号计数器一并回滚。事件读取在同一快照读取正文与水位；PruneEvents 原子删除连续前缀并推进水位，不使用逐条 TTL。此管理元数据维护不增加业务 Run.Version。

## 运行真实集成测试

具体方法重构另有三组回归测试：`GetTask` 的作用域隔离及取消、检查点更新被数据库拒绝后的完整回滚、快照事务重试及提交失败返回零结果。原有幂等、租约、分页、取消竞争和恢复消费测试均直接调用 `Database` 方法。

需要 Go 工具链以及真实 mongod。推荐让测试程序自动启动临时副本集；无需安装系统服务或启动 mongosh。测试程序选择空闲本机端口、只绑定 127.0.0.1，使用临时目录，初始化副本集后执行测试，最后关闭进程并删除自己的临时数据。

Windows PowerShell 示例（把路径替换为本机 mongod.exe）：

```powershell
$env:CAGENT_TEST_MONGOD = 'C:\MongoDB\bin\mongod.exe'
go test -v ./internal/adapter/mongodb -count=1
go test ./... -count=1
go test -race ./internal/adapter/mongodb ./internal/store ./internal/domain -count=1
go vet ./...
go build ./...
```

Linux/macOS 安装了 mongod 时同样可用：

```sh
CAGENT_TEST_MONGOD="$(command -v mongod)" go test -v ./internal/adapter/mongodb -count=1
```

本次本机二进制位于 `.local/mongodb/mongodb-win32-x86_64-windows-8.0.32/bin/mongod.exe`，`.local/` 已忽略，不进入源码。使用的 [MongoDB 官方 Windows ZIP](https://fastdl.mongodb.org/windows/mongodb-windows-x86_64-8.0.32.zip) SHA-256 为 `5a0675fdec49b544cd5d374ad50a9a594cf5b3278b5ace658aa51cbb4df17177`，下载后已校验；来源为 [官方发布清单](https://downloads.mongodb.org/current.json)。安装方式参考 [MongoDB Windows ZIP 文档](https://www.mongodb.com/docs/v8.0/tutorial/install-mongodb-on-windows-zip/)。

也可以指定已有的专用测试副本集：

```powershell
$env:CAGENT_TEST_MONGODB_URI = 'mongodb://127.0.0.1:27017/?replicaSet=rs0'
go test -v ./internal/adapter/mongodb -count=1
```

两者同时设置时 CAGENT_TEST_MONGOD 优先。测试仅创建/删除自动生成的 `cagent_p3_test_*` 数据库，不采用应用数据库名。凭据用环境变量提供，不写入仓库。服务器 failpoint 和额外 standalone 测试仅在本程序启动的隔离 mongod 上执行，不修改外部共享实例。

两个环境变量均未设置时，数据库集成测试明确显示 SKIP，只运行编码/错误单元测试；这不代表数据库验收通过。CI 验收必须设置其中之一。Windows race 测试需要可用的 CGO/C 编译器。

## 已验证与限制

2026-09-23 在 Windows/amd64、Go 1.27.1、官方驱动 v2.9.1、MongoDB Community 8.0.32 上验证真实单节点副本集：

- 相同 ID 跨租户/用户隔离、空 Scope 拒绝、并发幂等 Start、同键不同输入冲突、跨会话同键独立。
- 两个独立客户端的版本竞争、独占租约、到期/释放后接管、Fence 单调、续期和旧凭证拒绝；不依赖进程内锁。
- Start/Commit/Track/Observe/Apply 的提交前故障回滚，唯一索引中途失败回滚，序号不泄漏。
- 服务端 failCommand 注入真实 UnknownTransactionCommitResult，驱动重试及应用回执重放不重复输出。
- 分页与未来/过期游标、完整清理后的持久水位、终态事件和会话释放。
- 已终态句柄登记、观察错误与取消意图、重复快照不增加版本、缺失交付诊断、重新连接后消费、并行待完成调用保留、Apply/Cancel 跨客户端竞争、终态维护观察与 discarded 结算。
- 快照版本、过大覆盖范围拒绝、跨租户同 Run ID 的恢复分页和从头重扫；standalone/不安全排序规则拒绝。

本节 P3 测试中的“恢复”是数据库连接重建与持久检查点/交付消费；P9 已实现 ADK Runtime 恢复，P10.3 已补齐 HTTP/A2A/ADK 重启续接整体验收及真实提交 TCP 断连注入，详见 [整体验收](acceptance.md)。尚未验证多节点主节点选举/网络分区、真实 mongos、认证/TLS 部署、生产容量，保持待部署环境验证。外部模型/工具副作用仍不承诺恰好一次。
