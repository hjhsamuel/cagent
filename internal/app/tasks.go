package app

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/hjhsamuel/cagent/internal/agent"
	"github.com/hjhsamuel/cagent/internal/apperrors"
	"github.com/hjhsamuel/cagent/internal/domain"
	"github.com/hjhsamuel/cagent/internal/observability"
	"github.com/hjhsamuel/cagent/internal/store"
)

// GetTask 只返回认证作用域内的持久快照，查询不会启动或重新执行远端工具。
func (a *Application) GetTask(ctx context.Context, scope domain.Scope, id string) (domain.Task, error) {
	return a.db.GetTask(ctx, scope, id)
}

// CancelTask 先持久化取消意图。提供方请求由运行持有者发送，HTTP 断开不会丢失意图。
// 接纳取消并非确认远端 cancelled；完成竞争仍以 Get/Follow 的真实终态为准。
func (a *Application) CancelTask(ctx context.Context, scope domain.Scope, id string) error {
	task, e := a.db.CancelTask(ctx, scope, id)
	if e != nil {
		return e
	}
	a.schedule(scope, task.Call.RunID)
	return nil
}
func (a *Application) ResumeTaskMaintenance(ctx context.Context, scope domain.Scope, id string) error {
	task, err := a.db.ResumeTaskMaintenance(ctx, scope, id)
	if err != nil {
		return err
	}
	a.schedule(scope, task.Call.RunID)
	return nil
}
func (a *Application) maintenancePolicy() store.TaskMaintenancePolicy {
	m := a.opts.Maintenance
	return store.TaskMaintenancePolicy{Poll: a.opts.Tasks.PollInterval, Backoff: a.opts.Tasks.ReconnectBackoff, MaxBackoff: m.MaxBackoff, CancelGrace: m.CancelGrace, DetachedGrace: m.DetachedGrace, OutageGrace: m.OutageGrace, InteractionGrace: m.InteractionGrace}
}

func waitTask(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// maintainTasks 是 Run 内部维护步骤，不是执行队列。与模型执行使用同一租约，
// 网络观察在锁外进行，写入/续租串行；租约失效立即取消所有本次观察。
// 每页最多 32 个任务、最多 8 个并发观察，慢提供方不会阻止其他任务获得观察机会。
func (a *Application) maintainTasks(parent context.Context, scope domain.Scope, id string) (bool, error) {
	run, e := a.db.GetRun(parent, scope, id)
	if e != nil {
		return false, e
	}
	if run.Status == domain.RunQueued {
		return false, nil
	}
	release, err := a.maintenance.Try(parent)
	if err != nil {
		return false, err
	}
	defer release()
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	lease, e := a.db.AcquireLease(ctx, scope, id, a.owner, a.opts.LeaseDuration)
	if e != nil {
		return false, e
	}
	var mu sync.Mutex
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(a.opts.PollInterval)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				mu.Lock()
				next, err := a.db.RenewLease(ctx, lease, a.opts.LeaseDuration)
				if err == nil {
					lease = next
				}
				mu.Unlock()
				if err != nil {
					cancel()
					return
				}
			}
		}
	}()
	defer func() {
		cancel()
		<-done
		c, stop := context.WithTimeout(context.Background(), a.opts.LeaseDuration)
		defer stop()
		_ = a.db.ReleaseLease(c, lease)
	}()
	// 每个事务前重读版本：用户取消可以绕过持有者并发写入取消意图。
	guard := func() (store.WriteGuard, error) {
		current, err := a.db.GetRun(ctx, scope, id)
		if err != nil {
			return store.WriteGuard{}, err
		}
		run = current
		return store.WriteGuard{Lease: lease, RunVersion: run.Version}, nil
	}
	// P8 首个 Track 已持久化整批句柄；在开始观察之前补齐剩余记录。
	checkpoints, err := a.checkpoints(ctx, run)
	if err != nil {
		return false, err
	}
	for _, checkpoint := range checkpoints {
		req := agent.Request{Run: run, Caller: checkpoint.Caller, Checkpoint: &checkpoint}
		if recoverer, ok := a.runtime.(interface {
			PendingTasks(agent.Request) ([]domain.Task, error)
		}); ok && req.Checkpoint != nil {
			tasks, err := recoverer.PendingTasks(req)
			if err != nil {
				return false, err
			}
			for _, task := range tasks {
				cp, err := a.db.GetCheckpoint(ctx, scope, id, task.Call.Caller.InvocationID)
				if err != nil {
					return false, err
				}
				if !slices.Contains(cp.PendingCallIDs, task.Call.ID) {
					cp.PendingCallIDs = append(cp.PendingCallIDs, task.Call.ID)
				}
				task.ID = newID()
				mu.Lock()
				g, err := guard()
				if err == nil {
					_, err = a.db.TrackTask(ctx, store.TrackTaskRequest{Guard: g, Task: task, Checkpoint: cp, ExpectedCheckpointVersion: cp.Version, Events: taskEvents(run, task, domain.EventToolWaiting)})
				}
				mu.Unlock()
				if err != nil {
					return false, err
				}
			}
		}
	}
	var after string
	// Repair all handles first, then finalize a durable execution failure before
	// observing dependencies. Terminal runs continue maintenance without generation.
	if !run.Status.IsTerminal() {
		if runtime, ok := a.runtime.(interface{ CheckpointFailure(domain.Checkpoint) error }); ok {
			for _, cp := range checkpoints {
				if runtime.CheckpointFailure(cp) != nil {
					return false, nil
				}
			}
		}
	}
	for {
		page, err := a.db.ListUnsettledTasks(ctx, scope, id, store.KeyPage{After: after, Limit: 32})
		if err != nil {
			return false, err
		}
		type observation struct {
			task   domain.Task
			update domain.TaskUpdate
			err    error
			cancel cancellationObservation
		}
		results := make(chan observation, len(page.Items))
		slots := make(chan struct{}, 8)
		var observers sync.WaitGroup
		for _, task := range page.Items {
			if task.Status.IsTerminal() {
				results <- observation{task: task}
				continue
			}
			// Run 取消由数据库事实决定，即使原生成 goroutine 已退出也会继续维护。
			if run.Status == domain.RunCancelled && task.CancelRequestedAt == nil {
				mu.Lock()
				g, err := guard()
				if err == nil {
					var out store.TaskCommitResult
					out, err = a.db.RequestTaskCancel(ctx, store.TaskMetadataRequest{Guard: g, TaskID: task.ID, ExpectedTaskVersion: task.Version})
					task = out.Task
				}
				mu.Unlock()
				if err != nil {
					cancel()
					observers.Wait()
					return false, err
				}
			}
			if a.maintenancePolicy().Reason(task, run, time.Now()) != "" {
				mu.Lock()
				g, err := guard()
				if err == nil {
					var out store.TaskCommitResult
					out, err = a.db.RecordTaskMaintenance(ctx, g, task.ID, task.Version, a.maintenancePolicy(), false, false, false, "")
					task = out.Task
				}
				mu.Unlock()
				if err != nil {
					cancel()
					observers.Wait()
					return false, err
				}
				if task.Maintenance == domain.MaintenanceQuarantined {
					continue
				}
			}
			if task.NextObservationAt.After(time.Now()) {
				continue
			}
			observers.Add(1)
			deadline := a.maintenancePolicy().Deadline(task, run)
			go func(task domain.Task) {
				defer observers.Done()
				select {
				case slots <- struct{}{}:
				case <-ctx.Done():
					results <- observation{task: task, err: ctx.Err()}
					return
				}
				defer func() { <-slots }()
				observationCtx := ctx
				if !deadline.IsZero() {
					var stop context.CancelFunc
					observationCtx, stop = context.WithDeadline(ctx, deadline)
					defer stop()
				}
				var cancellation cancellationObservation
				update, err := a.observeTaskState(observationCtx, task, &cancellation)
				results <- observation{task: task, update: update, err: err, cancel: cancellation}
			}(task)
		}
		go func() { observers.Wait(); close(results) }()
		for observed := range results {
			readyBranch := false
			mu.Lock()
			err := func() error {
				g, err := guard()
				if err != nil {
					return err
				}
				task, err := a.db.GetTask(ctx, scope, observed.task.ID)
				if err != nil {
					return err
				}
				if !task.Status.IsTerminal() {
					meta := store.TaskMetadataRequest{Guard: g, TaskID: task.ID, ExpectedTaskVersion: task.Version, ObservationError: "task observation interrupted; retrying existing handle"}
					var out store.TaskCommitResult
					if observed.err != nil {
						out, err = a.db.RecordTaskObservationError(ctx, meta)
					} else {
						out, err = a.db.ObserveTask(ctx, store.ObserveTaskRequest{Guard: g, TaskID: task.ID, ExpectedTaskVersion: task.Version, Update: observed.update, Events: taskUpdateEvents(run, task, observed.update)})
					}
					if err != nil {
						return err
					}
					task = out.Task
					g.RunVersion = out.RunVersion
					run.Version = out.RunVersion
					out, err = a.db.RecordTaskMaintenance(ctx, g, task.ID, task.Version, a.maintenancePolicy(), true, observed.err == nil, observed.cancel.Attempted, observed.cancel.Error)
					if err != nil {
						return err
					}
					task = out.Task
					g.RunVersion = out.RunVersion
					run.Version = out.RunVersion
				}
				if task.Maintenance == domain.MaintenanceQuarantined {
					return nil
				}
				if !task.Status.IsTerminal() {
					return nil
				}
				delivery, err := a.db.GetTaskDelivery(ctx, scope, task.ID)
				if err != nil {
					return err
				}
				if run.Status.IsTerminal() {
					return a.db.DiscardTask(ctx, g, task.ID, delivery.Version)
				}
				cp, err := a.db.GetCheckpoint(ctx, scope, id, task.Call.Caller.InvocationID)
				if err != nil {
					return err
				}
				req := agent.Request{Run: run, Caller: task.Call.Caller, Checkpoint: &cp}
				emitted := false
				// Resume 的回调必须恰好提交一次接纳边界；任何模型输出都不属于消费事务。
				err = a.runtime.Resume(ctx, req, agent.Continuation{TaskID: task.ID, Call: task.Call, Status: task.Status, Result: task.Result}, func(callCtx context.Context, u agent.Update) error {
					if callCtx.Err() != nil {
						return callCtx.Err()
					}
					if emitted || u.AppliedTaskID != task.ID || u.Checkpoint == nil || u.Kind != domain.EventToolFinished || len(u.Tasks) > 0 || u.MessageRole != domain.RoleTool {
						return invalid("task.resume_output")
					}
					s, err := a.db.GetSession(ctx, scope, run.SessionID)
					if err != nil {
						return err
					}
					messages := []domain.Message{{Scope: scope, ID: newID(), SessionID: run.SessionID, RunID: id, Role: domain.RoleTool, Parts: u.Message}}
					_, err = a.db.ApplyTask(ctx, store.ApplyTaskRequest{Guard: g, OperationID: newID(), TaskID: task.ID, ExpectedTaskVersion: task.Version, ExpectedDeliveryVersion: delivery.Version, ExpectedSessionVersion: s.Version, Checkpoint: *u.Checkpoint, Messages: messages, Events: taskEvents(run, task, domain.EventToolFinished)})
					if err == nil {
						emitted = true
						readyBranch = len(u.Checkpoint.PendingCallIDs) == 0
					}
					return err
				})
				if err != nil {
					return err
				}
				if !emitted {
					return invalid("task.resume_missing_commit")
				}
				return nil
			}()
			mu.Unlock()
			if err != nil {
				cancel()
				observers.Wait()
				return false, err
			}
			// 已就绪分支不等待其他提供方的完整订阅超时。停止本轮未完成的观察，
			// 保留它们的持久游标，释放租约后立即推进该分支；下轮恢复同一任务。
			if readyBranch {
				cancel()
				observers.Wait()
				return false, nil
			}
		}
		if !page.HasMore {
			break
		}
		after = page.NextAfter
	}
	page, e := a.db.ListUnsettledTasks(ctx, scope, id, store.KeyPage{Limit: 1})
	if e != nil {
		return false, e
	}
	if len(page.Items) == 0 {
		return false, nil
	}
	checkpoints, e = a.checkpoints(ctx, run)
	if e != nil {
		return false, e
	}
	for _, cp := range checkpoints {
		if len(cp.PendingCallIDs) == 0 && !a.checkpointComplete(cp) {
			return false, nil
		}
	}
	return true, nil
}

// taskEvents 不在终态 Run 后追加 SSE；远端结果依然通过 Task 查询保留。
func taskEvents(run domain.Run, t domain.Task, kind domain.EventKind) []domain.Event {
	if run.Status.IsTerminal() {
		return nil
	}
	data, _ := json.Marshal(map[string]any{"task_id": t.ID, "tool_call_id": t.Call.ID, "agent_id": t.Call.Caller.AgentID, "invocation_id": t.Call.Caller.InvocationID, "parent_invocation_id": t.Call.Caller.ParentInvocationID, "status": t.Status, "progress": t.Progress})
	return []domain.Event{{Scope: run.Scope, RunID: run.ID, Kind: kind, Data: data}}
}

// observeTask 优先订阅；不支持或断流时完整查询，避免把网络故障映射成 failed。
// 每次只取一个通知，再由适配器完整快照协调顺序；游标从持久 Task 恢复。
type cancellationObservation struct {
	Attempted bool
	Error     string
}

func (a *Application) observeTask(ctx context.Context, t domain.Task) (domain.TaskUpdate, error) {
	return a.observeTaskState(ctx, t, nil)
}
func (a *Application) observeTaskState(ctx context.Context, t domain.Task, cancellation *cancellationObservation) (result domain.TaskUpdate, resultErr error) {
	ctx, finish := observability.Default.Start(ctx, "observation")
	defer func() { finish(resultErr) }()
	// 观察容量跨 Run 共享，过载只中断本地观察，绝不改变远端任务终态。
	release, err := a.observations.Try(ctx)
	if err != nil {
		return domain.TaskUpdate{}, err
	}
	defer release()
	if a.opts.Registry == nil {
		return domain.TaskUpdate{}, apperrors.ErrUnsupported
	}
	client, e := a.opts.Registry.ResolveTask(ctx, t.Scope, t.Handle)
	if e != nil {
		return domain.TaskUpdate{}, e
	}
	if t.CancelRequestedAt != nil {
		c, stop := context.WithTimeout(ctx, a.opts.Tasks.ObservationTimeout)
		cancelErr := client.Cancel(c, t.Call, t.Handle)
		if cancellation != nil {
			cancellation.Attempted = true
			if errors.Is(cancelErr, apperrors.ErrUnsupported) {
				cancellation.Error = "unsupported"
			} else if cancelErr != nil {
				cancellation.Error = "interrupted"
			}
		}
		stop()
	}
	c, stop := context.WithTimeout(ctx, a.opts.Tasks.ObservationTimeout)
	defer stop()
	var update domain.TaskUpdate
	received := false
	first := errors.New("observation received")
	e = client.Follow(c, t.Call, t.Handle, t.ProviderCursor, func(u domain.TaskUpdate) error { update = u; received = true; return first })
	if received {
		return update, nil
	}
	if ctx.Err() != nil {
		return domain.TaskUpdate{}, ctx.Err()
	}
	// 超时和断流后也重新查询；单次 Get 使用自己的超时，不沿用耗尽的订阅上下文。
	if e != nil && !errors.Is(e, apperrors.ErrUnsupported) {
		if err := waitTask(ctx, a.opts.Tasks.ReconnectBackoff); err != nil {
			return domain.TaskUpdate{}, err
		}
	}
	c2, stop2 := context.WithTimeout(ctx, a.opts.Tasks.ObservationTimeout)
	defer stop2()
	update, e = client.Get(c2, t.Call, t.Handle)
	if e != nil && ctx.Err() == nil {
		if waitErr := waitTask(ctx, a.opts.Tasks.ReconnectBackoff); waitErr != nil {
			return domain.TaskUpdate{}, waitErr
		}
	}
	return update, e
}
