package adk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/hjhsamuel/cagent/internal/agent"
	"github.com/hjhsamuel/cagent/internal/domain"
	sdkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

// Runtime 只共享无会话状态的模型。每次 Execute 创建独立 Agent/Runner/
// SDK Session，持久真相仍在 MongoDB，内存会话只是一次运行的 SDK 执行载体。
type Runtime struct {
	model       model.LLM
	toolOptions ToolOptions
}

// New 固定模型和工具选项；不建立网络连接，也不共享各 Run 的 SDK 会话。
func New(llm model.LLM, options ...ToolOptions) (*Runtime, error) {
	if llm == nil {
		return nil, invalid("runtime.dependencies")
	}
	var opt ToolOptions
	if len(options) > 1 {
		return nil, invalid("runtime.tools")
	}
	if len(options) == 1 {
		opt = options[0]
		if opt.Registry == nil || opt.MaxModelCalls < 1 {
			return nil, invalid("runtime.tools")
		}
	}
	return &Runtime{model: llm, toolOptions: opt}, nil
}

// OutputData 是事件 Data 的稳定 JSON 载荷；明确区分原应用调用链与 SDK InvocationID。
// 最终消息使用 message.completed，不把完整文本再次当 message.delta 拼接。
type OutputData struct {
	AgentID            string `json:"agent_id"`
	InvocationID       string `json:"invocation_id"`
	ParentInvocationID string `json:"parent_invocation_id,omitempty"`
	SDKInvocationID    string `json:"sdk_invocation_id"`
	Text               string `json:"text"`
	PromptTokens       int32  `json:"prompt_tokens,omitempty"`
	OutputTokens       int32  `json:"output_tokens,omitempty"`
}

// Execute 经真实 ADK Runner/LLMAgent 调用模型，禁用 SDK 自动历史拼装与压缩。
// BeforeModel 精确注入已经组装的上下文，保留系统指令及结构化工具历史。
// 未配置工具时只允许一次文本生成；配置后按 MaxModelCalls 有界循环，
// 即时调用/结果分别持久化，真实任务句柄经暂停检查点交接，结果由 Resume 接纳。
func (r *Runtime) Execute(ctx context.Context, req agent.Request, emit agent.Emit) error {
	if req.Checkpoint != nil {
		return unsupported("runtime.execute_checkpoint")
	}
	return r.run(ctx, req, emit)
}

// run 从应用消息重建模型上下文，每次使用全新 Runner，恢复不调用旧工具。
func (r *Runtime) run(ctx context.Context, req agent.Request, emit agent.Emit) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if emit == nil {
		return invalid("runtime.emit")
	}
	if err := req.Run.Validate(); err != nil {
		return err
	}
	if err := req.Caller.Validate(); err != nil {
		return err
	}
	var saved *pendingCheckpoint
	if req.Checkpoint != nil {
		v, err := r.pending(req)
		if err != nil {
			return err
		}
		saved = &v
		if failure := checkpointFailure(v.Failure); failure != nil {
			return failure
		}
		if len(req.Checkpoint.PendingCallIDs) > 0 {
			return agent.ErrWaiting
		}
		// 部分交接时 PendingCallIDs 尚未补齐，以整批执行记录为准。
		for _, p := range v.Tools {
			if !hasResult(v.Messages, req.Run.ID, p.Call.ID) {
				return agent.ErrWaiting
			}
		}
		req.Messages = v.Messages
	}
	// 后续追加交互不复用调用方切片的剩余容量，避免并发请求共享底层数组。
	req.Messages = append([]domain.Message{}, req.Messages...)
	for _, m := range req.Messages {
		if m.Scope != req.Run.Scope || m.SessionID != req.Run.SessionID {
			return invalid("runtime.messages")
		}
	}
	mapped, e := mapMessagesForRun(req.Messages, r.model.Name(), req.Run.ID)
	if e != nil {
		return e
	}
	// 初次执行必须以本 Run 的 user 输入结束；工具结果续接不能走首次 Execute。
	last := req.Messages[len(req.Messages)-1]
	if saved == nil && (last.Role != domain.RoleUser || last.RunID != req.Run.ID) {
		return invalid("runtime.current_input")
	}
	svc := session.InMemoryService()
	_, e = svc.Create(ctx, &session.CreateRequest{AppName: "cagent", UserID: req.Run.Scope.UserID, SessionID: req.Run.ID})
	if e != nil {
		return safeError(e)
	}
	calls := 0
	if saved != nil {
		calls = saved.ModelCalls
	}
	state := &toolRun{seen: map[string]bool{}}
	if saved != nil {
		for _, m := range saved.Messages {
			for _, p := range m.Parts {
				if m.RunID == req.Run.ID && p.Kind == domain.PartToolCall {
					state.seen[p.ToolCallID] = true
				}
			}
		}
	}
	// 只有持久化成功的完整交互才进入恢复历史，不记录流式增量或 deferred 占位。
	forward := emit
	emit = func(c context.Context, u agent.Update) error {
		if err := forward(c, u); err != nil {
			return err
		}
		if u.Message != nil {
			role := u.MessageRole
			if role == "" {
				role = domain.RoleAssistant
			}
			req.Messages = append(req.Messages, interaction(req, role, u.Message))
		}
		return nil
	}
	registered, e := r.tools(ctx, req, state)
	if e != nil {
		return e
	}
	a, e := llmagent.New(llmagent.Config{Name: "root", Model: r.model, IncludeContents: llmagent.IncludeContentsNone, Tools: registered,
		BeforeModelCallbacks: []llmagent.BeforeModelCallback{func(c sdkagent.Context, out *model.LLMRequest) (*model.LLMResponse, error) {
			// ADK 的 tool.Context 不支持 EndInvocation；在下一次模型边界截停，
			// 而不是依赖无效的工具 context 调用或把句柄当作模型结果。
			state.mu.Lock()
			pending, failure := len(state.pending) > 0, state.failure
			state.mu.Unlock()
			if pending {
				return nil, agent.ErrWaiting
			}
			if failure != nil {
				return nil, failure
			}
			calls++
			maxCalls := 1
			if r.toolOptions.Registry != nil {
				maxCalls = r.toolOptions.MaxModelCalls
			}
			if calls > maxCalls {
				return nil, unsupported("runtime.multiple_model_calls")
			}
			// 用 JSON 深拷贝让 SDK/模型无法修改原映射，各次调用保持输入隔离。
			// 保留 SDK 打包的可执行工具和声明；初始上下文与本轮工具历史由本适配器维护。
			declarations := out.Config.Tools
			mapped, err := mapMessagesForRun(req.Messages, r.model.Name(), req.Run.ID)
			if err != nil {
				return nil, err
			}
			data, err := json.Marshal(mapped)
			if err != nil {
				return nil, safeError(err)
			}
			// 工具参数允许超过 float64 精确整数范围，深拷贝必须保持 JSON 数字原文。
			decoder := json.NewDecoder(bytes.NewReader(data))
			decoder.UseNumber()
			// JSON 解码会复用切片内对象；先清零避免旧 FunctionResponse 等被
			// omitempty 省略的字段残留，导致恢复时一个 Part 同时携带调用和结果。
			*out = model.LLMRequest{Tools: out.Tools}
			if err = decoder.Decode(out); err != nil {
				return nil, safeError(err)
			}
			out.Config.Tools = declarations
			return nil, nil
		}}})
	if e != nil {
		return safeError(e)
	}
	runner, e := runner.New(runner.Config{AppName: "cagent", Agent: a, SessionService: svc})
	if e != nil {
		return safeError(e)
	}
	var final *session.Event
	input := mapped.Contents[len(mapped.Contents)-1]
	if saved != nil {
		// 仅启动全新 SDK 工作流；真正的输入由 BeforeModel 从应用历史注入。
		// 不把旧 FunctionCall/Response 交给 SDK 的工作流恢复逻辑。
		input = genai.NewContentFromText("continue", "user")
	}
	for ev, err := range runner.Run(ctx, req.Run.Scope.UserID, req.Run.ID, input, sdkagent.RunConfig{StreamingMode: sdkagent.StreamingModeSSE}) {
		if err != nil {
			if errors.Is(err, agent.ErrWaiting) {
				break
			}
			return safeError(err)
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		if ev == nil {
			continue
		}
		if ev.ErrorCode != "" || ev.Interrupted || len(ev.LongRunningToolIDs) > 0 || ev.RequestedInput != nil {
			return safeError(ErrModel)
		}
		if ev.Content == nil {
			continue
		}
		if !ev.Partial && r.toolOptions.Registry != nil {
			isTool, err := toolEvent(ctx, emit, ev.Content, state, outputData(req, ev, "").PromptTokens)
			if err != nil {
				return err
			}
			if isTool {
				continue
			}
		}
		var text strings.Builder
		for _, p := range ev.Content.Parts {
			if p == nil || p.FunctionCall != nil || p.FunctionResponse != nil || p.InlineData != nil || p.FileData != nil || p.Thought || len(p.ThoughtSignature) > 0 {
				return unsupported("runtime.output")
			}
			text.WriteString(p.Text)
		}
		if ev.Partial {
			if text.Len() == 0 {
				continue
			}
			data, _ := json.Marshal(outputData(req, ev, text.String()))
			if err = emit(ctx, agent.Update{Kind: domain.EventTextDelta, Data: data}); err != nil {
				return err
			}
			continue
		}
		if ev.FinishReason != genai.FinishReasonStop || text.Len() == 0 || final != nil {
			return safeError(ErrModel)
		}
		// 保存独立副本，SDK 在后续工作流控制事件处理中可以继续更新内部状态。
		data, err := json.Marshal(ev)
		if err != nil {
			return safeError(err)
		}
		if err = decodeJSON(data, &final); err != nil {
			return safeError(err)
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(state.pending) > 0 {
		failure := ""
		if state.failure != nil {
			failure = "tool_execution_failed"
			if errors.Is(state.failure, agent.ErrUncertain) {
				failure = "execution_outcome_uncertain"
			}
		}
		data, e := json.Marshal(pendingCheckpoint{ModelCalls: calls, Model: r.model.Name(), Messages: req.Messages, Scope: req.Run.Scope, RunID: req.Run.ID, Caller: req.Caller, Tools: state.pending, Failure: failure})
		if e != nil {
			return safeError(e)
		}
		cp := &domain.Checkpoint{Scope: req.Run.Scope, RunID: req.Run.ID, Caller: req.Caller, Format: PendingCheckpointFormat, Data: data}
		if req.Checkpoint != nil {
			cp.Version = req.Checkpoint.Version
		}
		for _, p := range state.pending {
			cp.PendingCallIDs = append(cp.PendingCallIDs, p.Call.ID)
		}
		if r.toolOptions.Handoff != nil {
			if e = r.toolOptions.Handoff(ctx, state.pending, cp); e != nil {
				return e
			}
		} else {
			var tasks []domain.Task
			for _, p := range state.pending {
				tasks = append(tasks, domain.Task{Scope: p.Call.Scope, Call: p.Call, Handle: p.Handle, Status: domain.TaskSubmitted})
			}
			if e = emit(ctx, agent.Update{Kind: domain.EventToolWaiting, Checkpoint: cp, Tasks: tasks}); e != nil {
				return e
			}
		}
		// Handoff must finish before returning a failure: other calls already started.
		if state.failure != nil {
			return state.failure
		}
		return agent.ErrWaiting
	}
	if state.failure != nil {
		return state.failure
	}
	if final == nil {
		return safeError(ErrModel)
	}
	parts := make([]domain.Part, 0, len(final.Content.Parts))
	var full strings.Builder
	for _, p := range final.Content.Parts {
		parts = append(parts, domain.Part{Kind: domain.PartText, Text: p.Text})
		full.WriteString(p.Text)
	}
	cpData, e := json.Marshal(completedCheckpoint{Model: r.model.Name(), Scope: req.Run.Scope, RunID: req.Run.ID, Caller: req.Caller, Final: parts})
	if e != nil {
		return safeError(e)
	}
	data, _ := json.Marshal(outputData(req, final, full.String()))
	cp := &domain.Checkpoint{Scope: req.Run.Scope, RunID: req.Run.ID, Caller: req.Caller, Format: CheckpointFormat, Data: cpData}
	if req.Checkpoint != nil {
		cp.Version = req.Checkpoint.Version
	}
	return emit(ctx, agent.Update{Kind: domain.EventMessageCompleted, Data: data, Message: parts, Checkpoint: cp, PromptTokens: outputData(req, final, "").PromptTokens})
}
func outputData(req agent.Request, ev *session.Event, text string) OutputData {
	v := OutputData{AgentID: req.Caller.AgentID, InvocationID: req.Caller.InvocationID, ParentInvocationID: req.Caller.ParentInvocationID, SDKInvocationID: ev.InvocationID, Text: text}
	if ev.UsageMetadata != nil {
		v.PromptTokens = ev.UsageMetadata.PromptTokenCount
		v.OutputTokens = ev.UsageMetadata.CandidatesTokenCount
	}
	return v
}

var _ agent.Runtime = (*Runtime)(nil)
