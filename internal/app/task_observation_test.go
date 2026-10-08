package app

import (
	"context"
	"errors"
	"github.com/hjhsamuel/cagent/internal/apperrors"
	"github.com/hjhsamuel/cagent/internal/config"
	"github.com/hjhsamuel/cagent/internal/domain"
	"github.com/hjhsamuel/cagent/internal/observability"
	"github.com/hjhsamuel/cagent/internal/tool"
	"testing"
	"time"
)

type observingClient struct {
	follow  func(context.Context, string, func(domain.TaskUpdate) error) error
	get     func(context.Context) (domain.TaskUpdate, error)
	cancels int
}

func (c *observingClient) Follow(ctx context.Context, _ domain.ToolCall, _ domain.TaskHandle, cursor string, emit func(domain.TaskUpdate) error) error {
	return c.follow(ctx, cursor, emit)
}
func (c *observingClient) Get(ctx context.Context, _ domain.ToolCall, _ domain.TaskHandle) (domain.TaskUpdate, error) {
	return c.get(ctx)
}
func (c *observingClient) Cancel(context.Context, domain.ToolCall, domain.TaskHandle) error {
	c.cancels++
	return apperrors.ErrUnsupported
}

func TestObserveSubscriptionCursorFallbackAndTimeout(t *testing.T) {
	for _, mode := range []string{"subscribed", "unsupported", "disconnected", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			gets, follows := 0, 0
			client := &observingClient{}
			client.follow = func(ctx context.Context, cursor string, emit func(domain.TaskUpdate) error) error {
				follows++
				if cursor != "persisted-cursor" {
					t.Error("lost cursor")
				}
				switch mode {
				case "subscribed":
					// 旧 running 通知只能唤醒查询，不能覆盖查询确认的暂停状态。
					return emit(domain.TaskUpdate{Status: domain.TaskRunning, Cursor: "next"})
				case "unsupported":
					return apperrors.ErrUnsupported
				case "disconnected":
					return errors.New("connection dropped")
				default:
					<-ctx.Done()
					return ctx.Err()
				}
			}
			client.get = func(ctx context.Context) (domain.TaskUpdate, error) {
				gets++
				if ctx.Err() != nil {
					t.Error("Get reused expired subscription context")
				}
				if mode == "timeout" {
					return domain.TaskUpdate{}, context.DeadlineExceeded
				}
				if mode == "subscribed" {
					return domain.TaskUpdate{Status: domain.TaskInputRequired}, nil
				}
				return domain.TaskUpdate{Status: domain.TaskRunning}, nil
			}
			catalog, e := tool.NewCatalog([]tool.Entry{{Scope: scope, ConnectionID: "c", Descriptor: tool.Descriptor{Name: "remote", Protocol: domain.ToolLocal, InputSchema: []byte(`{"type":"object"}`)}, Tasks: client, Executor: tool.ExecutorFunc(func(context.Context, domain.ToolCall) (domain.ToolOutcome, error) {
				t.Error("reexecuted tool")
				return domain.ToolOutcome{}, nil
			})}}, tool.Limits{Timeout: time.Second, MaxInputBytes: 1024, MaxOutputBytes: 4096})
			if e != nil {
				t.Fatal(e)
			}
			a := &Application{observations: observability.NewGate(1), opts: Options{Registry: catalog, Tasks: config.Tasks{ObservationTimeout: 15 * time.Millisecond, ReconnectBackoff: time.Millisecond}}}
			task := domain.Task{Scope: scope, ID: "task", Call: domain.ToolCall{Scope: scope, ID: "call", SessionID: "s", RunID: "r", Caller: domain.AgentExecution{AgentID: "a", InvocationID: "inv"}, Name: "remote", Protocol: domain.ToolLocal}, Handle: domain.TaskHandle{Protocol: domain.ToolLocal, ConnectionID: "c", RemoteID: "remote"}, ProviderCursor: "persisted-cursor"}
			update, e := a.observeTask(context.Background(), task)
			if mode == "timeout" {
				if !errors.Is(e, context.DeadlineExceeded) || update.Status.IsTerminal() {
					t.Fatal("timeout became remote failure", update, e)
				}
			} else if e != nil {
				t.Fatal(e)
			}
			wantGets := 1
			if mode == "subscribed" {
				if update.Status != domain.TaskInputRequired || update.Cursor != "next" {
					t.Fatal(update)
				}
			}
			if gets != wantGets || follows != 1 || client.cancels != 0 {
				t.Fatal(gets, follows, client.cancels)
			}
		})
	}
}
