package adk

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"

	"github.com/hjhsamuel/cagent/internal/agent"
	"github.com/hjhsamuel/cagent/internal/domain"
	"github.com/hjhsamuel/cagent/internal/tool"
	sdkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	sdktool "google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/toolutils"
	"google.golang.org/genai"
)

// ToolOptions 是可信装配入口。Handoff 在应用恢复记录可用后同步调用，注入方负责
// 原子 TrackTask。默认由 Emit 保存句柄及检查点，Run 等待后由服务持续观察。
// MaxModelCalls 限制工具循环，不限制远端任务寿命。
type ToolOptions struct {
	Registry      tool.Registry
	MaxModelCalls int
	Handoff       func(context.Context, PendingTools, *domain.Checkpoint) error
}
type PendingTool struct {
	Call   domain.ToolCall
	Handle domain.TaskHandle
}
type PendingTools []PendingTool

const PendingCheckpointFormat = "cagent.tools/v1"
const legacyPendingCheckpointFormat = "cagent.adk.tools.v2"

func IsPendingCheckpoint(cp domain.Checkpoint) bool {
	return cp.Format == PendingCheckpointFormat || cp.Format == legacyPendingCheckpointFormat
}

type pendingCheckpoint struct {
	// Messages 保存实际 LLM 交互，包括调用及已原子接纳的结果；不保存 SDK 状态。
	ModelCalls int
	// Model 与准备后消息保留暂停时的模型/上下文，不依赖恢复时已经变化的配置或摘要。
	Model    string
	Messages []domain.Message
	Scope    domain.Scope
	RunID    string
	Caller   domain.AgentExecution
	Tools    PendingTools
	// Failure is a safe classification, never a provider diagnostic or credential.
	Failure string `json:",omitempty"`
}
type toolRun struct {
	mu      sync.Mutex
	pending PendingTools
	seen    map[string]bool
	failure error
}
type bridge struct {
	desc     tool.Descriptor
	registry tool.Registry
	request  agent.Request
	state    *toolRun
}

func (b *bridge) Name() string { return modelToolName(b.desc.Name) }

// MCP 名称可含点，A2A Card 名称可含空格；OpenAI 函数名限制更严。
// 稳定摘要别名仅用于模型线格式，实际执行仍使用原始描述名，不改变远端工具身份。
func modelToolName(name string) string {
	valid := len(name) > 0 && len(name) <= 64
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			valid = false
		}
	}
	if valid {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	return "tool_" + hex.EncodeToString(sum[:16])
}
func (b *bridge) Description() string { return b.desc.Description }

// SDK 的静态标志不用于猜测远端是否异步；真正的 TaskOutcome 到达后才停止调用链。
func (b *bridge) IsLongRunning() bool  { return false }
func (b *bridge) DefersResponse() bool { return true }
func (b *bridge) Declaration() *genai.FunctionDeclaration {
	var schema any
	_ = decodeJSON(b.desc.InputSchema, &schema)
	return &genai.FunctionDeclaration{Name: b.Name(), Description: b.Description(), ParametersJsonSchema: schema}
}
func (b *bridge) ProcessRequest(ctx sdkagent.Context, r *model.LLMRequest) error {
	return toolutils.PackTool(r, b)
}
func (b *bridge) Run(ctx sdkagent.Context, args any) (result map[string]any, err error) {
	defer func() {
		if err != nil {
			b.state.mu.Lock()
			b.state.failure = err
			b.state.mu.Unlock()
		}
	}()
	data, e := json.Marshal(args)
	if e != nil {
		return nil, invalid("tool.arguments")
	}
	call := domain.ToolCall{Scope: b.request.Run.Scope, ID: ctx.FunctionCallID(), SessionID: b.request.Run.SessionID, RunID: b.request.Run.ID, Caller: b.request.Caller, Protocol: b.desc.Protocol, Name: b.desc.Name, Arguments: data}
	if e = call.Validate(); e != nil {
		return nil, e
	}
	// 同 Run 的模型调用 ID 必须唯一；拒绝模型重复 ID，防止重复外部副作用和历史错配。
	b.state.mu.Lock()
	if b.state.seen[call.ID] {
		b.state.mu.Unlock()
		return nil, invalid("tool.duplicate_call")
	}
	b.state.seen[call.ID] = true
	b.state.mu.Unlock()
	executor, e := b.registry.Resolve(ctx, call.Scope, call.Protocol, call.Name)
	if e != nil {
		return nil, e
	}
	out, e := executor.Execute(ctx, call)
	if e != nil {
		// Execute 报错不能证明远端未启动。没有可查询的句柄时保留不确定语义，
		// 让应用持久化原因；不能因超时或断线再次调用同一工具。
		return nil, errors.Join(agent.ErrUncertain, e)
	}
	if e = out.ValidateForCall(call); e != nil {
		return nil, errors.Join(agent.ErrUncertain, e)
	}
	if out.Task != nil {
		b.state.mu.Lock()
		b.state.pending = append(b.state.pending, PendingTool{Call: call, Handle: *out.Task})
		b.state.mu.Unlock()
		ctx.Actions().SkipSummarization = true
		return nil, nil
	}
	// 结果中的 Error 是工具业务结果，返回模型；传输/授权错误则中止运行。
	return map[string]any{"parts": out.Result.Parts, "error": out.Result.Error}, nil
}
func (r *Runtime) tools(ctx context.Context, req agent.Request, state *toolRun) ([]sdktool.Tool, error) {
	if r.toolOptions.Registry == nil {
		return nil, nil
	}
	descs, e := r.toolOptions.Registry.List(ctx, req.Run.Scope)
	if e != nil {
		return nil, e
	}
	out := make([]sdktool.Tool, 0, len(descs))
	names := map[string]bool{}
	for _, d := range descs {
		name := modelToolName(d.Name)
		if names[name] {
			return nil, invalid("tool.model_name")
		}
		names[name] = true
		out = append(out, &bridge{desc: d, registry: r.toolOptions.Registry, request: req, state: state})
	}
	return out, nil
}

// toolEvent 持久化完整调用/结果对。任务句柄不生成最终工具结果；未完成调用留在历史。
func toolEvent(ctx context.Context, emit agent.Emit, content *genai.Content, state *toolRun, promptTokens int32) (bool, error) {
	var calls, results []domain.Part
	hasCall := false
	for _, p := range content.Parts {
		if p == nil {
			continue
		}
		if f := p.FunctionCall; f != nil {
			hasCall = true
			data, e := json.Marshal(f.Args)
			if e != nil {
				return true, e
			}
			calls = append(calls, domain.Part{Kind: domain.PartToolCall, ToolCallID: f.ID, ToolName: f.Name, Data: data})
		}
		if p.Text != "" && !p.Thought && p.FunctionCall == nil && p.FunctionResponse == nil {
			calls = append(calls, domain.Part{Kind: domain.PartText, Text: p.Text})
		}
		if f := p.FunctionResponse; f != nil {
			pending := false
			state.mu.Lock()
			for _, v := range state.pending {
				if v.Call.ID == f.ID {
					pending = true
				}
			}
			state.mu.Unlock()
			if pending {
				continue
			}
			data, e := json.Marshal(f.Response)
			if e != nil {
				return true, e
			}
			results = append(results, domain.Part{Kind: domain.PartToolResult, ToolCallID: f.ID, ToolName: f.Name, Data: data})
		}
	}
	if hasCall {
		if e := emit(ctx, agent.Update{Kind: domain.EventToolStarted, Message: calls, PromptTokens: promptTokens}); e != nil {
			return true, e
		}
	}
	if len(results) > 0 {
		if e := emit(ctx, agent.Update{Kind: domain.EventToolFinished, Message: results, MessageRole: domain.RoleTool}); e != nil {
			return true, e
		}
	}
	for _, p := range content.Parts {
		if p != nil && (p.FunctionCall != nil || p.FunctionResponse != nil) {
			return true, nil
		}
	}
	return false, nil
}
