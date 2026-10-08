package store

import (
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hjhsamuel/cagent/internal/apperrors"
	"github.com/hjhsamuel/cagent/internal/domain"
)

var contractNow = time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)

func contractRun() domain.Run {
	return domain.Run{Scope: domain.Scope{TenantID: "tenant", UserID: "user"}, ID: "run", SessionID: "session", Status: domain.RunRunning, Version: 3}
}

func contractGuard() WriteGuard {
	run := contractRun()
	return WriteGuard{Lease: Lease{Scope: run.Scope, RunID: run.ID, Owner: "instance", Fence: 7, ExpiresAt: contractNow.Add(time.Minute)}, RunVersion: run.Version}
}

func expectKind(t *testing.T, err error, kind apperrors.Kind) {
	t.Helper()
	if !errors.Is(err, kind) {
		t.Fatalf("want %s, got %v", kind, err)
	}
	var detail *apperrors.Error
	if !errors.As(err, &detail) || detail.Field() == "" || strings.Contains(err.Error(), "private") {
		t.Fatalf("unsafe/unstructured error: %v", err)
	}
}

func TestLeaseFencingAndExpiry(t *testing.T) {
	lease := contractGuard().Lease
	if err := CheckLease(lease, lease, contractNow); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*Lease){
		"new owner":             func(l *Lease) { l.Owner = "private-new-owner" },
		"same owner reacquires": func(l *Lease) { l.Fence++ },
		"expired current":       func(l *Lease) { l.ExpiresAt = contractNow },
		"different tenant":      func(l *Lease) { l.Scope.TenantID = "private-tenant" },
		"different user":        func(l *Lease) { l.Scope.UserID = "private-user" },
		"different run":         func(l *Lease) { l.RunID = "private-run" },
	} {
		t.Run(name, func(t *testing.T) {
			current := lease
			change(&current)
			expectKind(t, CheckLease(lease, current, contractNow), ErrConflict)
		})
	}
	// 到期时刻不能续用旧凭证；续期后的存储有效期不能救活已过期的旧副本。
	renewed := lease
	released := lease
	released.Owner = ""
	released.ExpiresAt = time.Time{}
	expectKind(t, CheckLease(lease, released, contractNow), ErrConflict)
	renewed.ExpiresAt = lease.ExpiresAt.Add(time.Minute)
	expectKind(t, CheckLease(lease, renewed, lease.ExpiresAt), ErrConflict)
	if err := CheckLease(lease, renewed, contractNow); err != nil {
		t.Fatal(err)
	}
	expectKind(t, CheckLease(Lease{}, lease, contractNow), apperrors.ErrInvalidArgument)
	expectKind(t, CheckLease(lease, lease, time.Time{}), apperrors.ErrInvalidArgument)
}

func TestVersionCASAndOverflow(t *testing.T) {
	for _, v := range []int64{0, 1, 7, math.MaxInt64 - 1} {
		if err := CheckVersion(v, v); err != nil {
			t.Fatal(err)
		}
	}
	for _, pair := range [][2]int64{{1, 2}, {0, 1}, {math.MaxInt64, math.MaxInt64}} {
		expectKind(t, CheckVersion(pair[0], pair[1]), ErrConflict)
	}
	expectKind(t, CheckVersion(-1, 0), apperrors.ErrInvalidArgument)
}

func TestIdempotentStartInput(t *testing.T) {
	run := contractRun()
	run.Version = 0
	run.Status = domain.RunQueued
	run.IdempotencyKey = "key"
	request := StartRunRequest{Run: run, ExpectedSessionVersion: 1, Input: domain.Message{Scope: run.Scope, ID: "input", SessionID: run.SessionID, RunID: run.ID, Role: domain.RoleUser, Parts: []domain.Part{{Kind: "text", Text: "hello"}}}}
	if err := request.Validate(); err != nil {
		t.Fatal(err)
	}
	first := InputFingerprint(request.Input.Parts)
	retry := request
	retry.Run.ID = "new-run"
	retry.Input.RunID = retry.Run.ID
	retry.Input.ID = "new-input"
	retry.ExpectedSessionVersion = 99
	if err := retry.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := CheckIdempotentInput(first, InputFingerprint(retry.Input.Parts)); err != nil {
		t.Fatal("generated IDs/versions broke retry", err)
	}
	changed := []domain.Part{{Kind: "text", Text: "private-different-input"}}
	expectKind(t, CheckIdempotentInput(first, InputFingerprint(changed)), ErrConflict)
	// 摘要必须区分字段边界、顺序、空/nil 及非 UTF-8 原始字节，不能规范化输入。
	for _, pair := range [][2][]domain.Part{
		{{{Kind: "ab", Text: "c"}}, {{Kind: "a", Text: "bc"}}},
		{{{Text: "a"}, {Text: "b"}}, {{Text: "b"}, {Text: "a"}}},
		{nil, {}},
		{{{Data: nil}}, {{Data: []byte{}}}},
		{{{Text: string([]byte{0xff})}}, {{Text: string([]byte{0xfe})}}},
	} {
		if InputFingerprint(pair[0]) == InputFingerprint(pair[1]) {
			t.Fatal("distinct input collided")
		}
	}
	for _, mutate := range []func(*StartRunRequest){
		func(r *StartRunRequest) { r.Input.Scope.UserID = "private-other" },
		func(r *StartRunRequest) { r.Input.RunID = "private-other" },
		func(r *StartRunRequest) { r.Input.Sequence = 1 },
		func(r *StartRunRequest) { r.Run.Status = domain.RunRunning },
		func(r *StartRunRequest) { r.Run.Version = 1 },
		func(r *StartRunRequest) { r.Run.IdempotencyKey = " \t" },
		func(r *StartRunRequest) { r.ExpectedSessionVersion = 0 },
	} {
		bad := request
		mutate(&bad)
		expectKind(t, bad.Validate(), apperrors.ErrInvalidArgument)
	}
}

func TestPagesAndRetentionWatermark(t *testing.T) {
	for _, p := range []SequencePage{{0, 1}, {10, 1000}} {
		if err := p.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []SequencePage{{-1, 1}, {0, 0}, {0, 1001}} {
		expectKind(t, p.Validate(), apperrors.ErrInvalidArgument)
	}
	for _, tc := range []struct {
		after, pruned, last int64
		kind                apperrors.Kind
	}{
		{0, 0, 0, ""}, {0, 0, 10, ""}, {5, 5, 10, ""}, {10, 10, 10, ""},
		{0, 5, 10, ErrCursorExpired}, {4, 5, 10, ErrCursorExpired}, {9, 10, 10, ErrCursorExpired},
		{11, 5, 10, apperrors.ErrInvalidArgument}, {0, 11, 10, apperrors.ErrInvalidArgument}, {-1, 0, 10, apperrors.ErrInvalidArgument},
	} {
		err := CheckEventCursor(tc.after, tc.pruned, tc.last)
		if tc.kind == "" {
			if err != nil {
				t.Fatal(err)
			}
		} else {
			expectKind(t, err, tc.kind)
		}
	}
	if err := ValidateRecoveryPage(nil, 100); err != nil {
		t.Fatal(err)
	}
	if err := ValidateRecoveryPage(&RecoveryPosition{TenantID: "t", UserID: "u", RunID: "r"}, 1); err != nil {
		t.Fatal(err)
	}
	expectKind(t, ValidateRecoveryPage(&RecoveryPosition{RunID: "private-run"}, 1), apperrors.ErrInvalidArgument)
	expectKind(t, ValidateRecoveryPage(nil, 0), apperrors.ErrInvalidArgument)
}

func applicationFixture() (domain.Run, domain.Task, domain.TaskDelivery, domain.Checkpoint, ApplyTaskRequest) {
	run := contractRun()
	caller := domain.AgentExecution{AgentID: "agent", InvocationID: "child-1", ParentInvocationID: "root"}
	call := domain.ToolCall{Scope: run.Scope, ID: "call", SessionID: run.SessionID, RunID: run.ID, Caller: caller, Protocol: domain.ToolA2A, Name: "search"}
	task := domain.Task{Scope: run.Scope, ID: "task", Call: call, Handle: domain.TaskHandle{Protocol: domain.ToolA2A, ConnectionID: "conn", RemoteID: "remote"}, Status: domain.TaskSucceeded, Version: 2}
	delivery := domain.TaskDelivery{Scope: run.Scope, TaskID: task.ID, RunID: run.ID, Caller: caller, ToolCallID: call.ID, State: domain.DeliveryPending, Version: 1}
	current := domain.Checkpoint{Scope: run.Scope, RunID: run.ID, Caller: caller, Format: "test-v1", Version: 4, PendingCallIDs: []string{"call", "parallel"}}
	next := current
	next.PendingCallIDs = []string{"parallel"}
	next.Data = []byte("result incorporated")
	req := ApplyTaskRequest{Guard: contractGuard(), OperationID: "apply-once", TaskID: task.ID, ExpectedTaskVersion: task.Version, ExpectedDeliveryVersion: delivery.Version, ExpectedSessionVersion: 1, Checkpoint: next}
	return run, task, delivery, current, req
}

// 模拟崩溃后重新读取“终态+pending 意图”的前置条件，而非声称验证了数据库事务。
// 已消费/丢弃的状态必须拒绝再次应用；相同请求的网络重试由适配器回执返回旧结果。
func TestRecoveryApplicationAndDuplicateConsumption(t *testing.T) {
	run, task, delivery, current, req := applicationFixture()
	if err := req.ValidateAgainst(run, task, delivery, current); err != nil {
		t.Fatal(err)
	}
	before := current
	before.PendingCallIDs = append([]string(nil), current.PendingCallIDs...)
	for name, mutate := range map[string]func(*domain.Run, *domain.Task, *domain.TaskDelivery, *domain.Checkpoint, *ApplyTaskRequest){
		"already consumed": func(_ *domain.Run, t *domain.Task, _ *domain.TaskDelivery, _ *domain.Checkpoint, _ *ApplyTaskRequest) {
			t.AppliedAt = &contractNow
		},
		"settled delivery": func(_ *domain.Run, _ *domain.Task, d *domain.TaskDelivery, _ *domain.Checkpoint, _ *ApplyTaskRequest) {
			d.State = domain.DeliveryApplied
		},
		"discarded delivery": func(_ *domain.Run, _ *domain.Task, d *domain.TaskDelivery, _ *domain.Checkpoint, _ *ApplyTaskRequest) {
			d.State = domain.DeliveryDiscarded
		},
		"run cancelled": func(r *domain.Run, _ *domain.Task, _ *domain.TaskDelivery, _ *domain.Checkpoint, _ *ApplyTaskRequest) {
			r.Status = domain.RunCancelled
		},
		"task still running": func(_ *domain.Run, t *domain.Task, _ *domain.TaskDelivery, _ *domain.Checkpoint, _ *ApplyTaskRequest) {
			t.Status = domain.TaskRunning
		},
		"stale checkpoint": func(_ *domain.Run, _ *domain.Task, _ *domain.TaskDelivery, c *domain.Checkpoint, _ *ApplyTaskRequest) {
			c.Version++
		},
		"stale task": func(_ *domain.Run, t *domain.Task, _ *domain.TaskDelivery, _ *domain.Checkpoint, _ *ApplyTaskRequest) {
			t.Version++
		},
		"stale run": func(r *domain.Run, _ *domain.Task, _ *domain.TaskDelivery, _ *domain.Checkpoint, _ *ApplyTaskRequest) {
			r.Version++
		},
		"lost parallel call": func(_ *domain.Run, _ *domain.Task, _ *domain.TaskDelivery, _ *domain.Checkpoint, r *ApplyTaskRequest) {
			r.Checkpoint.PendingCallIDs = nil
		},
		"not consumed": func(_ *domain.Run, _ *domain.Task, _ *domain.TaskDelivery, _ *domain.Checkpoint, r *ApplyTaskRequest) {
			r.Checkpoint.PendingCallIDs = []string{"call", "parallel"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			r, task, d, c, q := applicationFixture()
			mutate(&r, &task, &d, &c, &q)
			expectKind(t, q.ValidateAgainst(r, task, d, c), ErrConflict)
		})
	}
	if !reflect.DeepEqual(current, before) {
		t.Fatal("validation changed pending calls")
	}
}

func TestRecoveryCannotCrossInvocationOrScope(t *testing.T) {
	for name, mutate := range map[string]func(*domain.TaskDelivery, *domain.Checkpoint, *ApplyTaskRequest){
		"delivery user": func(d *domain.TaskDelivery, _ *domain.Checkpoint, _ *ApplyTaskRequest) {
			d.Scope.UserID = "private-user"
		},
		"delivery call": func(d *domain.TaskDelivery, _ *domain.Checkpoint, _ *ApplyTaskRequest) { d.ToolCallID = "private-call" },
		"delivery branch": func(d *domain.TaskDelivery, _ *domain.Checkpoint, _ *ApplyTaskRequest) {
			d.Caller.InvocationID = "private-child"
		},
		"checkpoint branch": func(_ *domain.TaskDelivery, c *domain.Checkpoint, _ *ApplyTaskRequest) {
			c.Caller.InvocationID = "private-child"
		},
		"next checkpoint branch": func(_ *domain.TaskDelivery, _ *domain.Checkpoint, r *ApplyTaskRequest) {
			r.Checkpoint.Caller.InvocationID = "private-child"
		},
		"duplicate pending": func(_ *domain.TaskDelivery, _ *domain.Checkpoint, r *ApplyTaskRequest) {
			r.Checkpoint.PendingCallIDs = []string{"parallel", "parallel"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			run, task, d, c, req := applicationFixture()
			mutate(&d, &c, &req)
			expectKind(t, req.ValidateAgainst(run, task, d, c), apperrors.ErrInvalidArgument)
		})
	}
}

func TestCommitAndTrackPreconditions(t *testing.T) {
	run, task, _, cp, _ := applicationFixture()
	commit := CommitRunRequest{Guard: contractGuard(), OperationID: "output", Status: domain.RunWaiting, ExpectedSessionVersion: 1, Events: []domain.Event{{Scope: run.Scope, RunID: run.ID, Kind: domain.EventToolWaiting}}}
	if err := commit.ValidateForRun(run); err != nil {
		t.Fatal(err)
	}
	bad := commit
	bad.Events = []domain.Event{{Scope: run.Scope, RunID: run.ID, Sequence: 8}}
	expectKind(t, bad.ValidateForRun(run), apperrors.ErrInvalidArgument)
	bad = commit
	bad.Status = domain.RunCancelled
	expectKind(t, bad.ValidateForRun(run), apperrors.ErrInvalidArgument)
	finished := run
	finished.Status = domain.RunCompleted
	expectKind(t, commit.ValidateForRun(finished), ErrConflict)
	task.Version = 0
	track := TrackTaskRequest{Guard: contractGuard(), Task: task, Checkpoint: cp, ExpectedCheckpointVersion: cp.Version}
	if err := track.ValidateForRun(run); err != nil {
		t.Fatal(err)
	}
	track.Checkpoint.PendingCallIDs = []string{"parallel"}
	expectKind(t, track.ValidateForRun(run), apperrors.ErrInvalidArgument)
}

// 保存的回执可以在响应丢失后确认原事务结果，绝不能重新分配消息/事件序号。
// 这里只检验回执身份/内容规则；真正事务原子写入和网络故障重试由 P3 集成测试验证。
func TestReceiptAfterLostAcknowledgement(t *testing.T) {
	run := contractRun()
	digest := InputFingerprint([]domain.Part{{Text: "opaque mutation payload"}})
	receipt := MutationReceipt{Scope: run.Scope, RunID: run.ID, OperationID: "commit", Kind: MutationRunCommit, Digest: digest,
		Result: CommitResult{Run: run, Events: []domain.Event{{Scope: run.Scope, RunID: run.ID, Sequence: 42}}}}
	if err := receipt.Match(run.Scope, run.ID, "commit", MutationRunCommit, digest); err != nil {
		t.Fatal(err)
	}
	// 回执重放不要求旧 lease/version，结果序号保持不变。
	if receipt.Result.Events[0].Sequence != 42 {
		t.Fatal("receipt lost committed sequence")
	}
	expectKind(t, receipt.Match(run.Scope, run.ID, "commit", MutationTaskApply, digest), ErrConflict)
	expectKind(t, receipt.Match(run.Scope, run.ID, "different", MutationRunCommit, digest), ErrConflict)
	different := digest
	different[0] ^= 1
	expectKind(t, receipt.Match(run.Scope, run.ID, "commit", MutationRunCommit, different), ErrConflict)
	foreign := run.Scope
	foreign.UserID = "private-user"
	expectKind(t, receipt.Match(foreign, run.ID, "commit", MutationRunCommit, digest), apperrors.ErrInvalidArgument)
	expectKind(t, receipt.Match(domain.Scope{}, run.ID, "commit", MutationRunCommit, digest), apperrors.ErrInvalidArgument)
}

func TestLateTaskObservationDoesNotReopenEventStream(t *testing.T) {
	run, task, _, _, _ := applicationFixture()
	run.Status = domain.RunCancelled
	task.Status = domain.TaskRunning
	req := ObserveTaskRequest{Guard: contractGuard(), TaskID: task.ID, ExpectedTaskVersion: task.Version, Update: domain.TaskUpdate{Status: domain.TaskSucceeded}}
	if err := req.ValidateAgainst(run, task); err != nil {
		t.Fatal(err)
	}
	req.Events = []domain.Event{{Scope: run.Scope, RunID: run.ID, Kind: domain.EventToolFinished}}
	expectKind(t, req.ValidateAgainst(run, task), ErrConflict)
	run.Status = domain.RunRunning
	if err := req.ValidateAgainst(run, task); err != nil {
		t.Fatal(err)
	}
}
