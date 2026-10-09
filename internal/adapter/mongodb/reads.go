package mongodb

import (
	"context"
	"errors"

	"github.com/hjhsamuel/cagent/internal/domain"
	"github.com/hjhsamuel/cagent/internal/store"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// GetSession 在可信作用域内读取记录；不存在或属于其他作用域时返回同一种未找到错误。
func (r *Database) GetSession(ctx context.Context, s domain.Scope, id string) (domain.Session, error) {
	if err := validateKey(s, id); err != nil {
		return domain.Session{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	var vDoc document
	var v domain.Session
	err := r.collection(SessionCollection).FindOne(ctx, key(s, id)).Decode(&vDoc)
	if err == nil {
		err = r.decode(ctx, vDoc, &v)
	}
	return v, safeError(err)
}

// GetRun 在可信作用域内读取记录；不存在或属于其他作用域时返回同一种未找到错误。
func (r *Database) GetRun(ctx context.Context, s domain.Scope, id string) (domain.Run, error) {
	if err := validateKey(s, id); err != nil {
		return domain.Run{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	var vDoc document
	var v domain.Run
	err := r.collection(RunCollection).FindOne(ctx, key(s, id)).Decode(&vDoc)
	if err == nil {
		err = r.decode(ctx, vDoc, &v)
	}
	return v, safeError(err)
}

// GetTask 在可信作用域内读取记录；不存在或属于其他作用域时返回同一种未找到错误。
func (r *Database) GetTask(ctx context.Context, s domain.Scope, id string) (domain.Task, error) {
	if err := validateKey(s, id); err != nil {
		return domain.Task{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	var vDoc document
	var v domain.Task
	err := r.collection(TaskCollection).FindOne(ctx, key(s, id)).Decode(&vDoc)
	if err == nil {
		err = r.decode(ctx, vDoc, &v)
	}
	return v, safeError(err)
}

// GetTaskDelivery 在可信作用域内读取记录；不存在或属于其他作用域时返回同一种未找到错误。
func (r *Database) GetTaskDelivery(ctx context.Context, s domain.Scope, id string) (domain.TaskDelivery, error) {
	if err := validateKey(s, id); err != nil {
		return domain.TaskDelivery{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	var vDoc document
	var v domain.TaskDelivery
	err := r.collection(TaskDeliveryCollection).FindOne(ctx, key(s, id)).Decode(&vDoc)
	if err == nil {
		err = r.decode(ctx, vDoc, &v)
	}
	return v, safeError(err)
}

// GetCheckpoint 按 Scope、RunID 和 InvocationID 读取原执行分支的检查点，不能跨分支恢复。
func (r *Database) GetCheckpoint(ctx context.Context, s domain.Scope, runID, invocationID string) (domain.Checkpoint, error) {
	if err := validateKey(s, runID); err != nil {
		return domain.Checkpoint{}, err
	}
	if err := validateKey(s, invocationID); err != nil {
		return domain.Checkpoint{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	f := scoped(s)
	f["run_id"] = runID
	f["invocation_id"] = invocationID
	var vDoc document
	var v domain.Checkpoint
	err := r.collection(CheckpointCollection).FindOne(ctx, f).Decode(&vDoc)
	if err == nil {
		err = r.decode(ctx, vDoc, &v)
	}
	return v, safeError(err)
}

// FindRunByKey 只在指定会话和可信作用域内查找非空幂等键；不跨会话复用运行。
func (r *Database) FindRunByKey(ctx context.Context, s domain.Scope, sessionID, idem string) (domain.Run, error) {
	if err := validateKey(s, sessionID); err != nil {
		return domain.Run{}, err
	}
	if err := validateKey(s, idem); err != nil {
		return domain.Run{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	f := scoped(s)
	f["session_id"] = sessionID
	f["idempotency_key"] = idem
	var vDoc document
	var v domain.Run
	err := r.collection(RunCollection).FindOne(ctx, f).Decode(&vDoc)
	if err == nil {
		err = r.decode(ctx, vDoc, &v)
	}
	return v, safeError(err)
}

// CreateSession 要求初始 Version=0 且无活动运行，持久化时设为版本 1。
// 创建时间取自数据库；事务失败不会留下半初始化记录。
func (r *Database) CreateSession(ctx context.Context, s domain.Session) error {
	if err := s.Validate(); err != nil {
		return err
	}
	if s.Version != 0 || s.ActiveRunID != "" {
		return invalid("session")
	}
	err := r.withTransaction(ctx, "session.create", func(tx context.Context) error {
		now, err := r.now(tx)
		if err != nil {
			return err
		}
		value := s
		value.Version = 1
		value.CreatedAt = now
		value.UpdatedAt = now
		d, err := pack(s.Scope, s.ID, value, 1)
		if err != nil {
			return err
		}
		_, err = r.collection(SessionCollection).InsertOne(tx, d)
		return err
	})
	return err
}

// ListMessages 在同一快照内读取会话水位和消息，按序号升序分页。
// 游标不能超过已提交水位；多取一条判断 HasMore，不消费该条消息。
func (r *Database) ListMessages(ctx context.Context, s domain.Scope, id string, p store.SequencePage) (store.MessagePage, error) {
	if err := validateKey(s, id); err != nil {
		return store.MessagePage{}, err
	}
	if err := p.Validate(); err != nil {
		return store.MessagePage{}, err
	}
	var result store.MessagePage
	commitErr := r.withTransaction(ctx, "messages.list", func(tx context.Context) error {
		result = store.MessagePage{} // 每次重试独立构造结果，失败尝试不向调用方泄漏。
		out := store.MessagePage{NextAfter: p.After}
		var session document
		var storedSession domain.Session
		err := r.collection(SessionCollection).FindOne(tx, key(s, id)).Decode(&session)
		if err == nil {
			err = r.decode(tx, session, &storedSession)
		}
		if err != nil {
			return err
		}
		if p.After > session.LastSequence {
			return invalid("message.cursor")
		}
		f := scoped(s)
		f["session_id"] = id
		f["sequence"] = bson.M{"$gt": p.After}
		var docs []document
		cursor, err := r.collection(MessageCollection).Find(tx, f, options.Find().SetSort(bson.D{{Key: "sequence", Value: 1}}).SetLimit(int64(p.Limit+1)).SetCollation(&options.Collation{Locale: "simple"}))
		if err == nil {
			defer cursor.Close(tx)
			err = cursor.All(tx, &docs)
		}
		if err != nil {
			return err
		}
		if len(docs) > p.Limit {
			out.HasMore = true
			docs = docs[:p.Limit]
		}
		for _, d := range docs {
			var v domain.Message
			err := r.decode(tx, d, &v)
			if err != nil {
				return err
			}
			out.Items = append(out.Items, v)
			out.NextAfter = v.Sequence
		}
		result = out
		return nil
	})
	if commitErr != nil {
		return store.MessagePage{}, commitErr
	}
	return result, nil
}

// ListEvents 在同一快照内返回事件和保留水位，避免清理与分页产生不一致。
// 已清理游标返回明确错误，NextAfter 只推进到实际返回的最后一条。
func (r *Database) ListEvents(ctx context.Context, s domain.Scope, id string, p store.SequencePage) (store.EventPage, error) {
	if err := validateKey(s, id); err != nil {
		return store.EventPage{}, err
	}
	if err := p.Validate(); err != nil {
		return store.EventPage{}, err
	}
	var result store.EventPage
	commitErr := r.withTransaction(ctx, "events.list", func(tx context.Context) error {
		result = store.EventPage{} // 每次重试独立构造结果，失败尝试不向调用方泄漏。
		out := store.EventPage{NextAfter: p.After}
		var run document
		var storedRun domain.Run
		err := r.collection(RunCollection).FindOne(tx, key(s, id)).Decode(&run)
		if err == nil {
			err = r.decode(tx, run, &storedRun)
		}
		if err != nil {
			return err
		}
		out.LastSequence = run.LastSequence
		out.Terminal = storedRun.Status.IsTerminal()
		out.PrunedThrough = run.PrunedThrough
		if err = store.CheckEventCursor(p.After, run.PrunedThrough, run.LastSequence); err != nil {
			return err
		}
		f := scoped(s)
		f["run_id"] = id
		f["sequence"] = bson.M{"$gt": p.After}
		var docs []document
		cursor, err := r.collection(EventCollection).Find(tx, f, options.Find().SetSort(bson.D{{Key: "sequence", Value: 1}}).SetLimit(int64(p.Limit+1)).SetCollation(&options.Collation{Locale: "simple"}))
		if err == nil {
			defer cursor.Close(tx)
			err = cursor.All(tx, &docs)
		}
		if err != nil {
			return err
		}
		if len(docs) > p.Limit {
			out.HasMore = true
			docs = docs[:p.Limit]
		}
		for _, d := range docs {
			var v domain.Event
			err := r.decode(tx, d, &v)
			if err != nil {
				return err
			}
			out.Items = append(out.Items, v)
			out.NextAfter = v.Sequence
		}
		result = out
		return nil
	})
	if commitErr != nil {
		return store.EventPage{}, commitErr
	}
	return result, nil
}

// ListUnsettledTasks 返回活动任务及仍有 pending delivery 的终态任务。
// 终态任务缺少交付记录属于存储异常；已消费或丢弃的任务不再参与恢复。
func (r *Database) ListUnsettledTasks(ctx context.Context, s domain.Scope, id string, p store.KeyPage) (store.TaskPage, error) {
	if err := validateKey(s, id); err != nil {
		return store.TaskPage{}, err
	}
	if err := p.Validate(); err != nil {
		return store.TaskPage{}, err
	}
	var result store.TaskPage
	commitErr := r.withTransaction(ctx, "tasks.list", func(tx context.Context) error {
		result = store.TaskPage{} // 每次重试独立构造结果，失败尝试不向调用方泄漏。
		out := store.TaskPage{NextAfter: p.After}
		var value1629Doc document
		var storedRun domain.Run
		err := r.collection(RunCollection).FindOne(tx, key(s, id)).Decode(&value1629Doc)
		if err == nil {
			err = r.decode(tx, value1629Doc, &storedRun)
		}
		if err != nil {
			return err
		}
		f := scoped(s)
		f["run_id"] = id
		f["unsettled"] = 1
		f["id"] = bson.M{"$gt": p.After}
		var docs []document
		cursor, err := r.collection(TaskCollection).Find(tx, f, options.Find().SetSort(bson.D{{Key: "id", Value: 1}}).SetLimit(int64(p.Limit+1)).SetCollation(&options.Collation{Locale: "simple"}))
		if err == nil {
			defer cursor.Close(tx)
			err = cursor.All(tx, &docs)
		}
		if err != nil {
			return err
		}
		if len(docs) > p.Limit {
			out.HasMore = true
			docs = docs[:p.Limit]
		}
		for _, d := range docs {
			var t domain.Task
			err := r.decode(tx, d, &t)
			if err != nil {
				return err
			}
			if t.Status.IsTerminal() {
				var deliveryDoc document
				var delivery domain.TaskDelivery
				err := r.collection(TaskDeliveryCollection).FindOne(tx, key(s, t.ID)).Decode(&deliveryDoc)
				if err == nil {
					err = r.decode(tx, deliveryDoc, &delivery)
				}
				if err != nil {
					if errors.Is(err, mongo.ErrNoDocuments) {
						return invariant()
					}
					result = out
					return err
				}
				if delivery.State != domain.DeliveryPending {
					return invariant()
				}
			}
			out.Items = append(out.Items, t)
			out.NextAfter = t.ID
		}
		result = out
		return nil
	})
	if commitErr != nil {
		return store.TaskPage{}, commitErr
	}
	return result, nil
}

// LatestSnapshot 读取指定会话版本最高的派生快照；没有快照返回 ErrNotFound。
func (r *Database) LatestSnapshot(ctx context.Context, s domain.Scope, id string) (domain.ContextSnapshot, error) {
	if err := validateKey(s, id); err != nil {
		return domain.ContextSnapshot{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	f := scoped(s)
	f["session_id"] = id
	var docs []document
	cursor, err := r.collection(SnapshotCollection).Find(ctx, f, options.Find().SetSort(bson.D{{Key: "version", Value: -1}}).SetLimit(int64(1)).SetCollation(&options.Collation{Locale: "simple"}))
	if err == nil {
		defer cursor.Close(ctx)
		err = cursor.All(ctx, &docs)
	}
	if err != nil {
		return domain.ContextSnapshot{}, safeError(err)
	}
	if len(docs) == 0 {
		return domain.ContextSnapshot{}, safeError(mongo.ErrNoDocuments)
	}
	var v domain.ContextSnapshot
	err = r.decode(ctx, docs[0], &v)
	return v, safeError(err)
}
