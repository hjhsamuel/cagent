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

// NewOpenAIService 将动态模型配置、ADK、P5 预算与 P4 生命周期连接起来，不启动 HTTP。
// 模型参数为本实例的配置快照；更换配置需创建新实例，不能并发修改运行中配置。
// lifecycle 只提供租约/轮询参数，禁止覆盖已装配的预算与恢复回调。
func NewOpenAIService(parent context.Context, db *mongodb.Database, cfg config.Config, system []domain.Part, lifecycle Options, tools ...adk.ToolOptions) (*Application, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if lifecycle.Registry == nil && len(tools) == 1 {
		lifecycle.Registry = tools[0].Registry
	}
	if lifecycle.Tasks == (config.Tasks{}) {
		lifecycle.Tasks = cfg.Tasks
	}
	lifecycle.Capacity = cfg.Capacity
	lifecycle.Maintenance = cfg.Maintenance
	gate := observability.NewGate(cfg.Capacity.Models, "model")
	if cfg.Models != nil {
		return newSessionModelService(parent, db, cfg, system, lifecycle, gate, tools...)
	}
	runtime, opts, err := buildOpenAIRuntime(cfg, system, gate, tools...)
	if err != nil {
		return nil, err
	}
	return NewADKService(parent, db, runtime, opts, lifecycle)
}

// NewADKService 也允许测试/后续提供方注入真实 ADK Runtime。先检查已持久检查点，
// 再准备新模型上下文：已提交最终消息的恢复不必重新计数已经变长的历史，也不重复生成。
// 没有可恢复检查点的中断不调用 Execute，Recover 会明确拒绝。
func NewADKService(parent context.Context, db *mongodb.Database, runtime *adk.Runtime, opts ContextOptions, lifecycle Options) (*Application, error) {
	if runtime == nil || lifecycle.Prepare != nil || lifecycle.Recover != nil {
		return nil, invalid("adk.service")
	}
	// 禁用压缩仅停止生成新摘要；旧摘要可能没有完整用户要求，仍须保留用户原文。
	opts.PreserveUsers = !opts.ArchiveCompleted
	var engine contextengine.Engine
	var err error
	if opts.Compression != nil {
		engine, err = contextengine.NewCompressing(runtime, opts.Summarizer, *opts.Compression)
	} else {
		engine, err = contextengine.New(runtime)
	}
	if err != nil {
		return nil, err
	}
	prepare, err := NewContextPreparer(db, engine, opts)
	if err != nil {
		return nil, err
	}
	lifecycle.Prepare = func(ctx context.Context, run domain.Run) (agent.Request, error) {
		s, e := db.GetSession(ctx, run.Scope, run.SessionID)
		if e != nil {
			return agent.Request{}, e
		}
		caller := domain.AgentExecution{AgentID: s.AgentID, InvocationID: run.ID}
		cp, e := db.GetCheckpoint(ctx, run.Scope, run.ID, caller.InvocationID)
		if e == nil {
			return agent.Request{Run: run, Caller: caller, Checkpoint: &cp}, nil
		}
		if !errors.Is(e, apperrors.ErrNotFound) {
			return agent.Request{}, e
		}
		return prepare(ctx, run)
	}
	lifecycle.Recover = runtime.Recover
	return NewService(parent, db, runtime, lifecycle)
}

func buildOpenAIRuntime(cfg config.Config, system []domain.Part, gate *observability.Gate, tools ...adk.ToolOptions) (*adk.Runtime, ContextOptions, error) {
	llm, err := adk.NewOpenAI(cfg.Agent, nil)
	if err != nil {
		return nil, ContextOptions{}, err
	}
	llm.SetCapacity(gate)
	budget := contextengine.Budget{WindowTokens: cfg.Context.WindowTokens, OutputTokens: cfg.Context.OutputTokens, ToolTokens: cfg.Context.ToolTokens, SafetyTokens: cfg.Context.SafetyTokens}
	runtime, err := adk.New(llm, llm, budget, tools...)
	if err != nil {
		return nil, ContextOptions{}, err
	}
	opts := ContextOptions{System: system, Budget: budget, PolicyVersion: cfg.Context.PolicyVersion}
	opts.ArchiveCompleted = cfg.Context.ArchiveCompleted && cfg.Context.CompressionThresholdPercent > 0
	if opts.ArchiveCompleted {
		opts.PolicyVersion += "/archive-v1"
	}
	if cfg.Context.CompressionThresholdPercent > 0 {
		summaryCfg := cfg.Agent
		if cfg.SummaryAgent != nil {
			summaryCfg = *cfg.SummaryAgent
		}
		if cfg.Context.SummaryModel != "" {
			summaryCfg.Model = cfg.Context.SummaryModel
		}
		if cfg.Context.SummaryTokenEncoding != "" {
			summaryCfg.TokenEncoding = cfg.Context.SummaryTokenEncoding
		}
		summaryLLM, e := adk.NewOpenAI(summaryCfg, nil)
		if e != nil {
			return nil, ContextOptions{}, e
		}
		summaryLLM.SetCapacity(gate)
		opts.Summarizer, e = adk.NewSummaryModel(summaryLLM, summaryLLM, contextengine.Budget{WindowTokens: cfg.Context.SummaryWindowTokens, OutputTokens: cfg.Context.SummaryOutputTokens, SafetyTokens: cfg.Context.SafetyTokens})
		if e != nil {
			return nil, ContextOptions{}, e
		}
		opts.Compression = &contextengine.CompressionPolicy{ThresholdPercent: cfg.Context.CompressionThresholdPercent, KeepRecentRounds: cfg.Context.KeepRecentRounds}
	}
	return runtime, opts, nil
}
