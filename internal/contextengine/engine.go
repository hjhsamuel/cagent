// Package contextengine 负责模型输入组装、预算校验及压缩策略，不访问数据库。
// Builder 只做本地准备，CompressingBuilder 通过受限 Summarizer 请求派生摘要。
package contextengine

import (
	"context"
	"github.com/hjhsamuel/cagent/internal/domain"
)

// Budget 将模型窗口分为输入及三类预留。计数包含消息封装和本模块添加的说明，
// 工具定义等未放入 Messages 的开销由 ToolTokens 预留，不能重复计算。
type Budget struct{ WindowTokens, OutputTokens, ToolTokens, SafetyTokens int }

// Input normally starts at sequence 1. Only a scoped repository window and a
// validated compatible snapshot may authorize gaps in the covered prefix.
// RunID 指当前运行，必须在历史中存在 user 输入，且之后不能出现其他运行。
// System 是可信管理配置，独立于持久历史；PolicyVersion 用于拒绝不兼容摘要。
type Input struct {
	Session       domain.Session
	RunID         string
	System        []domain.Message
	History       []domain.Message
	Snapshot      *domain.ContextSnapshot
	Budget        Budget
	PolicyVersion string
	// PreserveUsers 在压缩策略中保留全部用户原文，不依赖模型判断哪些要求重要。
	PreserveUsers    bool
	ArchiveCompleted bool
	// VerifiedPrefix is provided only by the scoped repository window reader.
	// HistoryThrough is the session's message watermark from that same snapshot.
	VerifiedPrefix int64
	HistoryThrough int64
	// RunStates are loaded in the same scoped session by the trusted preparer.
	// Missing entries cannot authorize closing unresolved historical calls.
	RunStates map[string]domain.RunStatus
}

// Prepared 所有可变数据均独立于输入。EstimatedTokens 来自注入计数器对最终
// Messages 的整体计数，不信任快照的 TokenEstimate，不假设逐消息计数可相加。
type Prepared struct {
	Messages        []domain.Message
	EstimatedTokens int
	Snapshot        *domain.ContextSnapshot
	// NewSnapshot 是待 CAS 保存的候选；Version 仍为读到的旧版本，不能直接当作已持久化。
	NewSnapshot *domain.ContextSnapshot
}

// Engine 将可信配置和完整历史转换为通过预算检查的独立模型输入。
// 错误时不能返回可继续发送的部分结果；实现不得改变或删除原始持久历史。
type Engine interface {
	Prepare(context.Context, Input) (Prepared, error)
}

// TokenCounter 由 P6 模型适配器提供，必须覆盖角色/消息封装、工具结构及多模态开销。
// 实现应只读、并发安全并响应 context；无法计数应返回错误，不能默认为零。
// Prepare 至少包含当前用户消息，其计数必须为正，零或负计数视为适配错误。
// P5 不提供声称适用于所有模型的字符数估算器。
type TokenCounter interface {
	Count(context.Context, []domain.Message) (int, error)
}
