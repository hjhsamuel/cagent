package store

import (
	"math"
	"strings"
	"time"

	"github.com/hjhsamuel/cagent/internal/apperrors"
	"github.com/hjhsamuel/cagent/internal/domain"
)

func invalid(field, message string) error {
	return apperrors.New(apperrors.ErrInvalidArgument, field, message)
}
func conflict(field string) error {
	return apperrors.New(apperrors.ErrConflict, field, "stored state does not match write precondition")
}
func nonblank(field, value string) error {
	if strings.TrimSpace(value) == "" {
		return invalid(field, "must not be blank")
	}
	return nil
}

// CheckVersion 比较已读取的版本，供适配器/契约测试复用；不能代替数据库原子 CAS。
// 0 表示不存在，仅创建使用；已存在文档从 1 开始。溢出不回绕，要求人工处理。
func CheckVersion(expected, actual int64) error {
	if expected < 0 || actual < 0 {
		return invalid("version", "must not be negative")
	}
	if expected != actual || actual == math.MaxInt64 {
		return conflict("version")
	}
	return nil
}

// Lease 是某个 Run 的写入凭证，不是工具执行领取。每次 Acquire（包括同 owner
// 重取）都产生更大的 Fence；续期保持 Fence。Owner 是每次进程启动唯一的实例 ID。
// Fence 计数持久保留，Release/取消不能删除计数或重置为零，以防旧持有者复活。
type Lease struct {
	Scope     domain.Scope
	RunID     string
	Owner     string
	Fence     int64
	ExpiresAt time.Time
}

// Validate 验证凭证结构；是否仍有效只能以事务中的持久化租约和数据库时钟判断。
func (l Lease) Validate() error {
	if err := l.Scope.Validate(); err != nil {
		return err
	}
	if err := nonblank("lease.run_id", l.RunID); err != nil {
		return err
	}
	if err := nonblank("lease.owner", l.Owner); err != nil {
		return err
	}
	if l.Fence <= 0 || l.ExpiresAt.IsZero() {
		return invalid("lease", "fence and expiry are required")
	}
	return nil
}

// CheckLease 检查当前存储租约是否仍授权 presented 写入。相等到期时间已经失效；
// 本地时钟不能传作生产数据库判定时间。续期后旧副本仅在其原有效期内可用。
// 所有业务写入须在同一数据库事务内读写租约行（触发写冲突），不能先查后写。
func CheckLease(presented, current Lease, databaseNow time.Time) error {
	if err := presented.Validate(); err != nil {
		return err
	}
	if err := current.Validate(); err != nil {
		// Release/Cancel 会清空持有者与有效期；对旧凭证这是失去所有权的冲突，
		// 不是调用方参数无效。不存在有效持久租约同样不能授权任何新写入。
		return conflict("lease")
	}
	if databaseNow.IsZero() {
		return invalid("lease.time", "database time is required")
	}
	if presented.Scope != current.Scope || presented.RunID != current.RunID ||
		presented.Owner != current.Owner || presented.Fence != current.Fence ||
		!databaseNow.Before(current.ExpiresAt) || !databaseNow.Before(presented.ExpiresAt) {
		return conflict("lease")
	}
	return nil
}

// WriteGuard 是所有运行写入的共同前置条件；取消是唯一由可信用户意图直接提交的例外。
// RunVersion 来自已读取 Run；终态后的任务观察允许使用维护租约，但不能写模型输出。
type WriteGuard struct {
	Lease      Lease
	RunVersion int64
}

func (g WriteGuard) Validate() error {
	if err := g.Lease.Validate(); err != nil {
		return err
	}
	if g.RunVersion <= 0 {
		return invalid("run.version", "persisted version is required")
	}
	return nil
}

// SequencePage 以已消费序号排他分页，序号从 1 开始；0 表示从头。
// Limit 必须为 1..1000，不允许无界查询；Scope/父资源 ID 由方法的其他参数约束。
type SequencePage struct {
	After int64
	Limit int
}

func (p SequencePage) Validate() error {
	if p.After < 0 {
		return invalid("page.after", "must not be negative")
	}
	return pageLimit(p.Limit)
}

// KeyPage 使用不透明 ID 的二进制升序、排他游标；空 After 表示第一页。
// 不承诺跨页快照：恢复循环到末尾后须从头重扫，以发现游标前新增/重新变为活动的项。
type KeyPage struct {
	After string
	Limit int
}

func (p KeyPage) Validate() error { return pageLimit(p.Limit) }
func pageLimit(limit int) error {
	if limit < 1 || limit > 1000 {
		return invalid("page.limit", "must be between 1 and 1000")
	}
	return nil
}

// ErrCursorExpired 可用 errors.Is 判断，与版本冲突区分；传输层后续映射重置游标响应。
const ErrCursorExpired apperrors.Kind = "cursor_expired"

// CheckEventCursor 检查保留水位：prunedThrough 是已整体清理的最大连续前缀序号。
// after==prunedThrough 可重放全部剩余事件；更旧（含首次 after=0）明确报过期，
// 不静默跳过丢失事件。after>last 表示未来游标，报参数无效。即使全被清理仍保存水位。
func CheckEventCursor(after, prunedThrough, last int64) error {
	if after < 0 || prunedThrough < 0 || last < prunedThrough || after > last {
		return invalid("event.cursor", "invalid cursor or retention bounds")
	}
	if after < prunedThrough {
		return apperrors.New(ErrCursorExpired, "event.cursor", "requested events are no longer retained")
	}
	return nil
}
