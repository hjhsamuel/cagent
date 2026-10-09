// Package contextengine 负责模型输入组装及基于 LLM 用量的摘要策略，不访问数据库。
// Builder 只做本地准备，CompressingBuilder 通过受限 Summarizer 请求派生摘要。
package contextengine

import (
	"context"
	"github.com/hjhsamuel/cagent/internal/domain"
)

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

// Prepared 所有可变数据均独立于输入。
type Prepared struct {
	Messages []domain.Message
	Snapshot *domain.ContextSnapshot
	// NewSnapshot 是待 CAS 保存的候选；Version 仍为读到的旧版本，不能直接当作已持久化。
	NewSnapshot *domain.ContextSnapshot
}

// Engine 将可信配置和完整历史转换为独立模型输入。
// 错误时不能返回可继续发送的部分结果；实现不得改变或删除原始持久历史。
type Engine interface {
	Prepare(context.Context, Input) (Prepared, error)
}
