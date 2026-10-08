package app

import (
	"context"
	"github.com/hjhsamuel/cagent/internal/adapter/mongodb"
	"github.com/hjhsamuel/cagent/internal/observability"
	"github.com/hjhsamuel/cagent/internal/store"
	"github.com/sirupsen/logrus"
	"time"
)

// StartRecovery 接受独立管理连接，不把跨作用域 Scan 暴露给 HTTP 或普通仓储。
// 扫描只是发现候选，调度仍须 AcquireLease 后重读。末页归零重扫，覆盖游标前新增
// 或租约刚过期的运行；扫描失败保留游标并退避。生命周期与 Application.Close 一致。
func (a *Application) StartRecovery(recovery *mongodb.Recovery, interval time.Duration) error {
	if recovery == nil || interval <= 0 {
		return invalid("recovery.options")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return context.Canceled
	}
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		var after *store.RecoveryPosition
		for a.ctx.Err() == nil {
			scanCtx, finish := observability.Default.Start(a.ctx, "recovery")
			page, e := recovery.Scan(scanCtx, after, 64)
			finish(e)
			if e == nil {
				for _, candidate := range page.Items {
					a.schedule(candidate.Scope, candidate.RunID)
				}
				if page.HasMore {
					after = page.Next
				} else {
					after = nil
				}
			} else if a.ctx.Err() == nil {
				logrus.WithError(e).Warn("运行恢复扫描暂时失败")
			}
			if waitTask(a.ctx, interval) != nil {
				return
			}
		}
	}()
	return nil
}
