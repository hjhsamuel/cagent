package mongodb

import (
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hjhsamuel/cagent/internal/domain"
	"github.com/hjhsamuel/cagent/internal/storage/schema"
	"github.com/hjhsamuel/cagent/internal/store"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// Legacy envelopes included metadata from every collection, including unused
// zero fields. Each mapping must still decode the payload and keep only its own
// query fields when written back.
func TestCollectionMappingsReadLegacyEnvelopes(t *testing.T) {
	var session sessionDocument
	checkLegacyMapping(t, SessionCollection, domain.Session{Scope: testScope, ID: "session", ModelID: "model", APIKeyID: "key", Version: 7}, &session, func(value *domain.Session) error { return session.decode(value) },
		"model_id api_key_id last_sequence")
	var run runDocument
	checkLegacyMapping(t, RunCollection, domain.Run{Scope: testScope, ID: "run", SessionID: "session", Status: domain.RunRunning, Version: 7}, &run, func(value *domain.Run) error { return run.decode(value) },
		"session_id status idempotency_key last_sequence pruned_through unsettled recovery next_action_at start fingerprint")
	var message messageDocument
	checkLegacyMapping(t, MessageCollection, domain.Message{Scope: testScope, ID: "message", RunID: "run", SessionID: "session", Sequence: 9, Role: domain.RoleUser, Parts: []domain.Part{{Kind: domain.PartText, Data: []byte{0, 0xff}}}}, &message, func(value *domain.Message) error { return message.decode(value) },
		"session_id run_id sequence context_protected")
	var event eventDocument
	checkLegacyMapping(t, EventCollection, domain.Event{Scope: testScope, RunID: "run", Sequence: 9, Kind: domain.EventTextDelta, Data: []byte{0, 0xff}}, &event, func(value *domain.Event) error { return event.decode(value) },
		"run_id sequence")
	var task taskRecord
	checkLegacyMapping(t, TaskCollection, domain.Task{Scope: testScope, ID: "task", Version: 7, Progress: []domain.Part{}, Result: &domain.ToolResult{CallID: "call", Parts: nil}}, &task, func(value *domain.Task) error { return task.decode(value) },
		"session_id run_id invocation_id call_id protocol connection_id remote_id status unsettled next_action_at")
	var delivery deliveryDocument
	checkLegacyMapping(t, TaskDeliveryCollection, domain.TaskDelivery{Scope: testScope, TaskID: "task", RunID: "run", State: domain.DeliveryPending, Version: 7}, &delivery, func(value *domain.TaskDelivery) error { return delivery.decode(value) },
		"run_id status")
	var checkpoint checkpointDocument
	checkLegacyMapping(t, CheckpointCollection, domain.Checkpoint{Scope: testScope, RunID: "run", ModelID: "model", APIKeyID: "key", Version: 7, Format: "v1", Data: []byte{0, 0xff}, PendingCallIDs: []string{}}, &checkpoint, func(value *domain.Checkpoint) error { return checkpoint.decode(value) },
		"model_id api_key_id run_id invocation_id")
	var snapshot snapshotDocument
	checkLegacyMapping(t, SnapshotCollection, domain.ContextSnapshot{Scope: testScope, ID: "snapshot", SessionID: "session", Summary: "summary", ThroughSequence: 9, Version: 7}, &snapshot, func(value *domain.ContextSnapshot) error { return snapshot.decode(value) },
		"session_id")
	var receipt receiptDocument
	checkLegacyMapping(t, MutationReceiptCollection, persistedReceipt{MutationReceipt: store.MutationReceipt{Scope: testScope, RunID: "run", OperationID: "operation", Kind: store.MutationRunCommit}, Format: 1}, &receipt, func(value *persistedReceipt) error { return receipt.decode(value) },
		"run_id call_id")
}

func checkLegacyMapping[T any](t *testing.T, collection string, payload T, mapping any, decode func(*T) error, fields string) {
	t.Helper()
	t.Run(collection, func(t *testing.T) {
		raw, err := bson.Marshal(payload)
		check(t, err)
		legacy := bson.M{
			"tenant_id": testScope.TenantID, "user_id": testScope.UserID, "id": "stored-id",
			"schema": 1, "data": bson.Raw(raw), "version": int64(7),
			"model_id": "model", "api_key_id": "key", "session_id": "session", "run_id": "run",
			"invocation_id": "invocation", "call_id": "call", "protocol": "a2a", "connection_id": "connection", "remote_id": "remote",
			"status": "running", "idempotency_key": "idempotency", "sequence": int64(9),
			"last_sequence": int64(10), "pruned_through": int64(2), "unsettled": int64(1),
			"recovery": true, "context_protected": true, "next_action_at": time.Unix(1, 0).UTC(),
			"start": bson.M{"sessionversion": int64(7)}, "fingerprint": []byte{0, 0xff},
		}
		raw, err = bson.Marshal(legacy)
		check(t, err)
		check(t, bson.Unmarshal(raw, mapping))
		var decoded T
		check(t, decode(&decoded))
		if !reflect.DeepEqual(decoded, payload) {
			t.Fatalf("legacy payload changed: got %#v, want %#v", decoded, payload)
		}
		raw, err = bson.Marshal(mapping)
		check(t, err)
		var stored bson.M
		check(t, bson.Unmarshal(raw, &stored))
		want := strings.Fields("tenant_id user_id id schema data version " + fields)
		got := make([]string, 0, len(stored))
		for name, value := range stored {
			got = append(got, name)
			if !reflect.DeepEqual(value, legacy[name]) {
				// BSON decoding normalizes binary and embedded documents. Compare
				// their encoded bytes to check that persisted query values survive.
				before, err := bson.Marshal(bson.M{name: legacy[name]})
				check(t, err)
				after, err := bson.Marshal(bson.M{name: value})
				check(t, err)
				if !slices.Equal(before, after) {
					t.Fatalf("legacy field %s changed", name)
				}
			}
		}
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Fatalf("collection fields: got %v, want %v", got, want)
		}
	})
}

func TestCheckpointMappingRejectsInvalidPayloadEnvelope(t *testing.T) {
	db := &Database{}
	for _, d := range []checkpointDocument{
		{Schema: 2, Payload: &schema.PayloadRef{}},
		{Schema: 1, Data: bson.Raw{5, 0, 0, 0, 0}, Payload: &schema.PayloadRef{}},
	} {
		var value domain.Checkpoint
		if err := db.decodeCheckpoint(t.Context(), d, &value); err == nil {
			t.Fatal("invalid checkpoint envelope accepted")
		}
	}
}
