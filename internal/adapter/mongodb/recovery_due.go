package mongodb

import (
	"context"
	"time"

	"github.com/hjhsamuel/cagent/internal/domain"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// Backfill only legacy envelopes before enabling the indexed scan. The metadata
// is a scheduling hint; fenced lease acquisition still authorizes every write.
func (b *Database) migrateRecovery(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()
	terminal := bson.M{"$in": bson.A{"$status", bson.A{domain.RunCompleted, domain.RunFailed, domain.RunCancelled}}}
	needed := bson.M{"$or": bson.A{bson.M{"$not": bson.A{terminal}}, bson.M{"$gt": bson.A{"$unsettled", 0}}}}
	_, err := b.collection(RunCollection).UpdateMany(ctx, bson.M{"recovery": bson.M{"$exists": false}}, mongo.Pipeline{{{Key: "$set", Value: bson.M{"recovery": needed, "next_action_at": bson.DateTime(0)}}}})
	return safeError(err)
}
func (b *Database) DeferRecovery(ctx context.Context, scope domain.Scope, id string, version int64, delay time.Duration) error {
	if err := validateKey(scope, id); err != nil {
		return err
	}
	if delay < 0 {
		return invalid("recovery.delay")
	}
	ctx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()
	f := key(scope, id)
	f["version"] = version
	f["recovery"] = true
	_, err := b.collection(RunCollection).UpdateOne(ctx, f, mongo.Pipeline{{{Key: "$set", Value: bson.M{"next_action_at": bson.M{"$add": bson.A{"$$NOW", delay.Milliseconds()}}}}}})
	return safeError(err)
}
