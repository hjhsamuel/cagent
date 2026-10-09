package store

import (
	"time"

	"github.com/hjhsamuel/cagent/internal/domain"
)

type TaskMaintenancePolicy struct{ Poll, Backoff, MaxBackoff, CancelGrace, DetachedGrace, OutageGrace, InteractionGrace time.Duration }

type maintenanceDeadline struct {
	at     time.Time
	reason string
}

func (p TaskMaintenancePolicy) deadlines(t domain.Task, r domain.Run) []maintenanceDeadline {
	if t.Status.IsTerminal() || t.Maintenance == domain.MaintenanceQuarantined {
		return nil
	}
	var deadlines []maintenanceDeadline
	if t.CancelRequestedAt != nil {
		start := *t.CancelRequestedAt
		if t.MaintenanceResumedAt.After(start) {
			start = t.MaintenanceResumedAt
		}
		deadlines = append(deadlines, maintenanceDeadline{start.Add(p.CancelGrace), "cancel_deadline"})
	}
	ended := r.TerminatedAt
	if ended.IsZero() {
		ended = r.UpdatedAt
	}
	if t.MaintenanceResumedAt.After(ended) {
		ended = t.MaintenanceResumedAt
	}
	if r.Status.IsTerminal() {
		deadlines = append(deadlines, maintenanceDeadline{ended.Add(p.DetachedGrace), "run_ended"})
	}
	contact := t.LastContactAt
	if contact.IsZero() {
		contact = t.LastObservedAt
	}
	if contact.IsZero() {
		contact = t.CreatedAt
	}
	if t.MaintenanceResumedAt.After(contact) {
		contact = t.MaintenanceResumedAt
	}
	if t.ConsecutiveErrors > 0 {
		deadlines = append(deadlines, maintenanceDeadline{contact.Add(p.OutageGrace), "observer_unavailable"})
	}
	if !t.InteractionSince.IsZero() {
		deadlines = append(deadlines, maintenanceDeadline{t.InteractionSince.Add(p.InteractionGrace), "interaction_timeout"})
	}
	return deadlines
}

func (p TaskMaintenancePolicy) Reason(t domain.Task, r domain.Run, now time.Time) string {
	for _, deadline := range p.deadlines(t, r) {
		if !now.Before(deadline.at) {
			return deadline.reason
		}
	}
	return ""
}

// Deadline bounds both the next maintenance wake-up and an in-flight observation.
// NextObservationAt remains the provider retry time; an earlier deadline only
// wakes local maintenance to check whether it must stop.
func (p TaskMaintenancePolicy) Deadline(t domain.Task, r domain.Run) time.Time {
	var earliest time.Time
	for _, deadline := range p.deadlines(t, r) {
		if earliest.IsZero() || deadline.at.Before(earliest) {
			earliest = deadline.at
		}
	}
	return earliest
}
func (p TaskMaintenancePolicy) Validate() error {
	for _, v := range []time.Duration{p.Poll, p.Backoff, p.MaxBackoff, p.CancelGrace, p.DetachedGrace, p.OutageGrace, p.InteractionGrace} {
		if v <= 0 {
			return invalid("task.maintenance", "positive policy durations are required")
		}
	}
	return nil
}
