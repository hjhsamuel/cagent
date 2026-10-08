package adk

import (
	"context"
	"errors"
	"iter"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hjhsamuel/cagent/internal/agent"
	"github.com/hjhsamuel/cagent/internal/domain"
	"github.com/hjhsamuel/cagent/internal/tool"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// 真实 Runner 的暂停和新 Runtime 恢复：乱序接纳、提交失败、重复接纳、原 Subagent
// 身份校验都不能触发模型；最后一个持久结果到齐后才推进模型，旧工具只执行一次。
func TestResumeAtomicBoundaryAndRestart(t *testing.T) {
	ctx := context.Background()
	req := request("user")
	var models, executions atomic.Int32
	catalog, e := tool.NewCatalog([]tool.Entry{{Scope: req.Run.Scope, ConnectionID: "c", Descriptor: tool.Descriptor{Name: "remote", Protocol: domain.ToolLocal, InputSchema: []byte(`{"type":"object"}`)}, Executor: tool.ExecutorFunc(func(_ context.Context, c domain.ToolCall) (domain.ToolOutcome, error) {
		executions.Add(1)
		return domain.ToolOutcome{Task: &domain.TaskHandle{Protocol: c.Protocol, ConnectionID: "c", RemoteID: c.ID}}, nil
	})}}, tool.Limits{Timeout: time.Second, MaxInputBytes: 1024, MaxOutputBytes: 4096})
	if e != nil {
		t.Fatal(e)
	}
	m := modelFunc(func(_ context.Context, r *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
		return func(yield func(*model.LLMResponse, error) bool) {
			models.Add(1)
			if models.Load() == 1 {
				yield(&model.LLMResponse{Content: &genai.Content{Role: "model", Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: "one", Name: "remote", Args: map[string]any{}}}, {FunctionCall: &genai.FunctionCall{ID: "two", Name: "remote", Args: map[string]any{}}}}}}, nil)
			} else {
				results := map[string]int{}
				for _, c := range r.Contents {
					for _, p := range c.Parts {
						if p.FunctionResponse != nil {
							results[p.FunctionResponse.ID]++
							if p.FunctionResponse.Response == nil {
								t.Error("placeholder response")
							}
						}
					}
				}
				if results["one"] != 1 || results["two"] != 1 {
					t.Errorf("results: %v", results)
				}
				yield(finalResponse("resumed", 1, 1), nil)
			}
		}
	})
	makeRuntime := func() *Runtime {
		r, e := New(m, countFunc(fixedCounter), budget(), ToolOptions{Registry: catalog, MaxModelCalls: 4})
		if e != nil {
			t.Fatal(e)
		}
		return r
	}
	r := makeRuntime()
	var waiting agent.Update
	e = r.Execute(ctx, req, func(_ context.Context, u agent.Update) error {
		if u.Checkpoint != nil {
			waiting = u
		}
		return nil
	})
	if !errors.Is(e, agent.ErrWaiting) {
		t.Fatal(e)
	}
	req.Checkpoint = waiting.Checkpoint
	req.Checkpoint.Version = 1
	for _, index := range []int{1, 0} {
		task := waiting.Tasks[index]
		c := agent.Continuation{TaskID: task.Call.ID, Call: task.Call, Status: domain.TaskSucceeded, Result: &domain.ToolResult{CallID: task.Call.ID, Parts: []domain.Part{{Kind: domain.PartText, Text: "result"}}}}
		wrong := req
		wrong.Caller.InvocationID = "other"
		if e = r.Resume(ctx, wrong, c, func(context.Context, agent.Update) error { t.Fatal("wrong branch emitted"); return nil }); e == nil {
			t.Fatal("accepted wrong branch")
		}
		failure := errors.New("commit response lost")
		if e = r.Resume(ctx, req, c, func(context.Context, agent.Update) error { return failure }); !errors.Is(e, failure) {
			t.Fatal(e)
		}
		if models.Load() != 1 {
			t.Fatal("model before atomic commit")
		}
		e = r.Resume(ctx, req, c, func(_ context.Context, u agent.Update) error {
			if u.AppliedTaskID != c.TaskID || u.Message[0].ToolCallID != c.Call.ID {
				t.Fatal("wrong result")
			}
			req.Checkpoint = u.Checkpoint
			req.Checkpoint.Version++
			return nil
		})
		if e != nil {
			t.Fatal(e)
		}
		if e = r.Resume(ctx, req, c, func(context.Context, agent.Update) error { t.Fatal("duplicate emitted"); return nil }); e == nil {
			t.Fatal("accepted duplicate")
		}
		r = makeRuntime()
		if index == 1 {
			if e = r.Recover(ctx, req, func(context.Context, agent.Update) error { t.Fatal("premature emit"); return nil }); !errors.Is(e, agent.ErrWaiting) {
				t.Fatal(e)
			}
		}
	}
	final := false
	e = r.Recover(ctx, req, func(_ context.Context, u agent.Update) error {
		if u.Kind == domain.EventMessageCompleted {
			final = true
			if u.Checkpoint.Version != req.Checkpoint.Version {
				t.Fatal("lost CAS version")
			}
		}
		return nil
	})
	if e != nil || !final || models.Load() != 2 || executions.Load() != 2 {
		t.Fatalf("err=%v final=%v model=%d executions=%d", e, final, models.Load(), executions.Load())
	}
}
