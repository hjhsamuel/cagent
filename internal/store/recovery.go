package store

import (
	"github.com/hjhsamuel/cagent/internal/domain"
	"time"
)

// RecoveryPosition 使用持久资源身份组成的二进制字典序游标，避免只按 RunID
// 在多租户扫描时遗漏同名运行；不得来自 HTTP 参数或普通仓储的空 Scope。
type RecoveryPosition struct {
	TenantID     string
	UserID       string
	RunID        string
	NextActionAt time.Time
}

// RecoveryCandidate 只给出后续作用域内加载需要的身份，不包含提示词或凭据。
// 扫描不是领取，候选可能已变化；必须 Acquire 成功并重读 Run/任务/检查点后操作。
type RecoveryCandidate struct {
	Scope domain.Scope
	RunID string
}
type RecoveryPage struct {
	Items   []RecoveryCandidate
	Next    *RecoveryPosition
	HasMore bool
}

// ValidateRecoveryPage 拒绝部分游标和无界扫描；nil 才表示第一页。
func ValidateRecoveryPage(after *RecoveryPosition, limit int) error {
	if err := pageLimit(limit); err != nil {
		return err
	}
	if after == nil {
		return nil
	}
	if err := (domain.Scope{TenantID: after.TenantID, UserID: after.UserID}).Validate(); err != nil {
		return err
	}
	return nonblank("recovery.run_id", after.RunID)
}
