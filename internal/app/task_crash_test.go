package app

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hjhsamuel/cagent/internal/adapter/adk"
	"github.com/hjhsamuel/cagent/internal/agent"
	"github.com/hjhsamuel/cagent/internal/config"
	"github.com/hjhsamuel/cagent/internal/contextengine"
	"github.com/hjhsamuel/cagent/internal/domain"
	"github.com/hjhsamuel/cagent/internal/store"
	"github.com/hjhsamuel/cagent/internal/tool"
)

// 只在首次 Track 事务之后中断，留下真实 SDK 的整批句柄快照和一个已登记 Task。
// 不直接篡改数据库来制造状态，验证生产崩溃窗口能由后续正常事务补齐。
type partialHandoff struct {
	*adk.Runtime
	stop context.CancelFunc
}

func (r *partialHandoff) Execute(ctx context.Context, req agent.Request, emit agent.Emit) error {
	return r.Runtime.Execute(ctx, req, func(c context.Context, u agent.Update) error {
		if len(u.Tasks) > 1 {
			u.Tasks = u.Tasks[:1]
			if e := emit(c, u); e != nil {
				return e
			}
			r.stop()
			return context.Canceled
		}
		return emit(c, u)
	})
}

func TestPartialHandoffRepairIncludesCancelledRun(t *testing.T) {
	for _, mode := range []string{"complete", "cancelled", "interrupted-generation", "input", "authorization"} {
		t.Run(mode, func(t *testing.T) {
			cancelled := mode == "cancelled"
			interrupted := mode == "interrupted-generation"
			release := make(chan struct{})
			ctx := context.Background()
			db, _ := testDatabase(t)
			provider := &taskProvider{}
			var executions, models atomic.Int32
			model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if models.Add(1) == 1 {
					fmt.Fprint(w, `{"choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","tool_calls":[{"id":"one","type":"function","function":{"name":"remote","arguments":"{}"}},{"id":"two","type":"function","function":{"name":"remote","arguments":"{}"}}]}}]}`)
				} else {
					if interrupted {
						select {
						case <-r.Context().Done():
						case <-release:
						}
						return
					}
					fmt.Fprint(w, `{"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"done"}}]}`)
				}
			}))
			defer model.Close()
			defer close(release)
			catalog, e := tool.NewCatalog([]tool.Entry{{Scope: scope, ConnectionID: "c", Descriptor: tool.Descriptor{Name: "remote", Protocol: domain.ToolLocal, InputSchema: []byte(`{"type":"object"}`)}, Tasks: provider, Executor: tool.ExecutorFunc(func(_ context.Context, c domain.ToolCall) (domain.ToolOutcome, error) {
				executions.Add(1)
				return domain.ToolOutcome{Task: &domain.TaskHandle{Protocol: c.Protocol, ConnectionID: "c", RemoteID: "remote-" + c.ID}}, nil
			})}}, tool.Limits{Timeout: time.Second, MaxInputBytes: 1024, MaxOutputBytes: 4096})
			if e != nil {
				t.Fatal(e)
			}
			cfg := adkConfig(model.URL)
			budget := contextengine.Budget{WindowTokens: cfg.Context.WindowTokens, OutputTokens: cfg.Context.OutputTokens, ToolTokens: cfg.Context.ToolTokens, SafetyTokens: cfg.Context.SafetyTokens}
			llm, e := adk.NewOpenAI(cfg.Agent, nil)
			if e != nil {
				t.Fatal(e)
			}
			runtime, e := adk.New(llm, llm, budget, adk.ToolOptions{Registry: catalog, MaxModelCalls: 4})
			if e != nil {
				t.Fatal(e)
			}
			engine, e := contextengine.New(runtime)
			if e != nil {
				t.Fatal(e)
			}
			prepare, e := NewContextPreparer(db, engine, ContextOptions{Budget: budget, PolicyVersion: cfg.Context.PolicyVersion})
			if e != nil {
				t.Fatal(e)
			}
			parent, stop := context.WithCancel(ctx)
			defer stop()
			first, e := NewService(parent, db, &partialHandoff{Runtime: runtime, stop: stop}, Options{LeaseDuration: 3 * time.Second, PollInterval: 30 * time.Millisecond, Prepare: prepare, Recover: runtime.Recover})
			if e != nil {
				t.Fatal(e)
			}
			defer first.Close(ctx)
			run := start(t, first, session(t, first), "partial")
			eventually(t, func() bool { return parent.Err() != nil })
			if e = first.Close(ctx); e != nil {
				t.Fatal(e)
			}
			page, e := db.ListUnsettledTasks(ctx, scope, run.ID, store.KeyPage{Limit: 10})
			if e != nil || len(page.Items) != 1 {
				t.Fatal("did not interrupt between tracks", page, e)
			}
			cp, e := db.GetCheckpoint(ctx, scope, run.ID, run.ID)
			if e != nil || len(cp.PendingCallIDs) != 1 {
				t.Fatal(cp, e)
			}
			if cancelled {
				if e = first.CancelRun(ctx, scope, run.ID); e != nil {
					t.Fatal(e)
				}
			}
			second, e := NewOpenAIService(ctx, db, cfg, nil, Options{LeaseDuration: 3 * time.Second, PollInterval: 30 * time.Millisecond, Tasks: config.Tasks{PollInterval: 20 * time.Millisecond, ObservationTimeout: time.Second, ReconnectBackoff: 20 * time.Millisecond}}, adk.ToolOptions{Registry: catalog, MaxModelCalls: 4})
			if e != nil {
				t.Fatal(e)
			}
			defer second.Close(ctx)
			if e = second.RecoverRun(ctx, scope, run.ID); e != nil {
				t.Fatal(e)
			}
			eventually(t, func() bool {
				page, _ := db.ListUnsettledTasks(ctx, scope, run.ID, store.KeyPage{Limit: 10})
				return len(page.Items) == 2
			})
			page, e = db.ListUnsettledTasks(ctx, scope, run.ID, store.KeyPage{Limit: 10})
			if e != nil {
				t.Fatal(e)
			}
			if mode == "input" || mode == "authorization" {
				in := tool.TaskInput{Text: "continue"}
				authorization := mode == "authorization"
				if authorization {
					provider.pause.Store(2)
					in.CredentialRef = "trusted-reference"
				} else {
					provider.pause.Store(1)
				}
				if e := (TaskInputs{DB: db, Registry: catalog}).Submit(ctx, scope, page.Items[0].ID, in, authorization); e != nil {
					t.Fatal(e)
				}
			} else {
				provider.finish.Store(true)
			}
			eventually(t, func() bool {
				page, _ := db.ListUnsettledTasks(ctx, scope, run.ID, store.KeyPage{Limit: 10})
				return len(page.Items) == 0
			})
			for _, task := range page.Items {
				delivery, e := db.GetTaskDelivery(ctx, scope, task.ID)
				if e != nil {
					t.Fatal(e)
				}
				want := domain.DeliveryApplied
				if cancelled {
					want = domain.DeliveryDiscarded
				}
				if delivery.State != want {
					t.Fatal(delivery)
				}
			}
			want := domain.RunCompleted
			if cancelled {
				want = domain.RunCancelled
			}
			if interrupted {
				eventually(t, func() bool { return models.Load() == 2 })
				cp, e := db.GetCheckpoint(ctx, scope, run.ID, run.ID)
				if e != nil || cp.Format != agent.InFlightCheckpointFormat {
					t.Fatal("missing side-effect barrier", cp.Format, e)
				}
				if e = second.Close(ctx); e != nil {
					t.Fatal(e)
				}
				third, e := NewOpenAIService(ctx, db, cfg, nil, Options{LeaseDuration: 3 * time.Second, PollInterval: 30 * time.Millisecond}, adk.ToolOptions{Registry: catalog, MaxModelCalls: 4})
				if e != nil {
					t.Fatal(e)
				}
				defer third.Close(ctx)
				if e = third.RecoverRun(ctx, scope, run.ID); e != nil {
					t.Fatal(e)
				}
				want = domain.RunFailed
			}
			awaitStatus(t, db, run, want)
			if interrupted {
				events, e := db.ListEvents(ctx, scope, run.ID, store.SequencePage{Limit: 100})
				if e != nil {
					t.Fatal(e)
				}
				if !strings.Contains(string(events.Items[len(events.Items)-1].Data), "execution_outcome_uncertain") {
					t.Fatal("uncertainty was not recorded")
				}
			}
			wantModels := int32(2)
			if cancelled {
				wantModels = 1
			}
			if executions.Load() != 2 || models.Load() != wantModels {
				t.Fatal("reexecuted after crash", executions.Load(), models.Load())
			}
		})
	}
}
