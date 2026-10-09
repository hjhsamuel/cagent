package mongodb

import (
	"context"
	"errors"
	"reflect"
	"time"

	"github.com/hjhsamuel/cagent/internal/domain"
	"github.com/hjhsamuel/cagent/internal/store"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

func taskDocument(t domain.Task) (document, error) {
	d, err := pack(t.Scope, t.ID, t, t.Version)
	d.RunID = t.Call.RunID
	d.SessionID = t.Call.SessionID
	d.InvocationID = t.Call.Caller.InvocationID
	d.CallID = t.Call.ID
	d.Protocol = string(t.Handle.Protocol)
	d.ConnectionID = t.Handle.ConnectionID
	d.RemoteID = t.Handle.RemoteID
	d.Status = string(t.Status)
	d.NextActionAt = t.NextObservationAt
	return d, err
}
func (b *Database) createDelivery(ctx context.Context, t domain.Task, now time.Time) error {
	v := domain.TaskDelivery{Scope: t.Scope, TaskID: t.ID, RunID: t.Call.RunID, Caller: t.Call.Caller, ToolCallID: t.Call.ID, State: domain.DeliveryPending, Version: 1, CreatedAt: now}
	d, err := pack(t.Scope, t.ID, v, 1)
	if err != nil {
		return err
	}
	d.RunID = t.Call.RunID
	d.Status = string(v.State)
	_, err = b.collection(TaskDeliveryCollection).InsertOne(ctx, d)
	return err
}

// TrackTask 登记已启动工具的远端句柄及暂停检查点，不执行工具。
// 按原调用去重，重试不覆盖已推进的检查点；初始即终态时同时保存待交付结果。
func (b *Database) TrackTask(ctx context.Context, req store.TrackTaskRequest) (store.TaskCommitResult, error) {
	if err := req.Task.Validate(); err != nil {
		return store.TaskCommitResult{}, err
	}
	if err := req.Guard.Lease.Scope.ValidateAgainst(req.Task.Scope); err != nil {
		return store.TaskCommitResult{}, err
	}
	if req.Guard.Lease.RunID != req.Task.Call.RunID {
		return store.TaskCommitResult{}, invalid("task.run_id")
	}
	var result store.TaskCommitResult
	commitErr := b.withTransaction(ctx, "track", func(tx context.Context) error {
		result = store.TaskCommitResult{} // 每次重试独立构造结果，失败尝试不向调用方泄漏。
		var out store.TaskCommitResult
		f := scoped(req.Task.Scope)
		f["run_id"] = req.Task.Call.RunID
		f["invocation_id"] = req.Task.Call.Caller.InvocationID
		f["call_id"] = req.Task.Call.ID
		var previousDoc document
		var previous domain.Task
		err := b.collection(TaskCollection).FindOne(tx, f).Decode(&previousDoc)
		if err == nil {
			err = b.decode(tx, previousDoc, &previous)
		}
		if err == nil {
			if !reflect.DeepEqual(previous.Call, req.Task.Call) || previous.Handle != req.Task.Handle {
				return conflict("task.call")
			}
			var runDoc document
			var run domain.Run
			e := b.collection(RunCollection).FindOne(tx, key(previous.Scope, previous.Call.RunID)).Decode(&runDoc)
			if e == nil {
				e = b.decode(tx, runDoc, &run)
			}
			if e != nil {
				return e
			}
			var cpDoc document
			var cp domain.Checkpoint
			e = b.collection(CheckpointCollection).FindOne(tx, key(previous.Scope, compositeID(previous.Call.RunID, previous.Call.Caller.InvocationID))).Decode(&cpDoc)
			if e == nil {
				e = b.decode(tx, cpDoc, &cp)
			}
			result = store.TaskCommitResult{Task: previous, RunVersion: run.Version, CheckpointVersion: cp.Version}
			return e
		}
		if !errors.Is(err, mongo.ErrNoDocuments) {
			result = out
			return err
		}
		run, old, now, err := b.guard(tx, req.Guard)
		if err != nil {
			return err
		}
		if err = req.ValidateForRun(run); err != nil {
			return err
		}
		cp, _, err := b.checkpoint(tx, run, req.Checkpoint, req.ExpectedCheckpointVersion, "track", req.Task.Call.ID, now)
		if err != nil {
			return err
		}
		t := req.Task
		t.Version = 1
		t.CreatedAt = now
		t.UpdatedAt = now
		t.LastObservedAt = now
		t.LastContactAt = now
		t.NextObservationAt = now
		t.Maintenance = domain.MaintenanceActive
		t.CancelRequestedAt = nil
		t.ObservationError = ""
		d, err := taskDocument(t)
		if err != nil {
			return err
		}
		d.Unsettled = 1
		_, err = b.collection(TaskCollection).InsertOne(tx, d)
		if err != nil {
			return err
		}
		if t.Status.IsTerminal() {
			if err = b.createDelivery(tx, t, now); err != nil {
				return err
			}
		}
		updated := old
		updated.Unsettled, err = increment(updated.Unsettled)
		if err != nil {
			return err
		}
		out.Events, err = b.appendEvents(tx, run, &updated, req.Events, now, false)
		if err != nil {
			return err
		}
		if err = b.saveRun(tx, &run, old, updated, now); err != nil {
			return err
		}
		out.Task = t
		out.RunVersion = run.Version
		out.CheckpointVersion = cp.Version
		result = out
		return nil
	})
	if commitErr != nil {
		return store.TaskCommitResult{}, commitErr
	}
	return result, nil
}

// ObserveTask 合并远端观察结果；重复快照不推进业务版本或重复追加事件。
// 首次终态与 pending delivery 同事务落库；终态 Run 的迟到结果不重开事件流。
func (b *Database) ObserveTask(ctx context.Context, req store.ObserveTaskRequest) (store.TaskCommitResult, error) {
	if err := validateKey(req.Guard.Lease.Scope, req.TaskID); err != nil {
		return store.TaskCommitResult{}, err
	}
	var result store.TaskCommitResult
	commitErr := b.withTransaction(ctx, "observe", func(tx context.Context) error {
		result = store.TaskCommitResult{} // 每次重试独立构造结果，失败尝试不向调用方泄漏。
		var out store.TaskCommitResult
		run, old, now, err := b.guard(tx, req.Guard)
		if err != nil {
			return err
		}
		var d document
		var t domain.Task
		err = b.collection(TaskCollection).FindOne(tx, key(run.Scope, req.TaskID)).Decode(&d)
		if err == nil {
			err = b.decode(tx, d, &t)
		}
		if err != nil {
			return err
		}
		if err = req.ValidateAgainst(run, t); err != nil {
			return err
		}
		wasTerminal := t.Status.IsTerminal()
		changed, err := t.ApplyUpdate(req.Update, now)
		if err != nil {
			return err
		}
		out.Task = t
		out.RunVersion = run.Version
		if !changed {
			result = out
			return nil
		}
		t.Version, err = increment(t.Version)
		if err != nil {
			return err
		}
		next, err := taskDocument(t)
		if err != nil {
			return err
		}
		next.Unsettled = d.Unsettled
		dUpdate, err := b.collection(TaskCollection).ReplaceOne(tx, d.versionKey(), next)
		if err == nil && dUpdate.MatchedCount != 1 {
			err = conflict("version")
		}
		if err != nil {
			return err
		}
		if !wasTerminal && t.Status.IsTerminal() {
			if err = b.createDelivery(tx, t, now); err != nil {
				return err
			}
		}
		updated := old
		out.Events, err = b.appendEvents(tx, run, &updated, req.Events, now, false)
		if err != nil {
			return err
		}
		if err = b.saveRun(tx, &run, old, updated, now); err != nil {
			return err
		}
		out.Task = t
		out.RunVersion = run.Version
		result = out
		return nil
	})
	if commitErr != nil {
		return store.TaskCommitResult{}, commitErr
	}
	return result, nil
}

// 元数据修改独立于远端状态；不能通过错误字符串或请求取消伪造终态。
func (b *Database) metadata(ctx context.Context, req store.TaskMetadataRequest, cancelTask bool) (store.TaskCommitResult, error) {
	if err := validateKey(req.Guard.Lease.Scope, req.TaskID); err != nil {
		return store.TaskCommitResult{}, err
	}
	var result store.TaskCommitResult
	commitErr := b.withTransaction(ctx, "task.metadata", func(tx context.Context) error {
		result = store.TaskCommitResult{} // 每次重试独立构造结果，失败尝试不向调用方泄漏。
		var out store.TaskCommitResult
		run, old, now, err := b.guard(tx, req.Guard)
		if err != nil {
			return err
		}
		var d document
		var t domain.Task
		err = b.collection(TaskCollection).FindOne(tx, key(run.Scope, req.TaskID)).Decode(&d)
		if err == nil {
			err = b.decode(tx, d, &t)
		}
		if err != nil {
			return err
		}
		if err = t.ValidateForRun(run); err != nil {
			return err
		}
		if err = store.CheckVersion(req.ExpectedTaskVersion, t.Version); err != nil {
			return err
		}
		var changed bool
		if cancelTask {
			changed, err = t.RequestCancel(now)
		} else {
			changed, err = t.RecordObservationError(req.ObservationError, now)
		}
		if err != nil {
			return err
		}
		out.Task = t
		out.RunVersion = run.Version
		if !changed {
			result = out
			return nil
		}
		t.Version, err = increment(t.Version)
		if err != nil {
			return err
		}
		next, err := taskDocument(t)
		if err != nil {
			return err
		}
		next.Unsettled = d.Unsettled
		dUpdate, err := b.collection(TaskCollection).ReplaceOne(tx, d.versionKey(), next)
		if err == nil && dUpdate.MatchedCount != 1 {
			err = conflict("version")
		}
		if err != nil {
			return err
		}
		if err = b.saveRun(tx, &run, old, old, now); err != nil {
			return err
		}
		out.Task = t
		out.RunVersion = run.Version
		result = out
		return nil
	})
	if commitErr != nil {
		return store.TaskCommitResult{}, commitErr
	}
	return result, nil
}

// RequestTaskCancel 只记录取消意图，不推断远端已取消；实际状态继续由观察更新。
func (b *Database) RequestTaskCancel(ctx context.Context, r store.TaskMetadataRequest) (store.TaskCommitResult, error) {
	return b.metadata(ctx, r, true)
}

// RecordTaskObservationError 记录观察失败而不把它当作远端失败，也不重新执行工具。
func (b *Database) RecordTaskObservationError(ctx context.Context, r store.TaskMetadataRequest) (store.TaskCommitResult, error) {
	return b.metadata(ctx, r, false)
}

// ApplyTask 将结果消费标记、检查点、消息、事件和回执置于同一事务。
// 必须匹配原调用分支及所有版本；与取消竞争时由数据库决定串行结果。
func (b *Database) ApplyTask(ctx context.Context, req store.ApplyTaskRequest) (store.CommitResult, error) {
	if err := validateKey(req.Guard.Lease.Scope, req.TaskID); err != nil {
		return store.CommitResult{}, err
	}
	digest, err := mutationDigest(store.MutationTaskApply, req.TaskID, "", req.Messages, req.Events, &req.Checkpoint)
	if err != nil {
		return store.CommitResult{}, safeError(err)
	}
	var result store.CommitResult
	commitErr := b.withTransaction(ctx, "apply", func(tx context.Context) error {
		result = store.CommitResult{} // 每次重试独立构造结果，失败尝试不向调用方泄漏。
		out, found, err := b.receipt(tx, req.Guard.Lease.Scope, req.Guard.Lease.RunID, req.OperationID, store.MutationTaskApply, digest)
		if err != nil || found {
			result = out
			return err
		}
		run, old, now, err := b.guard(tx, req.Guard)
		if err != nil {
			return err
		}
		var td document
		var t domain.Task
		err = b.collection(TaskCollection).FindOne(tx, key(run.Scope, req.TaskID)).Decode(&td)
		if err == nil {
			err = b.decode(tx, td, &t)
		}
		if err != nil {
			return err
		}
		var dd document
		var delivery domain.TaskDelivery
		err = b.collection(TaskDeliveryCollection).FindOne(tx, key(run.Scope, req.TaskID)).Decode(&dd)
		if err == nil {
			err = b.decode(tx, dd, &delivery)
		}
		if errors.Is(err, mongo.ErrNoDocuments) && t.Status.IsTerminal() {
			return invariant()
		}
		if err != nil {
			return err
		}
		var currentDoc document
		var current domain.Checkpoint
		err = b.collection(CheckpointCollection).FindOne(tx, key(run.Scope, compositeID(run.ID, t.Call.Caller.InvocationID))).Decode(&currentDoc)
		if err == nil {
			err = b.decode(tx, currentDoc, &current)
		}
		if err != nil {
			return err
		}
		if err = req.ValidateAgainst(run, t, delivery, current); err != nil {
			return err
		}
		cp, _, err := b.checkpoint(tx, run, req.Checkpoint, current.Version, "apply", t.Call.ID, now)
		if err != nil {
			return err
		}
		out.Checkpoint = &cp
		t.AppliedAt = &now
		t.Maintenance = domain.MaintenanceSettled
		t.UpdatedAt = now
		t.Version, err = increment(t.Version)
		if err != nil {
			return err
		}
		next, err := taskDocument(t)
		if err != nil {
			return err
		}
		next.Unsettled = 0
		tdUpdate, err := b.collection(TaskCollection).ReplaceOne(tx, td.versionKey(), next)
		if err == nil && tdUpdate.MatchedCount != 1 {
			err = conflict("version")
		}
		if err != nil {
			return err
		}
		if err = b.settle(tx, delivery, dd, domain.DeliveryApplied, now); err != nil {
			return err
		}
		out.Messages, out.SessionVersion, err = b.output(tx, run, req.ExpectedSessionVersion, req.Messages, false, now)
		if err != nil {
			return err
		}
		updated := old
		if updated.Unsettled <= 0 {
			return invariant()
		}
		updated.Unsettled--
		out.Events, err = b.appendEvents(tx, run, &updated, req.Events, now, false)
		if err != nil {
			return err
		}
		if err = b.saveRun(tx, &run, old, updated, now); err != nil {
			return err
		}
		out.Run = run
		result = out
		return b.saveReceipt(tx, run.Scope, run.ID, req.OperationID, store.MutationTaskApply, digest, out)
	})
	if commitErr != nil {
		return store.CommitResult{}, commitErr
	}
	return result, nil
}

func (b *Database) settle(ctx context.Context, d domain.TaskDelivery, old document, state domain.DeliveryState, now time.Time) error {
	v, err := increment(d.Version)
	if err != nil {
		return err
	}
	d.Version = v
	d.State = state
	d.SettledAt = &now
	next, err := repack(old, d, v)
	if err != nil {
		return err
	}
	next.Status = string(state)
	oldUpdate, err := b.collection(TaskDeliveryCollection).ReplaceOne(ctx, old.versionKey(), next)
	if err == nil && oldUpdate.MatchedCount != 1 {
		err = conflict("version")
	}
	return err
}

// DiscardTask 仅为已终态 Run 丢弃 pending 交付，保留远端结果用于追溯。
// 不推进检查点、不设置 AppliedAt，也不追加运行事件；重复丢弃可安全返回。
func (b *Database) DiscardTask(ctx context.Context, g store.WriteGuard, id string, expected int64) error {
	if err := validateKey(g.Lease.Scope, id); err != nil {
		return err
	}
	err := b.withTransaction(ctx, "discard", func(tx context.Context) error {
		var dd document
		var d domain.TaskDelivery
		err := b.collection(TaskDeliveryCollection).FindOne(tx, key(g.Lease.Scope, id)).Decode(&dd)
		if err == nil {
			err = b.decode(tx, dd, &d)
		}
		if err != nil {
			return err
		}
		if d.RunID != g.Lease.RunID {
			return invalid("delivery.run_id")
		}
		if d.State == domain.DeliveryDiscarded {
			return nil
		}
		run, old, now, err := b.guard(tx, g)
		if err != nil {
			return err
		}
		if !run.Status.IsTerminal() || d.State != domain.DeliveryPending {
			return conflict("delivery.state")
		}
		if err = store.CheckVersion(expected, d.Version); err != nil {
			return err
		}
		var td document
		var t domain.Task
		err = b.collection(TaskCollection).FindOne(tx, key(run.Scope, id)).Decode(&td)
		if err == nil {
			err = b.decode(tx, td, &t)
		}
		if err != nil {
			return err
		}
		if err = t.ValidateForRun(run); err != nil {
			return err
		}
		if !t.Status.IsTerminal() || t.AppliedAt != nil {
			return invariant()
		}
		if err = b.settle(tx, d, dd, domain.DeliveryDiscarded, now); err != nil {
			return err
		}
		t.Maintenance = domain.MaintenanceSettled
		t.Version, err = increment(t.Version)
		if err != nil {
			return err
		}
		t.UpdatedAt = now
		next, err := taskDocument(t)
		if err != nil {
			return err
		}
		tdUpdate, err := b.collection(TaskCollection).ReplaceOne(tx, td.versionKey(), next)
		if err == nil && tdUpdate.MatchedCount != 1 {
			err = conflict("version")
		}
		if err != nil {
			return err
		}
		updated := old
		if updated.Unsettled <= 0 {
			return invariant()
		}
		updated.Unsettled--
		return b.saveRun(tx, &run, old, updated, now)
	})
	return err
}
