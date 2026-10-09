package store

import (
	"testing"
	"time"

	"github.com/hjhsamuel/cagent/internal/domain"
)

func TestMaintenanceDeadlineUsesEarliestActiveGrace(t *testing.T) {
	now := time.Now()
	p := TaskMaintenancePolicy{CancelGrace: time.Minute, DetachedGrace: time.Hour, OutageGrace: 30 * time.Second, InteractionGrace: 2 * time.Hour}
	task := domain.Task{Status: domain.TaskRunning, CancelRequestedAt: &now, LastContactAt: now, ConsecutiveErrors: 1, InteractionSince: now}
	run := domain.Run{Status: domain.RunCancelled, TerminatedAt: now}
	if got := p.Deadline(task, run); !got.Equal(now.Add(p.OutageGrace)) {
		t.Fatal("backoff can hide earlier exit deadline", got)
	}
	if reason := p.Reason(task, run, now.Add(p.OutageGrace)); reason != "observer_unavailable" {
		t.Fatal("deadline did not terminate maintenance", reason)
	}
	task.ConsecutiveErrors = 0
	if got := p.Deadline(task, run); !got.Equal(now.Add(p.CancelGrace)) {
		t.Fatal("successful contact erased cancellation deadline", got)
	}
	task.MaintenanceResumedAt = now.Add(10 * time.Minute)
	if got := p.Deadline(task, run); !got.Equal(task.MaintenanceResumedAt.Add(p.CancelGrace)) {
		t.Fatal("resumed observation did not get local grace", got)
	}
	task.Maintenance = domain.MaintenanceQuarantined
	if !p.Deadline(task, run).IsZero() {
		t.Fatal("quarantined handle is still scheduled")
	}
}
