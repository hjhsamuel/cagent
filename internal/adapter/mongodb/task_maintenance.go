package mongodb

import (
	"context"
	"time"

	"github.com/hjhsamuel/cagent/internal/domain"
	"github.com/hjhsamuel/cagent/internal/store"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// RecordTaskMaintenance persists successful contact even for an unchanged snapshot.
// Local deadlines are checked with database time, and never forge a remote result.
func (b *Database) RecordTaskMaintenance(ctx context.Context, g store.WriteGuard, id string, expected int64, p store.TaskMaintenancePolicy, observed, successful, cancelAttempted bool, cancelError string) (store.TaskCommitResult, error) {
	if err := validateKey(g.Lease.Scope, id); err != nil {
		return store.TaskCommitResult{}, err
	}
	if err := p.Validate(); err != nil {
		return store.TaskCommitResult{}, err
	}
	if cancelError != "" && cancelError != "unsupported" && cancelError != "interrupted" {
		return store.TaskCommitResult{}, invalid("task.cancel_error")
	}
	var out store.TaskCommitResult
	err := b.withTransaction(ctx, "task.maintenance_attempt", func(tx context.Context) error {
		run, rd, now, err := b.guard(tx, g)
		if err != nil {
			return err
		}
		var td document
		var t domain.Task
		if err = b.collection(TaskCollection).FindOne(tx, key(run.Scope, id)).Decode(&td); err != nil {
			return err
		}
		if err = b.decode(tx, td, &t); err != nil {
			return err
		}
		if err = t.ValidateForRun(run); err != nil {
			return err
		}
		if err = store.CheckVersion(expected, t.Version); err != nil {
			return err
		}
		out = store.TaskCommitResult{Task: t, RunVersion: run.Version}
		if t.Maintenance == domain.MaintenanceQuarantined || td.Unsettled == 0 {
			return nil
		}
		reason := p.Reason(t, run, now)
		if !observed && reason == "" {
			return nil
		}
		if observed {
			if successful {
				t.LastContactAt = now
				t.ConsecutiveErrors = 0
				t.NextObservationAt = now.Add(p.Poll)
			} else {
				t.ConsecutiveErrors = min(t.ConsecutiveErrors, 29) + 1
				delay := p.Backoff
				for i := 1; i < t.ConsecutiveErrors && delay < p.MaxBackoff; i++ {
					if delay > p.MaxBackoff/2 {
						delay = p.MaxBackoff
					} else {
						delay *= 2
					}
				}
				t.NextObservationAt = now.Add(min(delay, p.MaxBackoff))
			}
			if t.Status == domain.TaskInputRequired || t.Status == domain.TaskAuthRequired {
				if t.InteractionSince.IsZero() {
					t.InteractionSince = now
				}
			} else {
				t.InteractionSince = time.Time{}
			}
			if cancelAttempted {
				t.CancelAttempts = min(t.CancelAttempts, (1<<30)-1) + 1
				t.CancelError = cancelError
			}
			// A successful contact clears an outage but not a cancellation deadline.
			reason = p.Reason(t, run, now)
		}
		updated := rd
		if reason != "" {
			t.Maintenance = domain.MaintenanceQuarantined
			t.MaintenanceReason = reason
			t.NextObservationAt = time.Time{}
			if td.Unsettled != 1 || updated.Unsettled <= 0 {
				return invariant()
			}
			updated.Unsettled--
			td.Unsettled = 0
			if !run.Status.IsTerminal() {
				s, err := b.GetSession(tx, run.Scope, run.SessionID)
				if err != nil {
					return err
				}
				if _, _, err = b.output(tx, run, s.Version, nil, true, now); err != nil {
					return err
				}
				if _, err = b.appendEvents(tx, run, &updated, []domain.Event{{Scope: run.Scope, RunID: run.ID, Kind: domain.EventRunFailed, Data: []byte(`{"reason":"task_maintenance_quarantined","remote_outcome":"unknown","retry_tool":false}`)}}, now, true); err != nil {
					return err
				}
				run.Status = domain.RunFailed
			}
		} else {
			t.Maintenance = domain.MaintenanceActive
		}
		t.Version, err = increment(t.Version)
		if err != nil {
			return err
		}
		t.UpdatedAt = now
		next, err := taskDocument(t)
		if err != nil {
			return err
		}
		next.Unsettled = td.Unsettled
		if deadline := p.Deadline(t, run); !deadline.IsZero() && (next.NextActionAt.IsZero() || deadline.Before(next.NextActionAt)) {
			next.NextActionAt = deadline
		}
		result, err := b.collection(TaskCollection).ReplaceOne(tx, td.versionKey(), next)
		if err != nil {
			return err
		}
		if result.MatchedCount != 1 {
			return conflict("task.version")
		}
		if err = b.saveRun(tx, &run, rd, updated, now); err != nil {
			return err
		}
		out = store.TaskCommitResult{Task: t, RunVersion: run.Version}
		return nil
	})
	if err != nil {
		return store.TaskCommitResult{}, err
	}
	return out, nil
}
func (b *Database) MaintenanceDelay(ctx context.Context, scope domain.Scope, id string, fallback time.Duration) (time.Duration, error) {
	if err := validateKey(scope, id); err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()
	f := scoped(scope)
	f["run_id"] = id
	f["unsettled"] = 1
	var d document
	if err := b.collection(TaskCollection).FindOne(ctx, f, options.FindOne().SetSort(bson.D{{Key: "next_action_at", Value: 1}}).SetProjection(bson.M{"next_action_at": 1})).Decode(&d); err != nil {
		return fallback, safeError(err)
	}
	now, err := b.now(ctx)
	if err != nil {
		return fallback, safeError(err)
	}
	if d.NextActionAt.IsZero() {
		return fallback, nil
	}
	return max(d.NextActionAt.Sub(now), time.Millisecond), nil
}

// ResumeTaskMaintenance is a scoped command for the existing handle; it never
// reopens a terminal Run or re-executes the original tool call.
func (b *Database) ResumeTaskMaintenance(ctx context.Context, scope domain.Scope, id string) (domain.Task, error) {
	if err := validateKey(scope, id); err != nil {
		return domain.Task{}, err
	}
	var task domain.Task
	err := b.withTransaction(ctx, "task.maintenance_resume", func(tx context.Context) error {
		var td, rd document
		var run domain.Run
		if err := b.collection(TaskCollection).FindOne(tx, key(scope, id)).Decode(&td); err != nil {
			return err
		}
		if err := b.decode(tx, td, &task); err != nil {
			return err
		}
		if task.Maintenance != domain.MaintenanceQuarantined {
			return conflict("task.maintenance")
		}
		if err := b.collection(RunCollection).FindOne(tx, key(scope, task.Call.RunID)).Decode(&rd); err != nil {
			return err
		}
		if err := b.decode(tx, rd, &run); err != nil {
			return err
		}
		if err := task.ValidateForRun(run); err != nil {
			return err
		}
		now, err := b.now(tx)
		if err != nil {
			return err
		}
		task.Maintenance = domain.MaintenanceActive
		task.MaintenanceReason = ""
		task.ConsecutiveErrors = 0
		task.MaintenanceResumedAt = now
		task.NextObservationAt = now
		task.InteractionSince = time.Time{}
		task.UpdatedAt = now
		task.Version, err = increment(task.Version)
		if err != nil {
			return err
		}
		next, err := taskDocument(task)
		if err != nil {
			return err
		}
		next.Unsettled = 1
		result, err := b.collection(TaskCollection).ReplaceOne(tx, td.versionKey(), next)
		if err != nil {
			return err
		}
		if result.MatchedCount != 1 {
			return conflict("task.version")
		}
		rd.Unsettled, err = increment(rd.Unsettled)
		if err != nil {
			return err
		}
		return b.saveRun(tx, &run, rd, rd, now)
	})
	if err != nil {
		return domain.Task{}, err
	}
	return task, nil
}
