// Package store 保存存储操作的请求、结果和纯前置条件校验，不定义仓储接口。
// 调用方直接使用 mongodb.Database 的具体方法；数据库读写、事务及索引在适配器中实现。
// 本包不依赖 MongoDB 驱动，领域层也不引用 BSON 或数据库类型。
package store

import (
	"github.com/hjhsamuel/cagent/internal/apperrors"
	"github.com/hjhsamuel/cagent/internal/domain"
)

const (
	// ErrNotFound 保留既有存储契约的名称，与公共类别完全相同。
	// 新调用方应使用 errors.Is(err, apperrors.ErrNotFound)，不要比较错误文本。
	ErrNotFound = apperrors.ErrNotFound
	// ErrConflict 表示版本等并发约束冲突，兼容已有 errors.Is 判断。
	// 适配器应以 apperrors.Wrap 保留驱动原因，并提供不含连接信息的安全说明。
	ErrConflict = apperrors.ErrConflict
)

// MessagePage 按 Sequence 升序返回，NextAfter 为最后返回序号；空页保持请求游标。
// HasMore 仅表示当前读取时还有数据，不表示未来不会追加。消息不做 TTL 删除。
type MessagePage struct {
	Items     []domain.Message
	NextAfter int64
	HasMore   bool
}

// EventPage 在同一一致读中返回事件及保留水位。LastSequence 是当前已提交最大序号，
// PrunedThrough 是已清理的连续前缀；即使 Items 为空也必须返回持久水位。
// NextAfter 不越过实际返回事件，订阅方据此追赶，不能把 LastSequence 当已消费序号。
type EventPage struct {
	// Terminal 与水位来自同一快照；订阅已消费终止事件时无需再等待新事件。
	Terminal      bool
	Items         []domain.Event
	NextAfter     int64
	HasMore       bool
	LastSequence  int64
	PrunedThrough int64
}
