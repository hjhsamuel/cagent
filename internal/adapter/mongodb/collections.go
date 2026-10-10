package mongodb

import "github.com/hjhsamuel/cagent/internal/storage/schema"

// 集合名集中声明以便索引与业务方法保持一致；collection 返回原生驱动实例。
const (
	ToolConnectionCollection  = schema.ToolConnectionCollection
	ModelCollection           = schema.ModelCollection
	SessionCollection         = schema.SessionCollection
	RunCollection             = schema.RunCollection
	MessageCollection         = schema.MessageCollection
	EventCollection           = schema.EventCollection
	TaskCollection            = schema.TaskCollection
	TaskDeliveryCollection    = schema.TaskDeliveryCollection
	CheckpointCollection      = schema.CheckpointCollection
	SnapshotCollection        = schema.SnapshotCollection
	MutationReceiptCollection = schema.MutationReceiptCollection
	LeaseCollection           = schema.LeaseCollection
	ClockCollection           = schema.ClockCollection
	PayloadCollection         = schema.PayloadCollection
)
