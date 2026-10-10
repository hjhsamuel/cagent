package adk

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"

	"github.com/hjhsamuel/cagent/internal/agent"
	"github.com/hjhsamuel/cagent/internal/domain"
	"google.golang.org/genai"
)

// pending 只读取应用 LLM 历史、工具句柄及执行计数，不解释 ADK 事件或状态。
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
	if saved.Model != r.model.Name() || saved.Scope != req.Run.Scope || saved.RunID != req.Run.ID || saved.Caller != req.Caller || saved.ModelCalls < 1 {
		return saved, invalid("checkpoint.identity")
	}
	if cp.Format == legacyPendingCheckpointFormat {
		// v2 的实际 LLM 内容可直接迁移为领域消息；丢弃整个 SDK 字段。
		var old struct{ Contents []*genai.Content }
		if e := decodeJSON(cp.Data, &old); e != nil || len(old.Contents) == 0 {
			return saved, unsupported("checkpoint.history")
		}
		mapped, e := mapMessages(saved.Messages, r.model.Name())
		if e != nil {
			return saved, e
		}
		if len(old.Contents) < len(mapped.Contents) {
			return saved, invalid("checkpoint.history")
		}
		for _, c := range old.Contents[len(mapped.Contents):] {
			if c == nil {
				return saved, invalid("checkpoint.history")
			}
			role := domain.RoleAssistant
			if c.Role == "user" {
				role = domain.RoleTool
			}
			var parts []domain.Part
			for _, p := range c.Parts {
				if p == nil {
					return saved, invalid("checkpoint.history")
				}
				if f := p.FunctionCall; f != nil {
					data, e := json.Marshal(f.Args)
					if e != nil {
						return saved, invalid("checkpoint.history")
					}
					parts = append(parts, domain.Part{Kind: domain.PartToolCall, ToolCallID: f.ID, ToolName: f.Name, Data: data})
				} else if f := p.FunctionResponse; f != nil {
					if f.Response == nil {
						continue
					}
					data, e := json.Marshal(f.Response)
					if e != nil {
						return saved, invalid("checkpoint.history")
					}
					parts = append(parts, domain.Part{Kind: domain.PartToolResult, ToolCallID: f.ID, ToolName: f.Name, Data: data})
				} else if p.Text != "" && !p.Thought {
					parts = append(parts, domain.Part{Kind: domain.PartText, Text: p.Text})
				} else {
					return saved, unsupported("checkpoint.history")
				}
			}
			if len(parts) > 0 {
				saved.Messages = append(saved.Messages, interaction(req, role, parts))
			}
		}
	}
	calls, results := map[string]int{}, map[string]int{}
	for _, m := range saved.Messages {
		if m.Scope != req.Run.Scope || m.SessionID != req.Run.SessionID {
			return saved, invalid("checkpoint.messages")
		}
		if m.RunID == req.Run.ID {
			for _, p := range m.Parts {
				switch p.Kind {
				case domain.PartToolCall:
					calls[p.ToolCallID]++
					if calls[p.ToolCallID] != 1 {
						return saved, invalid("checkpoint.duplicate_call")
					}
				case domain.PartToolResult:
					results[p.ToolCallID]++
					if results[p.ToolCallID] != 1 {
						return saved, invalid("checkpoint.duplicate_result")
					}
				}
			}
		}
	}
	if _, e := mapMessagesForRun(saved.Messages, r.model.Name(), req.Run.ID); e != nil {
		return saved, e
	}
	seen := map[string]bool{}
	for _, p := range saved.Tools {
		if e := p.Call.ValidateForRun(req.Run); e != nil {
			return saved, e
		}
		if p.Call.Caller != req.Caller || seen[p.Call.ID] {
			return saved, invalid("checkpoint.tools")
		}
		if e := p.Handle.ValidateForCall(p.Call); e != nil {
			return saved, e
		}
		seen[p.Call.ID] = true
		found := false
		for _, m := range saved.Messages {
			if m.RunID != req.Run.ID {
				continue
			}
			for _, part := range m.Parts {
				if part.Kind == domain.PartToolCall && part.ToolCallID == p.Call.ID {
					args, e := object(part.Data)
					original, err := object(p.Call.Arguments)
					if e != nil || err != nil || part.ToolName != modelToolName(p.Call.Name) || !reflect.DeepEqual(args, original) {
						return saved, invalid("checkpoint.call")
					}
					found = true
				}
			}
		}
		if !found {
			return saved, invalid("checkpoint.call")
		}
	}
	for _, id := range cp.PendingCallIDs {
		if !seen[id] || hasResult(saved.Messages, req.Run.ID, id) {
			return saved, invalid("checkpoint.pending_call")
		}
	}
	if saved.Failure == "" {
		for id := range calls {
			if results[id] == 0 && !seen[id] {
				return saved, invalid("checkpoint.missing_tool")
			}
		}
	}
	return saved, nil
}

func interaction(req agent.Request, role domain.Role, parts []domain.Part) domain.Message {
	return domain.Message{Scope: req.Run.Scope, SessionID: req.Run.SessionID, RunID: req.Run.ID, Role: role, Parts: parts}
}

func hasResult(messages []domain.Message, run, id string) bool {
	for _, m := range messages {
		if m.RunID == run && m.Role == domain.RoleTool {
			for _, p := range m.Parts {
				if p.Kind == domain.PartToolResult && p.ToolCallID == id {
					return true
				}
			}
		}
	}
	return false
}

// PendingTasks 从整批工具执行记录补齐逐个 TrackTask 的崩溃窗口，不执行工具。
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
		if !hasResult(saved.Messages, req.Run.ID, p.Call.ID) {
			tasks = append(tasks, domain.Task{Scope: p.Call.Scope, Call: p.Call, Handle: p.Handle, Status: domain.TaskSubmitted})
		}
	}
	return tasks, nil
}

// Resume 仅接纳 TaskStore 的一个终态结果，消息、检查点及消费标记由 ApplyTask 原子提交。
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
	if !found || hasResult(saved.Messages, req.Run.ID, c.Call.ID) {
		return invalid("continuation.call")
	}
	result := c.Result
	if result == nil {
		result = &domain.ToolResult{CallID: c.Call.ID, Error: "remote task ended without a result: " + string(c.Status)}
	}
	if e = result.ValidateForCall(c.Call); e != nil {
		return e
	}
	data, e := json.Marshal(map[string]any{"status": c.Status, "parts": result.Parts, "error": result.Error})
	if e != nil {
		return safeError(e)
	}
	parts := []domain.Part{{Kind: domain.PartToolResult, ToolCallID: c.Call.ID, ToolName: modelToolName(c.Call.Name), Data: data}}
	saved.Messages = append(saved.Messages, interaction(req, domain.RoleTool, parts))
	cp := *req.Checkpoint
	cp.Format = PendingCheckpointFormat
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
	return emit(ctx, agent.Update{AppliedTaskID: c.TaskID, Kind: domain.EventToolFinished, Checkpoint: &cp, MessageRole: domain.RoleTool, Message: parts})
}
