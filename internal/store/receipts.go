package store

import "github.com/hjhsamuel/cagent/internal/domain"

// MutationKind 纳入提交摘要/回执身份，防止将普通输出重试当成任务消费重试。
type MutationKind string

const (
	MutationRunCommit MutationKind = "run_commit"
	MutationTaskApply MutationKind = "task_apply"
)

// MutationReceipt 与全部业务写入在同一事务创建，唯一键 Scope+RunID+OperationID。
// Digest 是包含 Kind 的版本化规范请求摘要；禁止包含 Guard/预期版本，否则接管后
// 无法重试。其余写入内容全部纳入，包含任务 ID、消息 ID、状态、事件和检查点字节。
// Result 是当次提交实际分配的版本/序号副本，不是重新读取的当前资源状态。
// 必须至少保留到 Run、任务交付及调用方重试窗口结束；不得提前 TTL 导致重复输出。
type MutationReceipt struct {
	Scope       domain.Scope
	RunID       string
	OperationID string
	Kind        MutationKind
	Digest      [32]byte
	Result      CommitResult
}

// Match 校验已查到的回执。成功只允许返回保存的 Result，禁止再次提交；因此即使
// 原租约已失效、Run 已完成或资源版本已经变化，网络结果未知的同请求仍可安全重放。
// 不存在回执时调用方必须正常检查租约/CAS，不能把“没找到回执”视为已提交。
// Scope 仍须来自可信认证，不凭 OperationID 跨用户查询回执。方法不返回/记录原摘要。
func (r MutationReceipt) Match(scope domain.Scope, runID, operationID string, kind MutationKind, digest [32]byte) error {
	if err := scope.ValidateAgainst(r.Scope); err != nil {
		return err
	}
	if err := nonblank("run.id", runID); err != nil {
		return err
	}
	if err := nonblank("operation.id", operationID); err != nil {
		return err
	}
	if kind != MutationRunCommit && kind != MutationTaskApply {
		return invalid("operation.kind", "unknown mutation kind")
	}
	if r.RunID != runID || r.OperationID != operationID || r.Kind != kind || r.Digest != digest {
		return conflict("operation")
	}
	return nil
}
