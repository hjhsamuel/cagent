package mongodb

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/hjhsamuel/cagent/internal/apperrors"
	"github.com/hjhsamuel/cagent/internal/domain"
	"github.com/hjhsamuel/cagent/internal/store"
)

// 两个独立连接同时保存同一压缩基线，数据库只能接受一个候选。新历史提交后，
// 即使候选水位仍合法，旧 Run/会话版本也不能将过时生成结果提交到新状态。
func TestSnapshotConcurrentCompressionAndStaleHistory(t *testing.T) {
	db, cfg := testDatabase(t)
	ctx := context.Background()
	_, g := startFixture(t, db)
	other, e := Open(ctx, cfg)
	check(t, e)
	defer other.Close(ctx)
	snap := domain.ContextSnapshot{Scope: testScope, ID: "summary", SessionID: "session", ThroughSequence: 1, Summary: "facts", PolicyVersion: "v1"}
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, conn := range []*Database{db, other} {
		wg.Add(1)
		go func(d *Database) {
			defer wg.Done()
			<-start
			_, err := d.SaveSnapshot(ctx, g, snap, 0, 2)
			results <- err
		}(conn)
	}
	close(start)
	wg.Wait()
	close(results)
	success, conflicts := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, apperrors.ErrConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflicts != 1 {
		t.Fatal(success, conflicts)
	}
	latest, e := db.LatestSnapshot(ctx, testScope, "session")
	check(t, e)
	guard := freshGuard(t, db, g.Lease)
	_, e = db.CommitRun(ctx, store.CommitRunRequest{Guard: guard, OperationID: "append-history", Status: domain.RunRunning, ExpectedSessionVersion: 2, Messages: []domain.Message{{Scope: testScope, ID: "new-answer", SessionID: "session", RunID: "run", Role: domain.RoleAssistant, Parts: []domain.Part{{Kind: domain.PartText, Text: "new fact"}}}}})
	check(t, e)
	latest.Summary = "stale overwrite"
	_, e = db.SaveSnapshot(ctx, guard, latest, 1, 2)
	wantKind(t, e, apperrors.ErrConflict)
	_, e = db.SaveSnapshot(ctx, freshGuard(t, db, g.Lease), latest, 1, 2)
	wantKind(t, e, apperrors.ErrConflict)
	got, e := db.LatestSnapshot(ctx, testScope, "session")
	check(t, e)
	if got.Summary != "facts" || got.Version != 1 {
		t.Fatal("stale overwrite accepted")
	}
	page, e := db.ListMessages(ctx, testScope, "session", store.SequencePage{Limit: 10})
	check(t, e)
	if len(page.Items) != 2 {
		t.Fatal("source history changed")
	}
}
