package app

import (
	"context"
	"errors"
	"time"

	"github.com/hjhsamuel/cagent/internal/apperrors"
	"github.com/hjhsamuel/cagent/internal/observability"
	"github.com/hjhsamuel/cagent/internal/store"
	"github.com/sirupsen/logrus"
)

// Waiting has no per-Run goroutine or generation slot. The bounded local map is
// an acceleration hint; durable indexed recovery remains authoritative after restart.
func (a *Application) runDue() {
	defer a.wg.Done()
	timer := time.NewTicker(25 * time.Millisecond)
	defer timer.Stop()
	for {
		select {
		case <-a.ctx.Done():
			return
		case now := <-timer.C:
			a.mu.Lock()
			var keys []runKey
			for key, due := range a.due {
				if !due.After(now) {
					keys = append(keys, key)
				}
			}
			a.mu.Unlock()
			for _, key := range keys {
				if a.ctx.Err() != nil {
					return
				}
				a.schedule(key.scope, key.id)
			}
		}
	}
}
func (a *Application) runScheduled(ctx context.Context, cancel context.CancelFunc, key runKey, release, workerRelease func()) {
	defer a.wg.Done()
	defer workerRelease()
	defer cancel()
	ctx, finish := observability.Default.Start(ctx, "run")
	var err error
	defer func() { finish(err) }()
	retry := false
	delay := a.opts.Tasks.PollInterval
	defer func() {
		if release != nil {
			release()
		}
		a.mu.Lock()
		delete(a.active, key)
		if retry && !a.closed && len(a.due) < 4096 {
			a.due[key] = time.Now().Add(delay)
		}
		a.mu.Unlock()
	}()
	waiting, e := a.maintainTasks(ctx, key.scope, key.id)
	err = e
	if err == nil && !waiting {
		if release == nil {
			release, err = a.runs.Try(ctx)
		}
		if err == nil {
			err = a.execute(ctx, key.scope, key.id)
		}
	}
	// Generation capacity is returned before scheduling any future observation.
	if release != nil {
		release()
		release = nil
	}
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, apperrors.ErrConflict) {
		logrus.WithFields(logrus.Fields{"tenant_id": key.scope.TenantID, "user_id": key.scope.UserID, "run_id": key.id}).WithError(err).Warn("运行执行或任务观察暂时中断")
	}
	if a.ctx.Err() != nil {
		return
	}
	// Cancellation stops this attempt; the process context maintains existing handles.
	run, e := a.db.GetRun(a.ctx, key.scope, key.id)
	if e != nil {
		retry = true
		delay = a.opts.Tasks.ReconnectBackoff
		return
	}
	page, e := a.db.ListUnsettledTasks(a.ctx, key.scope, key.id, store.KeyPage{Limit: 1})
	if e != nil {
		retry = true
		delay = a.opts.Tasks.ReconnectBackoff
		return
	}
	retry = !run.Status.IsTerminal() || len(page.Items) > 0
	if !retry {
		return
	}
	if err != nil {
		delay = a.opts.Tasks.ReconnectBackoff
	}
	if waiting && err == nil {
		if next, e := a.db.MaintenanceDelay(a.ctx, key.scope, key.id, delay); e == nil {
			delay = next
		}
	}
	_ = a.db.DeferRecovery(a.ctx, key.scope, key.id, run.Version, delay)
}
