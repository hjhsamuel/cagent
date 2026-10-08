package mongodb

import (
	"context"
	"github.com/hjhsamuel/cagent/internal/observability"
	"go.mongodb.org/mongo-driver/v2/event"
	"sync"
)

// commandMonitor 只计数命令生命周期，绝不读取 Command/Reply/Failure 的文本。
// 驱动的连接池和操作超时约束在途表；每个成功或失败回调删除对应条目。
// 每个客户端独立持有表，连接 ID 与请求 ID 联合匹配，避免并发命令错配。
func commandMonitor() *event.CommandMonitor {
	type key struct {
		connection string
		request    int64
	}
	var pending sync.Map
	finish := func(e event.CommandFinishedEvent, err error) {
		if f, ok := pending.LoadAndDelete(key{e.ConnectionID, e.RequestID}); ok {
			f.(func(error))(err)
		}
	}
	return &event.CommandMonitor{
		Started: func(ctx context.Context, e *event.CommandStartedEvent) {
			_, end := observability.Default.Start(ctx, "storage")
			pending.Store(key{e.ConnectionID, e.RequestID}, end)
		},
		Succeeded: func(_ context.Context, e *event.CommandSucceededEvent) { finish(e.CommandFinishedEvent, nil) },
		Failed:    func(_ context.Context, e *event.CommandFailedEvent) { finish(e.CommandFinishedEvent, e.Failure) },
	}
}
