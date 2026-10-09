package app

import (
	"context"
	"errors"
	"strings"

	"github.com/hjhsamuel/cagent/internal/adapter/mongodb"
	"github.com/hjhsamuel/cagent/internal/agent"
	"github.com/hjhsamuel/cagent/internal/apperrors"
	"github.com/hjhsamuel/cagent/internal/contextengine"
	"github.com/hjhsamuel/cagent/internal/domain"
	"github.com/hjhsamuel/cagent/internal/store"
)

// ContextOptions 来自可信装配配置。System 为固定约束，不能使用用户或工具文本填充。
type ContextOptions struct {
	System        []domain.Part
	PolicyVersion string
	// PreserveUsers 保护旧摘要覆盖范围中的用户原文；生产 ADK 装配始终开启，
	// 即使临时禁用新摘要生成，也不能因沿用旧快照而丢失用户要求。
	PreserveUsers    bool
	ArchiveCompleted bool
	// Compression 为 nil 时保持只读的 P5 准备行为。摘要候选由应用租约持有者保存。
	Compression *contextengine.CompressionPolicy
	Summarizer  contextengine.Summarizer
}

// NewContextPreparer 创建可直接赋给 Options.Prepare 的请求准备函数。
// 保留历史、快照和工具关联校验，不计算 Token 预算。
// 系统内容在构造时深拷贝，此后各次调用再次独立复制，避免用户之间共享可变消息。
func NewContextPreparer(db *mongodb.Database, engine contextengine.Engine, opts ContextOptions) (func(context.Context, domain.Run) (agent.Request, error), error) {
	if db == nil || engine == nil || strings.TrimSpace(opts.PolicyVersion) == "" {
		return nil, invalid("context.options")
	}
	system := copyParts(opts.System)
	return func(ctx context.Context, run domain.Run) (agent.Request, error) {
		if err := run.Validate(); err != nil {
			return agent.Request{}, err
		}
		session, err := db.GetSession(ctx, run.Scope, run.SessionID)
		if err != nil {
			return agent.Request{}, err
		}
		in := contextengine.Input{Session: session, RunID: run.ID, PolicyVersion: opts.PolicyVersion, PreserveUsers: opts.PreserveUsers, ArchiveCompleted: opts.ArchiveCompleted}
		if system != nil {
			in.System = []domain.Message{{Scope: run.Scope, ID: "configured-system", SessionID: run.SessionID, Role: domain.RoleSystem, Parts: copyParts(system)}}
		}
		var after int64
		snapshot, snapshotErr := db.LatestSnapshot(ctx, run.Scope, run.SessionID)
		if snapshotErr == nil {
			in.Snapshot = &snapshot
		} else if !errors.Is(snapshotErr, apperrors.ErrNotFound) {
			return agent.Request{}, snapshotErr
		}
		if snapshotErr == nil && snapshot.PolicyVersion != opts.PolicyVersion && opts.Compression == nil {
			in.Snapshot = nil
		}
		incremental := snapshotErr == nil && snapshot.PolicyVersion == opts.PolicyVersion && snapshot.ValidatedThrough == snapshot.ThroughSequence && snapshot.ThroughSequence > 0 && (!opts.ArchiveCompleted || snapshot.Archived)
		if incremental {
			in.History, in.HistoryThrough, err = db.ContextWindow(ctx, snapshot, session.Version, opts.ArchiveCompleted)
			if err != nil {
				return agent.Request{}, err
			}
			in.VerifiedPrefix = snapshot.ThroughSequence
		}
		for !incremental {
			page, err := db.ListMessages(ctx, run.Scope, run.SessionID, store.SequencePage{After: after, Limit: 128})
			if err != nil {
				return agent.Request{}, err
			}
			in.History = append(in.History, page.Items...)
			if !page.HasMore {
				break
			}
			after = page.NextAfter
		}
		ids := make(map[string]bool)
		for _, message := range in.History {
			if message.RunID == run.ID {
				continue
			}
			if opts.ArchiveCompleted {
				ids[message.RunID] = true
			}
			for _, part := range message.Parts {
				if part.Kind == domain.PartToolCall {
					ids[message.RunID] = true
				}
			}
		}
		var runIDs []string
		for id := range ids {
			runIDs = append(runIDs, id)
		}
		in.RunStates, err = db.SessionRunStates(ctx, run.Scope, run.SessionID, runIDs)
		if err != nil {
			return agent.Request{}, err
		}
		prepared, err := engine.Prepare(ctx, in)
		if err != nil {
			return agent.Request{}, err
		}
		// 当前 user 消息已经由 StartRun 持久化，不能另行追加 req.Input。
		// 准备阶段不能自行取得写权限；候选携带读取时的会话版本交给运行持有者。
		return agent.Request{Run: run, Caller: domain.AgentExecution{AgentID: session.AgentID, InvocationID: run.ID}, Messages: prepared.Messages,
			ContextSnapshot: prepared.NewSnapshot, ContextSessionVersion: session.Version}, nil
	}, nil
}
func copyParts(parts []domain.Part) []domain.Part {
	if parts == nil {
		return nil
	}
	out := make([]domain.Part, len(parts))
	copy(out, parts)
	for i := range out {
		if out[i].Data != nil {
			out[i].Data = append([]byte{}, out[i].Data...)
		}
	}
	return out
}
