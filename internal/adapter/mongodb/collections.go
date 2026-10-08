package mongodb

// 集合名集中声明以便索引与业务方法保持一致；collection 返回原生驱动实例。
const (
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
)
