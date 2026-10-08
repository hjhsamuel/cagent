package app

import (
	"context"
	"errors"

	"github.com/hjhsamuel/cagent/internal/adapter/adk"
	"github.com/hjhsamuel/cagent/internal/adapter/mongodb"
	"github.com/hjhsamuel/cagent/internal/agent"
	"github.com/hjhsamuel/cagent/internal/apperrors"
	"github.com/hjhsamuel/cagent/internal/config"
	"github.com/hjhsamuel/cagent/internal/contextengine"
	"github.com/hjhsamuel/cagent/internal/domain"
	"github.com/hjhsamuel/cagent/internal/observability"
)

// sessionModels 每次运行解析会话/分支的持久绑定，不维护默认模型或共享的当前模型。
type sessionModels struct {
	parent context.Context
	db     *mongodb.Database
	cfg    config.Config
	system []domain.Part
	gate   *observability.Gate
	tools  []adk.ToolOptions
}

func newSessionModelService(parent context.Context, db *mongodb.Database, cfg config.Config, system []domain.Part, lifecycle Options, gate *observability.Gate, tools ...adk.ToolOptions) (*Application, error) {
	if lifecycle.Prepare != nil || lifecycle.Recover != nil {
		return nil, invalid("adk.service")
	}
	r := &sessionModels{parent: parent, db: db, cfg: cfg, system: copyParts(system), gate: gate, tools: append([]adk.ToolOptions(nil), tools...)}
	lifecycle.SelectModel = cfg.Models.Select
	lifecycle.Prepare = r.prepare
	lifecycle.Recover = r.Recover
	return NewService(parent, db, r, lifecycle)
}

func (r *sessionModels) boundSession(ctx context.Context, run domain.Run) (domain.Session, error) {
	s, err := r.db.GetSession(ctx, run.Scope, run.SessionID)
	if err != nil {
		return s, err
	}
	if s.ModelID == "" && s.APIKeyID == "" {
		// 旧会话在首次使用时随机绑定一次；数据库事务保证多实例一致。
		selected, err := r.cfg.Models.Select("")
		if err != nil {
			return domain.Session{}, err
		}
		return r.db.BindSessionModel(ctx, s.Scope, s.ID, selected.ModelID, selected.APIKeyID)
	}
	return s, nil
}

func (r *sessionModels) selection(ctx context.Context, req agent.Request) (config.SelectedModel, error) {
	if req.Checkpoint != nil {
		if err := req.Checkpoint.ValidateForRun(req.Run); err != nil {
			return config.SelectedModel{}, err
		}
		if req.Checkpoint.Caller != req.Caller {
			return config.SelectedModel{}, invalid("checkpoint.caller")
		}
	}
	if req.Caller.ParentInvocationID != "" || req.Caller.InvocationID != req.Run.ID {
		if req.Checkpoint != nil {
			if req.Checkpoint.ModelID == "" || req.Checkpoint.APIKeyID == "" {
				return config.SelectedModel{}, agent.ErrUncertain
			}
			return r.cfg.Models.Bind(req.Checkpoint.ModelID, req.Checkpoint.APIKeyID)
		}
		// 子分支不会读取主会话绑定。
		return r.cfg.Models.Select("")
	}
	s, err := r.boundSession(ctx, req.Run)
	if err != nil {
		return config.SelectedModel{}, err
	}
	if req.Checkpoint != nil && req.Checkpoint.ModelID != "" && (req.Checkpoint.ModelID != s.ModelID || req.Checkpoint.APIKeyID != s.APIKeyID) {
		return config.SelectedModel{}, invalid("checkpoint.model_binding")
	}
	return r.cfg.Models.Bind(s.ModelID, s.APIKeyID)
}

func (r *sessionModels) build(selected config.SelectedModel, summary bool) (*adk.Runtime, ContextOptions, error) {
	cfg := r.cfg
	cfg.Agent = selected.Agent
	cfg.Context.WindowTokens, cfg.Context.OutputTokens = selected.Options.WindowTokens, selected.Options.OutputTokens
	cfg.Context.SummaryModel, cfg.Context.SummaryTokenEncoding = "", ""
	cfg.SummaryAgent = nil
	if summary && cfg.Context.CompressionThresholdPercent > 0 {
		// 摘要是独立内部调用，不作为会话主对话绑定。
		chosen, err := cfg.Models.Select("")
		if err != nil {
			return nil, ContextOptions{}, err
		}
		cfg.SummaryAgent = &chosen.Agent
		cfg.Context.SummaryWindowTokens = chosen.Options.WindowTokens
		cfg.Context.SummaryOutputTokens = min(cfg.Context.SummaryOutputTokens, chosen.Options.OutputTokens)
	} else {
		cfg.Context.CompressionThresholdPercent = 0
	}
	if err := cfg.Validate(); err != nil {
		return nil, ContextOptions{}, err
	}
	return buildOpenAIRuntime(cfg, r.system, r.gate, r.tools...)
}

func (r *sessionModels) prepare(ctx context.Context, run domain.Run) (agent.Request, error) {
	s, err := r.boundSession(ctx, run)
	if err != nil {
		return agent.Request{}, err
	}
	caller := domain.AgentExecution{AgentID: s.AgentID, InvocationID: run.ID}
	cp, err := r.db.GetCheckpoint(ctx, run.Scope, run.ID, caller.InvocationID)
	if err == nil {
		return agent.Request{Run: run, Caller: caller, Checkpoint: &cp}, nil
	}
	if !errors.Is(err, apperrors.ErrNotFound) {
		return agent.Request{}, err
	}
	selected, err := r.cfg.Models.Bind(s.ModelID, s.APIKeyID)
	if err != nil {
		return agent.Request{}, err
	}
	runtime, opts, err := r.build(selected, true)
	if err != nil {
		return agent.Request{}, err
	}
	opts.PreserveUsers = true
	var engine contextengine.Engine
	if opts.Compression != nil {
		engine, err = contextengine.NewCompressing(runtime, opts.Summarizer, *opts.Compression)
	} else {
		engine, err = contextengine.New(runtime)
	}
	if err != nil {
		return agent.Request{}, err
	}
	prepare, err := NewContextPreparer(r.db, engine, opts)
	if err != nil {
		return agent.Request{}, err
	}
	return prepare(ctx, run)
}

func selectedEmit(selected config.SelectedModel, emit agent.Emit) agent.Emit {
	return func(ctx context.Context, update agent.Update) error {
		if update.Checkpoint != nil {
			cp := *update.Checkpoint
			cp.ModelID, cp.APIKeyID = selected.ModelID, selected.APIKeyID
			update.Checkpoint = &cp
		}
		return emit(ctx, update)
	}
}

func (r *sessionModels) execute(ctx context.Context, req agent.Request, emit agent.Emit, action string, continuation agent.Continuation) error {
	if emit == nil {
		return invalid("runtime.emit")
	}
	if err := req.Caller.Validate(); err != nil {
		return err
	}
	if err := req.Run.Validate(); err != nil {
		return err
	}
	selected, err := r.selection(ctx, req)
	if err != nil {
		return err
	}
	runtime, _, err := r.build(selected, false)
	if err != nil {
		return err
	}
	emit = selectedEmit(selected, emit)
	switch action {
	case "resume":
		return runtime.Resume(ctx, req, continuation, emit)
	case "recover":
		return runtime.Recover(ctx, req, emit)
	default:
		return runtime.Execute(ctx, req, emit)
	}
}

func (r *sessionModels) Execute(ctx context.Context, req agent.Request, emit agent.Emit) error {
	return r.execute(ctx, req, emit, "execute", agent.Continuation{})
}
func (r *sessionModels) Recover(ctx context.Context, req agent.Request, emit agent.Emit) error {
	return r.execute(ctx, req, emit, "recover", agent.Continuation{})
}
func (r *sessionModels) Resume(ctx context.Context, req agent.Request, c agent.Continuation, emit agent.Emit) error {
	return r.execute(ctx, req, emit, "resume", c)
}
func (r *sessionModels) CheckpointComplete(cp domain.Checkpoint) bool {
	return cp.Format == adk.CheckpointFormat && len(cp.PendingCallIDs) == 0
}
func (r *sessionModels) PendingTasks(req agent.Request) ([]domain.Task, error) {
	if req.Checkpoint == nil || req.Checkpoint.Format != adk.PendingCheckpointFormat {
		return nil, nil
	}
	selected, err := r.selection(r.parent, req)
	if err != nil {
		return nil, err
	}
	runtime, _, err := r.build(selected, false)
	if err != nil {
		return nil, err
	}
	return runtime.PendingTasks(req)
}
