package app

import (
	"context"
	"slices"

	"github.com/hjhsamuel/cagent/internal/agent"
	"github.com/hjhsamuel/cagent/internal/domain"
	"github.com/hjhsamuel/cagent/internal/store"
)

// taskTracker 绑定一个持有租约的运行和当前暂停检查点，不能从 HTTP 构造。
// Track 由执行器在写锁内同步调用，lease/run/checkpoint 指针随事务结果推进，
// 因而多句柄连续登记不会使用旧 CAS 版本。它只接管已有句柄，不含 Execute 能力。
type taskTracker struct {
	app        *Application
	run        *domain.Run
	lease      *store.Lease
	checkpoint *domain.Checkpoint
}

func (t *taskTracker) Track(ctx context.Context, call domain.ToolCall, handle domain.TaskHandle) (domain.Task, error) {
	task := domain.Task{Scope: call.Scope, ID: newID(), Call: call, Handle: handle, Status: domain.TaskSubmitted}
	cp := *t.checkpoint
	cp.PendingCallIDs = slices.Clone(cp.PendingCallIDs)
	if !slices.Contains(cp.PendingCallIDs, call.ID) {
		cp.PendingCallIDs = append(cp.PendingCallIDs, call.ID)
	}
	result, e := t.app.db.TrackTask(ctx, store.TrackTaskRequest{Guard: store.WriteGuard{Lease: *t.lease, RunVersion: t.run.Version}, Task: task, Checkpoint: cp, ExpectedCheckpointVersion: cp.Version, Events: taskEvents(*t.run, task, domain.EventToolWaiting)})
	if e != nil {
		return domain.Task{}, e
	}
	t.run.Version = result.RunVersion
	cp.Version = result.CheckpointVersion
	*t.checkpoint = cp
	return result.Task, nil
}

func (t *taskTracker) Get(ctx context.Context, scope domain.Scope, id string) (domain.Task, error) {
	return t.app.GetTask(ctx, scope, id)
}
func (t *taskTracker) Cancel(ctx context.Context, scope domain.Scope, id string) error {
	return t.app.CancelTask(ctx, scope, id)
}

// Follow 只通知已持久化的快照，单个调用最多保留一个 Task，回调同步施加背压。
// 订阅断开只停止读取；服务生命周期中的观察继续运行，重连先返回最新持久版本。
func (t *taskTracker) Follow(ctx context.Context, scope domain.Scope, id string, notify func(domain.Task) error) error {
	if notify == nil {
		return invalid("task.follow_callback")
	}
	var version int64
	for {
		task, e := t.Get(ctx, scope, id)
		if e != nil {
			return e
		}
		if task.Version != version {
			if e = notify(task); e != nil {
				return e
			}
			version = task.Version
		}
		if task.Status.IsTerminal() {
			return nil
		}
		if e = waitTask(ctx, t.app.opts.Tasks.PollInterval); e != nil {
			return e
		}
	}
}

var _ agent.TaskTracker = (*taskTracker)(nil)
