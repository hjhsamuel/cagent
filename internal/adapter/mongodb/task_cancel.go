package mongodb

import (
	"context"
	"github.com/hjhsamuel/cagent/internal/domain"
)

// CancelTask 是认证用户的命令边界，与 CancelRun 一样不要求执行器租约。
// 只更新任务的取消意图和版本，不修改远端状态，也不撤销 Run 的生成租约。
// 同一事务写 Run 版本使执行器与取消竞争时重新读取；不能靠进程内信号保证持久性。
func (b *Database) CancelTask(ctx context.Context, scope domain.Scope, id string) (domain.Task, error) {
	var result domain.Task
	if e := validateKey(scope, id); e != nil {
		return result, e
	}
	e := b.withTransaction(ctx, "task.cancel_request", func(tx context.Context) error {
		var td taskRecord
		var task domain.Task
		if e := b.collection(TaskCollection).FindOne(tx, key(scope, id)).Decode(&td); e != nil {
			return e
		}
		if e := td.decode(&task); e != nil {
			return e
		}
		now, e := b.now(tx)
		if e != nil {
			return e
		}
		changed, e := task.RequestCancel(now)
		if e != nil {
			return e
		}
		result = task
		if !changed {
			return nil
		}
		var rd runDocument
		var run domain.Run
		if e = b.collection(RunCollection).FindOne(tx, key(scope, task.Call.RunID)).Decode(&rd); e != nil {
			return e
		}
		if e = rd.decode(&run); e != nil {
			return e
		}
		task.Version, e = increment(task.Version)
		if e != nil {
			return e
		}
		next, e := taskDocument(task)
		if e != nil {
			return e
		}
		next.Unsettled = td.Unsettled
		out, e := b.collection(TaskCollection).ReplaceOne(tx, td.versionKey(), next)
		if e != nil {
			return e
		}
		if out.MatchedCount != 1 {
			return conflict("task.version")
		}
		rd.NextActionAt = now
		if e = b.saveRun(tx, &run, rd, rd, now); e != nil {
			return e
		}
		result = task
		return nil
	})
	if e != nil {
		return domain.Task{}, e
	}
	return result, nil
}
