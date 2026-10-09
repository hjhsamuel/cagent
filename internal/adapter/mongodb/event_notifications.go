package mongodb

import (
	"context"
	"sync"
	"time"

	"github.com/hjhsamuel/cagent/internal/domain"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

type eventKey struct {
	Scope domain.Scope
	RunID string
}
type eventHead struct {
	Sequence int64  `bson:"last_sequence"`
	Pruned   int64  `bson:"pruned_through"`
	Status   string `bson:"status"`
}
type eventEntry struct {
	channel chan struct{}
	refs    int
	head    eventHead
}
type eventHub struct {
	db      *Database
	mu      sync.Mutex
	entries map[eventKey]*eventEntry
	stop    context.CancelFunc
}

// EventSubscription is only a wake-up hint. Consumers must always read their own
// scoped durable cursor. Capturing Channel before the read closes the lost-wakeup window.
type EventSubscription struct {
	hub  *eventHub
	key  eventKey
	once sync.Once
}

func (s *EventSubscription) Channel() <-chan struct{} {
	s.hub.mu.Lock()
	defer s.hub.mu.Unlock()
	return s.hub.entries[s.key].channel
}
func (s *EventSubscription) Close() {
	s.once.Do(func() {
		s.hub.mu.Lock()
		defer s.hub.mu.Unlock()
		e := s.hub.entries[s.key]
		e.refs--
		if e.refs == 0 {
			delete(s.hub.entries, s.key)
		}
	})
}
func (h *eventHub) notify(key eventKey) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if e := h.entries[key]; e != nil {
		close(e.channel)
		e.channel = make(chan struct{})
	}
}
func (h *eventHub) notifyAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, e := range h.entries {
		close(e.channel)
		e.channel = make(chan struct{})
	}
}
func (h *eventHub) updateHead(key eventKey, head eventHead) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if e := h.entries[key]; e != nil && e.head != head {
		e.head = head
		close(e.channel)
		e.channel = make(chan struct{})
	}
}

func (b *Database) notifications() *eventHub {
	b.eventOnce.Do(func() {
		ctx, stop := context.WithCancel(context.Background())
		b.eventHub = &eventHub{db: b, entries: make(map[eventKey]*eventEntry), stop: stop}
		go b.eventHub.watch(ctx)
		go b.eventHub.poll(ctx)
	})
	return b.eventHub
}
func (b *Database) SubscribeRunEvents(scope domain.Scope, runID string) (*EventSubscription, error) {
	if err := validateKey(scope, runID); err != nil {
		return nil, err
	}
	h := b.notifications()
	key := eventKey{scope, runID}
	h.mu.Lock()
	defer h.mu.Unlock()
	e := h.entries[key]
	if e == nil {
		e = &eventEntry{channel: make(chan struct{})}
		h.entries[key] = e
	}
	e.refs++
	return &EventSubscription{hub: h, key: key}, nil
}
func (b *Database) NotifyRunEvents(scope domain.Scope, runID string) {
	b.notifications().notify(eventKey{scope, runID})
}

// One database change stream serves every connection. Resume tokens are hints,
// never SSE cursors; after any interruption a durable read repairs missed changes.
func (h *eventHub) watch(ctx context.Context) {
	var token bson.Raw
	for ctx.Err() == nil {
		opts := options.ChangeStream().SetFullDocument(options.UpdateLookup).SetMaxAwaitTime(time.Second)
		if len(token) > 0 {
			opts.SetResumeAfter(token)
		}
		pipeline := mongo.Pipeline{
			{{Key: "$match", Value: bson.M{"$or": bson.A{bson.M{"ns.coll": EventCollection, "operationType": "insert"}, bson.M{"ns.coll": RunCollection, "operationType": bson.M{"$in": bson.A{"insert", "replace"}}}, bson.M{"ns.coll": RunCollection, "operationType": "update", "$or": bson.A{bson.M{"updateDescription.updatedFields.last_sequence": bson.M{"$exists": true}}, bson.M{"updateDescription.updatedFields.pruned_through": bson.M{"$exists": true}}, bson.M{"updateDescription.updatedFields.status": bson.M{"$exists": true}}}}}}}},
			{{Key: "$project", Value: bson.M{"_id": 1, "ns": 1, "fullDocument.tenant_id": 1, "fullDocument.user_id": 1, "fullDocument.id": 1, "fullDocument.run_id": 1, "fullDocument.last_sequence": 1, "fullDocument.pruned_through": 1, "fullDocument.status": 1}}},
		}
		stream, err := h.db.db.Watch(ctx, pipeline, opts)
		if err == nil {
			for stream.Next(ctx) {
				var change struct {
					NS struct {
						Collection string `bson:"coll"`
					} `bson:"ns"`
					Full document `bson:"fullDocument"`
				}
				if stream.Decode(&change) == nil {
					d := change.Full
					id := d.ID
					if change.NS.Collection == EventCollection {
						id = d.RunID
					}
					scope := domain.Scope{TenantID: d.Tenant, UserID: d.User}
					if validateKey(scope, id) == nil {
						if change.NS.Collection == EventCollection {
							h.notify(eventKey{scope, id})
						} else {
							h.updateHead(eventKey{scope, id}, eventHead{d.LastSequence, d.PrunedThrough, d.Status})
						}
					}
				}
				token = append(bson.Raw(nil), stream.ResumeToken()...)
			}
			_ = stream.Close(context.Background())
		}
		if ctx.Err() != nil {
			return
		}
		// Reset even an invalid/expired token, and wake all readers to reconcile.
		token = nil
		h.notifyAll()
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

// A lightweight metadata read per Run (not per subscriber, and no transaction)
// covers startup gaps and change-stream outages. Idle subscribers do no ListEvents polling.
func (h *eventHub) poll(ctx context.Context) {
	timer := time.NewTicker(3 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		h.mu.Lock()
		keys := make([]eventKey, 0, len(h.entries))
		for key := range h.entries {
			keys = append(keys, key)
		}
		h.mu.Unlock()
		for _, key := range keys {
			c, cancel := context.WithTimeout(ctx, h.db.timeout)
			var head eventHead
			err := h.db.collection(RunCollection).FindOne(c, keyFilter(key), options.FindOne().SetProjection(bson.M{"last_sequence": 1, "pruned_through": 1, "status": 1})).Decode(&head)
			cancel()
			h.mu.Lock()
			e := h.entries[key]
			if e != nil && (err != nil || e.head != head) {
				e.head = head
				close(e.channel)
				e.channel = make(chan struct{})
			}
			h.mu.Unlock()
		}
	}
}
func keyFilter(k eventKey) bson.M { return key(k.Scope, k.RunID) }
