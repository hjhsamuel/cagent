package domain

import "time"

type Session struct {
	Scope   Scope
	ID      string
	AgentID string
	Version int64
	// ActiveRunID 是跨实例会话占用；租约过期不清除此值，仅 Run 终态事务释放。
	ActiveRunID string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// 这些种类是模型历史的协议无关约定；其他多模态种类可由模型适配器定义。
// tool_call 只能位于 assistant，tool_result 只能位于 tool，均以 ToolCallID 关联。
// 同一 Run 的历史调用 ID 必须唯一；并行分支的提供方局部 ID 由适配器映射为
// 不冲突的模型历史 ID。Task/Checkpoint 的 InvocationID+原 CallID 路由不改变。
const (
	PartText       = "text"
	PartToolCall   = "tool_call"
	PartToolResult = "tool_result"
)

// Part 保留工具调用/结果配对；Data 是结构化内容，大结果使用 URI。
// 外部工具原文不能改为 system 角色或充当可信提示词。
type Part struct {
	Kind       string
	Text       string
	MIMEType   string
	URI        string
	ToolCallID string
	ToolName   string
	Data       []byte
}

type Message struct {
	Scope     Scope
	ID        string
	SessionID string
	RunID     string
	Sequence  int64
	Role      Role
	Parts     []Part
	CreatedAt time.Time
}
