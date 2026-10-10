package schema

// 集合名称与各集合的独立文档结构一同维护。
const (
	ToolConnectionCollection  = "tool_connections"
	ModelCollection           = "models"
	SessionCollection         = "sessions"
	RunCollection             = "runs"
	MessageCollection         = "messages"
	EventCollection           = "events"
	TaskCollection            = "tasks"
	TaskDeliveryCollection    = "task_deliveries"
	CheckpointCollection      = "agent_checkpoints"
	SnapshotCollection        = "context_snapshots"
	MutationReceiptCollection = "mutation_receipts"
	LeaseCollection           = "run_leases"
	ClockCollection           = "clock"
	PayloadCollection         = "immutable_payloads"
)
