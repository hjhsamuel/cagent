package domain

import "time"

type ToolProtocol string

const (
	ToolLocal ToolProtocol = "local"
	ToolMCP   ToolProtocol = "mcp"
	ToolA2A   ToolProtocol = "a2a"
)

type ToolCall struct {
	Scope          Scope
	ID             string
	SessionID      string
	RunID          string
	Caller         AgentExecution
	Protocol       ToolProtocol
	Name           string
	Arguments      []byte
	IdempotencyKey string
}

type ToolResult struct {
	CallID string
	Parts  []Part
	Error  string
}

// AgentExecution identifies an agent/subagent invocation within a run.
// ParentInvocationID is empty for the root agent.
type AgentExecution struct {
	AgentID            string
	InvocationID       string
	ParentInvocationID string
}

// ToolOutcome contains exactly one of Result or Task, determined at call time.
// A handle acknowledges ongoing work; it is not the final tool result.
type ToolOutcome struct {
	Result *ToolResult
	Task   *TaskHandle
}

// TaskHandle locates work already started by the tool/provider.
// ConnectionID refers to trusted scoped configuration, never raw credentials.
type TaskHandle struct {
	Protocol     ToolProtocol
	ConnectionID string
	RemoteID     string
	// ContextID 对 A2A 保存远端上下文，对 MCP 保存原协议会话 ID。
	// 重启查询须保留该值，不能用新握手会话替换；它不是凭据或可选的新任务参数。
	ContextID string
}

// TaskStatus 是提供方状态的标准化表示；零值与未知状态均非法。
// input_required/auth_required 是可恢复暂停，四种终态只能由实际观察确认。
type TaskStatus string

const (
	TaskSubmitted     TaskStatus = "submitted"
	TaskInputRequired TaskStatus = "input_required"
	TaskAuthRequired  TaskStatus = "auth_required"
	TaskRejected      TaskStatus = "rejected"
	TaskRunning       TaskStatus = "running"
	TaskSucceeded     TaskStatus = "succeeded"
	TaskFailed        TaskStatus = "failed"
	TaskCancelled     TaskStatus = "cancelled"
)

type Task struct {
	Scope          Scope
	ID             string
	Call           ToolCall
	Handle         TaskHandle
	Status         TaskStatus
	Result         *ToolResult
	Progress       []Part
	ProviderCursor string
	Version        int64
	// CancelRequestedAt 只记录首次取消意图，不代表提供方接受或完成取消。
	CancelRequestedAt *time.Time
	// LastObservedAt 是最近一次有变化（含首次或错误恢复）的成功观察时间，
	// 不是轮询心跳；重复快照不刷新它，观察失败不覆盖它。
	LastObservedAt time.Time
	// ObservationError 是本地观察失败的安全说明，与远端 failed 状态独立。
	ObservationError string
	// AppliedAt 表示终态结果已原子并入调用方检查点；Run 终止后丢弃交付不设置它，
	// 是否仍需恢复必须同时读取 TaskDelivery.State，不能只判断本字段为空。
	AppliedAt *time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
}

// TaskUpdate 是适配器已协调顺序的完整提供方观察快照，不是重新执行工具的请求。
// Status 必须是已知标准化状态；未知状态的原始诊断由适配层保留并重新查询。
// Result 仅在终态出现，Progress 不能替代最终结果；空字段表示清空而非保留旧值。
// Cursor 是不透明游标，领域层不通过字典序或本地观察时间推断通知先后。
type TaskUpdate struct {
	Status   TaskStatus
	Progress []Part
	Result   *ToolResult
	Cursor   string
}
