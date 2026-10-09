// Package agent defines agent execution, continuation and long-task tracking.
package agent

import (
	"context"
	"errors"

	"github.com/hjhsamuel/cagent/internal/domain"
)

// ErrWaiting 表示工具句柄及暂停检查点已经交接；应用保留活动 Run，不能提交 completed。
// 重启后必须走 P9 的任务恢复，禁止重新 Execute 已产生副作用的工具。
var ErrWaiting = errors.New("agent waiting for tool task")

// ErrUncertain 表示运行中断时没有可恢复的副作用边界。提供方尚无幂等查询能力时，
// 必须记录不确定结果并停止，不能以重新 Execute 猜测上一次工具是否启动成功。
var ErrUncertain = errors.New("external execution outcome is uncertain")

// InFlightCheckpointFormat 是应用推进恢复分支前的持久屏障。原 Data 保留供内部
// 排障，但不能再重放：接下来可能已经产生新的外部工具副作用。
const InFlightCheckpointFormat = "cagent.in-flight/v1"

type Request struct {
	Run      domain.Run
	Caller   domain.AgentExecution
	Messages []domain.Message
	// ContextSnapshot 仅在应用准备阶段暂存待保存摘要；应用以当前租约和 Run CAS
	// 保存成功后清空这两个字段，再交给 Runtime。失败禁止把候选输入发给模型。
	ContextSnapshot       *domain.ContextSnapshot
	ContextSessionVersion int64
	// Checkpoint 由运行恢复流程按 Scope+RunID+InvocationID 加载；首次执行可为空，
	// Resume 必须提供原分支检查点。适配器验证 Format，不支持时明确拒绝恢复，
	// 不能静默重新 Execute。检查点 Data 不作为普通提示词或 HTTP 响应输出。
	Checkpoint *domain.Checkpoint
}

// Continuation routes a terminal task outcome to the original tool invocation.
// The caller checkpoint must be loaded before resuming its agent branch.
// P2 中 TaskDelivery 是持久续接意图；消费结果后的检查点与 AppliedAt 必须经
// store.TaskTransactions.Apply 原子提交。外部模型调用不在数据库事务内，适配器
// 必须先保存已接纳结果的检查点再推进后续执行，不能声称外部调用恰好一次。
type Continuation struct {
	TaskID string
	Call   domain.ToolCall
	Status domain.TaskStatus
	Result *domain.ToolResult
}

// Update 是暂态运行输出，应用分配持久序号；Kind 只允许消息/工具事件。
// started/completed/failed/cancelled 由应用事务决定，运行时不得自行宣布终态。
type Update struct {
	// AppliedTaskID 仅用于 Resume 的接纳输出。应用必须将本次检查点、工具结果消息
	// 与对应 TaskDelivery 一起提交；回调成功前运行时不得调用模型或执行后续工具。
	AppliedTaskID string
	Kind          domain.EventKind
	Data          []byte
	// Message 非 nil 时表示本次完整 assistant 消息，与事件同事务持久化。
	// 流式增量仅放 Data，不能把每个增量都追加为模型历史；领域身份由应用生成。
	Message []domain.Part
	// MessageRole 仅允许 assistant/tool；空值兼容早期 assistant 输出。
	MessageRole domain.Role
	// PromptTokens 仅携带 LLM 报告的输入用量，与完整 assistant 消息一起持久化。
	PromptTokens int32
	// Tasks 交接已经启动的任务，必须同时携带暂停检查点；应用使用 TrackTask 事务
	// 保存关联与句柄。这里只登记，不负责观察、续接或再次启动工具。
	Tasks []domain.Task
	// Checkpoint 与本次输出同事务保存；适配器仅在可恢复边界提供。
	// Version 是旧持久版本，应用不得在输出落库后另行写检查点。
	Checkpoint *domain.Checkpoint
}

// Emit 提供同步持久化背压；返回错误必须停止输出。运行时须响应执行 context，
// Execute/Resume 返回前须等待自己启动的输出工作结束，不能保留回调后台继续生成。
type Emit func(context.Context, Update) error

type Runtime interface {
	Execute(context.Context, Request, Emit) error
	Resume(context.Context, Request, Continuation, Emit) error
}
