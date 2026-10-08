package app

import (
	"context"
	"github.com/hjhsamuel/cagent/internal/adapter/mongodb"
	"github.com/hjhsamuel/cagent/internal/apperrors"
	"github.com/hjhsamuel/cagent/internal/domain"
	"github.com/hjhsamuel/cagent/internal/store"
	"time"
)

// EventStream 发布携带租约、版本及稳定 OperationID，不能绕过运行事务。
// Follow 回调按序同步调用，须响应连接取消/写超时；返回错误即停止订阅。
type EventStream interface {
	Publish(context.Context, store.CommitRunRequest) (store.CommitResult, error)
	Follow(context.Context, domain.Scope, string, int64, func(domain.Event) error) error
}

// DurableEvents 用同一持久游标完成追赶和跟随，没有本地通知切换窗口。
// 每个订阅最多持有 pageSize 条事件，不启动生产者队列；慢消费者通过同步背压
// 停止后续读取，其他订阅和写入不受影响。单条大小限制由传输/运行适配层负责。
type DurableEvents struct {
	db       *mongodb.Database
	interval time.Duration
	pageSize int
}

// CheckCursor 在 HTTP 提交 SSE 响应头之前检查访问权限与游标水位。
// Follow 仍重复验证每次读取，检查后发生清理时断流，由客户端重连取得 410。
func (e *DurableEvents) CheckCursor(ctx context.Context, scope domain.Scope, runID string, after int64) error {
	_, err := e.db.ListEvents(ctx, scope, runID, store.SequencePage{After: after, Limit: 1})
	return err
}

// NewEventStream 使用有界轮询；部署只需 P3 要求的事务副本集或 mongos。
func NewEventStream(db *mongodb.Database, interval time.Duration, pageSize int) (*DurableEvents, error) {
	if db == nil || interval <= 0 {
		return nil, invalid("events.options")
	}
	if err := (store.SequencePage{Limit: pageSize}).Validate(); err != nil {
		return nil, err
	}
	return &DurableEvents{db: db, interval: interval, pageSize: pageSize}, nil
}

// Publish 成功后事件才可见；失败返回空结果。结果未知时必须使用原操作 ID/内容重试。
func (e *DurableEvents) Publish(ctx context.Context, req store.CommitRunRequest) (store.CommitResult, error) {
	return e.db.CommitRun(ctx, req)
}

// Follow 的水位和终态来自同一快照，避免终态检查竞态；已消费终止事件也正常返回。
// 过期/未来游标原样报告。HTTP 层须设置写超时：Go 无法强制中断不合作的回调，
// 本方法不另起可能泄漏的回调 goroutine。取消订阅不会改变 Run 状态。
func (e *DurableEvents) Follow(ctx context.Context, scope domain.Scope, runID string, after int64, consume func(domain.Event) error) error {
	if consume == nil {
		return invalid("events.callback")
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		page, err := e.db.ListEvents(ctx, scope, runID, store.SequencePage{After: after, Limit: e.pageSize})
		if err != nil {
			return err
		}
		for _, event := range page.Items {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := consume(event); err != nil {
				return err
			}
			after = event.Sequence
		}
		if page.Terminal && after == page.LastSequence {
			return nil
		}
		if page.HasMore {
			continue
		}
		timer := time.NewTimer(e.interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func invalid(field string) error {
	return apperrors.New(apperrors.ErrInvalidArgument, field, "invalid application argument")
}
