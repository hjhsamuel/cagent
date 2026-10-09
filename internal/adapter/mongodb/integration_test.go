package mongodb

import (
	"context"
	"errors"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/hjhsamuel/cagent/internal/apperrors"
	"github.com/hjhsamuel/cagent/internal/domain"
	"github.com/hjhsamuel/cagent/internal/storage/schema"
	"github.com/hjhsamuel/cagent/internal/store"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

var testScope = domain.Scope{TenantID: "tenant", UserID: "user"}

func check(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func wantKind(t *testing.T, err error, kind error) {
	t.Helper()
	if !errors.Is(err, kind) {
		t.Fatalf("wanted %v; got %v", kind, err)
	}
}
func newSession(t *testing.T, db *Database, s domain.Scope, id string) {
	t.Helper()
	check(t, db.CreateSession(context.Background(), domain.Session{Scope: s, ID: id, AgentID: "agent"}))
}
func startRequest(scope domain.Scope, session, id, key string, version int64) store.StartRunRequest {
	return store.StartRunRequest{Run: domain.Run{Scope: scope, SessionID: session, ID: id, Status: domain.RunQueued, IdempotencyKey: key}, Input: domain.Message{Scope: scope, SessionID: session, RunID: id, ID: "input-" + id, Role: domain.RoleUser, Parts: []domain.Part{{Kind: "text", Text: "hello"}}}, ExpectedSessionVersion: version}
}
func startFixture(t *testing.T, db *Database) (store.StartRunResult, store.WriteGuard) {
	t.Helper()
	ctx := context.Background()
	newSession(t, db, testScope, "session")
	out, err := db.StartRun(ctx, startRequest(testScope, "session", "run", "start", 1))
	check(t, err)
	lease, err := db.AcquireLease(ctx, testScope, "run", "owner", time.Minute)
	check(t, err)
	return out, store.WriteGuard{Lease: lease, RunVersion: out.Run.Version}
}
func freshGuard(t *testing.T, db *Database, l store.Lease) store.WriteGuard {
	t.Helper()
	run, err := db.GetRun(context.Background(), l.Scope, l.RunID)
	check(t, err)
	return store.WriteGuard{Lease: l, RunVersion: run.Version}
}

func TestMongoScopeIdempotencyAndConcurrentStart(t *testing.T) {
	db, cfg := testDatabase(t)
	ctx := context.Background()
	second, err := Open(ctx, cfg)
	check(t, err)
	defer second.Close(ctx) // 同索引初始化可重复执行，独立客户端模拟两个服务实例。
	newSession(t, db, testScope, "session")
	requests := []store.StartRunRequest{startRequest(testScope, "session", "run-a", "same-key", 1), startRequest(testScope, "session", "run-b", "same-key", 1)}
	results := make([]store.StartRunResult, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	for i, client := range []*Database{db, second} {
		go func(i int, c *Database) {
			defer wg.Done()
			results[i], errs[i] = c.StartRun(ctx, requests[i])
		}(i, client)
	}
	wg.Wait()
	check(t, errs[0])
	check(t, errs[1])
	if results[0].Run.ID != results[1].Run.ID || results[0].Replayed == results[1].Replayed {
		t.Fatal("concurrent duplicate start not replayed")
	}
	page, err := db.ListMessages(ctx, testScope, "session", store.SequencePage{Limit: 10})
	check(t, err)
	if len(page.Items) != 1 || page.Items[0].Sequence != 1 {
		t.Fatal("duplicate input appended")
	}
	different := requests[0]
	different.Input.Parts = []domain.Part{{Text: "different"}}
	_, err = db.StartRun(ctx, different)
	wantKind(t, err, apperrors.ErrConflict)
	_, err = db.StartRun(ctx, startRequest(testScope, "session", "another", "other-key", 2))
	wantKind(t, err, apperrors.ErrConflict)
	for _, scope := range []domain.Scope{{TenantID: "tenant", UserID: "other"}, {TenantID: "other", UserID: "user"}} {
		_, err = db.GetRun(ctx, scope, results[0].Run.ID)
		wantKind(t, err, apperrors.ErrNotFound)
		newSession(t, db, scope, "session")
		_, err = db.StartRun(ctx, startRequest(scope, "session", results[0].Run.ID, "same-key", 1))
		check(t, err)
	}
	_, err = db.GetRun(ctx, domain.Scope{}, "run")
	wantKind(t, err, apperrors.ErrInvalidArgument)
	newSession(t, db, testScope, "other-session")
	_, err = db.StartRun(ctx, startRequest(testScope, "other-session", "other-run", "same-key", 1))
	check(t, err)
}

func TestMongoRunCommitReceiptsSequenceAndRetention(t *testing.T) {
	db, cfg := testDatabase(t)
	ctx := context.Background()
	start, g := startFixture(t, db)
	req := store.CommitRunRequest{Guard: g, OperationID: "first-output", Status: domain.RunRunning, ExpectedSessionVersion: start.SessionVersion, Messages: []domain.Message{{Scope: testScope, SessionID: "session", RunID: "run", ID: "answer", Role: domain.RoleAssistant, Parts: []domain.Part{{Text: "answer"}}}}, Events: []domain.Event{{Scope: testScope, RunID: "run", Kind: domain.EventRunStarted}, {Scope: testScope, RunID: "run", Kind: domain.EventTextDelta, Data: []byte("answer")}}}
	out, err := db.CommitRun(ctx, req)
	check(t, err)
	if out.Run.Version != 2 || out.SessionVersion != 3 || out.Messages[0].Sequence != 2 || out.Events[1].Sequence != 2 {
		t.Fatal("wrong committed counters")
	}
	retry := req
	retry.Guard.RunVersion = 999
	retry.Guard.Lease.ExpiresAt = time.Unix(1, 0)
	again, err := db.CommitRun(ctx, retry)
	check(t, err)
	if !reflect.DeepEqual(again, out) {
		t.Fatal("lost-ack retry changed receipt")
	}
	retry.Status = domain.RunFailed
	_, err = db.CommitRun(ctx, retry)
	wantKind(t, err, apperrors.ErrConflict)
	second, err := Open(ctx, cfg)
	check(t, err)
	defer second.Close(ctx)
	requests := []store.CommitRunRequest{{Guard: freshGuard(t, db, g.Lease), OperationID: "a", Status: domain.RunRunning, ExpectedSessionVersion: 3, Events: []domain.Event{{Scope: testScope, RunID: "run", Kind: domain.EventTextDelta}}}, {Guard: freshGuard(t, db, g.Lease), OperationID: "b", Status: domain.RunRunning, ExpectedSessionVersion: 3, Events: []domain.Event{{Scope: testScope, RunID: "run", Kind: domain.EventTextDelta}}}}
	errs := make([]error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	for i, c := range []*Database{db, second} {
		go func(i int, c *Database) { defer wg.Done(); _, errs[i] = c.CommitRun(ctx, requests[i]) }(i, c)
	}
	wg.Wait()
	if (errs[0] == nil) == (errs[1] == nil) {
		t.Fatalf("CAS winners: %v", errs)
	}
	for _, err := range errs {
		if err != nil {
			wantKind(t, err, apperrors.ErrConflict)
		}
	}
	p, err := db.ListEvents(ctx, testScope, "run", store.SequencePage{Limit: 2})
	check(t, err)
	if !p.HasMore || p.NextAfter != 2 || p.LastSequence != 3 {
		t.Fatal("bad event page")
	}
	admin, err := OpenRecovery(ctx, cfg)
	check(t, err)
	defer admin.Close(ctx)
	check(t, admin.PruneEvents(ctx, testScope, "run", 2))
	_, err = db.ListEvents(ctx, testScope, "run", store.SequencePage{Limit: 2})
	wantKind(t, err, store.ErrCursorExpired)
	p, err = db.ListEvents(ctx, testScope, "run", store.SequencePage{After: 2, Limit: 2})
	check(t, err)
	if len(p.Items) != 1 || p.Items[0].Sequence != 3 {
		t.Fatal("prune skipped retained event")
	}
	_, err = db.ListEvents(ctx, testScope, "run", store.SequencePage{After: 99, Limit: 2})
	wantKind(t, err, apperrors.ErrInvalidArgument)
	finish, err := db.CommitRun(ctx, store.CommitRunRequest{Guard: freshGuard(t, db, g.Lease), OperationID: "finish", Status: domain.RunCompleted, ExpectedSessionVersion: 3})
	check(t, err)
	if finish.Events[0].Kind != domain.EventRunCompleted || finish.Events[0].Sequence != 4 {
		t.Fatal("terminal event missing")
	}
	check(t, admin.PruneEvents(ctx, testScope, "run", 4))
	p, err = db.ListEvents(ctx, testScope, "run", store.SequencePage{After: 4, Limit: 2})
	check(t, err)
	if len(p.Items) != 0 || p.PrunedThrough != 4 || p.LastSequence != 4 {
		t.Fatal("lost empty-stream watermark")
	}
	_, err = db.AcquireLease(ctx, testScope, "run", "new-owner", time.Minute)
	wantKind(t, err, apperrors.ErrConflict)
	_, err = db.StartRun(ctx, startRequest(testScope, "session", "next", "next", finish.SessionVersion))
	check(t, err)
	original, err := db.StartRun(ctx, startRequest(testScope, "session", "different-id", "start", 1))
	check(t, err)
	if !original.Replayed || original.Run.Status != domain.RunQueued || original.Run.ID != "run" {
		t.Fatal("start receipt overwritten")
	}
}

func TestMongoLeaseExpiryReleaseAndTakeover(t *testing.T) {
	db, cfg := testDatabase(t)
	ctx := context.Background()
	_, g := startFixture(t, db)
	second, err := Open(ctx, cfg)
	check(t, err)
	defer second.Close(ctx)
	_, err = second.AcquireLease(ctx, testScope, "run", "owner", time.Minute)
	wantKind(t, err, apperrors.ErrConflict)
	check(t, db.ReleaseLease(ctx, g.Lease))
	_, err = second.CommitRun(ctx, store.CommitRunRequest{Guard: g, OperationID: "stale", Status: domain.RunRunning, ExpectedSessionVersion: 2})
	wantKind(t, err, apperrors.ErrConflict)
	_, err = db.StartRun(ctx, startRequest(testScope, "session", "new", "new", 2))
	wantKind(t, err, apperrors.ErrConflict)
	l, err := second.AcquireLease(ctx, testScope, "run", "owner", 80*time.Millisecond)
	check(t, err)
	if l.Fence <= g.Lease.Fence {
		t.Fatal("fence reset")
	}
	// 测试真实数据库时钟的到期边界，不通过修改生产时钟或伪造租约文档。
	time.Sleep(120 * time.Millisecond)
	_, err = second.RenewLease(ctx, l, time.Minute)
	wantKind(t, err, apperrors.ErrConflict)
	next, err := db.AcquireLease(ctx, testScope, "run", "other-owner", time.Minute)
	check(t, err)
	_, err = second.CommitRun(ctx, store.CommitRunRequest{Guard: store.WriteGuard{Lease: l, RunVersion: 1}, OperationID: "expired", Status: domain.RunRunning, ExpectedSessionVersion: 2})
	wantKind(t, err, apperrors.ErrConflict)
	renewed, err := db.RenewLease(ctx, next, 2*time.Minute)
	check(t, err)
	if renewed.Fence != next.Fence || !renewed.ExpiresAt.After(next.ExpiresAt) {
		t.Fatal("bad renewal")
	}
	check(t, db.ReleaseLease(ctx, renewed))
}

func TestMongoRollbackStartAndCommit(t *testing.T) {
	db, _ := testDatabase(t)
	ctx := context.Background()
	newSession(t, db, testScope, "session")
	injected := errors.New("injected before commit")
	db.beforeCommit = func(name string) error {
		if name == "start" {
			return injected
		}
		return nil
	}
	_, err := db.StartRun(ctx, startRequest(testScope, "session", "run", "key", 1))
	if !errors.Is(err, injected) {
		t.Fatal(err)
	}
	db.beforeCommit = nil
	s, err := db.GetSession(ctx, testScope, "session")
	check(t, err)
	if s.Version != 1 || s.ActiveRunID != "" {
		t.Fatal("start partial commit")
	}
	_, err = db.GetRun(ctx, testScope, "run")
	wantKind(t, err, apperrors.ErrNotFound)
	out, err := db.StartRun(ctx, startRequest(testScope, "session", "run", "key", 1))
	check(t, err)
	if out.Input.Sequence != 1 {
		t.Fatal("rollback consumed sequence")
	}
	l, err := db.AcquireLease(ctx, testScope, "run", "owner", time.Minute)
	check(t, err)
	req := store.CommitRunRequest{Guard: store.WriteGuard{Lease: l, RunVersion: 1}, OperationID: "output", Status: domain.RunRunning, ExpectedSessionVersion: 2, Events: []domain.Event{{Scope: testScope, RunID: "run", Kind: domain.EventTextDelta}}, Messages: []domain.Message{{Scope: testScope, SessionID: "session", RunID: "run", ID: "result", Role: domain.RoleAssistant}}}
	db.beforeCommit = func(name string) error {
		if name == "commit" {
			return injected
		}
		return nil
	}
	_, err = db.CommitRun(ctx, req)
	if !errors.Is(err, injected) {
		t.Fatal(err)
	}
	db.beforeCommit = nil
	p, err := db.ListEvents(ctx, testScope, "run", store.SequencePage{Limit: 10})
	check(t, err)
	if p.LastSequence != 0 || len(p.Items) != 0 {
		t.Fatal("events leaked from rolled back transaction")
	}
	result, err := db.CommitRun(ctx, req)
	check(t, err)
	if result.Events[0].Sequence != 1 || result.Messages[0].Sequence != 2 {
		t.Fatal("rollback counters not restored")
	}
	// 数据库唯一索引在部分写入之后失败，也必须回滚 Run/计数器/回执。
	bad := req
	bad.Guard.RunVersion = result.Run.Version
	bad.ExpectedSessionVersion = result.SessionVersion
	bad.OperationID = "duplicate-message"
	_, err = db.CommitRun(ctx, bad)
	wantKind(t, err, apperrors.ErrConflict)
	run, err := db.GetRun(ctx, testScope, "run")
	check(t, err)
	if run.Version != result.Run.Version {
		t.Fatal("duplicate-key failure partially committed")
	}
}

func trackRequest(g store.WriteGuard, cp domain.Checkpoint, id string) store.TrackTaskRequest {
	call := domain.ToolCall{Scope: testScope, ID: "call-" + id, SessionID: "session", RunID: "run", Caller: cp.Caller, Protocol: domain.ToolA2A, Name: "search"}
	cp.PendingCallIDs = append(append([]string(nil), cp.PendingCallIDs...), call.ID)
	return store.TrackTaskRequest{Guard: g, Task: domain.Task{Scope: testScope, ID: id, Call: call, Handle: domain.TaskHandle{Protocol: domain.ToolA2A, ConnectionID: "connection", RemoteID: "remote-" + id}, Status: domain.TaskSubmitted}, Checkpoint: cp, ExpectedCheckpointVersion: cp.Version}
}

func TestMongoTaskRecoveryApplicationAndDiscard(t *testing.T) {
	db, cfg := testDatabase(t)
	ctx := context.Background()
	_, g := startFixture(t, db)
	cp := domain.Checkpoint{Scope: testScope, RunID: "run", Caller: domain.AgentExecution{AgentID: "agent", InvocationID: "branch", ParentInvocationID: "root"}, Format: "test-v1", Data: []byte{1}}
	setup, err := db.CommitRun(ctx, store.CommitRunRequest{Guard: g, OperationID: "running", Status: domain.RunRunning, ExpectedSessionVersion: 2, Checkpoint: &cp})
	check(t, err)
	cp = *setup.Checkpoint
	req := trackRequest(freshGuard(t, db, g.Lease), cp, "task-1")
	injected := errors.New("injected crash")
	db.beforeCommit = func(name string) error {
		if name == "track" {
			return injected
		}
		return nil
	}
	_, err = db.TrackTask(ctx, req)
	if !errors.Is(err, injected) {
		t.Fatal(err)
	}
	db.beforeCommit = nil
	_, err = db.GetTask(ctx, testScope, "task-1")
	wantKind(t, err, apperrors.ErrNotFound)
	first, err := db.TrackTask(ctx, req)
	check(t, err)
	cp, err = db.GetCheckpoint(ctx, testScope, "run", "branch")
	check(t, err)
	second, err := db.TrackTask(ctx, trackRequest(freshGuard(t, db, g.Lease), cp, "task-2"))
	check(t, err)
	replayed, err := db.TrackTask(ctx, req)
	check(t, err)
	if replayed.Task.ID != first.Task.ID || replayed.CheckpointVersion != second.CheckpointVersion {
		t.Fatal("Track retry overwrote progressed checkpoint")
	}
	observe := store.ObserveTaskRequest{Guard: freshGuard(t, db, g.Lease), TaskID: first.Task.ID, ExpectedTaskVersion: first.Task.Version, Update: domain.TaskUpdate{Status: domain.TaskSucceeded, Result: &domain.ToolResult{CallID: first.Task.Call.ID, Parts: []domain.Part{{Text: "found"}}}}, Events: []domain.Event{{Scope: testScope, RunID: "run", Kind: domain.EventToolFinished}}}
	db.beforeCommit = func(name string) error {
		if name == "observe" {
			return injected
		}
		return nil
	}
	_, err = db.ObserveTask(ctx, observe)
	if !errors.Is(err, injected) {
		t.Fatal(err)
	}
	db.beforeCommit = nil
	task, err := db.GetTask(ctx, testScope, first.Task.ID)
	check(t, err)
	if task.Status != domain.TaskSubmitted {
		t.Fatal("terminal escaped failed transaction")
	}
	_, err = db.GetTaskDelivery(ctx, testScope, task.ID)
	wantKind(t, err, apperrors.ErrNotFound)
	terminal, err := db.ObserveTask(ctx, observe)
	check(t, err)
	delivery, err := db.GetTaskDelivery(ctx, testScope, task.ID)
	check(t, err)
	if delivery.State != domain.DeliveryPending {
		t.Fatal("missing recovery intent")
	}
	// 模拟提交后进程退出：释放租约，使用新连接重读 pending 及检查点后续接。
	check(t, db.ReleaseLease(ctx, g.Lease))
	restarted, err := Open(ctx, cfg)
	check(t, err)
	defer restarted.Close(ctx)
	l, err := restarted.AcquireLease(ctx, testScope, "run", "restart", time.Minute)
	check(t, err)
	cp, err = restarted.GetCheckpoint(ctx, testScope, "run", "branch")
	check(t, err)
	cp.PendingCallIDs = []string{second.Task.Call.ID}
	cp.Data = []byte("result accepted")
	apply := store.ApplyTaskRequest{Guard: freshGuard(t, restarted, l), OperationID: "consume-first", TaskID: task.ID, ExpectedTaskVersion: terminal.Task.Version, ExpectedDeliveryVersion: delivery.Version, ExpectedSessionVersion: 2, Checkpoint: cp, Messages: []domain.Message{{Scope: testScope, SessionID: "session", RunID: "run", ID: "tool-result", Role: domain.RoleTool, Parts: []domain.Part{{ToolCallID: task.Call.ID, Text: "found"}}}}}
	bad := apply
	bad.Checkpoint.Caller.InvocationID = "other-branch"
	_, err = restarted.ApplyTask(ctx, bad)
	wantKind(t, err, apperrors.ErrInvalidArgument)
	restarted.beforeCommit = func(name string) error {
		if name == "apply" {
			return injected
		}
		return nil
	}
	_, err = restarted.ApplyTask(ctx, apply)
	if !errors.Is(err, injected) {
		t.Fatal(err)
	}
	restarted.beforeCommit = nil
	task, err = restarted.GetTask(ctx, testScope, task.ID)
	check(t, err)
	if task.AppliedAt != nil {
		t.Fatal("AppliedAt escaped rollback")
	}
	checkCP, err := restarted.GetCheckpoint(ctx, testScope, "run", "branch")
	check(t, err)
	if len(checkCP.PendingCallIDs) != 2 {
		t.Fatal("checkpoint escaped rollback")
	}
	applied, err := restarted.ApplyTask(ctx, apply)
	check(t, err)
	again, err := restarted.ApplyTask(ctx, apply)
	check(t, err)
	if !reflect.DeepEqual(applied, again) {
		t.Fatal("Apply not replayable")
	}
	page, err := restarted.ListUnsettledTasks(ctx, testScope, "run", store.KeyPage{Limit: 10})
	check(t, err)
	if len(page.Items) != 1 || page.Items[0].ID != second.Task.ID {
		t.Fatal("consumed task still unsettled")
	}
	cancelled, err := restarted.CancelRun(ctx, store.CancelRunRequest{Scope: testScope, RunID: "run", ExpectedRunVersion: applied.Run.Version})
	check(t, err)
	_, err = restarted.ObserveTask(ctx, store.ObserveTaskRequest{Guard: store.WriteGuard{Lease: l, RunVersion: cancelled.Run.Version}, TaskID: second.Task.ID, ExpectedTaskVersion: 1, Update: domain.TaskUpdate{Status: domain.TaskSucceeded}})
	wantKind(t, err, apperrors.ErrConflict)
	maintenance, err := restarted.AcquireLease(ctx, testScope, "run", "maintenance", time.Minute)
	check(t, err)
	late, err := restarted.ObserveTask(ctx, store.ObserveTaskRequest{Guard: freshGuard(t, restarted, maintenance), TaskID: second.Task.ID, ExpectedTaskVersion: 1, Update: domain.TaskUpdate{Status: domain.TaskSucceeded}})
	check(t, err)
	if len(late.Events) != 0 {
		t.Fatal("reopened terminal event stream")
	}
	delivery, err = restarted.GetTaskDelivery(ctx, testScope, second.Task.ID)
	check(t, err)
	discardGuard := freshGuard(t, restarted, maintenance)
	check(t, restarted.DiscardTask(ctx, discardGuard, second.Task.ID, delivery.Version))
	check(t, restarted.DiscardTask(ctx, discardGuard, second.Task.ID, delivery.Version))
	page, err = restarted.ListUnsettledTasks(ctx, testScope, "run", store.KeyPage{Limit: 10})
	check(t, err)
	if len(page.Items) != 0 {
		t.Fatal("discarded task remains unsettled")
	}
	task, err = restarted.GetTask(ctx, testScope, second.Task.ID)
	check(t, err)
	if task.AppliedAt != nil || task.Status != domain.TaskSucceeded {
		t.Fatal("discard changed actual remote outcome")
	}
}

func TestMongoSnapshotsAndRecoveryScan(t *testing.T) {
	db, cfg := testDatabase(t)
	ctx := context.Background()
	_, g := startFixture(t, db)
	snap := domain.ContextSnapshot{Scope: testScope, ID: "snapshot", SessionID: "session", ThroughSequence: 1, Summary: "summary", TokenEstimate: 1, PolicyVersion: "v1"}
	saved, err := db.SaveSnapshot(ctx, g, snap, 0, 2)
	check(t, err)
	if saved.Version != 1 {
		t.Fatal("snapshot version")
	}
	_, err = db.SaveSnapshot(ctx, freshGuard(t, db, g.Lease), snap, 0, 2)
	wantKind(t, err, apperrors.ErrConflict)
	latest, err := db.LatestSnapshot(ctx, testScope, "session")
	check(t, err)
	if !reflect.DeepEqual(latest, saved) {
		t.Fatal("latest snapshot mismatch")
	}
	saved.ThroughSequence = 2
	_, err = db.SaveSnapshot(ctx, freshGuard(t, db, g.Lease), saved, 1, 2)
	wantKind(t, err, apperrors.ErrInvalidArgument)
	admin, err := OpenRecovery(ctx, cfg)
	check(t, err)
	defer admin.Close(ctx)
	p, err := admin.Scan(ctx, nil, 1)
	check(t, err)
	if len(p.Items) != 0 {
		t.Fatal("leased run in recovery scan")
	}
	check(t, db.ReleaseLease(ctx, g.Lease))
	other := domain.Scope{TenantID: "z-tenant", UserID: "user"}
	newSession(t, db, other, "session")
	_, err = db.StartRun(ctx, startRequest(other, "session", "run", "", 1))
	check(t, err)
	p, err = admin.Scan(ctx, nil, 1)
	check(t, err)
	if len(p.Items) != 1 || !p.HasMore || p.Items[0].Scope != testScope {
		t.Fatal("first recovery page")
	}
	q, err := admin.Scan(ctx, p.Next, 1)
	check(t, err)
	if len(q.Items) != 1 || q.HasMore || q.Items[0].Scope != other {
		t.Fatal("cross-tenant same ID omitted")
	}
	_, err = admin.Scan(ctx, &store.RecoveryPosition{RunID: "run"}, 1)
	wantKind(t, err, apperrors.ErrInvalidArgument)
	// Due time precedes identity in the indexed cursor. A fresh scan still finds
	// candidates created with an earlier identity after the preceding page.
	earlier := domain.Scope{TenantID: "a-tenant", UserID: "user"}
	newSession(t, db, earlier, "session")
	_, err = db.StartRun(ctx, startRequest(earlier, "session", "run", "", 1))
	check(t, err)
	p, err = admin.Scan(ctx, nil, 10)
	check(t, err)
	found := false
	for _, candidate := range p.Items {
		found = found || candidate.Scope == earlier
	}
	if !found {
		t.Fatal("rescan missed earlier candidate")
	}
}

// 防止测试文件仅验证接口占位；这条查询验证真实 server 有活动副本集事务能力。
func TestMongoRealServer(t *testing.T) {
	db, _ := testDatabase(t)
	var reply schema.Hello
	check(t, db.client.Database("admin").RunCommand(context.Background(), bson.D{{Key: "hello", Value: 1}}).Decode(&reply))
	if reply.SetName == "" {
		t.Fatal("integration must use a real replica set")
	}
}

func TestMongoMetadataAndTerminalAtTrack(t *testing.T) {
	db, _ := testDatabase(t)
	ctx := context.Background()
	_, g := startFixture(t, db)
	cp := domain.Checkpoint{Scope: testScope, RunID: "run", Caller: domain.AgentExecution{AgentID: "agent", InvocationID: "branch"}, Format: "v1"}
	req := trackRequest(g, cp, "ready")
	req.Task.Status = domain.TaskSucceeded
	req.Task.Result = &domain.ToolResult{CallID: req.Task.Call.ID, Parts: []domain.Part{}}
	out, err := db.TrackTask(ctx, req)
	check(t, err)
	delivery, err := db.GetTaskDelivery(ctx, testScope, "ready")
	check(t, err)
	if delivery.State != domain.DeliveryPending {
		t.Fatal("already-terminal handle has no recovery intent")
	}
	unchanged, err := db.ObserveTask(ctx, store.ObserveTaskRequest{Guard: freshGuard(t, db, g.Lease), TaskID: "ready", ExpectedTaskVersion: 1, Update: domain.TaskUpdate{Status: domain.TaskSucceeded, Result: req.Task.Result}, Events: []domain.Event{{Scope: testScope, RunID: "run", Kind: domain.EventToolProgress}}})
	check(t, err)
	if unchanged.RunVersion != out.RunVersion || unchanged.Task.Version != 1 || len(unchanged.Events) != 0 {
		t.Fatal("duplicate terminal caused writes")
	}
	cp, err = db.GetCheckpoint(ctx, testScope, "run", "branch")
	check(t, err)
	active, err := db.TrackTask(ctx, trackRequest(freshGuard(t, db, g.Lease), cp, "active"))
	check(t, err)
	meta := store.TaskMetadataRequest{Guard: freshGuard(t, db, g.Lease), TaskID: active.Task.ID, ExpectedTaskVersion: 1, ObservationError: "observation timed out"}
	failedObservation, err := db.RecordTaskObservationError(ctx, meta)
	check(t, err)
	if failedObservation.Task.Status != domain.TaskSubmitted || failedObservation.Task.ObservationError == "" {
		t.Fatal("observation failure inferred remote failure")
	}
	meta.Guard = freshGuard(t, db, g.Lease)
	meta.ExpectedTaskVersion = failedObservation.Task.Version
	again, err := db.RecordTaskObservationError(ctx, meta)
	check(t, err)
	if again.Task.Version != failedObservation.Task.Version || again.RunVersion != failedObservation.RunVersion {
		t.Fatal("identical metadata updated versions")
	}
	cancelRequested, err := db.RequestTaskCancel(ctx, meta)
	check(t, err)
	if cancelRequested.Task.Status != domain.TaskSubmitted || cancelRequested.Task.CancelRequestedAt == nil {
		t.Fatal("cancel request inferred terminal")
	}
	observed, err := db.ObserveTask(ctx, store.ObserveTaskRequest{Guard: freshGuard(t, db, g.Lease), TaskID: active.Task.ID, ExpectedTaskVersion: cancelRequested.Task.Version, Update: domain.TaskUpdate{Status: domain.TaskInputRequired}})
	check(t, err)
	if observed.Task.ObservationError != "" {
		t.Fatal("successful observation did not clear error")
	}
	_, err = db.ObserveTask(ctx, store.ObserveTaskRequest{Guard: freshGuard(t, db, g.Lease), TaskID: "", ExpectedTaskVersion: 1, Update: domain.TaskUpdate{Status: domain.TaskSucceeded}})
	wantKind(t, err, apperrors.ErrInvalidArgument)
	// 真实数据库破坏恢复不变量时不能把任务视为已消费（只删除本测试库的记录）。
	_, err = db.collection(TaskDeliveryCollection).DeleteOne(ctx, key(testScope, "ready"))
	check(t, err)
	_, err = db.ListUnsettledTasks(ctx, testScope, "run", store.KeyPage{Limit: 10})
	if err == nil {
		t.Fatal("missing delivery silently ignored")
	}
}

func TestMongoCancelVersusApplyAcrossClients(t *testing.T) {
	db, cfg := testDatabase(t)
	ctx := context.Background()
	_, g := startFixture(t, db)
	cp := domain.Checkpoint{Scope: testScope, RunID: "run", Caller: domain.AgentExecution{AgentID: "agent", InvocationID: "branch"}, Format: "v1"}
	req := trackRequest(g, cp, "ready")
	req.Task.Status = domain.TaskSucceeded
	tracked, err := db.TrackTask(ctx, req)
	check(t, err)
	cp, err = db.GetCheckpoint(ctx, testScope, "run", "branch")
	check(t, err)
	cp.PendingCallIDs = nil
	cp.Data = []byte("accepted")
	apply := store.ApplyTaskRequest{Guard: freshGuard(t, db, g.Lease), OperationID: "apply", TaskID: "ready", ExpectedTaskVersion: 1, ExpectedDeliveryVersion: 1, ExpectedSessionVersion: 2, Checkpoint: cp}
	other, err := Open(ctx, cfg)
	check(t, err)
	defer other.Close(ctx)
	var applyErr, cancelErr error
	var wg sync.WaitGroup
	wg.Add(2)
	gate := make(chan struct{})
	go func() { defer wg.Done(); <-gate; _, applyErr = db.ApplyTask(ctx, apply) }()
	go func() {
		defer wg.Done()
		<-gate
		_, cancelErr = other.CancelRun(ctx, store.CancelRunRequest{Scope: testScope, RunID: "run", ExpectedRunVersion: tracked.RunVersion})
	}()
	close(gate)
	wg.Wait()
	if (applyErr == nil) == (cancelErr == nil) {
		t.Fatalf("expected exactly one CAS winner: apply=%v cancel=%v", applyErr, cancelErr)
	}
	if applyErr != nil {
		wantKind(t, applyErr, apperrors.ErrConflict)
	}
	if cancelErr != nil {
		wantKind(t, cancelErr, apperrors.ErrConflict)
		run, err := db.GetRun(ctx, testScope, "run")
		check(t, err)
		_, err = other.CancelRun(ctx, store.CancelRunRequest{Scope: testScope, RunID: "run", ExpectedRunVersion: run.Version})
		check(t, err)
	}
	task, err := db.GetTask(ctx, testScope, "ready")
	check(t, err)
	delivery, err := db.GetTaskDelivery(ctx, testScope, "ready")
	check(t, err)
	if applyErr == nil {
		if task.AppliedAt == nil || delivery.State != domain.DeliveryApplied {
			t.Fatal("winning apply lost")
		}
	} else {
		if task.AppliedAt != nil || delivery.State != domain.DeliveryPending {
			t.Fatal("losing apply partially committed")
		}
	}
	p, err := db.ListEvents(ctx, testScope, "run", store.SequencePage{Limit: 10})
	check(t, err)
	if p.Items[len(p.Items)-1].Kind != domain.EventRunCancelled {
		t.Fatal("cancel missing last event")
	}
}

func TestMongoRejectStandaloneAndUnsafeCollation(t *testing.T) {
	if executable := os.Getenv("CAGENT_TEST_MONGOD"); executable != "" {
		uri, stop, err := startMongo(executable, false)
		check(t, err)
		defer stop()
		_, err = Open(context.Background(), Options{URI: uri, Database: "unused", Timeout: 3 * time.Second})
		wantKind(t, err, apperrors.ErrUnsupported)
	}
	db, cfg := testDatabase(t)
	ctx := context.Background()
	// 集合中没有业务数据；改为不区分大小写后，重新打开必须拒绝以免合并用户标识。
	check(t, db.collection(SessionCollection).Drop(ctx))
	check(t, db.db.CreateCollection(ctx, "sessions", options.CreateCollection().SetCollation(&options.Collation{Locale: "en", Strength: 2})))
	_, err := Open(ctx, cfg)
	wantKind(t, err, apperrors.ErrUnsupported)
	_, err = OpenRecovery(ctx, cfg)
	wantKind(t, err, apperrors.ErrUnsupported)
}

// 仅对本测试启动的临时实例打开服务器 failpoint，绝不修改外部共享测试集群。
// commit 已执行但返回 UnknownTransactionCommitResult，验证驱动只重试提交，
// 不重复输出；随后应用层相同 OperationID 重试仍读取原回执。
func TestMongoUncertainCommitAndContextCancellation(t *testing.T) {
	if os.Getenv("CAGENT_TEST_MONGOD") == "" {
		t.Skip("server failpoint requires the isolated local mongod fixture")
	}
	db, _ := testDatabase(t)
	ctx := context.Background()
	_, g := startFixture(t, db)
	var before, after schema.Count
	check(t, db.client.Database("admin").RunCommand(ctx, bson.D{{Key: "configureFailPoint", Value: "failCommand"}, {Key: "mode", Value: bson.M{"times": 1}}, {Key: "data", Value: bson.M{"failCommands": bson.A{"commitTransaction"}, "appName": "cagent-storage", "writeConcernError": bson.M{"code": 64, "errmsg": "injected uncertain commit"}, "errorLabels": bson.A{"UnknownTransactionCommitResult"}}}}).Decode(&before))
	t.Cleanup(func() {
		_ = db.client.Database("admin").RunCommand(context.Background(), bson.D{{Key: "configureFailPoint", Value: "failCommand"}, {Key: "mode", Value: "off"}}).Err()
	})
	req := store.CommitRunRequest{Guard: g, OperationID: "uncertain", Status: domain.RunRunning, ExpectedSessionVersion: 2, Events: []domain.Event{{Scope: testScope, RunID: "run", Kind: domain.EventTextDelta}}}
	out, err := db.CommitRun(ctx, req)
	check(t, err)
	check(t, db.client.Database("admin").RunCommand(ctx, bson.D{{Key: "configureFailPoint", Value: "failCommand"}, {Key: "mode", Value: "off"}}).Decode(&after))
	if after.Count <= before.Count {
		t.Fatal("server failpoint was not exercised")
	}
	replay, err := db.CommitRun(ctx, req)
	check(t, err)
	if !reflect.DeepEqual(out, replay) {
		t.Fatal("uncertain commit replay differed")
	}
	p, err := db.ListEvents(ctx, testScope, "run", store.SequencePage{Limit: 10})
	check(t, err)
	if len(p.Items) != 1 || p.LastSequence != 1 {
		t.Fatal("commit retry duplicated events")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = db.GetRun(cancelled, testScope, "run")
	if !errors.Is(err, context.Canceled) {
		t.Fatal("lost context cancellation", err)
	}
}
