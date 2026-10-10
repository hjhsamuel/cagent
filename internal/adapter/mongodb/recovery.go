package mongodb

import (
	"context"

	"github.com/hjhsamuel/cagent/internal/domain"
	"github.com/hjhsamuel/cagent/internal/store"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// Recovery 是独立管理连接，必须使用仅内部恢复/保留流程可获得的配置和数据库身份。
// 普通 Database 不提供到本对象的转换。OpenRecovery 不初始化索引，部署先运行 Open。
// Go 类型不是权限沙箱；生产还应以 MongoDB RBAC 和服务装配隔离配置。
type Recovery struct{ b *Database }

func OpenRecovery(ctx context.Context, cfg Options) (*Recovery, error) {
	b, err := connect(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &Recovery{b: b}, nil
}
func (r *Recovery) Close(ctx context.Context) error { return r.b.Close(ctx) }

// Scan 使用完整三元组分页，不加载业务正文。服务端时钟过滤有效租约；扫描不是领取，
// 返回后必须 Acquire 并重读。末页后调用方从 nil 重扫，避免遗漏游标前的新候选。
func (r *Recovery) Scan(ctx context.Context, after *store.RecoveryPosition, limit int) (store.RecoveryPage, error) {
	var out store.RecoveryPage
	if err := store.ValidateRecoveryPage(after, limit); err != nil {
		return out, err
	}
	ctx, cancel := context.WithTimeout(ctx, r.b.timeout)
	defer cancel()
	now, err := r.b.now(ctx)
	if err != nil {
		return out, safeError(err)
	}
	match := bson.M{"recovery": true, "next_action_at": bson.M{"$lte": now}}
	if after != nil {
		match["$or"] = bson.A{bson.M{"next_action_at": bson.M{"$gt": after.NextActionAt}}, bson.M{"next_action_at": after.NextActionAt, "tenant_id": bson.M{"$gt": after.TenantID}}, bson.M{"next_action_at": after.NextActionAt, "tenant_id": after.TenantID, "user_id": bson.M{"$gt": after.UserID}}, bson.M{"next_action_at": after.NextActionAt, "tenant_id": after.TenantID, "user_id": after.UserID, "id": bson.M{"$gt": after.RunID}}}
	}
	pipeline := mongo.Pipeline{
		{{Key: "$match", Value: match}},
		{{Key: "$sort", Value: bson.D{{Key: "next_action_at", Value: 1}, {Key: "tenant_id", Value: 1}, {Key: "user_id", Value: 1}, {Key: "id", Value: 1}}}},
		{{Key: "$lookup", Value: bson.M{"from": "run_leases", "let": bson.M{"t": "$tenant_id", "u": "$user_id", "r": "$id"}, "pipeline": mongo.Pipeline{{{Key: "$match", Value: bson.M{"$expr": bson.M{"$and": bson.A{bson.M{"$eq": bson.A{"$tenant_id", "$$t"}}, bson.M{"$eq": bson.A{"$user_id", "$$u"}}, bson.M{"$eq": bson.A{"$id", "$$r"}}}}}}}}, "as": "lease"}}},
		{{Key: "$unwind", Value: bson.M{"path": "$lease", "preserveNullAndEmptyArrays": true}}},
		{{Key: "$match", Value: bson.M{"$expr": bson.M{"$lte": bson.A{bson.M{"$ifNull": bson.A{"$lease.expires_at", bson.DateTime(0)}}, "$$NOW"}}}}},
		{{Key: "$limit", Value: limit + 1}},
		{{Key: "$project", Value: bson.M{"_id": 0, "tenant_id": 1, "user_id": 1, "id": 1, "next_action_at": 1}}},
	}
	cur, err := r.b.collection(RunCollection).Aggregate(ctx, pipeline, options.Aggregate().SetHint("recovery_due"))
	if err != nil {
		return out, safeError(err)
	}
	defer cur.Close(ctx)
	var docs []runDocument
	if err = cur.All(ctx, &docs); err != nil {
		return out, safeError(err)
	}
	if len(docs) > limit {
		out.HasMore = true
		docs = docs[:limit]
	}
	for _, d := range docs {
		s := domain.Scope{TenantID: d.Tenant, UserID: d.User}
		if err = validateKey(s, d.ID); err != nil {
			return out, invariant()
		}
		out.Items = append(out.Items, store.RecoveryCandidate{Scope: s, RunID: d.ID})
		out.Next = &store.RecoveryPosition{TenantID: d.Tenant, UserID: d.User, RunID: d.ID, NextActionAt: d.NextActionAt}
	}
	return out, nil
}

// PruneEvents 原子清理连续前缀及推进水位。没有逐条 TTL，读者要么看到清理前完整
// 快照，要么得到明确过期错误。此管理维护只改水位、不推进业务 Run.Version。
func (r *Recovery) PruneEvents(ctx context.Context, scope domain.Scope, id string, through int64) error {
	if err := validateKey(scope, id); err != nil {
		return err
	}
	if through < 0 {
		return invalid("event.cursor")
	}
	err := r.b.withTransaction(ctx, "events.prune", func(tx context.Context) error {
		var old runDocument
		var storedRun domain.Run
		err := r.b.collection(RunCollection).FindOne(tx, key(scope, id)).Decode(&old)
		if err == nil {
			err = old.decode(&storedRun)
		}
		if err != nil {
			return err
		}
		if through > old.LastSequence {
			return invalid("event.cursor")
		}
		if through <= old.PrunedThrough {
			return nil
		}
		f := scoped(scope)
		f["run_id"] = id
		f["sequence"] = bson.M{"$lte": through}
		if _, err = r.b.collection(EventCollection).DeleteMany(tx, f); err != nil {
			return err
		}
		next := old
		next.PrunedThrough = through
		oldUpdate, err := r.b.collection(RunCollection).ReplaceOne(tx, old.versionKey(), next)
		if err == nil && oldUpdate.MatchedCount != 1 {
			err = conflict("version")
		}
		return err
	})
	return err
}
