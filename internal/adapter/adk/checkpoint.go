package adk

import (
	"context"

	"github.com/hjhsamuel/cagent/internal/agent"
	"github.com/hjhsamuel/cagent/internal/domain"
)

// 检查点格式属于应用，不随 ADK 版本改变。最终消息与完成标记同事务提交。
const CheckpointFormat = "cagent.completed/v1"

func (r *Runtime) CheckpointComplete(cp domain.Checkpoint) bool {
	return cp.Format == CheckpointFormat && len(cp.PendingCallIDs) == 0
}

type completedCheckpoint struct {
	Model  string
	Scope  domain.Scope
	RunID  string
	Caller domain.AgentExecution
	Final  []domain.Part
}

// Recover 从应用消息和工具记录重建新的执行，不恢复 SDK 会话或工作流。
// 完成边界只结算；缺失边界或 in-flight 表示外部副作用不确定，禁止重执行。
func (r *Runtime) Recover(ctx context.Context, req agent.Request, emit agent.Emit) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if req.Checkpoint != nil && IsPendingCheckpoint(*req.Checkpoint) {
		return r.run(ctx, req, emit)
	}
	cp := req.Checkpoint
	if cp == nil || cp.Format == agent.InFlightCheckpointFormat {
		return agent.ErrUncertain
	}
	if cp.Format != CheckpointFormat {
		return unsupported("checkpoint.format")
	}
	if e := cp.ValidateForRun(req.Run); e != nil {
		return e
	}
	if cp.Caller != req.Caller || cp.Version <= 0 || len(cp.PendingCallIDs) != 0 {
		return invalid("checkpoint.identity")
	}
	var saved completedCheckpoint
	if e := decodeJSON(cp.Data, &saved); e != nil {
		return invalid("checkpoint.data")
	}
	if saved.Model != r.model.Name() || saved.Scope != req.Run.Scope || saved.RunID != req.Run.ID || saved.Caller != req.Caller {
		return invalid("checkpoint.identity")
	}
	if len(saved.Final) == 0 {
		return invalid("checkpoint.final_message")
	}
	hasText := false
	for _, p := range saved.Final {
		if p.Kind != domain.PartText || p.Data != nil || p.ToolCallID != "" || p.ToolName != "" || p.URI != "" || p.MIMEType != "" {
			return invalid("checkpoint.final_message")
		}
		hasText = hasText || p.Text != ""
	}
	if !hasText {
		return invalid("checkpoint.final_message")
	}
	return nil
}
