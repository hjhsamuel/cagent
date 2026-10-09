package mongodb

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hjhsamuel/cagent/internal/apperrors"
	"github.com/hjhsamuel/cagent/internal/domain"
	"github.com/hjhsamuel/cagent/internal/store"
)

func maintenancePolicy() store.TaskMaintenancePolicy {
	return store.TaskMaintenancePolicy{Poll: time.Second, Backoff: time.Second, MaxBackoff: 4 * time.Second, CancelGrace: 40 * time.Millisecond, DetachedGrace: time.Hour, OutageGrace: 40 * time.Millisecond, InteractionGrace: time.Hour}
}
func TestMaintenanceContactBackoffQuarantineAndScopedResume(t *testing.T) {
	db, cfg := testDatabase(t)
	ctx := context.Background()
	_, g := startFixture(t, db)
	cp := domain.Checkpoint{Scope: testScope, RunID: "run", Caller: domain.AgentExecution{AgentID: "agent", InvocationID: "branch"}, Format: "test-v1"}
	tracked, err := db.TrackTask(ctx, trackRequest(g, cp, "task"))
	check(t, err)
	task := tracked.Task
	p := maintenancePolicy()
	p.OutageGrace = time.Hour // Backoff assertions must not race the exit deadline.
	time.Sleep(5 * time.Millisecond)
	heartbeat, err := db.RecordTaskMaintenance(ctx, freshGuard(t, db, g.Lease), task.ID, task.Version, p, true, true, false, "")
	check(t, err)
	if !heartbeat.Task.LastContactAt.After(task.LastContactAt) || !heartbeat.Task.LastObservedAt.Equal(task.LastObservedAt) || heartbeat.Task.Status != task.Status {
		t.Fatal("contact rewrote provider state")
	}
	first, err := db.RecordTaskMaintenance(ctx, freshGuard(t, db, g.Lease), task.ID, heartbeat.Task.Version, p, true, false, true, "unsupported")
	check(t, err)
	second, err := db.RecordTaskMaintenance(ctx, freshGuard(t, db, g.Lease), task.ID, first.Task.Version, p, true, false, false, "")
	check(t, err)
	if second.Task.ConsecutiveErrors != 2 || second.Task.CancelAttempts != 1 || second.Task.CancelError != "unsupported" || second.Task.NextObservationAt.Sub(first.Task.NextObservationAt) < 900*time.Millisecond {
		t.Fatal("backoff/cancel diagnostics lost")
	}
	p.OutageGrace = time.Millisecond
	quarantined, err := db.RecordTaskMaintenance(ctx, freshGuard(t, db, g.Lease), task.ID, second.Task.Version, p, false, false, false, "")
	check(t, err)
	if quarantined.Task.Maintenance != domain.MaintenanceQuarantined || quarantined.Task.MaintenanceReason != "observer_unavailable" || quarantined.Task.Status != task.Status || quarantined.Task.Handle != task.Handle || quarantined.Task.Result != nil {
		t.Fatal("quarantine forged remote outcome")
	}
	run, err := db.GetRun(ctx, testScope, "run")
	check(t, err)
	if run.Status != domain.RunFailed {
		t.Fatal("active run stayed stuck")
	}
	session, err := db.GetSession(ctx, testScope, "session")
	check(t, err)
	if session.ActiveRunID != "" {
		t.Fatal("quarantine did not release session")
	}
	page, err := db.ListUnsettledTasks(ctx, testScope, "run", store.KeyPage{Limit: 10})
	check(t, err)
	if len(page.Items) != 0 {
		t.Fatal("quarantine still scheduled")
	}
	check(t, db.ReleaseLease(ctx, g.Lease))
	admin, err := OpenRecovery(ctx, cfg)
	check(t, err)
	defer admin.Close(ctx)
	candidates, err := admin.Scan(ctx, nil, 10)
	check(t, err)
	if len(candidates.Items) != 0 {
		t.Fatal("quarantined run remains a candidate")
	}
	_, err = db.ResumeTaskMaintenance(ctx, domain.Scope{TenantID: testScope.TenantID, UserID: "other"}, task.ID)
	if !errors.Is(err, apperrors.ErrNotFound) {
		t.Fatal("cross-user resume", err)
	}
	resumed, err := db.ResumeTaskMaintenance(ctx, testScope, task.ID)
	check(t, err)
	if resumed.Handle != task.Handle || resumed.Maintenance != domain.MaintenanceActive || !resumed.LastContactAt.Equal(quarantined.Task.LastContactAt) {
		t.Fatal("resume changed handle or invented a contact")
	}
	run, err = db.GetRun(ctx, testScope, "run")
	check(t, err)
	if run.Status != domain.RunFailed {
		t.Fatal("resume reopened terminal run")
	}
	candidates, err = admin.Scan(ctx, nil, 10)
	check(t, err)
	if len(candidates.Items) != 1 {
		t.Fatal("resumed handle is not recoverable")
	}
}

func TestMaintenanceWakeAndCancellationPreemptLongBackoff(t *testing.T) {
	db, _ := testDatabase(t)
	ctx := context.Background()
	_, g := startFixture(t, db)
	cp := domain.Checkpoint{Scope: testScope, RunID: "run", Caller: domain.AgentExecution{AgentID: "agent", InvocationID: "branch"}, Format: "test-v1"}
	out, err := db.TrackTask(ctx, trackRequest(g, cp, "task"))
	check(t, err)
	p := maintenancePolicy()
	p.Backoff, p.MaxBackoff, p.OutageGrace = time.Hour, time.Hour, 30*time.Minute
	out, err = db.RecordTaskMaintenance(ctx, freshGuard(t, db, g.Lease), out.Task.ID, out.Task.Version, p, true, false, false, "")
	check(t, err)
	if out.Task.NextObservationAt.Sub(out.Task.LastContactAt) < 59*time.Minute {
		t.Fatal("provider retry was not backed off")
	}
	delay, err := db.MaintenanceDelay(ctx, testScope, "run", time.Hour)
	check(t, err)
	if delay <= 0 || delay > p.OutageGrace {
		t.Fatal("backoff delayed the local exit check", delay)
	}
	cancelled, err := db.CancelTask(ctx, testScope, out.Task.ID)
	check(t, err)
	if !cancelled.NextObservationAt.Before(out.Task.NextObservationAt) || !cancelled.NextObservationAt.Equal(*cancelled.CancelRequestedAt) {
		t.Fatal("new cancellation still waited for the old backoff")
	}
}
func TestCancelledTaskDeadlineRetainsIntentAndProviderStatus(t *testing.T) {
	db, _ := testDatabase(t)
	ctx := context.Background()
	_, g := startFixture(t, db)
	cp := domain.Checkpoint{Scope: testScope, RunID: "run", Caller: domain.AgentExecution{AgentID: "agent", InvocationID: "branch"}, Format: "test-v1"}
	_, err := db.TrackTask(ctx, trackRequest(g, cp, "task"))
	check(t, err)
	task, err := db.CancelTask(ctx, testScope, "task")
	check(t, err)
	time.Sleep(50 * time.Millisecond)
	out, err := db.RecordTaskMaintenance(ctx, freshGuard(t, db, g.Lease), task.ID, task.Version, maintenancePolicy(), false, false, false, "")
	check(t, err)
	if out.Task.MaintenanceReason != "cancel_deadline" || out.Task.Status == domain.TaskCancelled || out.Task.CancelRequestedAt == nil {
		t.Fatal("local timeout became provider cancellation")
	}
	resumed, err := db.ResumeTaskMaintenance(ctx, testScope, "task")
	check(t, err)
	if resumed.CancelRequestedAt == nil || !resumed.CancelRequestedAt.Equal(*task.CancelRequestedAt) {
		t.Fatal("resume erased cancel intent")
	}
	if reason := maintenancePolicy().Reason(resumed, domain.Run{Status: domain.RunFailed, TerminatedAt: time.Now().Add(-time.Hour)}, time.Now()); reason != "" {
		t.Fatal("resume has no fresh observation grace", reason)
	}
}
