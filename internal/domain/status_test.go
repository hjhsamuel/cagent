package domain

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hjhsamuel/cagent/internal/apperrors"
)

// 明确列出所有允许的边，逐一遍历笛卡尔积，避免只验证几条快乐路径。
func TestRunTransitionMatrix(t *testing.T) {
	states := []RunStatus{RunQueued, RunRunning, RunWaiting, RunCompleted, RunFailed, RunCancelled}
	allowed := map[RunStatus][]RunStatus{
		RunQueued:    {RunQueued, RunRunning, RunFailed, RunCancelled},
		RunRunning:   {RunRunning, RunWaiting, RunCompleted, RunFailed, RunCancelled},
		RunWaiting:   {RunWaiting, RunRunning, RunCompleted, RunFailed, RunCancelled},
		RunCompleted: {RunCompleted}, RunFailed: {RunFailed}, RunCancelled: {RunCancelled},
	}
	for i, from := range states {
		if from.IsTerminal() != (i >= 3) {
			t.Fatalf("terminal: %s", from)
		}
		for _, to := range states {
			t.Run(string(from)+"/"+string(to), func(t *testing.T) {
				want := false
				for _, target := range allowed[from] {
					want = want || to == target
				}
				err := from.ValidateTransition(to)
				if want && err != nil {
					t.Fatal(err)
				}
				if !want {
					assertStateError(t, err, apperrors.ErrConflict, "run.status")
				}
			})
		}
	}
}

func TestTaskTransitionMatrix(t *testing.T) {
	states := []TaskStatus{TaskSubmitted, TaskRunning, TaskInputRequired, TaskAuthRequired, TaskSucceeded, TaskFailed, TaskCancelled, TaskRejected}
	allowed := map[TaskStatus][]TaskStatus{
		TaskSubmitted:     {TaskSubmitted, TaskRunning, TaskInputRequired, TaskAuthRequired, TaskSucceeded, TaskFailed, TaskCancelled, TaskRejected},
		TaskRunning:       {TaskRunning, TaskInputRequired, TaskAuthRequired, TaskSucceeded, TaskFailed, TaskCancelled, TaskRejected},
		TaskInputRequired: {TaskRunning, TaskInputRequired, TaskAuthRequired, TaskSucceeded, TaskFailed, TaskCancelled, TaskRejected},
		TaskAuthRequired:  {TaskRunning, TaskInputRequired, TaskAuthRequired, TaskSucceeded, TaskFailed, TaskCancelled, TaskRejected},
		TaskSucceeded:     {TaskSucceeded}, TaskFailed: {TaskFailed}, TaskCancelled: {TaskCancelled}, TaskRejected: {TaskRejected},
	}
	for i, from := range states {
		if from.IsTerminal() != (i >= 4) || from.IsPaused() != (i == 2 || i == 3) {
			t.Fatalf("classification: %s", from)
		}
		for _, to := range states {
			t.Run(string(from)+"/"+string(to), func(t *testing.T) {
				want := false
				for _, target := range allowed[from] {
					want = want || to == target
				}
				err := from.ValidateTransition(to)
				if want && err != nil {
					t.Fatal(err)
				}
				if !want {
					assertStateError(t, err, apperrors.ErrConflict, "task.status")
				}
			})
		}
	}
}

func TestUnknownStatesNeverBecomeSuccess(t *testing.T) {
	for _, unknown := range []string{"", " ", "RUNNING", " running ", "private-provider-state"} {
		r, task := RunStatus(unknown), TaskStatus(unknown)
		if r.IsTerminal() || task.IsTerminal() || task.IsPaused() {
			t.Fatal("unknown classified as known")
		}
		for _, err := range []error{r.Validate(), r.ValidateTransition(RunCompleted), RunRunning.ValidateTransition(r), r.ValidateTransition(r)} {
			assertStateError(t, err, apperrors.ErrInvalidArgument, "run.status")
		}
		for _, err := range []error{task.Validate(), task.ValidateTransition(TaskSucceeded), TaskRunning.ValidateTransition(task), task.ValidateTransition(task)} {
			assertStateError(t, err, apperrors.ErrInvalidArgument, "task.status")
		}
	}
}

func assertStateError(t *testing.T, err error, kind apperrors.Kind, field string) {
	t.Helper()
	var detail *apperrors.Error
	if !errors.Is(err, kind) || !errors.As(err, &detail) || detail.Field() != field {
		t.Fatalf("wanted %s/%s, got %v", kind, field, err)
	}
	if strings.Contains(err.Error(), "private") {
		t.Fatal("raw input leaked")
	}
}

func stateTask() Task {
	s, _, call, handle := fixtures()
	return Task{Scope: s.Scope, ID: "task", Call: call, Handle: handle, Status: TaskSubmitted, Version: 7}
}

var stateNow = time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)

// 暂停、观察失败、恢复都必须继续使用原句柄和调用身份，且不产生消费标记。
func TestPauseObservationFailureAndRecovery(t *testing.T) {
	task := stateTask()
	initial := task
	for _, status := range []TaskStatus{TaskRunning, TaskInputRequired, TaskAuthRequired, TaskRunning} {
		changed, err := task.ApplyUpdate(TaskUpdate{Status: status, Cursor: string(status)}, stateNow)
		if !changed || err != nil {
			t.Fatalf("update: %v %v", changed, err)
		}
	}
	before := task
	changed, err := task.RecordObservationError("observation timed out", stateNow.Add(time.Second))
	if !changed || err != nil {
		t.Fatal(err)
	}
	expected := before
	expected.ObservationError = "observation timed out"
	expected.UpdatedAt = stateNow.Add(time.Second)
	if !reflect.DeepEqual(task, expected) {
		t.Fatal("observation error changed remote state")
	}
	changed, err = task.RecordObservationError("observation timed out", stateNow.Add(2*time.Second))
	if changed || err != nil || !reflect.DeepEqual(task, expected) {
		t.Fatal("duplicate error not idempotent")
	}
	changed, err = task.ApplyUpdate(TaskUpdate{Status: TaskRunning, Cursor: "running"}, stateNow.Add(3*time.Second))
	if !changed || err != nil || task.ObservationError != "" {
		t.Fatal("failed to clear observation error")
	}
	before = task
	changed, err = task.ApplyUpdate(TaskUpdate{Status: TaskRunning, Cursor: "running"}, stateNow.Add(4*time.Second))
	if changed || err != nil || !reflect.DeepEqual(task, before) {
		t.Fatal("identical snapshot not a no-op")
	}
	if !reflect.DeepEqual(task.Call, initial.Call) || task.Handle != initial.Handle || task.Version != 7 || task.AppliedAt != nil {
		t.Fatal("changed routing or persistence fields")
	}
}

func TestCancellationRacesPreserveActualOutcome(t *testing.T) {
	for _, terminal := range []TaskStatus{TaskSucceeded, TaskFailed, TaskCancelled, TaskRejected} {
		for _, cancelFirst := range []bool{true, false} {
			t.Run(string(terminal)+"/"+map[bool]string{true: "cancel first", false: "completion first"}[cancelFirst], func(t *testing.T) {
				task := stateTask()
				if cancelFirst {
					changed, err := task.RequestCancel(stateNow)
					if !changed || err != nil || task.Status != TaskSubmitted {
						t.Fatal("cancel inferred terminal state")
					}
					changed, err = task.RequestCancel(stateNow.Add(time.Second))
					if changed || err != nil || !task.CancelRequestedAt.Equal(stateNow) {
						t.Fatal("cancel retry changed first timestamp")
					}
				}
				if _, err := task.RecordObservationError("provider cancellation unsupported", stateNow); err != nil {
					t.Fatal(err)
				}
				changed, err := task.ApplyUpdate(TaskUpdate{Status: terminal}, stateNow.Add(2*time.Second))
				if !changed || err != nil || task.Status != terminal || task.ObservationError != "" {
					t.Fatal("actual provider outcome lost")
				}
				before := task
				changed, err = task.RequestCancel(stateNow.Add(3 * time.Second))
				if changed || err != nil || !reflect.DeepEqual(task, before) {
					t.Fatal("late cancel mutated terminal task")
				}
				changed, err = task.RecordObservationError("late timeout", stateNow.Add(4*time.Second))
				if changed || err != nil || !reflect.DeepEqual(task, before) {
					t.Fatal("late error mutated terminal task")
				}
			})
		}
	}
}

func TestTerminalSnapshotFrozenAndCopied(t *testing.T) {
	task := stateTask()
	update := TaskUpdate{Status: TaskSucceeded, Cursor: "final", Progress: []Part{{Data: []byte("progress")}}, Result: &ToolResult{CallID: task.Call.ID, Parts: []Part{{Data: []byte("result")}}}}
	if changed, err := task.ApplyUpdate(update, stateNow); !changed || err != nil {
		t.Fatal(err)
	}
	if changed, err := task.ApplyUpdate(update, stateNow.Add(time.Second)); changed || err != nil {
		t.Fatal("duplicate terminal failed")
	}
	update.Progress[0].Data[0] = 'X'
	update.Result.Parts[0].Data[0] = 'X'
	update.Result.CallID = "other"
	if string(task.Progress[0].Data) != "progress" || string(task.Result.Parts[0].Data) != "result" || task.Result.CallID != task.Call.ID {
		t.Fatal("input aliases stored terminal")
	}
	for _, late := range []TaskUpdate{
		{Status: TaskRunning}, {Status: TaskFailed},
		{Status: TaskSucceeded},
		{Status: TaskSucceeded, Cursor: "different", Progress: task.Progress, Result: task.Result},
		{Status: TaskSucceeded, Cursor: task.ProviderCursor, Result: task.Result},
		{Status: TaskSucceeded, Cursor: task.ProviderCursor, Progress: task.Progress, Result: &ToolResult{CallID: task.Call.ID, Error: "different"}},
	} {
		before := task
		changed, err := task.ApplyUpdate(late, stateNow.Add(time.Second))
		if changed || !errors.Is(err, apperrors.ErrConflict) || !reflect.DeepEqual(task, before) {
			t.Fatal("late snapshot overwrote terminal")
		}
	}
}

func TestUpdateValidationIsAtomic(t *testing.T) {
	for _, tc := range []struct {
		name   string
		update TaskUpdate
		at     time.Time
		field  string
	}{
		{"unknown", TaskUpdate{Status: "private-provider-state"}, stateNow, "task.status"},
		{"nonterminal result", TaskUpdate{Status: TaskRunning, Result: &ToolResult{CallID: "call"}}, stateNow, "task.result"},
		{"wrong call", TaskUpdate{Status: TaskSucceeded, Result: &ToolResult{CallID: "private-call"}}, stateNow, "result.call_id"},
		{"missing call", TaskUpdate{Status: TaskSucceeded, Result: &ToolResult{}}, stateNow, "result.call_id"},
		{"zero time", TaskUpdate{Status: TaskRunning}, time.Time{}, "task.observed_at"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task := stateTask()
			before := task
			changed, err := task.ApplyUpdate(tc.update, tc.at)
			assertStateError(t, err, apperrors.ErrInvalidArgument, tc.field)
			if changed || !reflect.DeepEqual(task, before) {
				t.Fatal("invalid update partially applied")
			}
		})
	}
}

func TestSameStatusProgressAndFirstObservation(t *testing.T) {
	task := stateTask()
	for _, update := range []TaskUpdate{
		{Status: TaskSubmitted}, // 首次观察即使状态未变仍记录时间。
		{Status: TaskSubmitted, Progress: []Part{{Text: "queued"}}},
		{Status: TaskSubmitted, Progress: []Part{{Text: "queued"}}, Cursor: "new"},
		{Status: TaskSubmitted}, // 完整快照允许清空进度及游标。
	} {
		changed, err := task.ApplyUpdate(update, stateNow)
		if !changed || err != nil || !task.LastObservedAt.Equal(stateNow) {
			t.Fatalf("lost same-state progress: %v", err)
		}
	}
}

func TestStateCommandsRejectInvalidInputWithoutMutation(t *testing.T) {
	for _, command := range []string{"cancel", "observation", "update"} {
		t.Run(command, func(t *testing.T) {
			for _, invalidStatus := range []bool{true, false} {
				task := stateTask()
				if invalidStatus {
					task.Status = "private-invalid"
				}
				before := task
				var changed bool
				var err error
				switch command {
				case "cancel":
					changed, err = task.RequestCancel(time.Time{})
				case "observation":
					changed, err = task.RecordObservationError("timeout", time.Time{})
				case "update":
					changed, err = task.ApplyUpdate(TaskUpdate{Status: TaskRunning}, time.Time{})
				}
				field := "task.observed_at"
				if invalidStatus {
					field = "task.status"
				}
				assertStateError(t, err, apperrors.ErrInvalidArgument, field)
				if changed || !reflect.DeepEqual(task, before) {
					t.Fatal("invalid command mutated task")
				}
			}
		})
	}
	task := stateTask()
	before := task
	changed, err := task.RecordObservationError(" \t", stateNow)
	assertStateError(t, err, apperrors.ErrInvalidArgument, "task.observation_error")
	if changed || !reflect.DeepEqual(task, before) {
		t.Fatal("blank error mutated task")
	}
}

// Run 取消只是本地生成边界，迟到的远端成功仍可记录，但不能重开 Run。
func TestCancelledRunRetainsLateTaskOutcome(t *testing.T) {
	_, run, _, _ := fixtures()
	run.Status = RunRunning
	if err := run.Status.ValidateTransition(RunCancelled); err != nil {
		t.Fatal(err)
	}
	run.Status = RunCancelled
	task := stateTask()
	if _, err := task.RequestCancel(stateNow); err != nil {
		t.Fatal(err)
	}
	if _, err := task.ApplyUpdate(TaskUpdate{Status: TaskSucceeded}, stateNow); err != nil {
		t.Fatal(err)
	}
	if run.Status != RunCancelled || task.Status != TaskSucceeded || task.AppliedAt != nil {
		t.Fatal("late completion resumed cancelled run")
	}
	assertStateError(t, run.Status.ValidateTransition(RunRunning), apperrors.ErrConflict, "run.status")
}
