package adk

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"

	"github.com/google/uuid"
	"github.com/hjhsamuel/cagent/internal/agent"
	"github.com/hjhsamuel/cagent/internal/domain"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

// pending 校验完整调用身份，不能仅凭 Agent 名称或 ToolCall ID 路由。
func (r *Runtime) pending(req agent.Request) (pendingCheckpoint, error) {
	var saved pendingCheckpoint
	cp := req.Checkpoint
	if cp == nil || !IsPendingCheckpoint(*cp) {
		return saved, unsupported("checkpoint.format")
	}
	if e := cp.ValidateForRun(req.Run); e != nil {
		return saved, e
	}
	if cp.Caller != req.Caller || cp.Version <= 0 {
		return saved, invalid("checkpoint.identity")
	}
	if e := decodeJSON(cp.Data, &saved); e != nil {
		return saved, invalid("checkpoint.data")
	}
	if saved.Model != r.model.Name() || saved.Scope != req.Run.Scope || saved.RunID != req.Run.ID || saved.Caller != req.Caller {
		return saved, invalid("checkpoint.identity")
	}
	// 兼容 P8 检查点：从初始上下文和 SDK 工具事件重建实际历史。
	if len(saved.Contents) == 0 {
		mapped, e := mapMessages(saved.Messages, r.model.Name(), int32(r.budget.OutputTokens))
		if e != nil {
			return saved, e
		}
		saved.Contents = mapped.Contents
		for _, ev := range saved.SDK.Events {
			if ev != nil && !ev.Partial && ev.Content != nil {
				for _, p := range ev.Content.Parts {
					if p.FunctionCall != nil || p.FunctionResponse != nil {
						saved.Contents = append(saved.Contents, ev.Content)
						break
					}
				}
			}
		}
	}
	return saved, nil
}

// PendingTasks 从整批持久句柄补齐逐个 TrackTask 的崩溃窗口。
// 返回原调用，不执行工具；调用方仍须通过 TrackTask 的唯一键去重。
func (r *Runtime) PendingTasks(req agent.Request) ([]domain.Task, error) {
	if req.Checkpoint == nil || !IsPendingCheckpoint(*req.Checkpoint) {
		return nil, nil
	}
	saved, e := r.pending(req)
	if e != nil {
		return nil, e
	}
	var tasks []domain.Task
	for _, p := range saved.Tools {
		if saved.Results[p.Call.ID] == nil {
			tasks = append(tasks, domain.Task{Scope: p.Call.Scope, Call: p.Call, Handle: p.Handle, Status: domain.TaskSubmitted})
		}
	}
	return tasks, nil
}

// Resume 只接纳一个终态结果，不调用模型。Emit 成功表示 ApplyTask 已将结果消息、
// 检查点与消费标记原子提交；之后 Recover 才继续。提交响应丢失时重启直接读取
// 接纳后的检查点，不重复消费，也不重新执行已启动工具。
func (r *Runtime) Resume(ctx context.Context, req agent.Request, c agent.Continuation, emit agent.Emit) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	saved, e := r.pending(req)
	if e != nil {
		return e
	}
	if emit == nil || c.TaskID == "" || !c.Status.IsTerminal() || c.Call.Caller != req.Caller || !slices.Contains(req.Checkpoint.PendingCallIDs, c.Call.ID) {
		return invalid("continuation.identity")
	}
	if e = c.Call.ValidateForRun(req.Run); e != nil {
		return e
	}
	found := false
	for _, p := range saved.Tools {
		if reflect.DeepEqual(p.Call, c.Call) {
			found = true
		}
	}
	if !found || saved.Results[c.Call.ID] != nil {
		return invalid("continuation.call")
	}
	result := c.Result
	if result == nil {
		result = &domain.ToolResult{CallID: c.Call.ID, Error: "remote task ended without a result: " + string(c.Status)}
	}
	if e = result.ValidateForCall(c.Call); e != nil {
		return e
	}
	response := &genai.FunctionResponse{ID: c.Call.ID, Name: modelToolName(c.Call.Name), Response: map[string]any{"status": c.Status, "parts": result.Parts, "error": result.Error}}
	if saved.Results == nil {
		saved.Results = map[string]*genai.FunctionResponse{}
	}
	saved.Results[c.Call.ID] = response
	// 去掉 deferred 空占位响应，按原工具顺序重建已确认结果。乱序到达不改变
	// 调用关联；尚未完成的调用没有伪造响应，也不会提前推进该分支。
	pending := map[string]bool{}
	for _, p := range saved.Tools {
		pending[p.Call.ID] = true
	}
	var contents []*genai.Content
	for _, content := range saved.Contents {
		copy := &genai.Content{Role: content.Role}
		for _, p := range content.Parts {
			if p.FunctionResponse == nil || !pending[p.FunctionResponse.ID] {
				copy.Parts = append(copy.Parts, p)
			}
		}
		if len(copy.Parts) > 0 {
			contents = append(contents, copy)
		}
	}
	for _, p := range saved.Tools {
		if result := saved.Results[p.Call.ID]; result != nil {
			contents = append(contents, &genai.Content{Role: "user", Parts: []*genai.Part{{FunctionResponse: result}}})
		}
	}
	saved.Contents = contents
	ev := &session.Event{ID: uuid.NewString(), Author: "user", LLMResponse: model.LLMResponse{Content: &genai.Content{Role: "user", Parts: []*genai.Part{{FunctionResponse: response}}}}}
	saved.SDK.Events = append(saved.SDK.Events, ev)
	cp := *req.Checkpoint
	cp.PendingCallIDs = nil
	for _, id := range req.Checkpoint.PendingCallIDs {
		if id != c.Call.ID {
			cp.PendingCallIDs = append(cp.PendingCallIDs, id)
		}
	}
	cp.Data, e = json.Marshal(saved)
	if e != nil {
		return safeError(e)
	}
	data, e := json.Marshal(response.Response)
	if e != nil {
		return safeError(e)
	}
	return emit(ctx, agent.Update{AppliedTaskID: c.TaskID, Kind: domain.EventToolFinished, Checkpoint: &cp, MessageRole: domain.RoleTool, Message: []domain.Part{{Kind: domain.PartToolResult, ToolCallID: c.Call.ID, ToolName: response.Name, Data: data}}})
}
