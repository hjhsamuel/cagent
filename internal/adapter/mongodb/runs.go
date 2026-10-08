package mongodb

import (
	"context"
	"errors"
	"strconv"

	"github.com/hjhsamuel/cagent/internal/domain"
	"github.com/hjhsamuel/cagent/internal/store"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

func stringInt(n int64) string { return strconv.FormatInt(n, 10) }

// ReplayRun 仅查找已提交的幂等请求，不创建新 Run。容量已满时仍允许客户端找回
// 丢失的响应；不存在返回 NotFound，调用方绝不能借此绕过新运行的容量预留。
func (b *Database) ReplayRun(ctx context.Context, scope domain.Scope, sessionID, idempotencyKey string, parts []domain.Part) (domain.Run, error) {
	if err := scope.Validate(); err != nil {
		return domain.Run{}, err
	}
	if sessionID == "" || idempotencyKey == "" {
		return domain.Run{}, invalid("run.idempotency_key")
	}
	ctx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()
	filter := scoped(scope)
	filter["session_id"] = sessionID
	filter["idempotency_key"] = idempotencyKey
	var d document
	if err := b.collection(RunCollection).FindOne(ctx, filter).Decode(&d); err != nil {
		return domain.Run{}, safeError(err)
	}
	if len(d.Fingerprint) != 32 {
		return domain.Run{}, invariant()
	}
	var saved [32]byte
	copy(saved[:], d.Fingerprint)
	if err := store.CheckIdempotentInput(saved, store.InputFingerprint(parts)); err != nil {
		return domain.Run{}, err
	}
	var run domain.Run
	err := d.decode(&run)
	return run, safeError(err)
}

// StartRun 原子保存运行、输入、会话占用及创建回执。
// 幂等查询先于版本校验，相同会话/键/输入重试返回原结果；不同输入冲突。
func (b *Database) StartRun(ctx context.Context, req store.StartRunRequest) (store.StartRunResult, error) {
	if err := req.Validate(); err != nil {
		return store.StartRunResult{}, err
	}
	digest := store.InputFingerprint(req.Input.Parts)
	var result store.StartRunResult
	commitErr := b.withTransaction(ctx, "start", func(tx context.Context) error {
		result = store.StartRunResult{} // 每次重试独立构造结果，失败尝试不向调用方泄漏。
		var out store.StartRunResult
		if req.Run.IdempotencyKey != "" {
			f := scoped(req.Run.Scope)
			f["session_id"] = req.Run.SessionID
			f["idempotency_key"] = req.Run.IdempotencyKey
			var d document
			var storedRun domain.Run
			err := b.collection(RunCollection).FindOne(tx, f).Decode(&d)
			if err == nil {
				err = d.decode(&storedRun)
			}
			if err == nil {
				var saved [32]byte
				if len(d.Fingerprint) != 32 {
					return invariant()
				}
				copy(saved[:], d.Fingerprint)
				if err = store.CheckIdempotentInput(saved, digest); err != nil {
					return err
				}
				if err = bson.Unmarshal(d.Start, &out); err != nil {
					return err
				}
				out.Replayed = true
				result = out
				return nil
			}
			if !errors.Is(err, mongo.ErrNoDocuments) {
				result = out
				return err
			}
		}
		var old document
		var s domain.Session
		err := b.collection(SessionCollection).FindOne(tx, key(req.Run.Scope, req.Run.SessionID)).Decode(&old)
		if err == nil {
			err = old.decode(&s)
		}
		if err != nil {
			return err
		}
		if err = store.CheckVersion(req.ExpectedSessionVersion, s.Version); err != nil {
			return err
		}
		if s.ActiveRunID != "" {
			return conflict("session.active_run_id")
		}
		now, err := b.now(tx)
		if err != nil {
			return err
		}
		run := req.Run
		run.Version = 1
		run.CreatedAt = now
		run.UpdatedAt = now
		input := req.Input
		input.Sequence, err = increment(old.LastSequence)
		if err != nil {
			return err
		}
		input.CreatedAt = now
		s.Version, err = increment(s.Version)
		if err != nil {
			return err
		}
		s.ActiveRunID = run.ID
		s.UpdatedAt = now
		updated, err := repack(old, s, s.Version)
		if err != nil {
			return err
		}
		updated.LastSequence = input.Sequence
		// 先写会话，使同会话并发提交以事务写冲突重试；重试后优先发现幂等命中。
		oldUpdate, err := b.collection(SessionCollection).ReplaceOne(tx, old.versionKey(), updated)
		if err == nil && oldUpdate.MatchedCount != 1 {
			err = conflict("version")
		}
		if err != nil {
			return err
		}
		out = store.StartRunResult{Run: run, Input: input, SessionVersion: s.Version}
		d, err := pack(run.Scope, run.ID, run, 1)
		if err != nil {
			return err
		}
		d.SessionID = run.SessionID
		d.Status = string(run.Status)
		d.IdempotencyKey = run.IdempotencyKey
		d.Fingerprint = digest[:]
		d.Start, err = bson.Marshal(out)
		if err != nil {
			return err
		}
		_, err = b.collection(RunCollection).InsertOne(tx, d)
		if err != nil {
			return err
		}
		m, err := pack(input.Scope, input.ID, input, 1)
		if err != nil {
			return err
		}
		m.SessionID = run.SessionID
		m.RunID = run.ID
		m.Sequence = input.Sequence
		_, err = b.collection(MessageCollection).InsertOne(tx, m)
		if err != nil {
			return err
		}
		_, err = b.collection(LeaseCollection).InsertOne(tx, leaseDocument{Tenant: run.Scope.TenantID, User: run.Scope.UserID, ID: run.ID})
		result = out
		return err
	})
	if commitErr != nil {
		return store.StartRunResult{}, commitErr
	}
	return result, nil
}

func terminalEvents(status domain.RunStatus, events []domain.Event, run domain.Run) ([]domain.Event, error) {
	want := domain.EventKind("")
	switch status {
	case domain.RunCompleted:
		want = domain.EventRunCompleted
	case domain.RunFailed:
		want = domain.EventRunFailed
	case domain.RunCancelled:
		want = domain.EventRunCancelled
	}
	count := 0
	for i, e := range events {
		switch e.Kind {
		case domain.EventRunCompleted, domain.EventRunFailed, domain.EventRunCancelled:
			count++
			if e.Kind != want || i != len(events)-1 {
				return nil, invalid("event.kind")
			}
		}
	}
	if count > 1 {
		return nil, invalid("event.kind")
	}
	if want != "" && count == 0 {
		events = append(append([]domain.Event(nil), events...), domain.Event{Scope: run.Scope, RunID: run.ID, Kind: want})
	}
	return events, nil
}

// CommitRun 在租约及版本检查后提交状态、消息、事件和可选检查点。
// OperationID 的回执用于确认丢失后的重放；终态提交同时释放会话并撤销租约。
func (b *Database) CommitRun(ctx context.Context, req store.CommitRunRequest) (store.CommitResult, error) {
	digest, err := mutationDigest(store.MutationRunCommit, "", req.Status, req.Messages, req.Events, req.Checkpoint)
	if err != nil {
		return store.CommitResult{}, safeError(err)
	}
	var result store.CommitResult
	commitErr := b.withTransaction(ctx, "commit", func(tx context.Context) error {
		result = store.CommitResult{} // 每次重试独立构造结果，失败尝试不向调用方泄漏。
		out, found, err := b.receipt(tx, req.Guard.Lease.Scope, req.Guard.Lease.RunID, req.OperationID, store.MutationRunCommit, digest)
		if err != nil || found {
			result = out
			return err
		}
		run, old, now, err := b.guard(tx, req.Guard)
		if err != nil {
			return err
		}
		if err = req.ValidateForRun(run); err != nil {
			return err
		}
		events, err := terminalEvents(req.Status, req.Events, run)
		if err != nil {
			return err
		}
		updated := old
		cpChanged := false
		if req.Checkpoint != nil {
			cp, changed, e := b.checkpoint(tx, run, *req.Checkpoint, req.ExpectedCheckpointVersion, "ordinary", "", now)
			if e != nil {
				return e
			}
			cpChanged = changed
			out.Checkpoint = &cp
		}
		out.Messages, out.SessionVersion, err = b.output(tx, run, req.ExpectedSessionVersion, req.Messages, req.Status.IsTerminal(), now)
		if err != nil {
			return err
		}
		out.Events, err = b.appendEvents(tx, run, &updated, events, now, req.Status.IsTerminal())
		if err != nil {
			return err
		}
		changed := run.Status != req.Status || len(out.Messages) > 0 || len(out.Events) > 0 || cpChanged
		run.Status = req.Status
		if changed {
			if err = b.saveRun(tx, &run, old, updated, now); err != nil {
				return err
			}
		}
		if run.Status.IsTerminal() {
			if err = b.revoke(tx, run.Scope, run.ID); err != nil {
				return err
			}
		}
		out.Run = run
		err = b.saveReceipt(tx, run.Scope, run.ID, req.OperationID, store.MutationRunCommit, digest, out)
		result = out
		return err
	})
	if commitErr != nil {
		return store.CommitResult{}, commitErr
	}
	return result, nil
}

// CancelRun 以可信作用域和 Run 版本原子取消运行，释放会话并递增 Fence。
// 不把远端任务直接改为 cancelled；后续跟踪使用维护租约处理剩余任务。
func (b *Database) CancelRun(ctx context.Context, req store.CancelRunRequest) (store.CommitResult, error) {
	if err := validateKey(req.Scope, req.RunID); err != nil {
		return store.CommitResult{}, err
	}
	if req.ExpectedRunVersion <= 0 {
		return store.CommitResult{}, invalid("run.version")
	}
	var result store.CommitResult
	commitErr := b.withTransaction(ctx, "cancel", func(tx context.Context) error {
		result = store.CommitResult{} // 每次重试独立构造结果，失败尝试不向调用方泄漏。
		var out store.CommitResult
		var old document
		var run domain.Run
		err := b.collection(RunCollection).FindOne(tx, key(req.Scope, req.RunID)).Decode(&old)
		if err == nil {
			err = old.decode(&run)
		}
		if err != nil {
			return err
		}
		out.Run = run
		var sDoc document
		var s domain.Session
		err = b.collection(SessionCollection).FindOne(tx, key(run.Scope, run.SessionID)).Decode(&sDoc)
		if err == nil {
			err = sDoc.decode(&s)
		}
		if err != nil {
			return err
		}
		out.SessionVersion = s.Version
		if run.Status.IsTerminal() {
			result = out
			return nil
		}
		if err = store.CheckVersion(req.ExpectedRunVersion, run.Version); err != nil {
			return err
		}
		if err = b.revoke(tx, run.Scope, run.ID); err != nil {
			return err
		}
		now, err := b.now(tx)
		if err != nil {
			return err
		}
		out.Messages, out.SessionVersion, err = b.output(tx, run, s.Version, nil, true, now)
		if err != nil {
			return err
		}
		run.Status = domain.RunCancelled
		updated := old
		events, _ := terminalEvents(run.Status, nil, run)
		out.Events, err = b.appendEvents(tx, run, &updated, events, now, true)
		if err != nil {
			return err
		}
		if err = b.saveRun(tx, &run, old, updated, now); err != nil {
			return err
		}
		out.Run = run
		result = out
		return nil
	})
	if commitErr != nil {
		return store.CommitResult{}, commitErr
	}
	return result, nil
}
