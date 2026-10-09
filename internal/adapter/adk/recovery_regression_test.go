package adk

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"strings"
	"testing"
	"time"

	"github.com/hjhsamuel/cagent/internal/agent"
	"github.com/hjhsamuel/cagent/internal/contextengine"
	"github.com/hjhsamuel/cagent/internal/domain"
	"github.com/hjhsamuel/cagent/internal/tool"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

func TestRegressionAsyncIntegerRoundTrip(t *testing.T) {
	req := request("user")
	catalog, e := tool.NewCatalog([]tool.Entry{{Scope: req.Run.Scope, ConnectionID: "c", Descriptor: tool.Descriptor{Name: "remote", Protocol: domain.ToolLocal, InputSchema: []byte(`{"type":"object"}`)}, Executor: tool.ExecutorFunc(func(_ context.Context, c domain.ToolCall) (domain.ToolOutcome, error) {
		if !strings.Contains(string(c.Arguments), "9007199254740993") {
			t.Fatal("original execution lost number")
		}
		return domain.ToolOutcome{Task: &domain.TaskHandle{Protocol: c.Protocol, ConnectionID: "c", RemoteID: c.ID}}, nil
	})}}, tool.Limits{Timeout: time.Second, MaxInputBytes: 1024, MaxOutputBytes: 4096})
	if e != nil {
		t.Fatal(e)
	}
	calls := 0
	var restoredArgs string
	m := modelFunc(func(_ context.Context, r *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
		return func(yield func(*model.LLMResponse, error) bool) {
			calls++
			if calls == 1 {
				yield(&model.LLMResponse{Content: &genai.Content{Role: "model", Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: "one", Name: "remote", Args: map[string]any{"n": json.Number("9007199254740993")}}}}}}, nil)
				return
			}
			for _, c := range r.Contents {
				for _, p := range c.Parts {
					if p.FunctionCall != nil {
						b, _ := json.Marshal(p.FunctionCall.Args)
						restoredArgs = string(b)
					}
				}
			}
			yield(finalResponse("done", 1, 1), nil)
		}
	})
	r, e := New(m, ToolOptions{Registry: catalog, MaxModelCalls: 4})
	if e != nil {
		t.Fatal(e)
	}
	var waiting agent.Update
	e = r.Execute(context.Background(), req, func(_ context.Context, u agent.Update) error {
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
	task := waiting.Tasks[0]
	e = r.Resume(context.Background(), req, agent.Continuation{TaskID: "task", Call: task.Call, Status: domain.TaskSucceeded, Result: &domain.ToolResult{CallID: task.Call.ID, Parts: []domain.Part{{Kind: domain.PartText, Text: "ok"}}}}, func(_ context.Context, u agent.Update) error {
		req.Checkpoint = u.Checkpoint
		req.Checkpoint.Version++
		return nil
	})
	if e != nil {
		t.Fatal(e)
	}
	if e = r.Recover(context.Background(), req, func(context.Context, agent.Update) error { return nil }); e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(restoredArgs, "9007199254740993") {
		t.Fatalf("restored arguments changed: %s", restoredArgs)
	}
}

func TestRegressionCancelledToolHistory(t *testing.T) {
	req := request("user")
	req.Run.ID = "new"
	req.Caller.InvocationID = "new"
	s := domain.Session{Scope: req.Run.Scope, ID: req.Run.SessionID, AgentID: "agent", ActiveRunID: "new", Version: 1}
	msg := func(id, run string, seq int64, role domain.Role, parts []domain.Part) domain.Message {
		return domain.Message{Scope: s.Scope, ID: id, SessionID: s.ID, RunID: run, Sequence: seq, Role: role, Parts: parts}
	}
	history := []domain.Message{
		msg("old-input", "old", 1, domain.RoleUser, []domain.Part{{Kind: domain.PartText, Text: "start task"}}),
		msg("old-call", "old", 2, domain.RoleAssistant, []domain.Part{{Kind: domain.PartToolCall, ToolCallID: "one", ToolName: "remote", Data: []byte(`{}`)}}),
		msg("new-input", "new", 3, domain.RoleUser, []domain.Part{{Kind: domain.PartText, Text: "new question"}}),
	}
	builder := contextengine.New()
	prepared, e := builder.Prepare(context.Background(), contextengine.Input{Session: s, RunID: "new", PolicyVersion: "v1", History: history, RunStates: map[string]domain.RunStatus{"old": domain.RunCancelled}})
	if e != nil {
		t.Fatal(e)
	}
	mapped, e := mapMessages(prepared.Messages, "test-model")
	if e != nil {
		t.Fatal(e)
	}
	wire, e := wireMessages(mapped)
	if e != nil {
		t.Fatal(e)
	}
	data, _ := json.Marshal(wire)
	if strings.Contains(string(data), "tool_calls") && !strings.Contains(string(data), "tool_call_id") {
		t.Fatalf("new run sends unresolved historical tool call: %s", data)
	}
}

func TestRegressionMixedTaskAndFailure(t *testing.T) {
	req := request("user")
	catalog, _ := tool.NewCatalog([]tool.Entry{{Scope: req.Run.Scope, ConnectionID: "c", Descriptor: tool.Descriptor{Name: "remote", Protocol: domain.ToolLocal, InputSchema: []byte(`{"type":"object"}`)}, Executor: tool.ExecutorFunc(func(_ context.Context, c domain.ToolCall) (domain.ToolOutcome, error) {
		if c.ID == "bad" {
			return domain.ToolOutcome{}, errors.New("remote launch response lost")
		}
		return domain.ToolOutcome{Task: &domain.TaskHandle{Protocol: c.Protocol, ConnectionID: "c", RemoteID: c.ID}}, nil
	})}}, tool.Limits{Timeout: time.Second, MaxInputBytes: 1024, MaxOutputBytes: 1024})
	m := modelFunc(func(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
		return func(yield func(*model.LLMResponse, error) bool) {
			yield(&model.LLMResponse{Content: &genai.Content{Role: "model", Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: "good", Name: "remote", Args: map[string]any{}}}, {FunctionCall: &genai.FunctionCall{ID: "bad", Name: "remote", Args: map[string]any{}}}}}}, nil)
		}
	})
	r, _ := New(m, ToolOptions{Registry: catalog, MaxModelCalls: 4})
	tracked := 0
	var checkpoint *domain.Checkpoint
	e := r.Execute(context.Background(), req, func(_ context.Context, u agent.Update) error {
		tracked += len(u.Tasks)
		if u.Checkpoint != nil {
			checkpoint = u.Checkpoint
		}
		return nil
	})
	if tracked != 1 {
		t.Fatalf("one remote task started but handoff count is %d; execution error: %v", tracked, e)
	}
	if !errors.Is(e, agent.ErrUncertain) {
		t.Fatalf("uncertain launch outcome lost when another call returns a task: %v", e)
	}
	if checkpoint == nil || !errors.Is(CheckpointFailure(*checkpoint), agent.ErrUncertain) {
		t.Fatal("failure absent from durable boundary")
	}
	req.Checkpoint = checkpoint
	checkpoint.Version = 1
	if e = r.Recover(context.Background(), req, func(context.Context, agent.Update) error { t.Fatal("failed checkpoint replayed output"); return nil }); !errors.Is(e, agent.ErrUncertain) {
		t.Fatal("restart lost failure", e)
	}
}
