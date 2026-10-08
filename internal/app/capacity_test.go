package app

import (
	"context"
	"errors"
	"github.com/hjhsamuel/cagent/internal/agent"
	"github.com/hjhsamuel/cagent/internal/apperrors"
	"github.com/hjhsamuel/cagent/internal/config"
	"github.com/hjhsamuel/cagent/internal/domain"
	"github.com/hjhsamuel/cagent/internal/observability"
	"github.com/hjhsamuel/cagent/internal/store"
	"testing"
	"time"
)

// 真实事务证明拒绝发生在落库前；满载幂等重放仍校验原始输入，不产生第二次执行。
func TestRunCapacityRejectsBeforePersistenceAndReleases(t *testing.T) {
	db, _ := testDatabase(t)
	entered := make(chan struct{}, 2)
	a, err := NewService(context.Background(), db, runtimeFunc(func(ctx context.Context, _ agent.Request, _ agent.Emit) error {
		entered <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	}), Options{LeaseDuration: time.Second, PollInterval: 20 * time.Millisecond, Capacity: config.Capacity{Runs: 1, Models: 1, Observations: 1}})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(context.Background())
	s1, s2 := session(t, a), session(t, a)
	r := start(t, a, s1, "key")
	<-entered
	blocked := StartRun{Scope: scope, SessionID: s2.ID, IdempotencyKey: "second", Input: []domain.Part{{Kind: "text", Text: "hello"}}}
	if _, err = a.StartRun(context.Background(), blocked); !errors.Is(err, apperrors.ErrOverloaded) {
		t.Fatal(err)
	}
	saved, err := db.GetSession(context.Background(), scope, s2.ID)
	if err != nil || saved.ActiveRunID != "" || saved.Version != s2.Version {
		t.Fatal("overload mutated session", saved, err)
	}
	replay, err := a.StartRun(context.Background(), StartRun{Scope: scope, SessionID: s1.ID, IdempotencyKey: "key", Input: []domain.Part{{Kind: "text", Text: "hello"}}})
	if err != nil || replay.ID != r.ID {
		t.Fatal("full capacity lost idempotent result", err)
	}
	if _, err = a.StartRun(context.Background(), StartRun{Scope: scope, SessionID: s1.ID, IdempotencyKey: "key", Input: []domain.Part{{Text: "different"}}}); !errors.Is(err, apperrors.ErrConflict) {
		t.Fatal(err)
	}
	if err = a.CancelRun(context.Background(), scope, r.ID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		_, err = a.StartRun(context.Background(), blocked)
		if err == nil {
			break
		}
		if !errors.Is(err, apperrors.ErrOverloaded) || time.Now().After(deadline) {
			t.Fatal("slot not released", err)
		}
		time.Sleep(time.Millisecond)
	}
	<-entered
	if err = a.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	release, err := a.runs.Try(context.Background())
	if err != nil {
		t.Fatal("shutdown leaked slot", err)
	}
	release()
}

// 已满的观察闸门在解析客户端前拒绝，不得通过重启工具来解决本地容量不足。
func TestObservationOverloadDoesNotInventTerminalState(t *testing.T) {
	a := &Application{observations: observability.NewGate(1)}
	release, _ := a.observations.Try(context.Background())
	defer release()
	result, err := a.observeTask(context.Background(), domain.Task{})
	if !errors.Is(err, apperrors.ErrOverloaded) || result.Status.IsTerminal() {
		t.Fatal(result, err)
	}
}

// 恢复没有无限内存队列；候选被跳过后仍是 queued，容量释放后可再次接纳。
func TestRecoveryCapacityPreservesQueuedCandidate(t *testing.T) {
	db, _ := testDatabase(t)
	a := service(t, db, func(context.Context, agent.Request, agent.Emit) error { return nil })
	a.runs = observability.NewGate(1)
	s := session(t, a)
	queued, err := db.StartRun(context.Background(), store.StartRunRequest{Run: domain.Run{Scope: scope, ID: "recover-capacity", SessionID: s.ID, Status: domain.RunQueued}, Input: domain.Message{Scope: scope, ID: "recover-input", SessionID: s.ID, RunID: "recover-capacity", Role: domain.RoleUser, Parts: []domain.Part{{Kind: "text", Text: "hello"}}}, ExpectedSessionVersion: s.Version})
	if err != nil {
		t.Fatal(err)
	}
	release, _ := a.runs.Try(context.Background())
	if err = a.RecoverRun(context.Background(), scope, queued.Run.ID); !errors.Is(err, apperrors.ErrOverloaded) {
		t.Fatal(err)
	}
	unchanged, err := db.GetRun(context.Background(), scope, queued.Run.ID)
	if err != nil || unchanged.Status != domain.RunQueued || unchanged.Version != queued.Run.Version {
		t.Fatal(unchanged, err)
	}
	release()
	if err = a.RecoverRun(context.Background(), scope, queued.Run.ID); err != nil {
		t.Fatal(err)
	}
	awaitStatus(t, db, queued.Run, domain.RunCompleted)
}

func TestModelOverloadHasDurableFailureReason(t *testing.T) {
	db, _ := testDatabase(t)
	a := service(t, db, func(context.Context, agent.Request, agent.Emit) error { return apperrors.ErrOverloaded })
	r := start(t, a, session(t, a), "")
	awaitStatus(t, db, r, domain.RunFailed)
	page, err := db.ListEvents(context.Background(), scope, r.ID, store.SequencePage{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	last := page.Items[len(page.Items)-1]
	if last.Kind != domain.EventRunFailed || string(last.Data) != `{"reason":"overloaded","retry_tool":false}` {
		t.Fatal(last)
	}
}
