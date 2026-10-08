package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hjhsamuel/cagent/internal/adapter/adk"
	"github.com/hjhsamuel/cagent/internal/adapter/mongodb"
	"github.com/hjhsamuel/cagent/internal/apperrors"
	"github.com/hjhsamuel/cagent/internal/config"
	"github.com/hjhsamuel/cagent/internal/domain"
	"github.com/hjhsamuel/cagent/internal/store"
	"github.com/hjhsamuel/cagent/internal/tool"
)

// 可控远端只替代提供方；SDK、HTTP 模型协议、MongoDB 事务及恢复扫描都使用真实实现。
type taskProvider struct {
	finish        atomic.Bool
	outage        atomic.Bool
	pause         atomic.Int32
	gets, cancels atomic.Int32
}

func (p *taskProvider) Get(_ context.Context, c domain.ToolCall, h domain.TaskHandle) (domain.TaskUpdate, error) {
	p.gets.Add(1)
	if h.RemoteID != "remote-"+c.ID {
		return domain.TaskUpdate{}, errors.New("changed remote handle")
	}
	if p.outage.Load() {
		return domain.TaskUpdate{}, context.DeadlineExceeded
	}
	if p.finish.Load() {
		return domain.TaskUpdate{Status: domain.TaskSucceeded, Result: &domain.ToolResult{CallID: c.ID, Parts: []domain.Part{{Kind: domain.PartText, Text: "result-" + c.ID}}}}, nil
	}
	if p.pause.Load() == 1 {
		return domain.TaskUpdate{Status: domain.TaskInputRequired}, nil
	}
	if p.pause.Load() == 2 {
		return domain.TaskUpdate{Status: domain.TaskAuthRequired}, nil
	}
	return domain.TaskUpdate{Status: domain.TaskRunning, Progress: []domain.Part{{Kind: domain.PartText, Text: "working"}}}, nil
}
func (p *taskProvider) Follow(context.Context, domain.ToolCall, domain.TaskHandle, string, func(domain.TaskUpdate) error) error {
	return apperrors.ErrUnsupported
}
func (p *taskProvider) Cancel(context.Context, domain.ToolCall, domain.TaskHandle) error {
	p.cancels.Add(1)
	return apperrors.ErrUnsupported
}

func (p *taskProvider) Continue(_ context.Context, c domain.ToolCall, h domain.TaskHandle, in tool.TaskInput) error {
	if h.RemoteID != "remote-"+c.ID || in.Text == "" {
		return apperrors.ErrInvalidArgument
	}
	p.finish.Store(true)
	return nil
}

func eventually(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatal("condition did not become true")
}

func TestTaskRestartCompetitionAndCancel(t *testing.T) {
	for _, cancelRun := range []bool{false, true} {
		t.Run(fmt.Sprint("cancel=", cancelRun), func(t *testing.T) {
			ctx := context.Background()
			db, dbopts := testDatabase(t)
			provider := &taskProvider{}
			var models, executions atomic.Int32
			model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if models.Add(1) == 1 {
					fmt.Fprint(w, `{"choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","tool_calls":[{"id":"one","type":"function","function":{"name":"remote","arguments":"{}"}},{"id":"two","type":"function","function":{"name":"remote","arguments":"{}"}}]}}]}`)
					return
				}
				var body struct {
					Messages []struct {
						Role string `json:"role"`
						ID   string `json:"tool_call_id"`
					} `json:"messages"`
				}
				if e := json.NewDecoder(r.Body).Decode(&body); e != nil {
					t.Error(e)
				}
				seen := map[string]int{}
				for _, m := range body.Messages {
					if m.Role == "tool" {
						seen[m.ID]++
					}
				}
				if seen["one"] != 1 || seen["two"] != 1 {
					t.Errorf("broken result pairing: %v", seen)
				}
				fmt.Fprint(w, `{"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"done"}}]}`)
			}))
			defer model.Close()
			catalog, e := tool.NewCatalog([]tool.Entry{{Scope: scope, ConnectionID: "local", Descriptor: tool.Descriptor{Name: "remote", Protocol: domain.ToolLocal, InputSchema: []byte(`{"type":"object"}`)}, Tasks: provider, Executor: tool.ExecutorFunc(func(_ context.Context, c domain.ToolCall) (domain.ToolOutcome, error) {
				executions.Add(1)
				return domain.ToolOutcome{Task: &domain.TaskHandle{Protocol: c.Protocol, ConnectionID: "local", RemoteID: "remote-" + c.ID}}, nil
			})}}, tool.Limits{Timeout: time.Second, MaxInputBytes: 1024, MaxOutputBytes: 4096})
			if e != nil {
				t.Fatal(e)
			}
			create := func(database *mongodb.Database) *Application {
				a, e := NewOpenAIService(ctx, database, adkConfig(model.URL), nil, Options{LeaseDuration: 3 * time.Second, PollInterval: 30 * time.Millisecond, Registry: catalog, Tasks: config.Tasks{PollInterval: 20 * time.Millisecond, ReconnectBackoff: 20 * time.Millisecond, ObservationTimeout: 100 * time.Millisecond}}, adk.ToolOptions{Registry: catalog, MaxModelCalls: 5})
				if e != nil {
					t.Fatal(e)
				}
				t.Cleanup(func() { a.Close(ctx) })
				return a
			}
			a := create(db)
			s := session(t, a)
			run := start(t, a, s, "key")
			awaitStatus(t, db, run, domain.RunWaiting)
			eventually(t, func() bool { return provider.gets.Load() >= 2 })
			tasks, e := db.ListUnsettledTasks(ctx, scope, run.ID, store.KeyPage{Limit: 10})
			if e != nil || len(tasks.Items) != 2 {
				t.Fatal(tasks, e)
			}
			if _, e = a.GetTask(ctx, domain.Scope{TenantID: "other", UserID: scope.UserID}, tasks.Items[0].ID); !errors.Is(e, apperrors.ErrNotFound) {
				t.Fatal("cross scope", e)
			}
			// 本地 Follow 先交付持久快照；订阅断开不会向远端发送取消。
			followCtx, stopFollow := context.WithCancel(ctx)
			notifications := 0
			tracker := &taskTracker{app: a}
			e = tracker.Follow(followCtx, scope, tasks.Items[0].ID, func(task domain.Task) error {
				notifications++
				if task.Version <= 0 {
					t.Error("unpersisted notification")
				}
				stopFollow()
				return nil
			})
			if !errors.Is(e, context.Canceled) || notifications != 1 || provider.cancels.Load() != 0 {
				t.Fatal("subscription affected remote task", e, notifications)
			}
			provider.outage.Store(true)
			eventually(t, func() bool {
				task, _ := db.GetTask(ctx, scope, tasks.Items[0].ID)
				return task.ObservationError != "" && !task.Status.IsTerminal()
			})
			if e = a.CancelTask(ctx, scope, tasks.Items[0].ID); e != nil {
				t.Fatal(e)
			}
			eventually(t, func() bool { return provider.cancels.Load() > 0 })
			if e = a.Close(ctx); e != nil {
				t.Fatal(e)
			}
			if cancelRun {
				if e = a.CancelRun(ctx, scope, run.ID); e != nil {
					t.Fatal(e)
				}
			}
			// 两个独立数据库客户端、两个独立扫描器同时恢复；不通过原应用内存表去重。
			db2, e := mongodb.Open(ctx, dbopts)
			if e != nil {
				t.Fatal(e)
			}
			defer db2.Close(ctx)
			b, c := create(db), create(db2)
			recovery, e := mongodb.OpenRecovery(ctx, dbopts)
			if e != nil {
				t.Fatal(e)
			}
			defer recovery.Close(ctx)
			if e = b.StartRecovery(recovery, 20*time.Millisecond); e != nil {
				t.Fatal(e)
			}
			if e = c.StartRecovery(recovery, 20*time.Millisecond); e != nil {
				t.Fatal(e)
			}
			provider.outage.Store(false)
			provider.finish.Store(true)
			if !cancelRun {
				awaitStatus(t, db, run, domain.RunCompleted)
			}
			eventually(t, func() bool {
				page, e := db.ListUnsettledTasks(ctx, scope, run.ID, store.KeyPage{Limit: 10})
				return e == nil && len(page.Items) == 0
			})
			for _, original := range tasks.Items {
				task, e := db.GetTask(ctx, scope, original.ID)
				if e != nil {
					t.Fatal(e)
				}
				delivery, e := db.GetTaskDelivery(ctx, scope, original.ID)
				if e != nil {
					t.Fatal(e)
				}
				if task.Status != domain.TaskSucceeded {
					t.Fatal("lost actual remote success", task.Status)
				}
				if cancelRun {
					if task.AppliedAt != nil || delivery.State != domain.DeliveryDiscarded {
						t.Fatal("resumed cancelled run")
					}
				} else {
					if task.AppliedAt == nil || delivery.State != domain.DeliveryApplied {
						t.Fatal("missing atomic application")
					}
				}
			}
			wantModels := int32(2)
			if cancelRun {
				wantModels = 1
			}
			if models.Load() != wantModels || executions.Load() != 2 {
				t.Fatalf("models=%d executions=%d", models.Load(), executions.Load())
			}
			eventPage, e := db.ListEvents(ctx, scope, run.ID, store.SequencePage{Limit: 100})
			if e != nil {
				t.Fatal(e)
			}
			terminals := 0
			for i, event := range eventPage.Items {
				if event.Sequence != int64(i+1) {
					t.Fatal("SSE replay gap")
				}
				if event.Kind == domain.EventRunCompleted || event.Kind == domain.EventRunCancelled {
					terminals++
					if i != len(eventPage.Items)-1 {
						t.Fatal("event after terminal")
					}
				}
			}
			if terminals != 1 {
				t.Fatal("duplicate terminal events", terminals)
			}
			b.Close(ctx)
			c.Close(ctx)
		})
	}
}
