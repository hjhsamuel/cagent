package adk

import (
	"context"
	"encoding/json"
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

func TestRealRunnerImmediateAndDynamicTask(t *testing.T) {
	for _, task := range []bool{false, true} {
		t.Run(map[bool]string{false: "immediate", true: "task"}[task], func(t *testing.T) {
			req := request("user")
			var executed, models atomic.Int32
			catalog, e := tool.NewCatalog([]tool.Entry{{Scope: req.Run.Scope, ConnectionID: "connection", Descriptor: tool.Descriptor{Name: "echo", Protocol: domain.ToolLocal, InputSchema: []byte(`{"type":"object"}`)}, Executor: tool.ExecutorFunc(func(_ context.Context, c domain.ToolCall) (domain.ToolOutcome, error) {
				executed.Add(1)
				if c.Caller != req.Caller || c.Scope != req.Run.Scope || c.ID != "original" {
					t.Error("lost correlation")
				}
				if task {
					return domain.ToolOutcome{Task: &domain.TaskHandle{Protocol: c.Protocol, ConnectionID: "connection", RemoteID: "remote"}}, nil
				}
				return domain.ToolOutcome{Result: &domain.ToolResult{CallID: c.ID, Parts: []domain.Part{{Kind: domain.PartText, Text: "value"}}}}, nil
			})}}, tool.Limits{Timeout: time.Second, MaxInputBytes: 1024, MaxOutputBytes: 1024})
			if e != nil {
				t.Fatal(e)
			}
			m := modelFunc(func(_ context.Context, r *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
				return func(yield func(*model.LLMResponse, error) bool) {
					n := models.Add(1)
					if len(r.Config.Tools) != 1 {
						t.Error("missing declarations")
					}
					if n == 1 {
						yield(&model.LLMResponse{Content: &genai.Content{Role: "model", Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: "original", Name: "echo", Args: map[string]any{}}}}}}, nil)
					} else {
						if len(r.Contents) != 4 || r.Contents[3].Parts[0].FunctionResponse == nil {
							t.Error("lost tool history")
						}
						yield(finalResponse("done", 1, 1), nil)
					}
				}
			})
			runtime, e := New(m, countFunc(fixedCounter), budget(), ToolOptions{Registry: catalog, MaxModelCalls: 4})
			if e != nil {
				t.Fatal(e)
			}
			var updates []agent.Update
			e = runtime.Execute(context.Background(), req, func(_ context.Context, u agent.Update) error { updates = append(updates, u); return nil })
			if task {
				if !errors.Is(e, agent.ErrWaiting) || models.Load() != 1 {
					t.Fatal("not paused", e, models.Load())
				}
				u := updates[len(updates)-1]
				if u.Kind != domain.EventToolWaiting || len(u.Tasks) != 1 || u.Checkpoint.PendingCallIDs[0] != "original" {
					t.Fatal("missing handoff", u)
				}
				var cp pendingCheckpoint
				if json.Unmarshal(u.Checkpoint.Data, &cp) != nil || cp.Tools[0].Handle.RemoteID != "remote" || cp.Model != m.Name() || len(cp.Messages) != len(req.Messages) {
					t.Fatal("lost remote task")
				}
				for _, u := range updates {
					if u.MessageRole == domain.RoleTool || u.Kind == domain.EventMessageCompleted {
						t.Fatal("handle became final output")
					}
				}
			} else {
				if e != nil || models.Load() != 2 {
					t.Fatal(e, models.Load())
				}
				if len(updates) != 3 || updates[1].MessageRole != domain.RoleTool || updates[2].Checkpoint == nil {
					t.Fatal("missing durable pair", updates)
				}
			}
			if executed.Load() != 1 {
				t.Fatal("reexecuted")
			}
		})
	}
}

func TestToolErrorStopsSDKLoop(t *testing.T) {
	req := request("user")
	failure := errors.New("transport failed")
	catalog, _ := tool.NewCatalog([]tool.Entry{{Scope: req.Run.Scope, ConnectionID: "c", Descriptor: tool.Descriptor{Name: "fail", Protocol: domain.ToolLocal, InputSchema: []byte(`{"type":"object"}`)}, Executor: tool.ExecutorFunc(func(context.Context, domain.ToolCall) (domain.ToolOutcome, error) {
		return domain.ToolOutcome{}, failure
	})}}, tool.Limits{Timeout: time.Second, MaxInputBytes: 1024, MaxOutputBytes: 1024})
	var calls int
	m := modelFunc(func(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
		return func(yield func(*model.LLMResponse, error) bool) {
			calls++
			yield(&model.LLMResponse{Content: &genai.Content{Role: "model", Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: "id", Name: "fail", Args: map[string]any{}}}}}}, nil)
		}
	})
	runtime, _ := New(m, countFunc(fixedCounter), budget(), ToolOptions{Registry: catalog, MaxModelCalls: 3})
	e := runtime.Execute(context.Background(), req, func(context.Context, agent.Update) error { return nil })
	if !errors.Is(e, failure) || !errors.Is(e, agent.ErrUncertain) || calls != 1 {
		t.Fatal(e, calls)
	}
}

func TestModelToolAliasStableAndKeepsRemoteIdentity(t *testing.T) {
	name := "remote.agent 中文"
	alias := modelToolName(name)
	if alias == name || alias != modelToolName(name) || len(alias) > 64 {
		t.Fatal(alias)
	}
	if modelToolName("simple_tool") != "simple_tool" {
		t.Fatal("changed compatible name")
	}
	b := &bridge{desc: tool.Descriptor{Name: name, InputSchema: []byte(`{"type":"object"}`)}}
	if b.Declaration().Name != alias || b.desc.Name != name {
		t.Fatal("lost original name")
	}
}
