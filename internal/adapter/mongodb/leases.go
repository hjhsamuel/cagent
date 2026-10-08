package mongodb

import (
	"context"
	"errors"
	"math"
	"strings"
	"time"

	"github.com/hjhsamuel/cagent/internal/domain"
	"github.com/hjhsamuel/cagent/internal/store"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func leaseValue(d leaseDocument) store.Lease {
	return store.Lease{Scope: domain.Scope{TenantID: d.Tenant, UserID: d.User}, RunID: d.ID, Owner: d.Owner, Fence: d.Fence, ExpiresAt: d.Expires}
}
func durationMillis(d time.Duration) (int64, error) {
	if d < time.Millisecond {
		return 0, invalid("lease.duration")
	}
	return d.Milliseconds(), nil
}

// AcquireLease 以数据库时钟授予租约；未到期时即使同 owner 也拒绝重复领取。
// Fence 单调递增；终态运行只有存在未结算任务时才允许获取维护租约。
func (b *Database) AcquireLease(ctx context.Context, scope domain.Scope, runID, owner string, duration time.Duration) (store.Lease, error) {
	if err := validateKey(scope, runID); err != nil {
		return store.Lease{}, err
	}
	if strings.TrimSpace(owner) == "" {
		return store.Lease{}, invalid("lease.owner")
	}
	ms, err := durationMillis(duration)
	if err != nil {
		return store.Lease{}, err
	}
	var result store.Lease
	commitErr := b.withTransaction(ctx, "acquire", func(tx context.Context) error {
		result = store.Lease{} // 每次重试独立构造结果，失败尝试不向调用方泄漏。
		var doc document
		var run domain.Run
		err := b.collection(RunCollection).FindOne(tx, key(scope, runID)).Decode(&doc)
		if err == nil {
			err = doc.decode(&run)
		}
		if err != nil {
			return err
		}
		if run.Status.IsTerminal() && doc.Unsettled == 0 {
			return conflict("lease")
		}
		f := key(scope, runID)
		f["fence"] = bson.M{"$lt": int64(math.MaxInt64)}
		f["revision"] = bson.M{"$lt": int64(math.MaxInt64)}
		f["$expr"] = bson.M{"$lte": bson.A{"$expires_at", "$$NOW"}}
		pipeline := mongo.Pipeline{{{Key: "$set", Value: bson.M{"owner": bson.M{"$literal": owner}, "fence": bson.M{"$add": bson.A{"$fence", 1}}, "revision": bson.M{"$add": bson.A{"$revision", 1}}, "expires_at": bson.M{"$add": bson.A{"$$NOW", ms}}}}}}
		var d leaseDocument
		err = b.collection(LeaseCollection).FindOneAndUpdate(tx, f, pipeline, options.FindOneAndUpdate().SetReturnDocument(options.After)).Decode(&d)
		if errors.Is(err, mongo.ErrNoDocuments) {
			err = conflict("lease")
		}
		result = leaseValue(d)
		return err
	})
	if commitErr != nil {
		return store.Lease{}, commitErr
	}
	return result, nil
}

// leaseFilter 中两个有效期条件都用服务端 $$NOW，防止应用时钟偏差或续期后的旧副本复活。
func leaseFilter(l store.Lease) bson.M {
	f := key(l.Scope, l.RunID)
	f["owner"] = l.Owner
	f["fence"] = l.Fence
	f["revision"] = bson.M{"$lt": int64(math.MaxInt64)}
	f["$expr"] = bson.M{"$and": bson.A{bson.M{"$gt": bson.A{"$expires_at", "$$NOW"}}, bson.M{"$gt": bson.A{l.ExpiresAt, "$$NOW"}}}}
	return f
}

// RenewLease 要求原 owner、Fence 和呈交租约均有效，按数据库时间延长有效期。
func (b *Database) RenewLease(ctx context.Context, l store.Lease, duration time.Duration) (store.Lease, error) {
	if err := l.Validate(); err != nil {
		return store.Lease{}, err
	}
	ms, err := durationMillis(duration)
	if err != nil {
		return store.Lease{}, err
	}
	var result store.Lease
	commitErr := b.withTransaction(ctx, "renew", func(tx context.Context) error {
		result = store.Lease{} // 每次重试独立构造结果，失败尝试不向调用方泄漏。
		var d leaseDocument
		err := b.collection(LeaseCollection).FindOneAndUpdate(tx, leaseFilter(l), mongo.Pipeline{{{Key: "$set", Value: bson.M{"revision": bson.M{"$add": bson.A{"$revision", 1}}, "expires_at": bson.M{"$add": bson.A{"$$NOW", ms}}}}}}, options.FindOneAndUpdate().SetReturnDocument(options.After)).Decode(&d)
		if errors.Is(err, mongo.ErrNoDocuments) {
			err = conflict("lease")
		}
		result = leaseValue(d)
		return err
	})
	if commitErr != nil {
		return store.Lease{}, commitErr
	}
	return result, nil
}

// ReleaseLease 撤销有效租约并保留 Fence；不释放会话占用，运行仍须正常终结。
func (b *Database) ReleaseLease(ctx context.Context, l store.Lease) error {
	if err := l.Validate(); err != nil {
		return err
	}
	err := b.withTransaction(ctx, "release", func(tx context.Context) error {
		r, err := b.collection(LeaseCollection).UpdateOne(tx, leaseFilter(l), bson.M{"$set": bson.M{"owner": "", "expires_at": time.Time{}}, "$inc": bson.M{"revision": 1}})
		if err != nil {
			return err
		}
		if r.MatchedCount != 1 {
			return conflict("lease")
		}
		return nil
	})
	return err
}

// guard 实际递增租约 Revision，使接管、续期、取消与业务写入在同一行产生写冲突。
// 不能把这次写入换成只读校验或无变化更新，否则快照隔离下旧持有者仍可能提交。
func (b *Database) guard(ctx context.Context, g store.WriteGuard) (domain.Run, document, time.Time, error) {
	var zero domain.Run
	var empty document
	if err := g.Validate(); err != nil {
		return zero, empty, time.Time{}, err
	}
	var doc document
	var run domain.Run
	err := b.collection(RunCollection).FindOne(ctx, key(g.Lease.Scope, g.Lease.RunID)).Decode(&doc)
	if err == nil {
		err = doc.decode(&run)
	}
	if err != nil {
		return run, doc, time.Time{}, err
	}
	if err = store.CheckVersion(g.RunVersion, run.Version); err != nil {
		return run, doc, time.Time{}, err
	}
	r, err := b.collection(LeaseCollection).UpdateOne(ctx, leaseFilter(g.Lease), bson.M{"$inc": bson.M{"revision": 1}})
	if err != nil {
		return run, doc, time.Time{}, err
	}
	if r.MatchedCount != 1 {
		return run, doc, time.Time{}, conflict("lease")
	}
	now, err := b.now(ctx)
	return run, doc, now, err
}
func (b *Database) revoke(ctx context.Context, scope domain.Scope, runID string) error {
	f := key(scope, runID)
	f["fence"] = bson.M{"$lt": int64(math.MaxInt64)}
	f["revision"] = bson.M{"$lt": int64(math.MaxInt64)}
	r, err := b.collection(LeaseCollection).UpdateOne(ctx, f, bson.M{"$inc": bson.M{"fence": 1, "revision": 1}, "$set": bson.M{"owner": "", "expires_at": time.Time{}}})
	if err != nil {
		return err
	}
	if r.MatchedCount != 1 {
		return conflict("lease")
	}
	return nil
}
