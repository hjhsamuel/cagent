package app

import (
	"context"
	"github.com/hjhsamuel/cagent/internal/domain"
	"github.com/hjhsamuel/cagent/internal/store"
)

// checkpoints 有界分页读取 Run 内分支，调用者持有运行租约时形成一致的调度视图。
// 只在服务内部使用，不暴露 SDK 数据给 HTTP。
func (a *Application) checkpoints(ctx context.Context, run domain.Run) ([]domain.Checkpoint, error) {
	var result []domain.Checkpoint
	var after string
	for {
		page, e := a.db.ListCheckpoints(ctx, run.Scope, run.ID, store.KeyPage{After: after, Limit: 64})
		if e != nil {
			return nil, e
		}
		result = append(result, page.Items...)
		if !page.HasMore {
			return result, nil
		}
		after = page.NextAfter
	}
}

// checkpointComplete 由运行时解释自身格式。完成检查点恢复仍须验证内容，不能把
// “没有 PendingCallIDs”当作已完成：它也可能是刚接纳全部结果、尚未推进的分支。
func (a *Application) checkpointComplete(cp domain.Checkpoint) bool {
	if runtime, ok := a.runtime.(interface{ CheckpointComplete(domain.Checkpoint) bool }); ok {
		return runtime.CheckpointComplete(cp)
	}
	return false
}

// branchStatus 只在运行时本次执行已停下时聚合。任一可执行分支存在就维持 running；
// 所有未完成分支均有依赖时才 waiting_tool；全部完成才释放会话占用。
func (a *Application) branchStatus(ctx context.Context, run domain.Run) (domain.RunStatus, error) {
	cps, e := a.checkpoints(ctx, run)
	if e != nil {
		return "", e
	}
	pending := false
	for _, cp := range cps {
		if a.checkpointComplete(cp) {
			continue
		}
		if len(cp.PendingCallIDs) == 0 {
			return domain.RunRunning, nil
		}
		pending = true
	}
	if pending {
		return domain.RunWaiting, nil
	}
	return domain.RunCompleted, nil
}

func taskUpdateEvents(run domain.Run, t domain.Task, u domain.TaskUpdate) []domain.Event {
	t.Status = u.Status
	t.Progress = u.Progress
	kind := domain.EventToolProgress
	if u.Status.IsPaused() {
		kind = domain.EventToolWaiting
	}
	return taskEvents(run, t, kind)
}
