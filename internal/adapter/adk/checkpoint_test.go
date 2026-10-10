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
	"github.com/hjhsamuel/cagent/internal/apperrors"
	"github.com/hjhsamuel/cagent/internal/domain"
	"github.com/hjhsamuel/cagent/internal/tool"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// 手工构造应用记录，完全没有 ADK Session/事件/工作流状态。
func nativePendingRequest(t *testing.T) agent.Request {
	t.Helper()
	req := request("user")
	call := domain.ToolCall{Scope: req.Run.Scope, ID: "original", SessionID: req.Run.SessionID, RunID: req.Run.ID, Caller: req.Caller, Protocol: domain.ToolLocal, Name: "remote", Arguments: []byte(`{"n":9007199254740993}`)}
	messages := append([]domain.Message{}, req.Messages...)
	messages = append(messages, interaction(req, domain.RoleAssistant, []domain.Part{{Kind: domain.PartToolCall, ToolCallID: call.ID, ToolName: call.Name, Data: call.Arguments}}))
	data, e := json.Marshal(pendingCheckpoint{Model: "test-model", Scope: req.Run.Scope, RunID: req.Run.ID, Caller: req.Caller, ModelCalls: 1, Messages: messages, Tools: PendingTools{{Call: call, Handle: domain.TaskHandle{Protocol: call.Protocol, ConnectionID: "connection", RemoteID: "existing-task"}}}})
	if e != nil {
		t.Fatal(e)
	}
	req.Messages = nil
	req.Checkpoint = &domain.Checkpoint{Scope: req.Run.Scope, RunID: req.Run.ID, Caller: req.Caller, Format: PendingCheckpointFormat, Data: data, PendingCallIDs: []string{call.ID}, Version: 1}
	return req
}

func TestPendingCheckpointRejectsInconsistentRecords(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*pendingCheckpoint)
	}{
		{"foreign-message", func(p *pendingCheckpoint) { p.Messages[0].Scope.UserID = "foreign" }},
		{"wrong-arguments", func(p *pendingCheckpoint) { p.Tools[0].Call.Arguments = []byte(`{"n":1}`) }},
		{"wrong-caller", func(p *pendingCheckpoint) { p.Tools[0].Call.Caller.InvocationID = "other" }},
		{"missing-call", func(p *pendingCheckpoint) { p.Messages = p.Messages[:len(p.Messages)-1] }},
		{"duplicate-call", func(p *pendingCheckpoint) { p.Messages = append(p.Messages, p.Messages[len(p.Messages)-1]) }},
		{"missing-tool", func(p *pendingCheckpoint) { p.Tools = nil }},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := nativePendingRequest(t)
			var saved pendingCheckpoint
			if e := decodeJSON(req.Checkpoint.Data, &saved); e != nil {
				t.Fatal(e)
			}
			test.change(&saved)
			req.Checkpoint.Data, _ = json.Marshal(saved)
			r := runtimeFor(t, modelFunc(func(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
				t.Fatal("invalid records reached model")
				return nil
			}))
			if e := r.Recover(context.Background(), req, func(context.Context, agent.Update) error { t.Fatal("invalid records emitted"); return nil }); !errors.Is(e, apperrors.ErrInvalidArgument) {
				t.Fatal(e)
			}
		})
	}
}

func TestLegacyPendingUsesLLMMessagesWithoutSDKState(t *testing.T) {
	req := nativePendingRequest(t)
	var saved pendingCheckpoint
	if e := decodeJSON(req.Checkpoint.Data, &saved); e != nil {
		t.Fatal(e)
	}
	initial := saved.Messages[:len(saved.Messages)-1]
	mapped, e := mapMessages(initial, saved.Model)
	if e != nil {
		t.Fatal(e)
	}
	mapped.Contents = append(mapped.Contents,
		&genai.Content{Role: "model", Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: "original", Name: "remote", Args: map[string]any{"n": json.Number("9007199254740993")}}}}},
		&genai.Content{Role: "user", Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{ID: "original", Name: "remote"}}}},
	)
	legacy := map[string]any{"Model": saved.Model, "ModelCalls": saved.ModelCalls, "Messages": initial, "Scope": saved.Scope, "RunID": saved.RunID, "Caller": saved.Caller, "Tools": saved.Tools, "Contents": mapped.Contents, "SDK": "invalid SDK state"}
	req.Checkpoint.Data, _ = json.Marshal(legacy)
	req.Checkpoint.Format = legacyPendingCheckpointFormat
	r := runtimeFor(t, modelFunc(func(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
		t.Fatal("migration called model")
		return nil
	}))
	tasks, e := r.PendingTasks(req)
	if e != nil || len(tasks) != 1 {
		t.Fatal(tasks, e)
	}
	if e = r.Resume(context.Background(), req, agent.Continuation{TaskID: "task", Call: tasks[0].Call, Status: domain.TaskSucceeded, Result: &domain.ToolResult{CallID: "original", Parts: []domain.Part{{Kind: domain.PartText, Text: "done"}}}}, func(_ context.Context, u agent.Update) error {
		if u.Checkpoint.Format != PendingCheckpointFormat || strings.Contains(string(u.Checkpoint.Data), "SDK") {
			t.Fatal("migration kept SDK dependency")
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
}

func TestPartialTrackingCannotAdvanceUnacceptedTool(t *testing.T) {
	req := nativePendingRequest(t)
	req.Checkpoint.PendingCallIDs = nil
	r := runtimeFor(t, modelFunc(func(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
		t.Fatal("advanced incomplete handoff")
		return nil
	}))
	if e := r.Recover(context.Background(), req, func(context.Context, agent.Update) error { t.Fatal("premature output"); return nil }); !errors.Is(e, agent.ErrWaiting) {
		t.Fatal(e)
	}
	tasks, e := r.PendingTasks(req)
	if e != nil || len(tasks) != 1 {
		t.Fatal(tasks, e)
	}
}

func TestRecoverFromApplicationMessagesAndTaskRecords(t *testing.T) {
	req := nativePendingRequest(t)
	modelCalls := 0
	catalog, e := tool.NewCatalog([]tool.Entry{{Scope: req.Run.Scope, ConnectionID: "connection", Descriptor: tool.Descriptor{Name: "remote", Protocol: domain.ToolLocal, InputSchema: []byte(`{"type":"object"}`)}, Executor: tool.ExecutorFunc(func(context.Context, domain.ToolCall) (domain.ToolOutcome, error) {
		t.Fatal("reexecuted existing tool")
		return domain.ToolOutcome{}, nil
	})}}, tool.Limits{Timeout: time.Second, MaxInputBytes: 1024, MaxOutputBytes: 1024})
	if e != nil {
		t.Fatal(e)
	}
	r, e := New(modelFunc(func(_ context.Context, input *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
		return func(yield func(*model.LLMResponse, error) bool) {
			modelCalls++
			if input.Config.SystemInstruction.Parts[0].Text != "rules" || len(input.Contents) != 4 {
				t.Fatalf("lost context: %+v", input)
			}
			call := input.Contents[2].Parts[0].FunctionCall
			result := input.Contents[3].Parts[0].FunctionResponse
			if call.ID != "original" || result.ID != call.ID || call.Args["n"] != json.Number("9007199254740993") {
				t.Fatal("lost tool correlation or integer precision")
			}
			yield(finalResponse("continued", 1, 1), nil)
		}
	}), ToolOptions{Registry: catalog, MaxModelCalls: 4})
	if e != nil {
		t.Fatal(e)
	}
	if e := r.Recover(context.Background(), req, func(context.Context, agent.Update) error { t.Fatal("generated while waiting"); return nil }); !errors.Is(e, agent.ErrWaiting) {
		t.Fatal(e)
	}
	tasks, e := r.PendingTasks(req)
	if e != nil || len(tasks) != 1 || tasks[0].Handle.RemoteID != "existing-task" {
		t.Fatal(tasks, e)
	}
	continuation := agent.Continuation{TaskID: "persisted-task", Call: tasks[0].Call, Status: domain.TaskSucceeded, Result: &domain.ToolResult{CallID: tasks[0].Call.ID, Parts: []domain.Part{{Kind: domain.PartText, Text: "result"}}}}
	if e = r.Resume(context.Background(), req, continuation, func(_ context.Context, u agent.Update) error {
		req.Checkpoint = u.Checkpoint
		req.Checkpoint.Version++
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	if modelCalls != 0 {
		t.Fatal("Resume called model")
	}
	for _, forbidden := range []string{`"SDK"`, `"sdk"`, `"Contents"`, `"Results"`, "workflow"} {
		if strings.Contains(string(req.Checkpoint.Data), forbidden) {
			t.Fatalf("SDK data in checkpoint: %s", forbidden)
		}
	}
	// 第一次调用已经用掉一次限制，不能因重启重置。
	r.toolOptions.MaxModelCalls = 1
	if e = r.Recover(context.Background(), req, func(context.Context, agent.Update) error { return nil }); e == nil || modelCalls != 0 {
		t.Fatal("reset model call budget", e)
	}
	r.toolOptions.MaxModelCalls = 4
	var completed *domain.Checkpoint
	if e = r.Recover(context.Background(), req, func(_ context.Context, u agent.Update) error { completed = u.Checkpoint; return nil }); e != nil || completed == nil || modelCalls != 1 {
		t.Fatal(completed, e)
	}
	if strings.Contains(string(completed.Data), "SDK") || strings.Contains(string(completed.Data), "final_event_id") {
		t.Fatal("SDK completion dependency")
	}
	completed.Version++
	req.Checkpoint = completed
	if e = r.Recover(context.Background(), req, func(context.Context, agent.Update) error { t.Fatal("replayed completed output"); return nil }); e != nil || modelCalls != 1 {
		t.Fatal(e)
	}
}
