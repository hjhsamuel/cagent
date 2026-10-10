package mongodb

import (
	"context"
	"reflect"

	"github.com/hjhsamuel/cagent/internal/domain"
	"github.com/hjhsamuel/cagent/internal/store"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// SaveSnapshot 校验租约、会话与快照版本，以及快照覆盖的已持久消息范围。
// 只保存派生数据，不删除历史；内容变化时原子推进快照和 Run 的版本。
func (r *Database) SaveSnapshot(ctx context.Context, g store.WriteGuard, next domain.ContextSnapshot, expected, sessionVersion int64) (domain.ContextSnapshot, error) {
	var result domain.ContextSnapshot
	commitErr := r.withTransaction(ctx, "snapshot.save", func(tx context.Context) error {
		result = domain.ContextSnapshot{} // 每次重试独立构造结果，失败尝试不向调用方泄漏。
		next := next                      // 驱动重放事务回调时，不能继承上次尝试自增后的版本。
		run, old, now, err := r.guard(tx, g)
		if err != nil {
			return err
		}
		if run.Status.IsTerminal() {
			return conflict("run.status")
		}
		var sd sessionDocument
		var s domain.Session
		err = r.collection(SessionCollection).FindOne(tx, key(run.Scope, run.SessionID)).Decode(&sd)
		if err == nil {
			err = sd.decode(&s)
		}
		if err != nil {
			return err
		}
		if err = next.ValidateForSession(s); err != nil {
			return err
		}
		if s.ActiveRunID != run.ID {
			return conflict("session.active_run_id")
		}
		if err = store.CheckVersion(sessionVersion, s.Version); err != nil {
			return err
		}
		if next.Version != expected || next.ThroughSequence < 0 || next.ThroughSequence > sd.LastSequence {
			return invalid("snapshot")
		}
		f := scoped(s.Scope)
		f["session_id"] = s.ID
		var docs []snapshotDocument
		cursor, err := r.collection(SnapshotCollection).Find(tx, f, options.Find().SetSort(bson.D{{Key: "version", Value: -1}}).SetLimit(int64(1)).SetCollation(&options.Collation{Locale: "simple"}))
		if err == nil {
			defer cursor.Close(tx)
			err = cursor.All(tx, &docs)
		}
		if err != nil {
			return err
		}
		current := domain.ContextSnapshot{}
		if len(docs) > 0 {
			err = docs[0].decode(&current)
			if err != nil {
				return err
			}
		}
		if err = store.CheckVersion(expected, current.Version); err != nil {
			return err
		}
		if next.ThroughSequence < current.ThroughSequence {
			return conflict("snapshot.through_sequence")
		}
		compare := next
		compare.CreatedAt = current.CreatedAt
		if reflect.DeepEqual(compare, current) {
			result = current
			return nil
		}
		next.Version, err = increment(expected)
		if err != nil {
			return err
		}
		next.CreatedAt = now
		d, err := packSnapshot(next.Scope, compositeID(s.ID, stringInt(next.Version)), next, next.Version)
		if err != nil {
			return err
		}
		d.SessionID = s.ID
		_, err = r.collection(SnapshotCollection).InsertOne(tx, d)
		if err != nil {
			return err
		}
		if err = r.saveRun(tx, &run, old, old, now); err != nil {
			return err
		}
		result = next
		return nil
	})
	if commitErr != nil {
		return domain.ContextSnapshot{}, commitErr
	}
	return result, nil
}
