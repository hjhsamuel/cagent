package domain

import (
	"time"

	"github.com/hjhsamuel/cagent/internal/apperrors"
)

// Checkpoint 保存一个 Agent/Subagent 调用分支的可恢复状态，不解释 SDK 数据格式。
// 唯一键为 Scope + RunID + Caller.InvocationID；Format 标识适配器及编码版本。
// PendingCallIDs 是当前检查点尚未消费的工具调用集合，允许并行任务；消费某任务
// 必须仅移除对应调用。Data 可能包含私有上下文，仅限可信运行时和存储适配器使用。
// Version=0 表示尚未创建；首次保存为 1，此后由存储在 CAS 成功时递增。
type Checkpoint struct {
	Scope  Scope
	RunID  string
	Caller AgentExecution
	// 子 Agent 的模型/凭据绑定只记录在分支检查点中。
	ModelID        string
	APIKeyID       string
	Format         string
	Data           []byte
	PendingCallIDs []string
	Version        int64
	UpdatedAt      time.Time
}

// ValidateForRun 校验作用域和分支身份，不验证 SDK 检查点是否真正可恢复。
// Data 允许为空（部分 SDK 用引用或空初始状态），Format 必须显式指定。
func (c Checkpoint) ValidateForRun(run Run) error {
	if err := validateModelBinding(c.ModelID, c.APIKeyID); err != nil {
		return err
	}
	if err := run.Validate(); err != nil {
		return err
	}
	if err := c.Scope.ValidateAgainst(run.Scope); err != nil {
		return err
	}
	if err := equal("checkpoint.run_id", c.RunID, run.ID); err != nil {
		return err
	}
	if err := c.Caller.Validate(); err != nil {
		return err
	}
	if err := required("checkpoint.format", c.Format); err != nil {
		return err
	}
	if c.Version < 0 {
		return apperrors.New(apperrors.ErrInvalidArgument, "checkpoint.version", "must not be negative")
	}
	seen := make(map[string]bool, len(c.PendingCallIDs))
	for _, id := range c.PendingCallIDs {
		if err := required("checkpoint.pending_call_ids", id); err != nil {
			return err
		}
		if seen[id] {
			return apperrors.New(apperrors.ErrInvalidArgument, "checkpoint.pending_call_ids", "duplicate call")
		}
		seen[id] = true
	}
	return nil
}

// TaskDelivery 是持久化续接意图，不是工具执行队列；唯一键为 Scope + TaskID。
// 终态 Task 与 pending 记录原子提交，避免落库后崩溃丢失续接。
// applied 表示结果已并入检查点，discarded 表示 Run 已终止、不再生成模型输出。
// discarded 不设置 Task.AppliedAt，但从未交付扫描排除；保留审计记录及终态结果。
type TaskDelivery struct {
	Scope      Scope
	TaskID     string
	RunID      string
	Caller     AgentExecution
	ToolCallID string
	State      DeliveryState
	Version    int64
	CreatedAt  time.Time
	SettledAt  *time.Time
}

// DeliveryState 是本地续接处理状态，与远端任务成功/失败状态独立。
type DeliveryState string

const (
	DeliveryPending   DeliveryState = "pending"
	DeliveryApplied   DeliveryState = "applied"
	DeliveryDiscarded DeliveryState = "discarded"
)
