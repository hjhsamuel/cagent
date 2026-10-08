package mongodb

import (
	"context"
	"github.com/hjhsamuel/cagent/internal/domain"
	"github.com/hjhsamuel/cagent/internal/store"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// ListCheckpoints 在完整 Scope+Run 下分页，恢复器不得使用空 Scope 全量读取。
// 分页不是所有权证明；推进任何分支前仍须取得 Run 租约并重读当前检查点。
func (b *Database) ListCheckpoints(ctx context.Context, scope domain.Scope, id string, p store.KeyPage) (store.CheckpointPage, error) {
	out := store.CheckpointPage{NextAfter: p.After}
	if e := validateKey(scope, id); e != nil {
		return out, e
	}
	if e := p.Validate(); e != nil {
		return out, e
	}
	if _, e := b.GetRun(ctx, scope, id); e != nil {
		return out, e
	}
	ctx, stop := context.WithTimeout(ctx, b.timeout)
	defer stop()
	filter := scoped(scope)
	filter["run_id"] = id
	filter["invocation_id"] = bson.M{"$gt": p.After}
	cur, e := b.collection(CheckpointCollection).Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "invocation_id", Value: 1}}).SetLimit(int64(p.Limit+1)))
	if e != nil {
		return out, safeError(e)
	}
	defer cur.Close(ctx)
	var docs []document
	if e = cur.All(ctx, &docs); e != nil {
		return out, safeError(e)
	}
	if len(docs) > p.Limit {
		out.HasMore = true
		docs = docs[:p.Limit]
	}
	for _, doc := range docs {
		var cp domain.Checkpoint
		if e = doc.decode(&cp); e != nil {
			return store.CheckpointPage{}, safeError(e)
		}
		out.Items = append(out.Items, cp)
		out.NextAfter = cp.Caller.InvocationID
	}
	return out, nil
}
