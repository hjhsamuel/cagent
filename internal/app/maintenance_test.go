package app

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hjhsamuel/cagent/internal/adapter/adk"
	"github.com/hjhsamuel/cagent/internal/agent"
	"github.com/hjhsamuel/cagent/internal/config"
	"github.com/hjhsamuel/cagent/internal/domain"
	"github.com/hjhsamuel/cagent/internal/store"
	"github.com/hjhsamuel/cagent/internal/tool"
)

func TestUnavailableRegistryStillExitsTaskMaintenance(t *testing.T) {
	db, _ := testDatabase(t)
	ctx := context.Background()
	m := config.Defaults().Maintenance
	m.OutageGrace = 500 * time.Millisecond
	runtime := runtimeFunc(func(ctx context.Context, req agent.Request, emit agent.Emit) error {
		call := domain.ToolCall{Scope: req.Run.Scope, SessionID: req.Run.SessionID, RunID: req.Run.ID, ID: "call", Caller: req.Caller, Name: "remote", Protocol: domain.ToolLocal}
		cp := domain.Checkpoint{Scope: req.Run.Scope, RunID: req.Run.ID, Caller: req.Caller, Format: "waiting", PendingCallIDs: []string{call.ID}}
		task := domain.Task{Scope: req.Run.Scope, Call: call, Handle: domain.TaskHandle{Protocol: call.Protocol, ConnectionID: "removed", RemoteID: "existing"}, Status: domain.TaskSubmitted}
		if err := emit(ctx, agent.Update{Kind: domain.EventToolWaiting, Checkpoint: &cp, Tasks: []domain.Task{task}}); err != nil {
			return err
		}
		return agent.ErrWaiting
	})
	opts := Options{LeaseDuration: 3 * time.Second, PollInterval: 30 * time.Millisecond, Maintenance: m, Tasks: config.Tasks{PollInterval: 20 * time.Millisecond, ObservationTimeout: 50 * time.Millisecond, ReconnectBackoff: 20 * time.Millisecond}, Recover: func(context.Context, agent.Request, agent.Emit) error { return agent.ErrWaiting }}
	a, err := NewService(ctx, db, runtime, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(ctx)
	run := start(t, a, session(t, a), "")
	awaitStatus(t, db, run, domain.RunWaiting)
	initial, err := db.ListUnsettledTasks(ctx, scope, run.ID, store.KeyPage{Limit: 10})
	if err != nil || len(initial.Items) != 1 {
		t.Fatal("task was not persisted", err)
	}
	taskID := initial.Items[0].ID
	awaitStatus(t, db, run, domain.RunFailed)
	page, err := db.ListUnsettledTasks(ctx, scope, run.ID, store.KeyPage{Limit: 10})
	if err != nil || len(page.Items) != 0 {
		t.Fatal("missing registry left tasks scheduled", err)
	}
	if err = a.Close(ctx); err != nil {
		t.Fatal(err)
	}
	// A terminal Run can also recover a manually resumed handle without tools.
	if _, err = db.ResumeTaskMaintenance(ctx, scope, taskID); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewService(ctx, db, runtime, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close(ctx)
	if err = restarted.RecoverRun(ctx, scope, run.ID); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		task, err := db.GetTask(ctx, scope, taskID)
		return err == nil && task.Maintenance == domain.MaintenanceQuarantined
	})
}

func TestWaitingReleasesGenerationAndCancelledMaintenanceExits(t *testing.T) {
	db, _ := testDatabase(t)
	ctx := context.Background()
	provider := &taskProvider{}
	var models, executions atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		models.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","tool_calls":[{"id":"one","type":"function","function":{"name":"remote","arguments":"{}"}}]}}]}`)
	}))
	defer srv.Close()
	catalog, err := tool.NewCatalog([]tool.Entry{{Scope: scope, ConnectionID: "local", Descriptor: tool.Descriptor{Name: "remote", Protocol: domain.ToolLocal, InputSchema: []byte(`{"type":"object"}`)}, Tasks: provider, Executor: tool.ExecutorFunc(func(_ context.Context, c domain.ToolCall) (domain.ToolOutcome, error) {
		executions.Add(1)
		return domain.ToolOutcome{Task: &domain.TaskHandle{Protocol: c.Protocol, ConnectionID: "local", RemoteID: "remote-" + c.ID}}, nil
	})}}, tool.Limits{Timeout: time.Second, MaxInputBytes: 1024, MaxOutputBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	cfg := adkConfig(srv.URL)
	cfg.Capacity.Runs = 1
	cfg.Maintenance.Workers = 1
	cfg.Maintenance.CancelGrace = 150 * time.Millisecond
	a, err := NewOpenAIService(ctx, db, cfg, nil, Options{LeaseDuration: 3 * time.Second, PollInterval: 30 * time.Millisecond, Registry: catalog, Tasks: config.Tasks{PollInterval: 30 * time.Millisecond, ObservationTimeout: 100 * time.Millisecond, ReconnectBackoff: 20 * time.Millisecond}}, adk.ToolOptions{Registry: catalog, MaxModelCalls: 3})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(ctx)
	first := start(t, a, session(t, a), "")
	awaitStatus(t, db, first, domain.RunWaiting)
	eventually(t, func() bool {
		release, err := a.runs.Try(ctx)
		if err != nil {
			return false
		}
		release()
		return true
	})
	second := start(t, a, session(t, a), "")
	awaitStatus(t, db, second, domain.RunWaiting)
	if models.Load() != 2 || executions.Load() != 2 {
		t.Fatal("waiting blocked generation or re-executed", models.Load(), executions.Load())
	}
	if err = a.CancelRun(ctx, scope, first.ID); err != nil {
		t.Fatal(err)
	}
	var task domain.Task
	eventually(t, func() bool {
		page, e := db.ListUnsettledTasks(ctx, scope, first.ID, store.KeyPage{Limit: 10})
		if e != nil {
			return false
		}
		if len(page.Items) > 0 {
			task = page.Items[0]
			return false
		}
		return task.ID != ""
	})
	task, err = db.GetTask(ctx, scope, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if task.Maintenance != domain.MaintenanceQuarantined || task.Status == domain.TaskCancelled || task.CancelAttempts == 0 || task.CancelError != "unsupported" {
		t.Fatal("cancel maintenance did not exit truthfully", task.Maintenance, task.Status, task.CancelAttempts, task.CancelError)
	}
	gets := provider.gets.Load()
	time.Sleep(100 * time.Millisecond)
	// The other run still observes; no terminal run keeps a local active loop.
	a.mu.Lock()
	_, active := a.active[runKey{scope, first.ID}]
	_, pending := a.due[runKey{scope, first.ID}]
	a.mu.Unlock()
	if active || pending || provider.gets.Load() < gets {
		t.Fatal("quarantined run remained locally scheduled")
	}
}
