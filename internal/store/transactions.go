package store

import (
	"crypto/sha256"
	"encoding/binary"
	"hash"
	"slices"

	"github.com/hjhsamuel/cagent/internal/domain"
)

// StartRunRequest 描述唯一合法的运行创建入口，禁止拆成 Run.Create + Message.Append。
// Run/Input 的 ID 由可信应用生成，Version/Sequence 必须为零；ExpectedSessionVersion
// 来自已读会话。事务创建 queued Run、追加一条 user 消息、占用 Session.ActiveRunID。
type StartRunRequest struct {
	Run                    domain.Run
	Input                  domain.Message
	ExpectedSessionVersion int64
}

// Validate 校验新建请求结构，不查询会话存在性/占用，也不承担认证或幂等查询。
func (r StartRunRequest) Validate() error {
	if err := r.Run.Validate(); err != nil {
		return err
	}
	if err := r.Input.ValidateForRun(r.Run); err != nil {
		return err
	}
	if r.Run.Status != domain.RunQueued || r.Run.Version != 0 {
		return invalid("run", "new run must be queued with zero version")
	}
	if r.Input.Role != domain.RoleUser || r.Input.Sequence != 0 {
		return invalid("input", "input must be an unsequenced user message")
	}
	if r.ExpectedSessionVersion <= 0 {
		return invalid("session.version", "persisted version is required")
	}
	if r.Run.IdempotencyKey != "" {
		return nonblank("run.idempotency_key", r.Run.IdempotencyKey)
	}
	return nil
}

// InputFingerprint 对 StartRun 的语义输入生成版本化摘要；不包含新生成的资源 ID、
// 时间和预期版本，故提交结果未知时使用新 ID 重试仍可找回原 Run。当前请求语义输入
// 只有 Parts；未来新增模型/选项等字段时必须升级编码版本并纳入摘要。
// 保留 Part 顺序和全部字段，nil/空切片有区别；不 trim、不解析 Data 内的 JSON。
// 摘要仅用于幂等匹配，不是加密、权限证明或可公开的提示词标识。
func InputFingerprint(parts []domain.Part) [32]byte {
	h := sha256.New()
	h.Write([]byte("cagent-input-v1"))
	if parts == nil {
		h.Write([]byte{0})
	} else {
		h.Write([]byte{1})
	}
	writeLength(h, len(parts))
	for _, p := range parts {
		for _, value := range []string{p.Kind, p.Text, p.MIMEType, p.URI, p.ToolCallID, p.ToolName} {
			writeLength(h, len(value))
			h.Write([]byte(value))
		}
		if p.Data == nil {
			h.Write([]byte{0})
		} else {
			h.Write([]byte{1})
		}
		writeLength(h, len(p.Data))
		h.Write(p.Data)
	}
	var digest [32]byte
	copy(digest[:], h.Sum(nil))
	return digest
}

// 长度前缀避免字段拼接歧义，同时保留非 UTF-8 字节，不能用 JSON 的替换字符合并输入。
func writeLength(h hash.Hash, size int) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], uint64(size))
	h.Write(encoded[:])
}

// CheckIdempotentInput 在相同 Scope+SessionID+非空 key 已存在时调用。
// 命中先于会话版本/活动 Run 检查；不同输入冲突，禁止返回另一次请求的结果。
func CheckIdempotentInput(stored, requested [32]byte) error {
	if stored != requested {
		return conflict("run.idempotency_key")
	}
	return nil
}

type StartRunResult struct {
	Run            domain.Run
	Input          domain.Message
	SessionVersion int64
	Replayed       bool
}

// CommitRunRequest 原子提交一次运行输出：状态、新消息、新事件和可选检查点。
// OperationID 为本次逻辑提交的稳定 ID；网络结果未知必须原 ID/原内容重试。
// 存储保留 Scope+RunID+OperationID 的摘要与完整结果回执；相同内容返回原序号，
// 不同内容冲突。摘要排除 Guard 及预期版本，包含所有写入内容（含消息 ID）。
// 回执查找先于租约/版本检查；只返回旧结果，不授权旧持有者产生任何新写入。
type CommitRunRequest struct {
	Guard                     WriteGuard
	OperationID               string
	Status                    domain.RunStatus
	ExpectedSessionVersion    int64
	Messages                  []domain.Message
	Events                    []domain.Event
	Checkpoint                *domain.Checkpoint
	ExpectedCheckpointVersion int64
}

// ValidateForRun 校验提交形状和已读取 Run 的 CAS；数据库仍须原子重复这些前置条件。
// 创建检查点期望版本为 0；覆盖检查点时要求其携带预期旧版本，不能由客户端自增。
func (r CommitRunRequest) ValidateForRun(current domain.Run) error {
	if err := validateGuardForRun(r.Guard, current); err != nil {
		return err
	}
	if err := nonblank("operation.id", r.OperationID); err != nil {
		return err
	}
	if current.Status.IsTerminal() {
		return conflict("run.status")
	}
	if err := current.Status.ValidateTransition(r.Status); err != nil {
		return err
	}
	if r.Status == domain.RunCancelled {
		return invalid("run.status", "cancellation requires the cancel transaction")
	}
	if r.ExpectedSessionVersion <= 0 {
		return invalid("session.version", "persisted version is required")
	}
	if err := validateOutput(current, r.Messages, r.Events); err != nil {
		return err
	}
	if r.Checkpoint != nil {
		if err := r.Checkpoint.ValidateForRun(current); err != nil {
			return err
		}
		if err := CheckVersion(r.ExpectedCheckpointVersion, r.Checkpoint.Version); err != nil {
			return err
		}
	} else if r.ExpectedCheckpointVersion != 0 {
		return invalid("checkpoint.version", "checkpoint is required")
	}
	return nil
}

func validateGuardForRun(guard WriteGuard, run domain.Run) error {
	if err := guard.Validate(); err != nil {
		return err
	}
	if err := run.Validate(); err != nil {
		return err
	}
	if err := run.Status.Validate(); err != nil {
		return err
	}
	if err := guard.Lease.Scope.ValidateAgainst(run.Scope); err != nil {
		return err
	}
	if guard.Lease.RunID != run.ID {
		return invalid("lease.run_id", "does not match run")
	}
	return CheckVersion(guard.RunVersion, run.Version)
}

func validateOutput(run domain.Run, messages []domain.Message, events []domain.Event) error {
	ids := make(map[string]bool, len(messages))
	for _, message := range messages {
		if err := message.ValidateForRun(run); err != nil {
			return err
		}
		if message.Sequence != 0 {
			return invalid("message.sequence", "must be allocated by storage")
		}
		if ids[message.ID] {
			return invalid("message.id", "duplicate message")
		}
		ids[message.ID] = true
	}
	for _, event := range events {
		if err := event.ValidateForRun(run); err != nil {
			return err
		}
		if event.Sequence != 0 {
			return invalid("event.sequence", "must be allocated by storage")
		}
	}
	return nil
}

// CommitResult 返回适配器实际分配的序号和版本。每个发生变化的资源每次事务递增一次；
// 失败不返回可发布结果。事件只能在提交成功之后通知订阅者。
type CommitResult struct {
	Run            domain.Run
	SessionVersion int64
	Messages       []domain.Message
	Events         []domain.Event
	Checkpoint     *domain.Checkpoint
}

// CancelRunRequest 是来自已认证 Scope 的用户取消命令，不要求当前执行器的租约。
// 同事务执行：CAS Run、cancelled 状态、取消事件、清空属于该 Run 的会话占用、
// 递增 Fence 并撤销租约，防止并发生成提交。已终态返回当前 Run，保留实际终态。
// 取消不自动把远端 Task 设为 cancelled；跟踪器随后取得维护租约继续观察/丢弃交付。
type CancelRunRequest struct {
	Scope              domain.Scope
	RunID              string
	ExpectedRunVersion int64
}

// TrackTaskRequest 把已有句柄及其暂停检查点原子落库，不执行工具。
// 以 Scope+RunID+InvocationID+CallID 去重；相同调用/句柄返回已有 Task，
// 不用重试携带的旧检查点覆盖新检查点；不同内容冲突。已终态任务同时创建交付记录。
type TrackTaskRequest struct {
	Guard                     WriteGuard
	Task                      domain.Task
	Checkpoint                domain.Checkpoint
	ExpectedCheckpointVersion int64
	// Events 与首次跟踪同事务追加；幂等命中旧任务时不重复追加。
	Events []domain.Event
}

// ValidateForRun 校验新任务与暂停分支的一致性；其他待完成调用的保留由存储加载
// 旧检查点后比较，不能凭请求中的 PendingCallIDs 覆盖其他并行任务。
func (r TrackTaskRequest) ValidateForRun(run domain.Run) error {
	if err := validateGuardForRun(r.Guard, run); err != nil {
		return err
	}
	// 已终止 Run 可以补齐持久检查点内的既有句柄，以便取消/保留迟到结果；
	// 这不是再次执行工具。终止事件之后不得再发布任何运行事件。
	if run.Status.IsTerminal() && len(r.Events) > 0 {
		return conflict("run.events")
	}
	if err := validateOutput(run, nil, r.Events); err != nil {
		return err
	}
	if err := r.Task.ValidateForRun(run); err != nil {
		return err
	}
	if err := r.Task.Status.Validate(); err != nil {
		return err
	}
	if r.Task.Version != 0 || r.Task.AppliedAt != nil {
		return invalid("task", "new task must not be persisted or consumed")
	}
	if r.Task.Result != nil && !r.Task.Status.IsTerminal() {
		return invalid("task.result", "result requires terminal status")
	}
	if err := r.Checkpoint.ValidateForRun(run); err != nil {
		return err
	}
	if r.Checkpoint.Caller != r.Task.Call.Caller || !slices.Contains(r.Checkpoint.PendingCallIDs, r.Task.Call.ID) {
		return invalid("checkpoint", "checkpoint must preserve the original pending call")
	}
	return CheckVersion(r.ExpectedCheckpointVersion, r.Checkpoint.Version)
}

// ObserveTaskRequest 在 Guard 和 TaskVersion CAS 下应用 P1.5 的完整快照。
// 终态 Task 和 pending TaskDelivery 原子保存；该事务不恢复模型、不设置 AppliedAt。
// 无变化不增加 TaskVersion；任何新增记录/变化必须递增 Run.Version，使其他分支重读。
type ObserveTaskRequest struct {
	Guard               WriteGuard
	TaskID              string
	ExpectedTaskVersion int64
	Update              domain.TaskUpdate
	// Events 与实际变化同事务追加；无变化忽略，不制造重复进度事件。
	// Run 已终态时必须为空，迟到结果仅保存 Task/Delivery，不在终止事件后追加事件。
	Events []domain.Event
}

// ValidateAgainst 允许维护租约观察已结束 Run 的远端任务，但禁止继续该 Run 的事件流。
// 只验证快照形状与迁移，实际应用必须使用 P1.5 ApplyUpdate 并在变化时创建交付记录。
func (r ObserveTaskRequest) ValidateAgainst(run domain.Run, task domain.Task) error {
	if err := validateGuardForRun(r.Guard, run); err != nil {
		return err
	}
	if err := task.ValidateForRun(run); err != nil {
		return err
	}
	if r.TaskID != task.ID {
		return invalid("task.id", "does not match task")
	}
	if task.Version <= 0 {
		return conflict("task.version")
	}
	if err := CheckVersion(r.ExpectedTaskVersion, task.Version); err != nil {
		return err
	}
	if run.Status.IsTerminal() && len(r.Events) != 0 {
		return conflict("run.events")
	}
	if err := validateOutput(run, nil, r.Events); err != nil {
		return err
	}
	if err := task.Status.ValidateTransition(r.Update.Status); err != nil {
		return err
	}
	if r.Update.Result != nil {
		if !r.Update.Status.IsTerminal() {
			return invalid("task.result", "result requires terminal status")
		}
		return r.Update.Result.ValidateForCall(task.Call)
	}
	return nil
}

// TaskCommitResult 返回实际落库任务和事件，便于提交后发布；每次变化后的 RunVersion
// 供同一持有者继续提交。Track 幂等重试/Observe 无变化返回空 Events，不能重新发布。
type TaskCommitResult struct {
	Task              domain.Task
	RunVersion        int64
	CheckpointVersion int64
	Events            []domain.Event
}

// TaskMetadataRequest 分别用于取消意图/观察错误，只允许 P1.5 方法规定的字段变化。
// ObservationError 由适配器生成安全说明；记录取消意图不能视为远端取消确认。
type TaskMetadataRequest struct {
	Guard               WriteGuard
	TaskID              string
	ExpectedTaskVersion int64
	ObservationError    string
}

// ApplyTaskRequest 原子提交已消费工具结果后的检查点、Task.AppliedAt、交付状态及输出。
// Checkpoint.Version 是待更新检查点的原版本；Guard 及各版本都在事务中比较。
// 同一交付仅能结算一次。OperationID 与内容摘要写入交付回执；相同重试返回原结果，
// 不同重试冲突。回执查找先于旧版本/过期租约检查，但不得再次执行任何写入。
type ApplyTaskRequest struct {
	Guard                   WriteGuard
	OperationID             string
	TaskID                  string
	ExpectedTaskVersion     int64
	ExpectedDeliveryVersion int64
	ExpectedSessionVersion  int64
	Checkpoint              domain.Checkpoint
	Messages                []domain.Message
	Events                  []domain.Event
}

// ValidateAgainst 验证已读取版本、输出关联及交付前提；只用于未命中回执的新提交。
// 租约有效期、会话版本以及全部资源 CAS 仍须在同一持久化事务内验证。
func (r ApplyTaskRequest) ValidateAgainst(run domain.Run, task domain.Task, delivery domain.TaskDelivery, current domain.Checkpoint) error {
	if err := validateGuardForRun(r.Guard, run); err != nil {
		return err
	}
	if err := nonblank("operation.id", r.OperationID); err != nil {
		return err
	}
	if r.TaskID != task.ID {
		return invalid("task.id", "does not match task")
	}
	if task.Version <= 0 || delivery.Version <= 0 {
		return conflict("task.version")
	}
	if err := CheckVersion(r.ExpectedTaskVersion, task.Version); err != nil {
		return err
	}
	if err := CheckVersion(r.ExpectedDeliveryVersion, delivery.Version); err != nil {
		return err
	}
	if r.ExpectedSessionVersion <= 0 {
		return invalid("session.version", "persisted version is required")
	}
	if err := validateOutput(run, r.Messages, r.Events); err != nil {
		return err
	}
	return ValidateTaskApplication(run, task, delivery, current, r.Checkpoint)
}

// ValidateTaskApplication 以事务内读取的对象检查续接前提，保护并行 Subagent 路由。
// next 必须保留其他待完成调用、仅移除当前 CallID。它表达“结果已并入检查点”，
// 不表示外部模型恰好执行一次；模型或工具的外部副作用仍须适配器幂等协调。
func ValidateTaskApplication(run domain.Run, task domain.Task, delivery domain.TaskDelivery, current, next domain.Checkpoint) error {
	if err := run.Status.Validate(); err != nil {
		return err
	}
	if run.Status.IsTerminal() {
		return conflict("run.status")
	}
	if err := task.ValidateForRun(run); err != nil {
		return err
	}
	if err := task.Status.Validate(); err != nil {
		return err
	}
	if !task.Status.IsTerminal() || task.AppliedAt != nil {
		return conflict("task.applied_at")
	}
	if err := delivery.Scope.ValidateAgainst(task.Scope); err != nil {
		return err
	}
	if delivery.TaskID != task.ID || delivery.RunID != run.ID || delivery.Caller != task.Call.Caller || delivery.ToolCallID != task.Call.ID {
		return invalid("delivery", "delivery does not match original invocation")
	}
	if delivery.State != domain.DeliveryPending || delivery.SettledAt != nil {
		return conflict("delivery.state")
	}
	for _, cp := range []domain.Checkpoint{current, next} {
		if err := cp.ValidateForRun(run); err != nil {
			return err
		}
		if cp.Caller != task.Call.Caller {
			return invalid("checkpoint.caller", "checkpoint does not match original invocation")
		}
	}
	if err := CheckVersion(next.Version, current.Version); err != nil {
		return err
	}
	if current.Version == 0 {
		return conflict("checkpoint.version")
	}
	if !slices.Contains(current.PendingCallIDs, task.Call.ID) || slices.Contains(next.PendingCallIDs, task.Call.ID) {
		return conflict("checkpoint.pending_call_ids")
	}
	remaining := make([]string, 0, len(current.PendingCallIDs)-1)
	for _, id := range current.PendingCallIDs {
		if id != task.Call.ID {
			remaining = append(remaining, id)
		}
	}
	actual := slices.Clone(next.PendingCallIDs)
	slices.Sort(remaining)
	slices.Sort(actual)
	if !slices.Equal(remaining, actual) {
		return conflict("checkpoint.pending_call_ids")
	}
	return nil
}
