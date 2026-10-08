package app

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hjhsamuel/cagent/internal/adapter/mongodb"
	"github.com/hjhsamuel/cagent/internal/agent"
	"github.com/hjhsamuel/cagent/internal/apperrors"
	"github.com/hjhsamuel/cagent/internal/domain"
	"github.com/hjhsamuel/cagent/internal/store"
)

type runtimeFunc func(context.Context, agent.Request, agent.Emit) error

func (f runtimeFunc) Execute(c context.Context, r agent.Request, e agent.Emit) error {
	return f(c, r, e)
}
func (f runtimeFunc) Resume(context.Context, agent.Request, agent.Continuation, agent.Emit) error {
	return apperrors.ErrUnsupported
}

var scope = domain.Scope{TenantID: "tenant", UserID: "user"}

func service(t *testing.T, db *mongodb.Database, f runtimeFunc) *Application {
	t.Helper()
	a, e := NewService(context.Background(), db, f, Options{LeaseDuration: 3 * time.Second, PollInterval: 30 * time.Millisecond})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		ctx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		if e := a.Close(ctx); e != nil {
			t.Error(e)
		}
	})
	return a
}
func session(t *testing.T, a *Application) domain.Session {
	t.Helper()
	s, e := a.CreateSession(context.Background(), scope, "agent")
	if e != nil {
		t.Fatal(e)
	}
	return s
}
func start(t *testing.T, a *Application, s domain.Session, key string) domain.Run {
	t.Helper()
	r, e := a.StartRun(context.Background(), StartRun{Scope: s.Scope, SessionID: s.ID, IdempotencyKey: key, Input: []domain.Part{{Kind: "text", Text: "hello"}}})
	if e != nil {
		t.Fatal(e)
	}
	return r
}
func awaitStatus(t *testing.T, db *mongodb.Database, r domain.Run, status domain.RunStatus) domain.Run {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		v, e := db.GetRun(context.Background(), r.Scope, r.ID)
		if e != nil {
			t.Fatal(e)
		}
		if v.Status == status {
			return v
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("run did not reach %s", status)
	return domain.Run{}
}

// 客户端请求退出不取消运行；只有完整消息进入历史，重放包含连续序号与唯一终止事件。
func TestLifecycleReplayAndRequestDisconnect(t *testing.T) {
	db, _ := testDatabase(t)
	entered, release := make(chan struct{}), make(chan struct{})
	a := service(t, db, func(ctx context.Context, r agent.Request, emit agent.Emit) error {
		if len(r.Messages) != 1 || r.Messages[0].Role != domain.RoleUser {
			return fmt.Errorf("input duplicated")
		}
		close(entered)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-release:
		}
		return emit(ctx, agent.Update{Kind: domain.EventTextDelta, Data: []byte("answer"), Message: []domain.Part{{Kind: "text", Text: "answer"}}})
	})
	s := session(t, a)
	ctx, cancel := context.WithCancel(context.Background())
	r, e := a.StartRun(ctx, StartRun{Scope: scope, SessionID: s.ID, Input: []domain.Part{{Text: "hello"}}})
	if e != nil {
		t.Fatal(e)
	}
	<-entered
	cancel()
	close(release)
	awaitStatus(t, db, r, domain.RunCompleted)
	events, _ := NewEventStream(db, time.Millisecond, 1)
	var seen []domain.Event
	if e = events.Follow(context.Background(), scope, r.ID, 0, func(v domain.Event) error { seen = append(seen, v); return nil }); e != nil {
		t.Fatal(e)
	}
	if len(seen) != 3 || seen[2].Kind != domain.EventRunCompleted {
		t.Fatalf("events: %+v", seen)
	}
	for i, v := range seen {
		if v.Sequence != int64(i+1) {
			t.Fatal("sequence gap")
		}
	}
	if e = events.Follow(context.Background(), scope, r.ID, 3, func(domain.Event) error { t.Fatal("unexpected replay"); return nil }); e != nil {
		t.Fatal(e)
	}
	page, e := db.ListMessages(context.Background(), scope, s.ID, store.SequencePage{Limit: 10})
	if e != nil || len(page.Items) != 2 || page.Items[1].Parts[0].Text != "answer" {
		t.Fatalf("messages: %+v %v", page, e)
	}
	again := start(t, a, s, "second")
	awaitStatus(t, db, again, domain.RunFailed) // 替身拒绝重复历史形状，验证异常终态释放占用。
}

func TestConcurrentIdempotencyAndIsolation(t *testing.T) {
	db, cfg := testDatabase(t)
	other, e := mongodb.Open(context.Background(), cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer other.Close(context.Background())
	var calls atomic.Int32
	f := runtimeFunc(func(ctx context.Context, _ agent.Request, _ agent.Emit) error {
		calls.Add(1)
		<-ctx.Done()
		return ctx.Err()
	})
	a, b := service(t, db, f), service(t, other, f)
	s := session(t, a)
	const n = 12
	results := make(chan domain.Run, n)
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			svc := a
			if i%2 == 1 {
				svc = b
			}
			r, e := svc.StartRun(context.Background(), StartRun{Scope: scope, SessionID: s.ID, IdempotencyKey: "same", Input: []domain.Part{{Text: "same"}}})
			results <- r
			errs <- e
		}(i)
	}
	wg.Wait()
	close(results)
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	var id string
	for r := range results {
		if id != "" && r.ID != id {
			t.Fatal("duplicate run")
		}
		id = r.ID
	}
	r := domain.Run{Scope: scope, ID: id}
	awaitStatus(t, db, r, domain.RunRunning)
	_, e = a.StartRun(context.Background(), StartRun{Scope: scope, SessionID: s.ID, IdempotencyKey: "same", Input: []domain.Part{{Text: "different"}}})
	if !errors.Is(e, apperrors.ErrConflict) {
		t.Fatalf("different input: %v", e)
	}
	_, e = a.StartRun(context.Background(), StartRun{Scope: scope, SessionID: s.ID})
	if !errors.Is(e, apperrors.ErrConflict) {
		t.Fatalf("active session: %v", e)
	}
	foreign := domain.Scope{TenantID: scope.TenantID, UserID: "other"}
	if _, e = a.GetRun(context.Background(), foreign, id); !errors.Is(e, apperrors.ErrNotFound) {
		t.Fatal(e)
	}
	if e = a.CancelRun(context.Background(), foreign, id); !errors.Is(e, apperrors.ErrNotFound) {
		t.Fatal(e)
	}
	if e = b.CancelRun(context.Background(), scope, id); e != nil {
		t.Fatal(e)
	}
	if e = a.CancelRun(context.Background(), scope, id); e != nil {
		t.Fatal(e)
	}
	awaitStatus(t, db, r, domain.RunCancelled)
	if calls.Load() != 1 {
		t.Fatalf("executions: %d", calls.Load())
	}
}

// 从独立客户端提交事件，并在消费第一页时追加，验证追赶/跟随没有本地通知缺口。
// 回调暂时阻塞不阻止写入；恢复后按持久游标继续，不依赖无界内存队列。
func TestCrossInstanceFollowBackpressureAndFailure(t *testing.T) {
	db, cfg := testDatabase(t)
	writer, e := mongodb.Open(context.Background(), cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer writer.Close(context.Background())
	s := domain.Session{Scope: scope, ID: newID(), AgentID: "agent"}
	if e = db.CreateSession(context.Background(), s); e != nil {
		t.Fatal(e)
	}
	r := domain.Run{Scope: scope, ID: newID(), SessionID: s.ID, Status: domain.RunQueued}
	out, e := db.StartRun(context.Background(), store.StartRunRequest{Run: r, Input: domain.Message{Scope: scope, ID: newID(), SessionID: s.ID, RunID: r.ID, Role: domain.RoleUser}, ExpectedSessionVersion: 1})
	if e != nil {
		t.Fatal(e)
	}
	lease, e := writer.AcquireLease(context.Background(), scope, r.ID, "writer", time.Minute)
	if e != nil {
		t.Fatal(e)
	}
	stream, _ := NewEventStream(db, 5*time.Millisecond, 1)
	publish, _ := NewEventStream(writer, time.Millisecond, 1)
	req := store.CommitRunRequest{Guard: store.WriteGuard{Lease: lease, RunVersion: out.Run.Version}, OperationID: newID(), Status: domain.RunRunning, ExpectedSessionVersion: out.SessionVersion, Events: []domain.Event{{Scope: scope, RunID: r.ID, Kind: domain.EventTextDelta}}}
	first, e := publish.Publish(context.Background(), req)
	if e != nil {
		t.Fatal(e)
	}
	replay, e := publish.Publish(context.Background(), req)
	if e != nil || replay.Events[0].Sequence != 1 {
		t.Fatal("receipt replay", e)
	}
	req.OperationID = newID() // 旧 RunVersion 必须失败，不能发布幽灵事件。
	if _, e = publish.Publish(context.Background(), req); !errors.Is(e, apperrors.ErrConflict) {
		t.Fatal(e)
	}
	paused, resume := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	var seen []int64
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() {
		done <- stream.Follow(ctx, scope, r.ID, 0, func(event domain.Event) error {
			seen = append(seen, event.Sequence)
			if event.Sequence == 1 {
				close(paused)
				select {
				case <-resume:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return nil
		})
	}()
	<-paused
	req.Guard.RunVersion = first.Run.Version
	req.OperationID = newID()
	req.Status = domain.RunCompleted
	final, e := publish.Publish(context.Background(), req)
	if e != nil {
		t.Fatal(e)
	}
	close(resume)
	if e = <-done; e != nil {
		t.Fatal(e)
	}
	if fmt.Sprint(seen) != "[1 2 3]" {
		t.Fatalf("gap or phantom: %v", seen)
	}
	if e = stream.Follow(ctx, scope, r.ID, final.Events[len(final.Events)-1].Sequence+1, func(domain.Event) error { return nil }); !errors.Is(e, apperrors.ErrInvalidArgument) {
		t.Fatal(e)
	}
	sentinel := errors.New("callback failure")
	if e = stream.Follow(ctx, scope, r.ID, 0, func(domain.Event) error { return sentinel }); !errors.Is(e, sentinel) {
		t.Fatal(e)
	}
	recovery, e := mongodb.OpenRecovery(context.Background(), cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer recovery.Close(context.Background())
	if e = recovery.PruneEvents(context.Background(), scope, r.ID, 1); e != nil {
		t.Fatal(e)
	}
	if e = stream.Follow(ctx, scope, r.ID, 0, func(domain.Event) error { return nil }); !errors.Is(e, store.ErrCursorExpired) {
		t.Fatal(e)
	}
}

func TestRemoteCancellationStopsRuntimeAndFencesLateOutput(t *testing.T) {
	db, cfg := testDatabase(t)
	remote, e := mongodb.Open(context.Background(), cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer remote.Close(context.Background())
	stopped := make(chan error, 1)
	a := service(t, db, func(ctx context.Context, _ agent.Request, emit agent.Emit) error {
		<-ctx.Done()
		stopped <- emit(context.Background(), agent.Update{Kind: domain.EventTextDelta, Data: []byte("late")})
		return nil
	})
	s := session(t, a)
	r := start(t, a, s, "")
	current := awaitStatus(t, db, r, domain.RunRunning)
	if _, e = remote.CancelRun(context.Background(), store.CancelRunRequest{Scope: scope, RunID: r.ID, ExpectedRunVersion: current.Version}); e != nil {
		t.Fatal(e)
	}
	select {
	case e = <-stopped:
		if e == nil {
			t.Fatal("late write accepted")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("remote cancellation not observed")
	}
	page, e := db.ListEvents(context.Background(), scope, r.ID, store.SequencePage{Limit: 10})
	if e != nil || len(page.Items) != 2 || page.Items[1].Kind != domain.EventRunCancelled {
		t.Fatalf("late event: %+v %v", page, e)
	}
}

func TestCloseAndRecoveryDoNotReexecuteStartedRun(t *testing.T) {
	db, _ := testDatabase(t)
	a := service(t, db, func(ctx context.Context, _ agent.Request, _ agent.Emit) error { <-ctx.Done(); return ctx.Err() })
	s := session(t, a)
	r := start(t, a, s, "")
	awaitStatus(t, db, r, domain.RunRunning)
	if e := a.Close(context.Background()); e != nil {
		t.Fatal(e)
	}
	if _, e := a.StartRun(context.Background(), StartRun{Scope: scope, SessionID: s.ID}); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	b := service(t, db, func(context.Context, agent.Request, agent.Emit) error {
		t.Error("unsafe Execute during recovery")
		return nil
	})
	if e := b.RecoverRun(context.Background(), scope, r.ID); !errors.Is(e, apperrors.ErrUnsupported) {
		t.Fatal(e)
	}
	b.opts.Recover = func(ctx context.Context, r agent.Request, emit agent.Emit) error {
		return emit(ctx, agent.Update{Kind: domain.EventTextDelta, Data: []byte("resumed")})
	}
	if e := b.RecoverRun(context.Background(), scope, r.ID); e != nil {
		t.Fatal(e)
	}
	awaitStatus(t, db, r, domain.RunCompleted)
}

// 空闲轮询期间的新提交来自数据库；订阅取消只结束 Follow，不取消执行上下文。
// 运行跨越多个租期后仍能写入，证明续期更新了业务提交使用的凭证。
func TestIdleFollowCancellationAndLeaseRenewal(t *testing.T) {
	db, _ := testDatabase(t)
	release := make(chan struct{})
	entered := make(chan struct{})
	a, e := NewService(context.Background(), db, runtimeFunc(func(ctx context.Context, _ agent.Request, emit agent.Emit) error {
		close(entered)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-release:
		}
		return emit(ctx, agent.Update{Kind: domain.EventTextDelta, Data: []byte("after renewals")})
	}), Options{LeaseDuration: 900 * time.Millisecond, PollInterval: 100 * time.Millisecond})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { a.Close(context.Background()) })
	s := session(t, a)
	r := start(t, a, s, "")
	<-entered
	events, _ := NewEventStream(db, 10*time.Millisecond, 1)
	cancelledCtx, stop := context.WithCancel(context.Background())
	stop()
	if e = events.Follow(cancelledCtx, scope, r.ID, 1, func(domain.Event) error { return nil }); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	var count int
	go func() { done <- events.Follow(ctx, scope, r.ID, 1, func(domain.Event) error { count++; return nil }) }()
	time.Sleep(2 * time.Second)
	current, e := a.GetRun(ctx, scope, r.ID)
	if e != nil || current.Status != domain.RunRunning {
		t.Fatalf("subscription cancelled run: %+v %v", current, e)
	}
	close(release)
	if e = <-done; e != nil {
		t.Fatal(e)
	}
	if count != 2 {
		t.Fatalf("idle follow count: %d", count)
	}
	awaitStatus(t, db, r, domain.RunCompleted)
}

func TestQueuedRecoveryAndRuntimeEmissionContract(t *testing.T) {
	db, _ := testDatabase(t)
	var late agent.Emit
	a := service(t, db, func(ctx context.Context, _ agent.Request, emit agent.Emit) error {
		late = emit
		// 即使运行时忽略非法输出错误，应用也不能把运行标记为 completed。
		_ = emit(ctx, agent.Update{Kind: domain.EventRunCompleted})
		return nil
	})
	s := session(t, a)
	r := domain.Run{Scope: scope, ID: newID(), SessionID: s.ID, Status: domain.RunQueued}
	_, e := db.StartRun(context.Background(), store.StartRunRequest{Run: r, Input: domain.Message{Scope: scope, ID: newID(), SessionID: s.ID, RunID: r.ID, Role: domain.RoleUser}, ExpectedSessionVersion: s.Version})
	if e != nil {
		t.Fatal(e)
	}
	if e = a.RecoverRun(context.Background(), scope, r.ID); e != nil {
		t.Fatal(e)
	}
	awaitStatus(t, db, r, domain.RunFailed)
	if e = a.Close(context.Background()); e != nil {
		t.Fatal(e)
	} // 等待工作退出，建立对回调变量的同步。
	if e = late(context.Background(), agent.Update{Kind: domain.EventTextDelta}); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	page, e := db.ListEvents(context.Background(), scope, r.ID, store.SequencePage{Limit: 10})
	if e != nil || len(page.Items) != 2 || page.Items[1].Kind != domain.EventRunFailed {
		t.Fatalf("invalid terminal: %+v %v", page, e)
	}
}

func TestApplicationOptionValidation(t *testing.T) {
	if _, e := NewEventStream(nil, time.Second, 1); !errors.Is(e, apperrors.ErrInvalidArgument) {
		t.Fatal(e)
	}
	for _, limit := range []int{0, -1, 1000000} {
		if _, e := NewEventStream(&mongodb.Database{}, time.Second, limit); !errors.Is(e, apperrors.ErrInvalidArgument) {
			t.Fatal(e)
		}
	}
	f := runtimeFunc(func(context.Context, agent.Request, agent.Emit) error { return nil })
	if _, e := NewService(context.Background(), &mongodb.Database{}, f, Options{LeaseDuration: time.Second, PollInterval: time.Second}); !errors.Is(e, apperrors.ErrInvalidArgument) {
		t.Fatal(e)
	}
	stream, _ := NewEventStream(&mongodb.Database{}, time.Second, 1)
	if e := stream.Follow(context.Background(), scope, "run", 0, nil); !errors.Is(e, apperrors.ErrInvalidArgument) {
		t.Fatal(e)
	}
}
