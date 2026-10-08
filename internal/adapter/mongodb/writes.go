package mongodb

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"reflect"
	"slices"
	"time"

	"github.com/hjhsamuel/cagent/internal/domain"
	"github.com/hjhsamuel/cagent/internal/store"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// compositeID 使用 BSON 长度编码避免含分隔符的资源 ID 相互混淆；只作内部文档 ID。
func compositeID(parts ...string) string {
	raw, _ := bson.Marshal(struct{ Parts []string }{parts})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// mutationDigest 仅编码无 map 的固定结构，BSON 保留原字节和 nil/空数组区别。
// 屏蔽存储生成字段与 CAS；格式前缀属于持久协议，字段增删须升级版本及迁移。
func mutationDigest(kind store.MutationKind, taskID string, status domain.RunStatus, messages []domain.Message, events []domain.Event, cp *domain.Checkpoint) ([32]byte, error) {
	ms := slices.Clone(messages)
	es := slices.Clone(events)
	for i := range ms {
		ms[i].CreatedAt = time.Time{}
		ms[i].Sequence = 0
	}
	for i := range es {
		es[i].CreatedAt = time.Time{}
		es[i].Sequence = 0
	}
	var checkpoint *domain.Checkpoint
	if cp != nil {
		v := *cp
		v.Version = 0
		v.UpdatedAt = time.Time{}
		checkpoint = &v
	}
	payload := struct {
		Format     string
		Kind       store.MutationKind
		TaskID     string
		Status     domain.RunStatus
		Messages   []domain.Message
		Events     []domain.Event
		Checkpoint *domain.Checkpoint
	}{"cagent-mutation-v1", kind, taskID, status, ms, es, checkpoint}
	raw, err := bson.Marshal(payload)
	return sha256.Sum256(raw), err
}
func (b *Database) receipt(ctx context.Context, scope domain.Scope, runID, op string, kind store.MutationKind, digest [32]byte) (store.CommitResult, bool, error) {
	if err := validateKey(scope, runID); err != nil {
		return store.CommitResult{}, false, err
	}
	if err := validateKey(scope, op); err != nil {
		return store.CommitResult{}, false, err
	}
	var rDoc document
	var r store.MutationReceipt
	err := b.collection(MutationReceiptCollection).FindOne(ctx, key(scope, compositeID(runID, op))).Decode(&rDoc)
	if err == nil {
		err = rDoc.decode(&r)
	}
	if errors.Is(err, mongo.ErrNoDocuments) {
		return store.CommitResult{}, false, nil
	}
	if err != nil {
		return store.CommitResult{}, false, err
	}
	if err = r.Match(scope, runID, op, kind, digest); err != nil {
		return store.CommitResult{}, false, err
	}
	return r.Result, true, nil
}
func (b *Database) saveReceipt(ctx context.Context, scope domain.Scope, runID, op string, kind store.MutationKind, digest [32]byte, result store.CommitResult) error {
	r := store.MutationReceipt{Scope: scope, RunID: runID, OperationID: op, Kind: kind, Digest: digest, Result: result}
	d, err := pack(scope, compositeID(runID, op), r, 1)
	if err != nil {
		return err
	}
	d.RunID = runID
	d.CallID = op
	_, err = b.collection(MutationReceiptCollection).InsertOne(ctx, d)
	return err
}

func (b *Database) saveRun(ctx context.Context, run *domain.Run, old document, updated document, now time.Time) error {
	v, err := increment(run.Version)
	if err != nil {
		return err
	}
	run.Version = v
	run.UpdatedAt = now
	updated, err = repack(updated, *run, v)
	if err != nil {
		return err
	}
	updated.Status = string(run.Status)
	oldUpdate, err := b.collection(RunCollection).ReplaceOne(ctx, old.versionKey(), updated)
	if err == nil && oldUpdate.MatchedCount != 1 {
		err = conflict("version")
	}
	return err
}

// appendEvents 仅向当前事务写入，绝不发布。终态标记仅由运行状态事务产生。
func (b *Database) appendEvents(ctx context.Context, run domain.Run, doc *document, events []domain.Event, now time.Time, allowTerminal bool) ([]domain.Event, error) {
	out := slices.Clone(events)
	for i := range out {
		e := &out[i]
		if err := e.ValidateForRun(run); err != nil {
			return nil, err
		}
		if e.Sequence != 0 {
			return nil, invalid("event.sequence")
		}
		switch e.Kind {
		case domain.EventRunCompleted, domain.EventRunFailed, domain.EventRunCancelled:
			if !allowTerminal {
				return nil, invalid("event.kind")
			}
		}
		seq, err := increment(doc.LastSequence)
		if err != nil {
			return nil, err
		}
		doc.LastSequence = seq
		e.Sequence = seq
		e.CreatedAt = now
		d, err := pack(run.Scope, compositeID(run.ID, stringInt(seq)), *e, 1)
		if err != nil {
			return nil, err
		}
		d.RunID = run.ID
		d.Sequence = seq
		_, err = b.collection(EventCollection).InsertOne(ctx, d)
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// output 原子更新会话消息计数及占用；调用前必须校验父 Run 和租约。
func (b *Database) output(ctx context.Context, run domain.Run, expected int64, messages []domain.Message, release bool, now time.Time) ([]domain.Message, int64, error) {
	var old document
	var s domain.Session
	err := b.collection(SessionCollection).FindOne(ctx, key(run.Scope, run.SessionID)).Decode(&old)
	if err == nil {
		err = old.decode(&s)
	}
	if err != nil {
		return nil, 0, err
	}
	if err = store.CheckVersion(expected, s.Version); err != nil {
		return nil, 0, err
	}
	if s.ActiveRunID != run.ID {
		return nil, 0, conflict("session.active_run_id")
	}
	out := slices.Clone(messages)
	updated := old
	for i := range out {
		m := &out[i]
		if err = m.ValidateForRun(run); err != nil {
			return nil, 0, err
		}
		if m.Sequence != 0 {
			return nil, 0, invalid("message.sequence")
		}
		seq, e := increment(updated.LastSequence)
		if e != nil {
			return nil, 0, e
		}
		updated.LastSequence = seq
		m.Sequence = seq
		m.CreatedAt = now
		d, e := pack(run.Scope, m.ID, *m, 1)
		if e != nil {
			return nil, 0, e
		}
		d.SessionID = run.SessionID
		d.RunID = run.ID
		d.Sequence = seq
		_, e = b.collection(MessageCollection).InsertOne(ctx, d)
		if e != nil {
			return nil, 0, e
		}
	}
	if len(out) > 0 || release {
		if release {
			s.ActiveRunID = ""
		}
		s.Version, err = increment(s.Version)
		if err != nil {
			return nil, 0, err
		}
		s.UpdatedAt = now
		updated, err = repack(updated, s, s.Version)
		if err != nil {
			return nil, 0, err
		}
		oldUpdate, err := b.collection(SessionCollection).ReplaceOne(ctx, old.versionKey(), updated)
		if err == nil && oldUpdate.MatchedCount != 1 {
			err = conflict("version")
		}
		if err != nil {
			return nil, 0, err
		}
	}
	return out, s.Version, nil
}

func (b *Database) checkpoint(ctx context.Context, run domain.Run, next domain.Checkpoint, expected int64, mode, callID string, now time.Time) (domain.Checkpoint, bool, error) {
	if err := next.ValidateForRun(run); err != nil {
		return next, false, err
	}
	if err := store.CheckVersion(expected, next.Version); err != nil {
		return next, false, err
	}
	id := compositeID(run.ID, next.Caller.InvocationID)
	var old document
	var current domain.Checkpoint
	err := b.collection(CheckpointCollection).FindOne(ctx, key(run.Scope, id)).Decode(&old)
	if err == nil {
		err = old.decode(&current)
	}
	exists := err == nil
	if err != nil && !errors.Is(err, mongo.ErrNoDocuments) {
		return next, false, err
	}
	if err = store.CheckVersion(expected, current.Version); err != nil {
		return next, false, err
	}
	if exists && current.Caller != next.Caller {
		return next, false, conflict("checkpoint.caller")
	}
	before := slices.Clone(current.PendingCallIDs)
	after := slices.Clone(next.PendingCallIDs)
	switch mode {
	case "track":
		if slices.Contains(before, callID) {
			return next, false, conflict("checkpoint.pending_call_ids")
		}
		before = append(before, callID)
	case "apply":
		before = slices.DeleteFunc(before, func(id string) bool { return id == callID })
	}
	slices.Sort(before)
	slices.Sort(after)
	if !slices.Equal(before, after) {
		return next, false, conflict("checkpoint.pending_call_ids")
	}
	compare := next
	compare.UpdatedAt = current.UpdatedAt
	if exists && reflect.DeepEqual(compare, current) {
		return current, false, nil
	}
	next.Version, err = increment(expected)
	if err != nil {
		return next, false, err
	}
	next.UpdatedAt = now
	d, err := pack(run.Scope, id, next, next.Version)
	if err != nil {
		return next, false, err
	}
	d.RunID = run.ID
	d.InvocationID = next.Caller.InvocationID
	if exists {
		oldUpdate, err := b.collection(CheckpointCollection).ReplaceOne(ctx, old.versionKey(), d)
		if err != nil {
			return next, false, err
		}
		if oldUpdate.MatchedCount != 1 {
			return next, false, conflict("version")
		}
	} else {
		_, err = b.collection(CheckpointCollection).InsertOne(ctx, d)
	}
	return next, true, err
}
