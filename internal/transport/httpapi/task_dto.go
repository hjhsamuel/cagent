package httpapi

import (
	"github.com/hjhsamuel/cagent/internal/domain"
	"time"
)

// taskView 使用显式白名单；领域结构将来新增私有字段不会自动进入客户端响应。
// Data 按已有事件约定使用 base64 保留任意字节，URI 只是工具结果引用，不携带连接凭据。
type partView struct {
	Kind     string `json:"kind"`
	Text     string `json:"text,omitempty"`
	MIMEType string `json:"mime_type,omitempty"`
	URI      string `json:"uri,omitempty"`
	Data     []byte `json:"data,omitempty"`
}
type resultView struct {
	CallID string     `json:"tool_call_id"`
	Parts  []partView `json:"parts"`
	Error  string     `json:"error,omitempty"`
}
type taskView struct {
	ID                 string            `json:"id"`
	RunID              string            `json:"run_id"`
	ToolCallID         string            `json:"tool_call_id"`
	AgentID            string            `json:"agent_id"`
	InvocationID       string            `json:"invocation_id"`
	ParentInvocationID string            `json:"parent_invocation_id,omitempty"`
	Status             domain.TaskStatus `json:"status"`
	Progress           []partView        `json:"progress"`
	Result             *resultView       `json:"result,omitempty"`
	CancelRequestedAt  *time.Time        `json:"cancel_requested_at,omitempty"`
	ObservationError   string            `json:"observation_error,omitempty"`
	AppliedAt          *time.Time        `json:"applied_at,omitempty"`
}

func partsDTO(parts []domain.Part) []partView {
	result := make([]partView, 0, len(parts))
	for _, p := range parts {
		result = append(result, partView{Kind: p.Kind, Text: p.Text, MIMEType: p.MIMEType, URI: p.URI, Data: p.Data})
	}
	return result
}
func taskDTO(t domain.Task) taskView {
	out := taskView{ID: t.ID, RunID: t.Call.RunID, ToolCallID: t.Call.ID, AgentID: t.Call.Caller.AgentID, InvocationID: t.Call.Caller.InvocationID, ParentInvocationID: t.Call.Caller.ParentInvocationID, Status: t.Status, Progress: partsDTO(t.Progress), CancelRequestedAt: t.CancelRequestedAt, ObservationError: t.ObservationError, AppliedAt: t.AppliedAt}
	if t.Result != nil {
		out.Result = &resultView{CallID: t.Result.CallID, Parts: partsDTO(t.Result.Parts), Error: t.Result.Error}
	}
	return out
}
