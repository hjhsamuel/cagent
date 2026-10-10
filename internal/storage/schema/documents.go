// Package schema 集中定义 MongoDB 集合名、文档和命令返回值的 BSON 映射。
// 此包只维护存储结构，不加载环境、连接数据库或执行加解密。
package schema

import (
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

const DocumentVersion = 1

// 每个集合独立定义持久化映射；Data 是领域载荷的 BSON 子文档。
// Data 的 schema=1 编码保持既有 Go 字段小写映射和 nil/空切片区别。
// 修改既有载荷编码须升级 Schema 并迁移，不能隐式改变持久格式。
// Session 映射 sessions 集合，保存主会话和消息序号计数。
type Session struct {
	// Tenant 是所属租户标识，与 User 一起限定数据访问范围。
	Tenant string `bson:"tenant_id"`
	// User 是租户内的用户标识，查询和唯一索引必须同时包含 Tenant、User。
	User string `bson:"user_id"`
	// ID 是会话标识，在同一租户和用户范围内唯一。
	ID string `bson:"id"`
	// ModelID 是主对话绑定的 models 文档标识；子 Agent 的绑定保存在各自检查点中。
	ModelID string `bson:"model_id,omitempty"`
	// APIKeyID 是绑定模型内的稳定凭据标识，只保存引用，不保存明文 API 密钥。
	APIKeyID string `bson:"api_key_id,omitempty"`
	// Schema 是文档编码格式版本，当前为 1；用于解码校验，与业务 Version 不同。
	Schema int `bson:"schema"`
	// Data 是 domain.Session 的 BSON 子文档，保存 Agent、活动 Run、模型绑定和时间等业务数据。
	Data bson.Raw `bson:"data,omitempty"`
	// Version 是会话的业务版本，更新时用旧版本作为条件，防止并发覆盖。
	Version int64 `bson:"version"`
	// LastSequence 是此会话已提交的最大消息序号；新消息从该值递增分配，初始为 0。
	LastSequence int64 `bson:"last_sequence"`
}

// Run 映射 runs 集合，保存运行、事件水位、幂等启动回执和恢复调度信息。
type Run struct {
	// Tenant 是所属租户标识，与 User 一起限定数据访问范围。
	Tenant string `bson:"tenant_id"`
	// User 是租户内的用户标识，查询和唯一索引必须同时包含 Tenant、User。
	User string `bson:"user_id"`
	// ID 是运行标识，在同一租户和用户范围内唯一。
	ID string `bson:"id"`
	// Schema 是文档编码格式版本，当前为 1；用于解码校验，与业务 Version 不同。
	Schema int `bson:"schema"`
	// Data 是 domain.Run 的 BSON 子文档，保存运行状态、会话关系及创建、更新、终止时间。
	Data bson.Raw `bson:"data,omitempty"`
	// Version 是运行的业务版本，提交状态、消息、事件或检查点变化时递增，用于并发更新校验。
	Version int64 `bson:"version"`
	// SessionID 是所属会话标识，用于会话关联查询和幂等键的唯一性约束。
	SessionID string `bson:"session_id,omitempty"`
	// Status 是运行状态的查询副本，如 running、waiting_tool、completed、failed、cancelled；对应 Data 中的 Status。
	Status string `bson:"status,omitempty"`
	// IdempotencyKey 是启动运行的幂等键；同一会话内非空键唯一，重复请求返回原启动结果。
	IdempotencyKey string `bson:"idempotency_key,omitempty"`
	// LastSequence 是此运行已提交的最大事件序号；新事件从该值递增分配，初始为 0。
	LastSequence int64 `bson:"last_sequence"`
	// PrunedThrough 是已清理事件连续前缀的最大序号，包含该序号；早于该水位的游标已过期。
	PrunedThrough int64 `bson:"pruned_through"`
	// Unsettled 是仍需跟踪或交付处理的任务数量；已消费、丢弃或隔离的任务不计入。
	Unsettled int64 `bson:"unsettled"`
	// Recovery 表示运行仍需恢复处理：运行未终结，或终态运行仍有未结算任务。
	Recovery bool `bson:"recovery"`
	// NextActionAt 是恢复扫描的候选调度时间；领取或续期租约时设为到期时间，扫描还会校验租约是否有效。
	NextActionAt time.Time `bson:"next_action_at"`
	// Start 是首次启动成功的 store.StartRunResult BSON 回执；幂等重试直接返回它，保留原输入序号和会话版本。
	Start bson.Raw `bson:"start,omitempty"`
	// Fingerprint 是启动输入内容的 32 字节摘要，用于拒绝相同幂等键但输入不同的请求。
	Fingerprint []byte `bson:"fingerprint,omitempty"`
}

// Message 映射 messages 集合，保存会话中已提交的消息。
type Message struct {
	// Tenant 是所属租户标识，与 User 一起限定数据访问范围。
	Tenant string `bson:"tenant_id"`
	// User 是租户内的用户标识，查询和唯一索引必须同时包含 Tenant、User。
	User string `bson:"user_id"`
	// ID 是消息标识，在同一租户和用户范围内唯一。
	ID string `bson:"id"`
	// Schema 是文档编码格式版本，当前为 1；用于解码校验，与业务 Version 不同。
	Schema int `bson:"schema"`
	// Data 是 domain.Message 的 BSON 子文档，保存角色、内容片段、创建时间和模型输入用量等。
	Data bson.Raw `bson:"data,omitempty"`
	// Version 是消息文档版本；消息创建后不更新，当前写入固定为 1。
	Version int64 `bson:"version"`
	// SessionID 是所属会话标识，与 Sequence 一起用于消息排序和分页。
	SessionID string `bson:"session_id,omitempty"`
	// RunID 是产生或接收此消息的运行标识，用于关联该次运行。
	RunID string `bson:"run_id,omitempty"`
	// Sequence 是会话内从 1 开始递增的消息序号；同一会话内唯一。
	Sequence int64 `bson:"sequence,omitempty"`
	// ContextProtected 标记用户消息或含工具调用、工具结果的消息；非归档上下文窗口中，即使被摘要覆盖也会保留。
	ContextProtected bool `bson:"context_protected"`
}

// Event 映射 events 集合，保存可分页回放的持久事件。
type Event struct {
	// Tenant 是所属租户标识，与 User 一起限定数据访问范围。
	Tenant string `bson:"tenant_id"`
	// User 是租户内的用户标识，查询和唯一索引必须同时包含 Tenant、User。
	User string `bson:"user_id"`
	// ID 是根据 RunID 和事件序号计算的内部复合标识，用于作用域内唯一定位事件。
	ID string `bson:"id"`
	// Schema 是文档编码格式版本，当前为 1；用于解码校验，与业务 Version 不同。
	Schema int `bson:"schema"`
	// Data 是 domain.Event 的 BSON 子文档，保存事件种类、事件内容字节及创建时间等。
	Data bson.Raw `bson:"data,omitempty"`
	// Version 是事件文档版本；事件创建后不更新，当前写入固定为 1。
	Version int64 `bson:"version"`
	// RunID 是所属运行标识，用于事件流查询和回放。
	RunID string `bson:"run_id,omitempty"`
	// Sequence 是运行内从 1 开始递增的事件序号；同一运行内唯一，供分页和断线续读使用。
	Sequence int64 `bson:"sequence,omitempty"`
}

// Task 映射 tasks 集合，保存已启动工具任务的跟踪和恢复信息。
type Task struct {
	// Tenant 是所属租户标识，与 User 一起限定数据访问范围。
	Tenant string `bson:"tenant_id"`
	// User 是租户内的用户标识，查询和唯一索引必须同时包含 Tenant、User。
	User string `bson:"user_id"`
	// ID 是本地任务标识，在同一租户和用户范围内唯一；与远端 RemoteID 不同。
	ID string `bson:"id"`
	// Schema 是文档编码格式版本，当前为 1；用于解码校验，与业务 Version 不同。
	Schema int `bson:"schema"`
	// Data 是 domain.Task 的 BSON 子文档，保存原调用、远端句柄、进度、结果及本地维护信息。
	Data bson.Raw `bson:"data,omitempty"`
	// Version 是任务的业务版本，观察结果、维护或结算变化时递增，用于并发更新校验。
	Version int64 `bson:"version"`
	// SessionID 是原工具调用所属的会话标识，对应 Data.Call.SessionID。
	SessionID string `bson:"session_id,omitempty"`
	// RunID 是原工具调用所属的运行标识，对应 Data.Call.RunID。
	RunID string `bson:"run_id,omitempty"`
	// InvocationID 是发起工具调用的 Agent 执行分支标识，用于将结果交付给原分支。
	InvocationID string `bson:"invocation_id,omitempty"`
	// CallID 是该分支中的原工具调用标识，与 RunID、InvocationID 一起构成调用去重键。
	CallID string `bson:"call_id,omitempty"`
	// Protocol 是远端任务句柄的工具协议，如 mcp、a2a；用于选择对应的查询和恢复适配器。
	Protocol string `bson:"protocol,omitempty"`
	// ConnectionID 是受信工具连接的标识，引用所属作用域的连接配置，不保存凭据。
	ConnectionID string `bson:"connection_id,omitempty"`
	// RemoteID 是工具提供方返回的远端任务标识，用于查询已启动任务的状态和结果。
	RemoteID string `bson:"remote_id,omitempty"`
	// Status 是远端任务的标准化状态，如 running、succeeded、failed；与 Run 状态及本地维护状态不同。
	Status string `bson:"status,omitempty"`
	// Unsettled 是待处理任务的 0/1 查询标记；1 表示仍需跟踪或交付，消费、丢弃或隔离后置 0。
	Unsettled int64 `bson:"unsettled"`
	// NextActionAt 是任务下次观察或维护处理的调度时间，可取下一次观察时间与维护截止时间中的较早值。
	NextActionAt time.Time `bson:"next_action_at"`
}

// TaskDelivery 映射 task_deliveries 集合，保存终态任务结果的交付与结算意图。
type TaskDelivery struct {
	// Tenant 是所属租户标识，与 User 一起限定数据访问范围。
	Tenant string `bson:"tenant_id"`
	// User 是租户内的用户标识，查询和唯一索引必须同时包含 Tenant、User。
	User string `bson:"user_id"`
	// ID 等于关联任务的 TaskID；每个作用域内的任务最多有一条交付记录。
	ID string `bson:"id"`
	// Schema 是文档编码格式版本，当前为 1；用于解码校验，与业务 Version 不同。
	Schema int `bson:"schema"`
	// Data 是 domain.TaskDelivery 的 BSON 子文档，保存接收分支、原调用标识、交付状态和结算时间。
	Data bson.Raw `bson:"data,omitempty"`
	// Version 是交付记录的业务版本，从 1 开始，消费或丢弃时递增，用于并发结算校验。
	Version int64 `bson:"version"`
	// RunID 是所属运行标识，用于查询该运行的待交付记录。
	RunID string `bson:"run_id,omitempty"`
	// Status 是本地交付状态的查询副本：pending 待交付、applied 已消费、discarded 已丢弃。
	Status string `bson:"status,omitempty"`
}

// Checkpoint 映射 agent_checkpoints 集合，保存各 Agent 执行分支的可恢复状态。
type Checkpoint struct {
	// Tenant 是所属租户标识，与 User 一起限定数据访问范围。
	Tenant string `bson:"tenant_id"`
	// User 是租户内的用户标识，查询和唯一索引必须同时包含 Tenant、User。
	User string `bson:"user_id"`
	// ID 是根据 RunID 和 InvocationID 计算的内部复合标识，每个运行分支只保留当前检查点。
	ID string `bson:"id"`
	// ModelID 是此 Agent 执行分支绑定的 models 文档标识，独立于主会话的模型绑定。
	ModelID string `bson:"model_id,omitempty"`
	// APIKeyID 是绑定模型内的稳定凭据标识，只保存引用，不保存明文 API 密钥。
	APIKeyID string `bson:"api_key_id,omitempty"`
	// Schema 是文档编码格式版本，当前为 1；用于解码校验，与业务 Version 不同。
	Schema int `bson:"schema"`
	// Data 是 domain.Checkpoint 的内联 BSON 子文档，包含恢复数据、格式、待消费调用及模型绑定；与 Payload 互斥。
	Data bson.Raw `bson:"data,omitempty"`
	// Payload 是完整检查点 BSON 载荷的不可变分块引用；载荷超过分块阈值时使用，并清空内联 Data。
	Payload *PayloadRef `bson:"payload,omitempty"`
	// Version 是检查点的业务版本，每次内容变化时递增，用于校验原分支的并发更新。
	Version int64 `bson:"version"`
	// RunID 是所属运行标识，与 InvocationID 一起用于检查点查询和唯一索引。
	RunID string `bson:"run_id,omitempty"`
	// InvocationID 是 Agent 或子 Agent 的执行分支标识，对应 Data.Caller.InvocationID。
	InvocationID string `bson:"invocation_id,omitempty"`
}

// ContextSnapshot 映射 context_snapshots 集合，保存可从历史消息重建的摘要快照。
type ContextSnapshot struct {
	// Tenant 是所属租户标识，与 User 一起限定数据访问范围。
	Tenant string `bson:"tenant_id"`
	// User 是租户内的用户标识，查询和唯一索引必须同时包含 Tenant、User。
	User string `bson:"user_id"`
	// ID 是根据 SessionID 和快照版本计算的内部复合标识；各版本快照独立保存。
	ID string `bson:"id"`
	// Schema 是文档编码格式版本，当前为 1；用于解码校验，与业务 Version 不同。
	Schema int `bson:"schema"`
	// Data 是 domain.ContextSnapshot 的 BSON 子文档，保存摘要、覆盖消息水位、策略版本和归档标记等。
	Data bson.Raw `bson:"data,omitempty"`
	// Version 是会话内递增的快照版本；用于唯一定位快照及选择最新版本，已有快照不覆盖。
	Version int64 `bson:"version"`
	// SessionID 是摘要对应的会话标识，与 Version 一起构成唯一索引。
	SessionID string `bson:"session_id,omitempty"`
}

// MutationReceipt 映射 mutation_receipts 集合，保存已提交操作的幂等回执。
type MutationReceipt struct {
	// Tenant 是所属租户标识，与 User 一起限定数据访问范围。
	Tenant string `bson:"tenant_id"`
	// User 是租户内的用户标识，查询和唯一索引必须同时包含 Tenant、User。
	User string `bson:"user_id"`
	// ID 是根据 RunID 和操作标识计算的内部复合标识，用于唯一定位已提交操作的回执。
	ID string `bson:"id"`
	// Schema 是文档编码格式版本，当前为 1；用于解码校验，与业务 Version 不同。
	Schema int `bson:"schema"`
	// Data 是 persistedReceipt 的 BSON 子文档，保存操作身份、请求摘要以及原提交结果或其分块引用。
	Data bson.Raw `bson:"data,omitempty"`
	// Version 是回执文档版本；回执提交后不可变，当前写入固定为 1。
	Version int64 `bson:"version"`
	// RunID 是提交操作所属的运行标识，用于查找幂等回执。
	RunID string `bson:"run_id,omitempty"`
	// CallID 在本集合保存 OperationID，即提交操作标识；用于与 RunID 组成去重索引，不是工具调用 ID。
	CallID string `bson:"call_id,omitempty"`
}

// PayloadChunk 映射 immutable_payloads 集合，保存检查点或提交结果的不可变载荷分块。
type PayloadChunk struct {
	// Tenant 是所属租户标识，与 User 一起限定数据访问范围。
	Tenant string `bson:"tenant_id"`
	// User 是租户内的用户标识，查询和唯一索引必须同时包含 Tenant、User。
	User string `bson:"user_id"`
	// ID 是根据完整载荷的 Hash 和分块序号计算的内部复合标识；相同作用域内同一载荷的同一块只保存一次。
	ID string `bson:"id"`
	// Hash 是完整 BSON 载荷的 SHA-256 十六进制摘要；所有分块共用，用于拼接后的完整性校验。
	Hash string `bson:"hash"`
	// Index 是从 0 开始的分块序号，读取时按此顺序拼接。
	Index int `bson:"index"`
	// Data 是此分块的原始 BSON 字节片段；单块最大 4 MiB，它本身不一定是完整 BSON 文档。
	Data []byte `bson:"data"`
}

// PayloadRef 是作用域内不可变 BSON 载荷的分块引用；支持读取旧内联文档，引用和分块不按 TTL 自动过期。
type PayloadRef struct {
	// Format 是分块引用格式版本，当前为 1；读取时必须匹配支持的格式。
	Format int `bson:"format"`
	// Hash 是完整 BSON 载荷的 SHA-256 十六进制摘要，用于定位分块并校验拼接后的字节。
	Hash string `bson:"hash"`
	// Bytes 是完整载荷的字节总数，用于校验分块长度与拼接结果，当前上限为 64 MiB。
	Bytes int `bson:"bytes"`
	// Chunks 是分块总数，等于 Bytes 按每块 4 MiB 向上取整的结果。
	Chunks int `bson:"chunks"`
}

// Lease 映射 run_leases 集合，协调不同实例对同一运行的写入所有权。
type Lease struct {
	// Tenant 是所属租户标识，与 User 一起限定数据访问范围。
	Tenant string `bson:"tenant_id"`
	// User 是租户内的用户标识，查询和唯一索引必须同时包含 Tenant、User。
	User string `bson:"user_id"`
	// ID 等于所属运行的 RunID；每个作用域内的运行有一条租约记录。
	ID string `bson:"id"`
	// Owner 是当前租约持有者标识；释放或撤销后为空，业务写入必须匹配持有者。
	Owner string `bson:"owner"`
	// Fence 是单调递增的租约代次，领取或撤销时递增；旧代次的持有者不能继续写入。
	Fence int64 `bson:"fence"`
	// Expires 是按 MongoDB 服务端时间计算的租约到期时刻；未到期且持有者、代次匹配才可写入。
	Expires time.Time `bson:"expires_at"`
	// Revision 是租约内部更新计数，领取、续期、释放及受保护写入时递增，使并发接管与业务写入产生事务写冲突。
	Revision int64 `bson:"revision"`
}

// Clock 映射 clock 集合，提供查询 MongoDB 服务端时间所需的固定锚点。
type Clock struct {
	// ID 是 MongoDB 主键，固定为 clock；此集合只保存服务端时钟查询的锚点文档。
	ID string `bson:"_id"`
	// Schema 是时钟锚点的存储格式版本，当前为 1；此文档不包含租户业务数据。
	Schema int `bson:"schema"`
}
