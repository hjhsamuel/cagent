package mongodb

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/hjhsamuel/cagent/internal/apperrors"
	"github.com/hjhsamuel/cagent/internal/storage/schema"
)

func TestListToolConnectionsFromMongoDB(t *testing.T) {
	db, _ := testDatabase(t)
	ctx := context.Background()
	empty, err := db.ListToolConnections(ctx)
	if err != nil || len(empty) != 0 {
		t.Fatal("empty connection collection failed", err)
	}
	docs := []schema.ToolConnection{
		{TenantID: "a", UserID: "alice", ID: "a2a", Protocol: "a2a", URL: "https://agent.example.invalid", CardPath: "/card", CredentialRef: "primary", Credentials: map[string]string{"primary": "AGENT_AUTH"}, Tools: []string{"remote_agent"}},
		{TenantID: "a", UserID: "bob", ID: "a2a", Protocol: "mcp", URL: "https://mcp.example.invalid"},
		{TenantID: "b", UserID: "alice", ID: "a2a", Protocol: "mcp", URL: "https://other.example.invalid"},
	}
	for i := len(docs) - 1; i >= 0; i-- {
		if _, err := db.collection(ToolConnectionCollection).InsertOne(ctx, docs[i]); err != nil {
			t.Fatal(safeError(err))
		}
	}
	got, err := db.ListToolConnections(ctx)
	if err != nil || !reflect.DeepEqual(got, docs) {
		t.Fatal("connection fields or stable scope ordering lost", err)
	}
	if _, err := db.collection(ToolConnectionCollection).InsertOne(ctx, docs[0]); !errors.Is(safeError(err), apperrors.ErrConflict) {
		t.Fatal("duplicate scoped connection ID accepted", safeError(err))
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := db.ListToolConnections(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled connection read accepted", err)
	}
}
