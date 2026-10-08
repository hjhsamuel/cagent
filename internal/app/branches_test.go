package app

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/hjhsamuel/cagent/internal/agent"
	"github.com/hjhsamuel/cagent/internal/apperrors"
	"github.com/hjhsamuel/cagent/internal/config"
	"github.com/hjhsamuel/cagent/internal/domain"
	"github.com/hjhsamuel/cagent/internal/store"
	"github.com/hjhsamuel/cagent/internal/tool"
)

// 此替身专门验证应用层的多 Invocation 调度；ADK SDK 的结果接纳另有真实测试。
// 两个分支使用相同 AgentID，不允许按名称把结果续接到另一个调用。
type branchRuntime struct {
	mu                 sync.Mutex
	resumed, recovered map[string]int
	ready              map[string]bool
}

func (r *branchRuntime) Execute(ctx context.Context, req agent.Request, emit agent.Emit) error {
	for _, id := range []string{"left", "right"} {
		caller := domain.AgentExecution{AgentID: "same-agent", InvocationID: id, ParentInvocationID: req.Caller.InvocationID}
		call := domain.ToolCall{Scope: req.Run.Scope, SessionID: req.Run.SessionID, RunID: req.Run.ID, ID: "call-" + id, Caller: caller, Name: "remote", Protocol: domain.ToolLocal}
		cp := domain.Checkpoint{Scope: req.Run.Scope, RunID: req.Run.ID, Caller: caller, Format: "branch.waiting", PendingCallIDs: []string{call.ID}}
		task := domain.Task{Scope: req.Run.Scope, Call: call, Handle: domain.TaskHandle{Protocol: call.Protocol, ConnectionID: "c", RemoteID: id}, Status: domain.TaskSubmitted}
		if e := emit(ctx, agent.Update{Kind: domain.EventToolWaiting, Checkpoint: &cp, Tasks: []domain.Task{task}}); e != nil {
			return e
		}
	}
	return agent.ErrWaiting
}
func (r *branchRuntime) Resume(ctx context.Context, req agent.Request, c agent.Continuation, emit agent.Emit) error {
	if req.Caller != c.Call.Caller || req.Checkpoint.Caller != c.Call.Caller || c.Result.CallID != c.Call.ID {
		return apperrors.ErrInvalidArgument
	}
	cp := *req.Checkpoint
	cp.Format = "branch.ready"
	cp.PendingCallIDs = nil
	e := emit(ctx, agent.Update{AppliedTaskID: c.TaskID, Kind: domain.EventToolFinished, Checkpoint: &cp, MessageRole: domain.RoleTool, Message: []domain.Part{{Kind: domain.PartToolResult, ToolCallID: c.Call.ID, ToolName: c.Call.Name, Data: []byte(`{}`)}}})
	if e == nil {
		r.mu.Lock()
		r.resumed[req.Caller.InvocationID]++
		r.mu.Unlock()
	}
	return e
}
func (r *branchRuntime) Recover(ctx context.Context, req agent.Request, emit agent.Emit) error {
	if req.Checkpoint == nil {
		return nil
	}
	if len(req.Checkpoint.PendingCallIDs) > 0 {
		return agent.ErrWaiting
	}
	if req.Checkpoint.Format == "branch.done" {
		return nil
	}
	cp := *req.Checkpoint
	cp.Format = "branch.done"
	e := emit(ctx, agent.Update{Kind: domain.EventMessageCompleted, Checkpoint: &cp, Message: []domain.Part{{Kind: domain.PartText, Text: "done-" + req.Caller.InvocationID}}})
	if e == nil {
		r.mu.Lock()
		r.recovered[req.Caller.InvocationID]++
		r.mu.Unlock()
	}
	return e
}
func (r *branchRuntime) CheckpointComplete(cp domain.Checkpoint) bool {
	return cp.Format == "branch.done"
}
func (r *branchRuntime) Get(_ context.Context, c domain.ToolCall, h domain.TaskHandle) (domain.TaskUpdate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c.Caller.InvocationID != h.RemoteID {
		return domain.TaskUpdate{}, apperrors.ErrInvalidArgument
	}
	if r.ready[h.RemoteID] {
		return domain.TaskUpdate{Status: domain.TaskSucceeded, Result: &domain.ToolResult{CallID: c.ID}}, nil
	}
	return domain.TaskUpdate{Status: domain.TaskRunning}, nil
}
func (r *branchRuntime) Follow(context.Context, domain.ToolCall, domain.TaskHandle, string, func(domain.TaskUpdate) error) error {
	return apperrors.ErrUnsupported
}
func (r *branchRuntime) Cancel(context.Context, domain.ToolCall, domain.TaskHandle) error { return nil }

func TestParallelBranchesResumeIndependently(t *testing.T) {
	ctx := context.Background()
	db, _ := testDatabase(t)
	runtime := &branchRuntime{resumed: map[string]int{}, recovered: map[string]int{}, ready: map[string]bool{"left": true}}
	catalog, e := tool.NewCatalog([]tool.Entry{{Scope: scope, ConnectionID: "c", Descriptor: tool.Descriptor{Name: "remote", Protocol: domain.ToolLocal, InputSchema: []byte(`{"type":"object"}`)}, Tasks: runtime, Executor: tool.ExecutorFunc(func(context.Context, domain.ToolCall) (domain.ToolOutcome, error) {
		t.Error("observer executed tool")
		return domain.ToolOutcome{}, apperrors.ErrUnsupported
	})}}, tool.Limits{Timeout: time.Second, MaxInputBytes: 1024, MaxOutputBytes: 4096})
	if e != nil {
		t.Fatal(e)
	}
	a, e := NewService(ctx, db, runtime, Options{LeaseDuration: 3 * time.Second, PollInterval: 30 * time.Millisecond, Registry: catalog, Recover: runtime.Recover, Tasks: config.Tasks{PollInterval: 20 * time.Millisecond, ObservationTimeout: time.Second, ReconnectBackoff: 20 * time.Millisecond}})
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close(ctx)
	run := start(t, a, session(t, a), "parallel")
	eventually(t, func() bool { runtime.mu.Lock(); defer runtime.mu.Unlock(); return runtime.recovered["left"] == 1 })
	current, e := db.GetRun(ctx, scope, run.ID)
	if e != nil || current.Status.IsTerminal() {
		t.Fatal("completed while another branch waits", current, e)
	}
	eventually(t, func() bool { current, _ := db.GetRun(ctx, scope, run.ID); return current.Status == domain.RunWaiting })
	runtime.mu.Lock()
	runtime.ready["right"] = true
	runtime.mu.Unlock()
	awaitStatus(t, db, run, domain.RunCompleted)
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	for _, id := range []string{"left", "right"} {
		if runtime.resumed[id] != 1 || runtime.recovered[id] != 1 {
			t.Fatal(fmt.Sprint(runtime.resumed, runtime.recovered))
		}
	}
	page, e := db.ListCheckpoints(ctx, scope, run.ID, store.KeyPage{Limit: 1})
	if e != nil || !page.HasMore || len(page.Items) != 1 {
		t.Fatal(page, e)
	}
	next, e := db.ListCheckpoints(ctx, scope, run.ID, store.KeyPage{After: page.NextAfter, Limit: 1})
	if e != nil || next.HasMore || next.Items[0].Caller.InvocationID == page.Items[0].Caller.InvocationID {
		t.Fatal(next, e)
	}
}

// 无恢复扫描器也应继续维护本机已取消 Run 的任务；生成用的 context 取消后，
// 新的维护步骤从进程 context 出发，实际成功仅保存/丢弃，绝不调用 Resume。
func TestCancelRunKeepsLocalTaskMaintenance(t *testing.T) {
	ctx := context.Background()
	db, _ := testDatabase(t)
	runtime := &branchRuntime{resumed: map[string]int{}, recovered: map[string]int{}, ready: map[string]bool{}}
	catalog, e := tool.NewCatalog([]tool.Entry{{Scope: scope, ConnectionID: "c", Descriptor: tool.Descriptor{Name: "remote", Protocol: domain.ToolLocal, InputSchema: []byte(`{"type":"object"}`)}, Tasks: runtime, Executor: tool.ExecutorFunc(func(context.Context, domain.ToolCall) (domain.ToolOutcome, error) {
		t.Error("unexpected execution")
		return domain.ToolOutcome{}, nil
	})}}, tool.Limits{Timeout: time.Second, MaxInputBytes: 1024, MaxOutputBytes: 4096})
	if e != nil {
		t.Fatal(e)
	}
	a, e := NewService(ctx, db, runtime, Options{LeaseDuration: 3 * time.Second, PollInterval: 30 * time.Millisecond, Registry: catalog, Recover: runtime.Recover, Tasks: config.Tasks{PollInterval: 20 * time.Millisecond, ObservationTimeout: time.Second, ReconnectBackoff: 20 * time.Millisecond}})
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close(ctx)
	run := start(t, a, session(t, a), "cancel-live")
	awaitStatus(t, db, run, domain.RunWaiting)
	if e = a.CancelRun(ctx, scope, run.ID); e != nil {
		t.Fatal(e)
	}
	runtime.mu.Lock()
	runtime.ready["left"] = true
	runtime.ready["right"] = true
	runtime.mu.Unlock()
	eventually(t, func() bool {
		page, e := db.ListUnsettledTasks(ctx, scope, run.ID, store.KeyPage{Limit: 10})
		return e == nil && len(page.Items) == 0
	})
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if len(runtime.resumed) != 0 || len(runtime.recovered) != 0 {
		t.Fatal("generated after cancellation")
	}
}
