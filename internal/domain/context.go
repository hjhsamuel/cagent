package domain

import "time"

// ContextSnapshot 是可重建的派生摘要，不能替代或删除持久化原始消息。
// 覆盖水位只用于选择可省略的普通旧消息；用户要求、工具对和当前 Run 仍由策略保护。
type ContextSnapshot struct {
	// ValidatedThrough permits trusted incremental preparation after the source
	// prefix was checked. Zero denotes a legacy snapshot requiring a full read.
	ValidatedThrough int64
	Archived         bool
	Scope            Scope
	ID               string
	SessionID        string
	// ThroughSequence 是摘要覆盖的连续前缀上界，保存时不得超过历史或倒退。
	ThroughSequence int64
	Summary         string
	// PolicyVersion identifies the trusted policy; incompatible snapshots rebuild
	// from original history, while compatible validated prefixes may be incremental.
	PolicyVersion string
	// Version 是数据库 CAS 版本；待保存候选携带旧版本，成功提交后加一。
	Version   int64
	CreatedAt time.Time
}
