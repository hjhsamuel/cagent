package domain

import "time"

// ContextSnapshot 是可重建的派生摘要，不能替代或删除持久化原始消息。
// 覆盖水位只用于选择可省略的普通旧消息；用户要求、工具对和当前 Run 仍由策略保护。
type ContextSnapshot struct {
	Scope     Scope
	ID        string
	SessionID string
	// ThroughSequence 是摘要覆盖的连续前缀上界，保存时不得超过历史或倒退。
	ThroughSequence int64
	Summary         string
	// TokenEstimate 仅供诊断，发送模型前必须重新整体计数，不能以此绕过预算。
	TokenEstimate int
	// PolicyVersion 标识可信压缩策略；升级时从原始历史重建，不递归摘要旧摘要。
	PolicyVersion string
	// Version 是数据库 CAS 版本；待保存候选携带旧版本，成功提交后加一。
	Version   int64
	CreatedAt time.Time
}
