package app

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hjhsamuel/cagent/internal/agent"
	"github.com/hjhsamuel/cagent/internal/contextengine"
	"github.com/hjhsamuel/cagent/internal/domain"
	"github.com/hjhsamuel/cagent/internal/store"
)

// 使用真实持久历史跨越 128 条分页边界，确认读取最新摘要且不重复插入当前输入。
// 原历史在准备前后条数保持不变，Prepare 不隐式压缩或修改已持久化内容。
func TestContextPreparerLoadsSnapshotAndPagedHistory(t *testing.T) {
	db, _ := testDatabase(t)
	ctx := context.Background()
	s := domain.Session{Scope: scope, ID: newID(), AgentID: "agent"}
	if e := db.CreateSession(ctx, s); e != nil {
		t.Fatal(e)
	}
	old := domain.Run{Scope: scope, ID: newID(), SessionID: s.ID, Status: domain.RunQueued}
	startResult, e := db.StartRun(ctx, store.StartRunRequest{Run: old, Input: domain.Message{Scope: scope, ID: newID(), SessionID: s.ID, RunID: old.ID, Role: domain.RoleUser, Parts: []domain.Part{{Text: "old input"}}}, ExpectedSessionVersion: 1})
	if e != nil {
		t.Fatal(e)
	}
	lease, e := db.AcquireLease(ctx, scope, old.ID, "seed", time.Minute)
	if e != nil {
		t.Fatal(e)
	}
	var messages []domain.Message
	for i := 0; i < 129; i++ {
		messages = append(messages, domain.Message{Scope: scope, ID: newID(), SessionID: s.ID, RunID: old.ID, Role: domain.RoleAssistant, Parts: []domain.Part{{Kind: domain.PartText, Text: fmt.Sprint("old output ", i)}}})
	}
	result, e := db.CommitRun(ctx, store.CommitRunRequest{Guard: store.WriteGuard{Lease: lease, RunVersion: startResult.Run.Version}, OperationID: newID(), Status: domain.RunRunning, ExpectedSessionVersion: startResult.SessionVersion, Messages: messages})
	if e != nil {
		t.Fatal(e)
	}
	snap := domain.ContextSnapshot{Scope: scope, ID: newID(), SessionID: s.ID, ThroughSequence: 130, Summary: "earlier summary", PolicyVersion: "v1"}
	saved, e := db.SaveSnapshot(ctx, store.WriteGuard{Lease: lease, RunVersion: result.Run.Version}, snap, 0, result.SessionVersion)
	if e != nil {
		t.Fatal(e)
	}
	old, e = db.GetRun(ctx, scope, old.ID)
	if e != nil {
		t.Fatal(e)
	}
	saved.Summary = "latest summary"
	if _, e = db.SaveSnapshot(ctx, store.WriteGuard{Lease: lease, RunVersion: old.Version}, saved, 1, result.SessionVersion); e != nil {
		t.Fatal(e)
	}
	old, e = db.GetRun(ctx, scope, old.ID)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = db.CancelRun(ctx, store.CancelRunRequest{Scope: scope, RunID: old.ID, ExpectedRunVersion: old.Version}); e != nil {
		t.Fatal(e)
	}
	engine := contextengine.New()
	opts := ContextOptions{PolicyVersion: "v1", System: []domain.Part{{Kind: domain.PartText, Text: "fixed", Data: []byte("original")}}}
	prepare, e := NewContextPreparer(db, engine, opts)
	if e != nil {
		t.Fatal(e)
	}
	opts.System[0].Text = "mutated"
	opts.System[0].Data[0] = 'X'
	a := service(t, db, func(ctx context.Context, req agent.Request, emit agent.Emit) error {
		if len(req.Messages) != 4 {
			return fmt.Errorf("unexpected messages")
		}
		if req.Messages[0].Parts[0].Text != "fixed" || string(req.Messages[0].Parts[0].Data) != "original" {
			return fmt.Errorf("system aliases caller")
		}
		if req.Messages[2].Role != domain.RoleUser || !strings.Contains(req.Messages[2].Parts[0].Text, "latest summary") {
			return fmt.Errorf("wrong snapshot")
		}
		if req.Messages[3].RunID != req.Run.ID || req.Messages[3].Sequence != 131 || req.Messages[3].Role != domain.RoleUser {
			return fmt.Errorf("missing or duplicated current input")
		}
		return nil
	})
	a.opts.Prepare = prepare
	r := start(t, a, s, "")
	awaitStatus(t, db, r, domain.RunCompleted)
	page, e := db.ListMessages(ctx, scope, s.ID, store.SequencePage{After: 128, Limit: 10})
	if e != nil || len(page.Items) != 3 {
		t.Fatalf("history modified: %+v %v", page, e)
	}
	latest, e := db.LatestSnapshot(ctx, scope, s.ID)
	if e != nil || latest.Version != 2 || latest.Summary != "latest summary" {
		t.Fatal("snapshot modified", e)
	}
}
