package mongodb

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/hjhsamuel/cagent/internal/apperrors"
	"github.com/hjhsamuel/cagent/internal/domain"
	"github.com/hjhsamuel/cagent/internal/store"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// 具体读取方法自行构造查询条件。用真实持久任务验证不同租户、用户和缺失身份
// 都无法读到目标记录，同时确认取消的 context 仍保留原始错误身份。
func TestMongoGetTaskScopeAndCancellation(t *testing.T) {
	db, _ := testDatabase(t)
	ctx := context.Background()
	_, guard := startFixture(t, db)
	cp := domain.Checkpoint{Scope: testScope, RunID: "run", Caller: domain.AgentExecution{AgentID: "agent", InvocationID: "branch"}, Format: "test-v1"}
	tracked, err := db.TrackTask(ctx, trackRequest(guard, cp, "task"))
	check(t, err)
	got, err := db.GetTask(ctx, testScope, "task")
	check(t, err)
	if !reflect.DeepEqual(got, tracked.Task) {
		t.Fatal("concrete read did not return the persisted task")
	}
	for _, scope := range []domain.Scope{
		{TenantID: testScope.TenantID, UserID: "other-user"},
		{TenantID: "other-tenant", UserID: testScope.UserID},
	} {
		got, err = db.GetTask(ctx, scope, "task")
		wantKind(t, err, apperrors.ErrNotFound)
		if !reflect.DeepEqual(got, domain.Task{}) {
			t.Fatal("cross-scope read leaked task data")
		}
	}
	_, err = db.GetTask(ctx, domain.Scope{}, "task")
	wantKind(t, err, apperrors.ErrInvalidArgument)
	_, err = db.GetTask(ctx, testScope, " ")
	wantKind(t, err, apperrors.ErrInvalidArgument)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = db.GetTask(cancelled, testScope, "task")
	if !errors.Is(err, context.Canceled) {
		t.Fatal("concrete read lost context cancellation", err)
	}
}

// 已有检查点走 ReplaceOne 分支。让数据库拒绝新版本，验证该分支的驱动错误
// 传出事务、返回零结果且不保存任务；解除限制后同一请求可以正常重试。
func TestMongoCheckpointUpdateFailureRollsBackTrack(t *testing.T) {
	db, _ := testDatabase(t)
	ctx := context.Background()
	_, guard := startFixture(t, db)
	cp := domain.Checkpoint{Scope: testScope, RunID: "run", Caller: domain.AgentExecution{AgentID: "agent", InvocationID: "branch"}, Format: "test-v1"}
	setup, err := db.CommitRun(ctx, store.CommitRunRequest{Guard: guard, OperationID: "setup", Status: domain.RunRunning, ExpectedSessionVersion: 2, Checkpoint: &cp})
	check(t, err)
	cp = *setup.Checkpoint
	req := trackRequest(freshGuard(t, db, guard.Lease), cp, "task")
	check(t, db.db.RunCommand(ctx, bson.D{
		{Key: "collMod", Value: CheckpointCollection},
		{Key: "validator", Value: bson.M{"version": bson.M{"$lte": cp.Version}}},
		{Key: "validationLevel", Value: "strict"},
		{Key: "validationAction", Value: "error"},
	}).Err())
	out, err := db.TrackTask(ctx, req)
	var serverError mongo.ServerError
	if !errors.As(err, &serverError) || !serverError.HasErrorCode(121) {
		t.Fatal("checkpoint validation error was swallowed", err)
	}
	if !reflect.DeepEqual(out, store.TaskCommitResult{}) {
		t.Fatal("failed transaction returned an uncommitted result")
	}
	_, err = db.GetTask(ctx, testScope, "task")
	wantKind(t, err, apperrors.ErrNotFound)
	persisted, err := db.GetCheckpoint(ctx, testScope, "run", "branch")
	check(t, err)
	if !reflect.DeepEqual(persisted, cp) {
		t.Fatal("failed checkpoint update changed the original checkpoint")
	}
	run, err := db.GetRun(ctx, testScope, "run")
	check(t, err)
	if run.Version != setup.Run.Version {
		t.Fatal("failed checkpoint update advanced the run")
	}
	check(t, db.db.RunCommand(ctx, bson.D{{Key: "collMod", Value: CheckpointCollection}, {Key: "validator", Value: bson.M{}}}).Err())
	out, err = db.TrackTask(ctx, req)
	check(t, err)
	if out.Task.Version != 1 || out.CheckpointVersion != cp.Version+1 {
		t.Fatal("retry failed to persist the original task and next checkpoint")
	}
}

// 在写入完成、提交开始前模拟瞬态事务冲突。驱动会重放回调；快照版本和 Run
// 版本应只推进一次。随后模拟不可重试错误，确认对外返回零值且没有保存新快照。
func TestMongoSnapshotRetryAndFailedResult(t *testing.T) {
	db, _ := testDatabase(t)
	ctx := context.Background()
	_, guard := startFixture(t, db)
	next := domain.ContextSnapshot{Scope: testScope, ID: "snapshot", SessionID: "session", ThroughSequence: 1}
	attempts := 0
	db.beforeCommit = func(name string) error {
		if name == "snapshot.save" {
			attempts++
			if attempts == 1 {
				return mongo.CommandError{Code: 112, Message: "injected transaction conflict", Labels: []string{"TransientTransactionError"}}
			}
		}
		return nil
	}
	saved, err := db.SaveSnapshot(ctx, guard, next, 0, 2)
	db.beforeCommit = nil
	check(t, err)
	if attempts != 2 || saved.Version != 1 {
		t.Fatalf("retry inherited a previous attempt: attempts=%d version=%d", attempts, saved.Version)
	}
	run, err := db.GetRun(ctx, testScope, "run")
	check(t, err)
	if run.Version != guard.RunVersion+1 {
		t.Fatal("transaction retry advanced the run more than once")
	}
	next = saved
	next.Summary += " updated"
	guard.RunVersion = run.Version
	injected := errors.New("injected commit rejection")
	db.beforeCommit = func(name string) error {
		if name == "snapshot.save" {
			return injected
		}
		return nil
	}
	out, err := db.SaveSnapshot(ctx, guard, next, saved.Version, 2)
	db.beforeCommit = nil
	if !errors.Is(err, injected) || !reflect.DeepEqual(out, domain.ContextSnapshot{}) {
		t.Fatal("failed snapshot returned an uncommitted result", err)
	}
	latest, err := db.LatestSnapshot(ctx, testScope, "session")
	check(t, err)
	if !reflect.DeepEqual(latest, saved) {
		t.Fatal("failed transaction left a new snapshot")
	}
}
