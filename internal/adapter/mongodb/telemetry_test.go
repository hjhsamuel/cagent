package mongodb

import (
	"context"
	"errors"
	"github.com/hjhsamuel/cagent/internal/observability"
	"go.mongodb.org/mongo-driver/v2/event"
	"testing"
)

func TestCommandMonitoringPairsConnectionsAndRecordsFailure(t *testing.T) {
	monitor := commandMonitor()
	ctx, end := observability.Default.Start(context.Background(), "recovery")
	defer end(nil)
	before := observability.Default.Spans()
	monitor.Started(ctx, &event.CommandStartedEvent{ConnectionID: "one", RequestID: 1})
	monitor.Started(ctx, &event.CommandStartedEvent{ConnectionID: "two", RequestID: 1})
	monitor.Succeeded(ctx, &event.CommandSucceededEvent{CommandFinishedEvent: event.CommandFinishedEvent{ConnectionID: "one", RequestID: 1}})
	monitor.Failed(ctx, &event.CommandFailedEvent{CommandFinishedEvent: event.CommandFinishedEvent{ConnectionID: "two", RequestID: 1}, Failure: errors.New("secret-query")})
	monitor.Failed(ctx, &event.CommandFailedEvent{CommandFinishedEvent: event.CommandFinishedEvent{ConnectionID: "two", RequestID: 1}})
	spans := observability.Default.Spans()
	if len(spans) != min(256, len(before)+2) {
		t.Fatal("unmatched callbacks")
	}
	a, b := spans[len(spans)-2], spans[len(spans)-1]
	if a.Operation != "storage" || a.Failed || !b.Failed || a.TraceID != observability.TraceID(ctx) || b.TraceID != a.TraceID {
		t.Fatal(a, b)
	}
}
