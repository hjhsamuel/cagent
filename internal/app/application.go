package app

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/hjhsamuel/cagent/internal/adapter/mongodb"
	"github.com/hjhsamuel/cagent/internal/agent"
	"github.com/hjhsamuel/cagent/internal/apperrors"
	"github.com/hjhsamuel/cagent/internal/config"
	"github.com/hjhsamuel/cagent/internal/domain"
	"github.com/hjhsamuel/cagent/internal/observability"
	"github.com/hjhsamuel/cagent/internal/store"
	"github.com/hjhsamuel/cagent/internal/tool"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// Options 由启动装配提供，生命周期不继承任何 HTTP 请求。
// Prepare 在租约保护下准备模型请求；NewContextPreparer 提供历史与工具关联校验，
// 为空仅保留 P4 原始历史替身路径。
// Recover 专门处理已经开始的运行，必须加载持久检查点，禁止静默重新 Execute。
// StartRecovery 使用独立管理连接发现候选；服务内观察不构成工具执行队列。
type Options struct {
	// Registry 只解析已有句柄；Tasks 限制单次观察和重连，绝不限制远端任务寿命。
	// Capacity 为零时使用默认值，显式配置必须为正。
	Capacity      config.Capacity
	Registry      tool.Registry
	Tasks         config.Tasks
	Maintenance   config.Maintenance
	LeaseDuration time.Duration
	PollInterval  time.Duration
	Prepare       func(context.Context, domain.Run) (agent.Request, error)
	Recover       func(context.Context, agent.Request, agent.Emit) error
	SelectModel   func(string) (config.SelectedModel, error)
}

// Application 的本地表仅去重和发送取消信号，跨实例所有权完全由 MongoDB 租约保证。
// Close 先拒绝新运行，再取消服务上下文；数据库应在 Close 完成后关闭。
type Application struct {
	runs         *observability.Gate
	observations *observability.Gate
	maintenance  *observability.Gate
	workers      *observability.Gate
	db           *mongodb.Database
	runtime      agent.Runtime
	events       *DurableEvents
	opts         Options
	owner        string
	ctx          context.Context
	stop         context.CancelFunc
	mu           sync.Mutex
	closed       bool
	closeDone    chan struct{}
	active       map[runKey]context.CancelFunc
	due          map[runKey]time.Time
	wg           sync.WaitGroup
}
type runKey struct {
	scope domain.Scope
	id    string
}

// NewService 不自行打开数据库；parent 必须为进程生命周期上下文，不能是请求上下文。
func NewService(parent context.Context, db *mongodb.Database, runtime agent.Runtime, opts Options) (*Application, error) {
	if opts.Maintenance == (config.Maintenance{}) {
		opts.Maintenance = config.Defaults().Maintenance
	}
	if err := opts.Maintenance.Validate(); err != nil {
		return nil, err
	}
	if opts.Capacity == (config.Capacity{}) {
		opts.Capacity = config.Defaults().Capacity
	}
	if err := opts.Capacity.Validate(); err != nil {
		return nil, err
	}
	if opts.Tasks == (config.Tasks{}) {
		opts.Tasks = config.Defaults().Tasks
	}
	if opts.Tasks.PollInterval <= 0 || opts.Tasks.ObservationTimeout <= 0 || opts.Tasks.ReconnectBackoff <= 0 {
		return nil, invalid("service.tasks")
	}
	if parent == nil || db == nil || runtime == nil || opts.PollInterval <= 0 || opts.LeaseDuration < 3*time.Millisecond || opts.PollInterval > opts.LeaseDuration/3 {
		return nil, invalid("service.options")
	}
	events, err := NewEventStream(db, opts.PollInterval, 128)
	if err != nil {
		return nil, err
	}
	ctx, stop := context.WithCancel(parent)
	a := &Application{runs: observability.NewGate(opts.Capacity.Runs, "run"), workers: observability.NewGate(opts.Capacity.Runs+opts.Maintenance.Workers, "worker"), maintenance: observability.NewGate(opts.Maintenance.Workers, "maintenance"), observations: observability.NewGate(opts.Capacity.Observations, "observation"), db: db, runtime: runtime, events: events, opts: opts, owner: newID(), ctx: ctx, stop: stop, active: make(map[runKey]context.CancelFunc), due: make(map[runKey]time.Time), closeDone: make(chan struct{})}
	a.wg.Add(1)
	go a.runDue()
	return a, nil
}
func newID() string { return bson.NewObjectID().Hex() }

// CreateSession 的 ID 在可信服务内生成；返回数据库实际分配的版本和时间。
func (a *Application) CreateSession(ctx context.Context, scope domain.Scope, agentID string) (domain.Session, error) {
	return a.CreateSessionWithModel(ctx, scope, agentID, "")
}

func (a *Application) CreateSessionWithModel(ctx context.Context, scope domain.Scope, agentID, modelID string) (domain.Session, error) {
	s := domain.Session{Scope: scope, ID: newID(), AgentID: agentID}
	if err := scope.Validate(); err != nil {
		return domain.Session{}, err
	}
	if a.opts.SelectModel != nil {
		selected, err := a.opts.SelectModel(modelID)
		if err != nil {
			return domain.Session{}, err
		}
		s.ModelID, s.APIKeyID = selected.ModelID, selected.APIKeyID
	} else if modelID != "" {
		return domain.Session{}, apperrors.ErrUnsupported
	}
	if err := a.db.CreateSession(ctx, s); err != nil {
		return domain.Session{}, err
	}
	return a.db.GetSession(ctx, scope, s.ID)
}

// GetSession 不以资源 ID 替代作用域，越权与不存在返回相同类别。
func (a *Application) GetSession(ctx context.Context, scope domain.Scope, id string) (domain.Session, error) {
	return a.db.GetSession(ctx, scope, id)
}

// GetRun 查询持久状态，不读取可能滞后的本地执行表。
func (a *Application) GetRun(ctx context.Context, scope domain.Scope, id string) (domain.Run, error) {
	return a.db.GetRun(ctx, scope, id)
}

// StartRun 原子持久化输入、运行和会话占用后调度。幂等重试仍交给存储校验输入摘要，
// 不仅按 key 返回旧 Run；重放返回当前状态。响应丢失可用同 key 重试找回持久结果。
func (a *Application) StartRun(ctx context.Context, req StartRun) (domain.Run, error) {
	a.mu.Lock()
	closed := a.closed || a.ctx.Err() != nil
	a.mu.Unlock()
	if closed {
		return domain.Run{}, context.Canceled
	}
	// 在落库前预留运行槽位，容量不足不创建 queued Run。事务失败自动归还。
	release, err := a.runs.Try(ctx)
	if err != nil {
		if errors.Is(err, apperrors.ErrOverloaded) && req.IdempotencyKey != "" {
			run, replayErr := a.db.ReplayRun(ctx, req.Scope, req.SessionID, req.IdempotencyKey, req.Input)
			if replayErr == nil {
				return run, nil
			}
			if !errors.Is(replayErr, apperrors.ErrNotFound) {
				return domain.Run{}, replayErr
			}
		}
		return domain.Run{}, err
	}
	defer func() {
		if release != nil {
			release()
		}
	}()
	session, err := a.db.GetSession(ctx, req.Scope, req.SessionID)
	if err != nil {
		return domain.Run{}, err
	}
	run := domain.Run{Scope: req.Scope, ID: newID(), SessionID: req.SessionID, Status: domain.RunQueued, IdempotencyKey: req.IdempotencyKey}
	input := domain.Message{Scope: req.Scope, ID: newID(), SessionID: req.SessionID, RunID: run.ID, Role: domain.RoleUser, Parts: req.Input}
	result, err := a.db.StartRun(ctx, store.StartRunRequest{Run: run, Input: input, ExpectedSessionVersion: session.Version})
	if err != nil {
		return domain.Run{}, err
	}
	// 后续启动不使用请求 ctx：客户端在事务提交后断开也不会取消已接纳的运行。
	a.scheduleReserved(result.Run.Scope, result.Run.ID, release, ctx)
	release = nil
	return a.db.GetRun(ctx, result.Run.Scope, result.Run.ID)
}

// CancelRun 先提交取消事实，再通知本地执行器；跨实例通过租约失效/状态轮询停止。
// 有限重试处理输出与取消的 CAS 竞争；耗尽返回冲突，调用方可以重试明确的用户意图。
func (a *Application) CancelRun(ctx context.Context, scope domain.Scope, id string) error {
	for attempt := 0; attempt < 8; attempt++ {
		run, err := a.db.GetRun(ctx, scope, id)
		if err != nil {
			return err
		}
		_, err = a.db.CancelRun(ctx, store.CancelRunRequest{Scope: scope, RunID: id, ExpectedRunVersion: run.Version})
		if errors.Is(err, apperrors.ErrConflict) {
			continue
		}
		if err != nil {
			return err
		}
		a.mu.Lock()
		if cancel := a.active[runKey{scope, id}]; cancel != nil {
			cancel()
		}
		a.mu.Unlock()
		return nil
	}
	return apperrors.New(apperrors.ErrConflict, "run.version", "concurrent updates prevented cancellation")
}

// RecoverRun 是恢复扫描器的作用域内入口。queued 可安全首次执行；已开始的运行
// 必须配置 Recover 回调，否则明确拒绝。Acquire 后仍重新读取，防止扫描/领取竞态。
func (a *Application) RecoverRun(ctx context.Context, scope domain.Scope, id string) error {
	run, err := a.db.GetRun(ctx, scope, id)
	if err != nil {
		return err
	}
	if !run.Status.IsTerminal() && run.Status != domain.RunQueued && a.opts.Recover == nil {
		return apperrors.New(apperrors.ErrUnsupported, "run.recovery", "checkpoint recovery is not configured")
	}
	var release func()
	if run.Status == domain.RunQueued {
		release, err = a.runs.Try(ctx)
		if err != nil {
			return err
		}
	}
	if !a.scheduleReserved(scope, id, release, ctx) {
		if a.ctx.Err() != nil {
			return a.ctx.Err()
		}
		return apperrors.ErrOverloaded
	}
	return nil
}
func (a *Application) schedule(scope domain.Scope, id string) bool {
	return a.scheduleReserved(scope, id, nil, a.ctx)
}

// scheduleReserved 接收 StartRun 预留槽位；恢复扫描无槽位则跳过，下一轮重试。
// 所有退出分支归还所有权，不创建无界等待队列。
func (a *Application) scheduleReserved(scope domain.Scope, id string, release func(), traceContext context.Context) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed || a.ctx.Err() != nil {
		if release != nil {
			release()
		}
		return false
	}
	key := runKey{scope, id}
	if a.active[key] != nil {
		if release != nil {
			release()
		}
		return true
	}
	workerRelease, err := a.workers.Try(a.ctx)
	if err != nil {
		if release != nil {
			release()
		}
		if len(a.due) < 4096 {
			a.due[key] = time.Now().Add(a.opts.Tasks.ReconnectBackoff)
		}
		return false
	}
	delete(a.due, key)
	ctx, cancel := context.WithCancel(observability.Link(a.ctx, traceContext))
	a.active[key] = cancel
	a.wg.Add(1)
	go a.runScheduled(ctx, cancel, key, release, workerRelease)
	return true
}

// Close 可重复调用；运行时须遵守 context 和 Emit 错误。调用方的等待超时不会
// 强行关闭数据库或丢弃仍在退出的 goroutine，后续可以再次 Close 等待。
func (a *Application) Close(ctx context.Context) error {
	a.mu.Lock()
	if !a.closed {
		a.closed = true
		a.stop()
		// 与 schedule 的 Add 共用锁：启动等待后不再允许新增运行。
		// 所有 Close 共享一个等待者，重复超时不会积累 goroutine。
		go func() { a.wg.Wait(); close(a.closeDone) }()
	}
	a.mu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-a.closeDone:
		return nil
	}
}

// execute 持有一个运行的租约。续期与提交串行更新凭证，避免使用已过期的本地副本。
// 失去租约、服务关闭或持久化失败都停止生成，并保留运行供恢复，不能猜测已提交结果。
func (a *Application) execute(parent context.Context, scope domain.Scope, id string) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	lease, err := a.db.AcquireLease(ctx, scope, id, a.owner, a.opts.LeaseDuration)
	if err != nil {
		return err
	}
	defer func() {
		c, stop := context.WithTimeout(context.Background(), a.opts.LeaseDuration)
		defer stop()
		_ = a.db.ReleaseLease(c, lease)
	}()
	run, err := a.db.GetRun(ctx, scope, id)
	if err != nil || run.Status.IsTerminal() {
		return err
	}
	recovering := run.Status != domain.RunQueued
	if recovering && a.opts.Recover == nil {
		return apperrors.ErrUnsupported
	}
	var writeMu sync.Mutex
	monitorDone := make(chan struct{})
	go func() {
		defer close(monitorDone)
		ticker := time.NewTicker(a.opts.PollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				writeMu.Lock()
				next, e := a.db.RenewLease(ctx, lease, a.opts.LeaseDuration)
				if e == nil {
					lease = next
				}
				writeMu.Unlock()
				if e != nil {
					cancel()
					return
				}
			}
		}
	}()
	defer func() { cancel(); <-monitorDone }()
	var writeErr error
	commit := func(status domain.RunStatus, events []domain.Event, messages []domain.Message, checkpoint *domain.Checkpoint) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		if writeErr != nil {
			return writeErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		session, e := a.db.GetSession(ctx, scope, run.SessionID)
		if e == nil {
			var result store.CommitResult
			req := store.CommitRunRequest{Guard: store.WriteGuard{Lease: lease, RunVersion: run.Version}, OperationID: newID(), Status: status, ExpectedSessionVersion: session.Version, Events: events, Messages: messages, Checkpoint: checkpoint}
			if checkpoint != nil {
				req.ExpectedCheckpointVersion = checkpoint.Version
			}
			result, e = a.events.Publish(ctx, req)
			if e == nil {
				run = result.Run
			}
		}
		if e != nil {
			writeErr = e
			cancel()
		}
		return e
	}
	if !recovering {
		if err = commit(domain.RunRunning, []domain.Event{{Scope: scope, RunID: id, Kind: domain.EventRunStarted}}, nil, nil); err != nil {
			return err
		}
	}
	request, err := a.prepare(ctx, run)
	if err == nil && recovering {
		// 根分支等待时，精确选择其他已经接纳全部依赖的 Invocation；同名 Agent
		// 的不同调用互不覆盖。没有可执行分支则让原检查点返回 ErrWaiting。
		cps, e := a.checkpoints(ctx, run)
		if e != nil {
			err = e
		} else {
			if failure, ok := a.runtime.(interface{ CheckpointFailure(domain.Checkpoint) error }); ok {
				for _, cp := range cps {
					if failure.CheckpointFailure(cp) != nil {
						request = agent.Request{Run: run, Caller: cp.Caller, Checkpoint: &cp}
						break
					}
				}
			}
			for _, cp := range cps {
				if failure, ok := a.runtime.(interface{ CheckpointFailure(domain.Checkpoint) error }); ok && request.Checkpoint != nil && failure.CheckpointFailure(*request.Checkpoint) != nil {
					break
				}
				if len(cp.PendingCallIDs) == 0 && !a.checkpointComplete(cp) {
					request = agent.Request{Run: run, Caller: cp.Caller, Checkpoint: &cp}
					break
				}
			}
		}
	}
	if err == nil {
		if request.Run != run {
			err = invalid("request.run")
		} else {
			err = request.Caller.Validate()
		}
	}
	if err == nil && recovering && request.Checkpoint != nil {
		cp := *request.Checkpoint
		if cp.Format == agent.InFlightCheckpointFormat {
			err = agent.ErrUncertain
		} else if len(cp.PendingCallIDs) == 0 && !a.checkpointComplete(cp) && a.opts.Registry != nil {
			// 接纳后的检查点不能无限重放：推进后可能已执行新工具，而新句柄
			// 尚未落库。先标记不确定窗口，再进入 SDK；新的安全输出覆盖屏障。
			cp.Format = agent.InFlightCheckpointFormat
			if e := commit(domain.RunRunning, nil, nil, &cp); e != nil {
				return e
			}
			persisted, e := a.db.GetCheckpoint(ctx, scope, id, cp.Caller.InvocationID)
			if e != nil {
				return e
			}
			original := *request.Checkpoint
			original.Version = persisted.Version
			request.Checkpoint = &original
			request.Run = run
		}
	}
	if err == nil && request.ContextSnapshot != nil {
		// 摘要模型在锁外执行，续租不受网络耗时阻塞；仅落库阶段与续租/其他写入串行。
		// 保存会推进 Run.Version，必须重读后再生成和提交，不能沿用准备时的旧版本。
		writeMu.Lock()
		_, err = a.db.SaveSnapshot(ctx, store.WriteGuard{Lease: lease, RunVersion: run.Version}, *request.ContextSnapshot,
			request.ContextSnapshot.Version, request.ContextSessionVersion)
		if err == nil {
			run, err = a.db.GetRun(ctx, scope, id)
			request.Run = run
		}
		request.ContextSnapshot = nil
		request.ContextSessionVersion = 0
		if err != nil {
			writeErr = err
			cancel()
		}
		writeMu.Unlock()
		if err != nil {
			return err
		}
	}
	if err == nil {
		// 即使替身错误地忽略 Emit 错误，也不能继续提交或伪造 completed。
		// 运行时返回与并行 Emit 通过同一锁交接；返回之后的迟到回调不再写入。
		var emitMu sync.Mutex
		var emitFailure error
		accepting := true
		emit := func(callCtx context.Context, update agent.Update) (emitErr error) {
			emitMu.Lock()
			defer emitMu.Unlock()
			if !accepting {
				return context.Canceled
			}
			if emitFailure != nil {
				return emitFailure
			}
			defer func() {
				if emitErr != nil {
					emitFailure = emitErr
				}
			}()
			if e := callCtx.Err(); e != nil {
				return e
			}
			switch update.Kind {
			case domain.EventTextDelta, domain.EventMessageCompleted, domain.EventToolStarted, domain.EventToolWaiting, domain.EventToolProgress, domain.EventToolFinished:
			default:
				return invalid("update.kind")
			}
			events := []domain.Event{{Scope: scope, RunID: id, Kind: update.Kind, Data: update.Data}}
			var messages []domain.Message
			if update.Message != nil {
				role := update.MessageRole
				if role == "" {
					role = domain.RoleAssistant
				}
				if role != domain.RoleAssistant && role != domain.RoleTool {
					return invalid("update.message_role")
				}
				if update.PromptTokens < 0 || (role != domain.RoleAssistant && update.PromptTokens != 0) {
					return invalid("update.prompt_tokens")
				}
				messages = []domain.Message{{Scope: scope, ID: newID(), SessionID: request.Run.SessionID, RunID: id, Role: role, Parts: update.Message, PromptTokens: update.PromptTokens}}
			}
			if update.Checkpoint != nil && update.Checkpoint.Caller != request.Caller && len(update.Tasks) == 0 {
				return invalid("update.checkpoint.caller")
			}
			if len(update.Tasks) > 0 {
				if update.Kind != domain.EventToolWaiting || update.Checkpoint == nil || len(messages) > 0 {
					return invalid("update.tasks")
				}
				writeMu.Lock()
				defer writeMu.Unlock()
				if writeErr != nil {
					return writeErr
				}
				cp := *update.Checkpoint
				cp.PendingCallIDs = nil
				// 每个 TrackTask 都将句柄和含全部 SDK 状态的检查点原子保存。
				// PendingCallIDs 按登记顺序增加，遵守存储禁止凭空新增待完成调用的约束。
				tracker := &taskTracker{app: a, run: &run, lease: &lease, checkpoint: &cp}
				for _, task := range update.Tasks {
					if _, e := tracker.Track(ctx, task.Call, task.Handle); e != nil {
						writeErr = e
						cancel()
						return e
					}
				}
				return nil
			}
			return commit(domain.RunRunning, events, messages, update.Checkpoint)
		}
		batch := newDeltaBatch(ctx, emit)
		if recovering {
			err = a.opts.Recover(ctx, request, batch.Emit)
		} else {
			err = a.runtime.Execute(ctx, request, batch.Emit)
		}
		if flushErr := batch.Finish(); flushErr != nil {
			err = flushErr
		}
		emitMu.Lock()
		accepting = false
		if err == nil {
			err = emitFailure
		}
		emitMu.Unlock()
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	status := domain.RunCompleted
	if errors.Is(err, agent.ErrWaiting) {
		status, e := a.branchStatus(ctx, run)
		if e != nil {
			return e
		}
		if status == domain.RunCompleted {
			status = domain.RunWaiting
		}
		return commit(status, nil, nil, nil)
	}
	if err == nil && a.opts.Registry != nil {
		status, err = a.branchStatus(ctx, run)
	}
	if err != nil {
		status = domain.RunFailed
	}
	// 不持久化原始运行错误，防止模型凭据/提示词进入公共事件；内部日志保留原因。
	var terminal []domain.Event
	if errors.Is(err, apperrors.ErrOverloaded) {
		terminal = []domain.Event{{Scope: scope, RunID: id, Kind: domain.EventRunFailed, Data: []byte(`{"reason":"overloaded","retry_tool":false}`)}}
	}
	if errors.Is(err, agent.ErrUncertain) {
		terminal = []domain.Event{{Scope: scope, RunID: id, Kind: domain.EventRunFailed, Data: []byte(`{"reason":"execution_outcome_uncertain","retry_tool":false}`)}}
	}
	if e := commit(status, terminal, nil, nil); e != nil {
		return e
	}
	return err
}

func (a *Application) prepare(ctx context.Context, run domain.Run) (agent.Request, error) {
	if a.opts.Prepare != nil {
		return a.opts.Prepare(ctx, run)
	}
	session, err := a.db.GetSession(ctx, run.Scope, run.SessionID)
	if err != nil {
		return agent.Request{}, err
	}
	req := agent.Request{Run: run, Caller: domain.AgentExecution{AgentID: session.AgentID, InvocationID: run.ID}}
	var after int64
	for {
		page, e := a.db.ListMessages(ctx, run.Scope, run.SessionID, store.SequencePage{After: after, Limit: 128})
		if e != nil {
			return agent.Request{}, e
		}
		req.Messages = append(req.Messages, page.Items...)
		if !page.HasMore {
			return req, nil
		}
		after = page.NextAfter
	}
}

var _ Service = (*Application)(nil)
var _ EventStream = (*DurableEvents)(nil)
