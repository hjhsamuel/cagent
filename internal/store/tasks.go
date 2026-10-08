package store

import (
	"github.com/hjhsamuel/cagent/internal/domain"
)

// TaskPage 按任务 ID 升序返回未结算任务，空页保持请求游标。
type TaskPage struct {
	Items     []domain.Task
	NextAfter string
	HasMore   bool
}

// CheckpointPage 按 InvocationID 分页列出同一 Run 的所有分支；用于恢复聚合，
// 不能用 AgentID 代替调用身份，也不能仅恢复根分支而遗漏已就绪的子分支。
type CheckpointPage struct {
	Items     []domain.Checkpoint
	NextAfter string
	HasMore   bool
}
