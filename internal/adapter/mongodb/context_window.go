package mongodb

import (
	"context"
	"github.com/hjhsamuel/cagent/internal/domain"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func (b *Database) migrateContextMetadata(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()
	toolParts := bson.M{"$filter": bson.M{"input": bson.M{"$ifNull": bson.A{"$data.parts", bson.A{}}}, "as": "p", "cond": bson.M{"$in": bson.A{"$$p.kind", bson.A{domain.PartToolCall, domain.PartToolResult}}}}}
	protected := bson.M{"$or": bson.A{bson.M{"$eq": bson.A{"$data.role", domain.RoleUser}}, bson.M{"$gt": bson.A{bson.M{"$size": toolParts}, 0}}}}
	_, err := b.collection(MessageCollection).UpdateMany(ctx, bson.M{"context_protected": bson.M{"$exists": false}}, mongo.Pipeline{{{Key: "$set", Value: bson.M{"context_protected": protected}}}})
	return safeError(err)
}

// ContextWindow validates the scoped session and immutable summary reference in
// one snapshot. Covered plain messages need not be loaded to validate the tail.
func (b *Database) ContextWindow(ctx context.Context, snapshot domain.ContextSnapshot, sessionVersion int64, archive bool) ([]domain.Message, int64, error) {
	if err := validateKey(snapshot.Scope, snapshot.SessionID); err != nil {
		return nil, 0, err
	}
	if snapshot.ValidatedThrough != snapshot.ThroughSequence || snapshot.ThroughSequence <= 0 || (archive && !snapshot.Archived) {
		return nil, 0, invalid("context.window")
	}
	var history []domain.Message
	var end int64
	err := b.withTransaction(ctx, "context.window", func(tx context.Context) error {
		history = nil
		var sd sessionDocument
		var session domain.Session
		if err := b.collection(SessionCollection).FindOne(tx, key(snapshot.Scope, snapshot.SessionID)).Decode(&sd); err != nil {
			return err
		}
		if err := sd.decode(&session); err != nil {
			return err
		}
		if session.Version != sessionVersion {
			return conflict("session.version")
		}
		if err := snapshot.ValidateForSession(session); err != nil {
			return err
		}
		var stored snapshotDocument
		var cp domain.ContextSnapshot
		if err := b.collection(SnapshotCollection).FindOne(tx, key(snapshot.Scope, compositeID(session.ID, stringInt(snapshot.Version)))).Decode(&stored); err != nil {
			return err
		}
		if err := stored.decode(&cp); err != nil {
			return err
		}
		if cp != snapshot || cp.ThroughSequence > sd.LastSequence {
			return invariant()
		}
		end = sd.LastSequence
		f := scoped(snapshot.Scope)
		f["session_id"] = session.ID
		f["sequence"] = bson.M{"$gt": snapshot.ThroughSequence}
		if !archive {
			delete(f, "sequence")
			f["$or"] = bson.A{bson.M{"sequence": bson.M{"$gt": snapshot.ThroughSequence}}, bson.M{"context_protected": true, "sequence": bson.M{"$lte": snapshot.ThroughSequence}}}
		}
		cursor, err := b.collection(MessageCollection).Find(tx, f, options.Find().SetSort(bson.D{{Key: "sequence", Value: 1}}))
		if err != nil {
			return err
		}
		defer cursor.Close(tx)
		for cursor.Next(tx) {
			var d messageDocument
			var m domain.Message
			if err := cursor.Decode(&d); err != nil {
				return err
			}
			if err := d.decode(&m); err != nil {
				return err
			}
			history = append(history, m)
		}
		return cursor.Err()
	})
	return history, end, err
}
