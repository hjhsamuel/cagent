package mongodb

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/hjhsamuel/cagent/internal/apperrors"
	"github.com/hjhsamuel/cagent/internal/domain"
	"github.com/hjhsamuel/cagent/internal/store"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestLargeReceiptAndCheckpointSurviveOverwriteAndRetry(t *testing.T) {
	db, _ := testDatabase(t)
	ctx := context.Background()
	_, g := startFixture(t, db)
	large := strings.Repeat("x", 6<<20)
	cp := domain.Checkpoint{Scope: testScope, RunID: "run", Caller: domain.AgentExecution{AgentID: "agent", InvocationID: "run"}, Format: "large-test/v1", Data: []byte(large)}
	req := store.CommitRunRequest{Guard: g, OperationID: "large", Status: domain.RunRunning, ExpectedSessionVersion: 2, Checkpoint: &cp, Messages: []domain.Message{{Scope: testScope, ID: "large-message", SessionID: "session", RunID: "run", Role: domain.RoleAssistant, Parts: []domain.Part{{Kind: domain.PartText, Text: large}}}}, Events: []domain.Event{{Scope: testScope, RunID: "run", Kind: domain.EventTextDelta, Data: []byte(large)}}}
	out, err := db.CommitRun(ctx, req)
	check(t, err)
	var receipt receiptDocument
	check(t, db.collection(MutationReceiptCollection).FindOne(ctx, key(testScope, compositeID("run", "large"))).Decode(&receipt))
	raw, err := bson.Marshal(receipt)
	check(t, err)
	if len(raw) > 32<<10 {
		t.Fatal("receipt retained large bodies", len(raw))
	}
	saved, err := db.GetCheckpoint(ctx, testScope, "run", "run")
	check(t, err)
	if string(saved.Data) != large {
		t.Fatal("chunked checkpoint changed")
	}
	var checkpoint checkpointDocument
	check(t, db.collection(CheckpointCollection).FindOne(ctx, key(testScope, compositeID("run", "run"))).Decode(&checkpoint))
	if checkpoint.Payload == nil {
		t.Fatal("checkpoint was not offloaded")
	}
	_, err = db.readPayload(ctx, domain.Scope{TenantID: "other", UserID: testScope.UserID}, checkpoint.Payload)
	if err == nil {
		t.Fatal("payload crossed scope")
	}
	saved.Data = []byte("replacement")
	_, err = db.CommitRun(ctx, store.CommitRunRequest{Guard: freshGuard(t, db, g.Lease), OperationID: "replace", Status: domain.RunRunning, ExpectedSessionVersion: out.SessionVersion, ExpectedCheckpointVersion: saved.Version, Checkpoint: &saved})
	check(t, err)
	retry, err := db.CommitRun(ctx, req)
	check(t, err)
	if !reflect.DeepEqual(retry, out) {
		t.Fatal("retry returned the overwritten checkpoint or current versions")
	}
}
func TestChunkedPayloadRollsBackWithMutation(t *testing.T) {
	db, _ := testDatabase(t)
	ctx := context.Background()
	_, g := startFixture(t, db)
	db.beforeCommit = func(name string) error {
		if name == "commit" {
			return errors.New("injected abort")
		}
		return nil
	}
	cp := domain.Checkpoint{Scope: testScope, RunID: "run", Caller: domain.AgentExecution{AgentID: "agent", InvocationID: "run"}, Format: "large-test/v1", Data: []byte(strings.Repeat("x", 5<<20))}
	_, err := db.CommitRun(ctx, store.CommitRunRequest{Guard: g, OperationID: "aborted", Status: domain.RunRunning, ExpectedSessionVersion: 2, Checkpoint: &cp})
	if err == nil {
		t.Fatal("injected failure committed")
	}
	db.beforeCommit = nil
	count, err := db.collection(PayloadCollection).CountDocuments(ctx, scoped(testScope))
	check(t, err)
	if count != 0 {
		t.Fatal("aborted chunks leaked", count)
	}
	_, err = db.GetCheckpoint(ctx, testScope, "run", "run")
	if !errors.Is(err, apperrors.ErrNotFound) {
		t.Fatal("aborted reference visible", err)
	}
}
