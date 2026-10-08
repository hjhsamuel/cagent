package adk

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/hjhsamuel/cagent/internal/agent"
	"github.com/hjhsamuel/cagent/internal/domain"
	"google.golang.org/adk/v2/session"
)

// SDKSnapshot 保存真实 SDK 的事件日志及状态，不保存 Go goroutine/迭代器。
// ADK v2.4.0 的工作流从事件重建执行状态；事件和状态须一起迁移。
type sdkSnapshot struct {
	Events []*session.Event `json:"events"`
	State  map[string]any   `json:"state"`
}

func snapshotSDK(s session.Session) (sdkSnapshot, error) {
	snap := sdkSnapshot{State: map[string]any{}}
	for k, v := range s.State().All() {
		if !strings.HasPrefix(k, "temp:") {
			snap.State[k] = v
		}
	}
	for event := range s.Events().All() {
		snap.Events = append(snap.Events, event)
	}
	// JSON 往返隔离 SDK 内部指针，并及早拒绝不可序列化状态。
	data, e := json.Marshal(snap)
	if e != nil {
		return sdkSnapshot{}, safeError(e)
	}
	var copy sdkSnapshot
	e = json.Unmarshal(data, &copy)
	return copy, safeError(e)
}
func restoreSDK(ctx context.Context, snap sdkSnapshot, app, user, id string) (session.Service, session.Session, error) {
	svc := session.InMemoryService()
	created, e := svc.Create(ctx, &session.CreateRequest{AppName: app, UserID: user, SessionID: id})
	if e != nil {
		return nil, nil, safeError(e)
	}
	for _, event := range snap.Events {
		if event == nil {
			return nil, nil, invalid("checkpoint.event")
		}
		if e = svc.AppendEvent(ctx, created.Session, event); e != nil {
			return nil, nil, safeError(e)
		}
	}
	// 先重放 delta，再应用最终状态，避免重复应用旧 delta 覆盖最后快照。
	for k, v := range snap.State {
		if e = created.Session.State().Set(k, v); e != nil {
			return nil, nil, safeError(e)
		}
	}
	return svc, created.Session, nil
}

const CheckpointFormat = "adk-go/2.4.0/completed/v1"

// CheckpointComplete 仅用于调度就绪判断，Recover 仍校验身份与真实 SDK 最终事件。
func (r *Runtime) CheckpointComplete(cp domain.Checkpoint) bool {
	return cp.Format == CheckpointFormat && len(cp.PendingCallIDs) == 0
}

type completedCheckpoint struct {
	Model        string                `json:"model"`
	Scope        domain.Scope          `json:"scope"`
	RunID        string                `json:"run_id"`
	Caller       domain.AgentExecution `json:"caller"`
	SDK          sdkSnapshot           `json:"sdk"`
	FinalEventID string                `json:"final_event_id"`
}

// Recover 对完成边界只结算，对暂停边界仅在依赖全部接纳后推进新 Runner。
// 缺失检查点或 in-flight 屏障表示外部副作用不确定；未知格式明确拒绝，
// 任何路径都不会用 Execute 重放已经启动的工具。
func (r *Runtime) Recover(ctx context.Context, req agent.Request, emit agent.Emit) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if req.Checkpoint != nil && req.Checkpoint.Format == PendingCheckpointFormat {
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
	if e := json.Unmarshal(cp.Data, &saved); e != nil {
		return invalid("checkpoint.data")
	}
	if saved.Model != r.model.Name() || saved.Scope != req.Run.Scope || saved.RunID != req.Run.ID || saved.Caller != req.Caller {
		return invalid("checkpoint.identity")
	}
	_, s, e := restoreSDK(ctx, saved.SDK, "cagent", req.Run.Scope.UserID, req.Run.ID)
	if e != nil {
		return e
	}
	for event := range s.Events().All() {
		if event.ID == saved.FinalEventID && event.Content != nil && !event.Partial && event.FinishReason == "STOP" {
			return nil
		}
	}
	return invalid("checkpoint.final_event")
}
