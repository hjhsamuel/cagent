package mongodb

import (
	"context"
	"testing"
	"time"

	"github.com/hjhsamuel/cagent/internal/domain"
	"github.com/hjhsamuel/cagent/internal/store"
)

func TestEventHintsSharedScopedAndCrossInstance(t *testing.T) {
	db, cfg := testDatabase(t)
	ctx := context.Background()
	_, g := startFixture(t, db)
	var subscriptions []*EventSubscription
	var wakeups []<-chan struct{}
	for range 32 {
		sub, err := db.SubscribeRunEvents(testScope, "run")
		check(t, err)
		subscriptions = append(subscriptions, sub)
		wakeups = append(wakeups, sub.Channel())
	}
	other, err := db.SubscribeRunEvents(domain.Scope{TenantID: testScope.TenantID, UserID: "other"}, "run")
	check(t, err)
	otherWake := other.Channel()
	defer other.Close()
	hub := db.notifications()
	hub.mu.Lock()
	if len(hub.entries) != 2 {
		t.Fatal("one entry was allocated per subscriber")
	}
	hub.mu.Unlock()
	hub.notify(eventKey{testScope, "run"})
	select {
	case <-otherWake:
		t.Fatal("notification crossed user scope")
	default:
	}
	for i, sub := range subscriptions {
		wakeups[i] = sub.Channel()
	}
	remote, err := Open(ctx, cfg)
	check(t, err)
	defer remote.Close(ctx)
	// Direct remote commit deliberately omits the local Publish fast path.
	_, err = remote.CommitRun(ctx, store.CommitRunRequest{Guard: g, OperationID: "remote-event", Status: domain.RunRunning, ExpectedSessionVersion: 2, Events: []domain.Event{{Scope: testScope, RunID: "run", Kind: domain.EventTextDelta, Data: []byte("delta")}}})
	check(t, err)
	for _, wake := range wakeups {
		select {
		case <-wake:
		case <-time.After(6 * time.Second):
			t.Fatal("cross-instance event missed")
		}
	}
	for _, sub := range subscriptions {
		sub.Close()
	}
	hub.mu.Lock()
	if len(hub.entries) != 1 {
		t.Fatal("idle entry leaked")
	}
	hub.mu.Unlock()
}
