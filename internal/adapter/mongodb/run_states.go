package mongodb

import (
	"context"

	"github.com/hjhsamuel/cagent/internal/domain"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// SessionRunStates never uses historical message IDs as authorization. Both the
// scope and session are mandatory, and only metadata is loaded in one query.
func (b *Database) SessionRunStates(ctx context.Context, scope domain.Scope, sessionID string, ids []string) (map[string]domain.RunStatus, error) {
	if err := validateKey(scope, sessionID); err != nil {
		return nil, err
	}
	states := make(map[string]domain.RunStatus, len(ids))
	if len(ids) == 0 {
		return states, nil
	}
	for _, id := range ids {
		if err := validateKey(scope, id); err != nil {
			return nil, err
		}
	}
	f := scoped(scope)
	f["session_id"], f["id"] = sessionID, bson.M{"$in": ids}
	ctx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()
	cur, err := b.collection(RunCollection).Find(ctx, f, options.Find().SetProjection(bson.M{"id": 1, "status": 1}))
	if err != nil {
		return nil, safeError(err)
	}
	defer cur.Close(ctx)
	for cur.Next(ctx) {
		var row struct {
			ID     string           `bson:"id"`
			Status domain.RunStatus `bson:"status"`
		}
		if err := cur.Decode(&row); err != nil {
			return nil, safeError(err)
		}
		states[row.ID] = row.Status
	}
	if err := cur.Err(); err != nil {
		return nil, safeError(err)
	}
	if len(states) != len(ids) {
		return nil, safeError(invalid("context.run_states"))
	}
	return states, nil
}
