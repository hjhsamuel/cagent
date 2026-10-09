package adk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	appagent "github.com/hjhsamuel/cagent/internal/agent"
	"github.com/hjhsamuel/cagent/internal/apperrors"
	"github.com/hjhsamuel/cagent/internal/domain"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

type modelFunc func(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error]

func (f modelFunc) Name() string { return "test-model" }
func (f modelFunc) GenerateContent(c context.Context, r *model.LLMRequest, s bool) iter.Seq2[*model.LLMResponse, error] {
	return f(c, r, s)
}

func request(user string) appagent.Request {
	scope := domain.Scope{TenantID: "tenant", UserID: user}
	run := domain.Run{Scope: scope, ID: "run", SessionID: "session", Status: domain.RunRunning, Version: 2}
	m := func(id string, role domain.Role, text string) domain.Message {
		return domain.Message{Scope: scope, ID: id, SessionID: run.SessionID, RunID: run.ID, Role: role, Parts: []domain.Part{{Kind: domain.PartText, Text: text}}}
	}
	return appagent.Request{Run: run, Caller: domain.AgentExecution{AgentID: "configured-agent", InvocationID: "inv", ParentInvocationID: "parent"}, Messages: []domain.Message{m("system", domain.RoleSystem, "rules"), m("old", domain.RoleAssistant, "history"), m("input", domain.RoleUser, user)}}
}
func runtimeFor(t *testing.T, m model.LLM) *Runtime {
	t.Helper()
	r, e := New(m)
	if e != nil {
		t.Fatal(e)
	}
	return r
}

// 经过真实 ADK Runner/LLMAgent，验证 callback 替换后的系统/历史恰好一次，增量不写历史。
func TestExecuteFinalCheckpointAndIdentity(t *testing.T) {
	req := request("user")
	var calls atomic.Int32
	m := modelFunc(func(ctx context.Context, r *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
		return func(yield func(*model.LLMResponse, error) bool) {
			calls.Add(1)
			if !stream {
				t.Error("streaming disabled")
			}
			if len(r.Contents) != 2 || r.Config.SystemInstruction.Parts[0].Text != "rules" || r.Contents[1].Parts[0].Text != "user" || r.Config.MaxOutputTokens != 0 {
				t.Error("history/config mismatch")
			}
			for _, text := range []string{"你", "好"} {
				if !yield(&model.LLMResponse{Content: genai.NewContentFromText(text, "model"), Partial: true}, nil) {
					return
				}
			}
			yield(finalResponse("你好", 11, 2), nil)
		}
	})
	runtime := runtimeFor(t, m)
	var updates []appagent.Update
	if e := runtime.Execute(context.Background(), req, func(_ context.Context, u appagent.Update) error { updates = append(updates, u); return nil }); e != nil {
		t.Fatal(e)
	}
	if len(updates) != 3 || updates[0].Message != nil || updates[1].Message != nil || updates[2].Kind != domain.EventMessageCompleted || updates[2].Message[0].Text != "你好" {
		t.Fatalf("updates: %+v", updates)
	}
	for _, u := range updates {
		var data OutputData
		if e := json.Unmarshal(u.Data, &data); e != nil {
			t.Fatal(e)
		}
		if data.AgentID != req.Caller.AgentID || data.InvocationID != req.Caller.InvocationID || data.ParentInvocationID != "parent" || data.SDKInvocationID == "" {
			t.Fatal("lost caller identity")
		}
	}
	cp := updates[2].Checkpoint
	if cp == nil || cp.Format != CheckpointFormat || cp.Caller != req.Caller || cp.Version != 0 {
		t.Fatal("missing checkpoint")
	}
	cp.Version = 1
	req.Checkpoint = cp
	if e := runtime.Recover(context.Background(), req, func(context.Context, appagent.Update) error { t.Error("replayed durable output"); return nil }); e != nil {
		t.Fatal(e)
	}
	if calls.Load() != 1 {
		t.Fatal("recovery called model")
	}
	if e := runtime.Execute(context.Background(), req, func(context.Context, appagent.Update) error { return nil }); !errors.Is(e, apperrors.ErrUnsupported) {
		t.Fatal(e)
	}
	copyCP := *cp
	copyCP.Format = "unknown"
	req.Checkpoint = &copyCP
	if e := runtime.Recover(context.Background(), req, nil); !errors.Is(e, apperrors.ErrUnsupported) {
		t.Fatal(e)
	}
	copyCP = *cp
	copyCP.Caller.InvocationID = "wrong"
	req.Checkpoint = &copyCP
	if e := runtime.Recover(context.Background(), req, nil); !errors.Is(e, apperrors.ErrInvalidArgument) {
		t.Fatal(e)
	}
}

func TestFailureCancellationAndEmitBackpressure(t *testing.T) {
	sentinel := errors.New("stop emission")
	ended := make(chan struct{})
	m := modelFunc(func(ctx context.Context, r *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
		return func(yield func(*model.LLMResponse, error) bool) {
			defer close(ended)
			for i := 0; i < 100; i++ {
				if !yield(&model.LLMResponse{Content: genai.NewContentFromText("x", "model"), Partial: true}, nil) {
					return
				}
			}
		}
	})
	runtime := runtimeFor(t, m)
	if e := runtime.Execute(context.Background(), request("u"), func(context.Context, appagent.Update) error { return sentinel }); !errors.Is(e, sentinel) {
		t.Fatal(e)
	}
	select {
	case <-ended:
	case <-time.After(time.Second):
		t.Fatal("SDK producer leaked")
	}
	for name, response := range map[string]*model.LLMResponse{"empty": nil, "truncated": {Content: genai.NewContentFromText("partial", "model"), FinishReason: genai.FinishReasonMaxTokens}, "tool": {Content: &genai.Content{Role: "model", Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{Name: "unknown", ID: "call"}}}}}} {
		t.Run(name, func(t *testing.T) {
			r := runtimeFor(t, modelFunc(func(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
				return func(yield func(*model.LLMResponse, error) bool) {
					if response != nil {
						yield(response, nil)
					}
				}
			}))
			if e := r.Execute(context.Background(), request("u"), func(context.Context, appagent.Update) error { t.Error("invalid final emitted"); return nil }); e == nil {
				t.Fatal("false success")
			}
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	r := runtimeFor(t, modelFunc(func(c context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
		return func(yield func(*model.LLMResponse, error) bool) { <-c.Done(); yield(nil, c.Err()) }
	}))
	if e := r.Execute(ctx, request("u"), func(context.Context, appagent.Update) error { return nil }); !errors.Is(e, context.DeadlineExceeded) {
		t.Fatal(e)
	}
}

func TestScopeAndConcurrentRuns(t *testing.T) {
	var calls atomic.Int32
	m := modelFunc(func(c context.Context, r *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
		return func(yield func(*model.LLMResponse, error) bool) {
			calls.Add(1)
			yield(finalResponse(r.Contents[len(r.Contents)-1].Parts[0].Text, 1, 1), nil)
		}
	})
	r := runtimeFor(t, m)
	req := request("u")
	req.Messages[1].Scope.UserID = "foreign"
	if e := r.Execute(context.Background(), req, func(context.Context, appagent.Update) error { return nil }); !errors.Is(e, apperrors.ErrInvalidArgument) {
		t.Fatal(e)
	}
	if calls.Load() != 0 {
		t.Fatal("unsafe model call")
	}
	r = runtimeFor(t, m)
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			user := fmt.Sprint(i)
			if e := r.Execute(context.Background(), request(user), func(_ context.Context, u appagent.Update) error {
				if u.Message[0].Text != user || u.Checkpoint.Scope.UserID != user {
					t.Error("cross-user state")
				}
				return nil
			}); e != nil {
				t.Error(e)
			}
		}(i)
	}
	wg.Wait()
	if calls.Load() != 12 {
		t.Fatal("lost execution")
	}
}

func TestStructuredMappingAndSafeErrors(t *testing.T) {
	req := request("u")
	call := req.Messages[1]
	call.Parts = []domain.Part{{Kind: domain.PartToolCall, ToolCallID: "a", ToolName: "lookup", Data: []byte(`{"n":9007199254740993}`)}}
	result := call
	result.Role = domain.RoleTool
	result.Parts = []domain.Part{{Kind: domain.PartToolResult, ToolCallID: "a", Text: "untrusted"}}
	req.Messages = []domain.Message{req.Messages[0], call, result, req.Messages[2]}
	mapped, e := mapMessages(req.Messages, "test")
	if e != nil {
		t.Fatal(e)
	}
	wire, e := wireMessages(mapped)
	if e != nil {
		t.Fatal(e)
	}
	data, e := json.Marshal(wire)
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(string(data), "9007199254740993") || !strings.Contains(string(data), `"role":"tool"`) {
		t.Fatalf("mapping lost semantics: %s", data)
	}
	// 继续走真实 Runner 的 BeforeModel 深拷贝，防止只测初始映射而漏掉整数舍入。
	m := modelFunc(func(_ context.Context, actual *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
		return func(yield func(*model.LLMResponse, error) bool) {
			wire, err := wireMessages(actual)
			if err != nil {
				t.Error(err)
			}
			data, _ := json.Marshal(wire)
			if !strings.Contains(string(data), "9007199254740993") {
				t.Error("SDK callback rounded tool argument")
			}
			yield(finalResponse("ok", 1, 1), nil)
		}
	})
	if err := runtimeFor(t, m).Execute(context.Background(), req, func(context.Context, appagent.Update) error { return nil }); err != nil {
		t.Fatal(err)
	}
	req.Messages[0].Parts[0].URI = "file://unsupported"
	if _, e = mapMessages(req.Messages, "test"); !errors.Is(e, apperrors.ErrUnsupported) {
		t.Fatal(e)
	}
	cause := errors.New("secret credential and prompt")
	e = safeError(cause)
	for _, format := range []string{"%v", "%+v", "%#v", "%q"} {
		if strings.Contains(fmt.Sprintf(format, e), "secret") {
			t.Fatal("unsafe error")
		}
	}
	if !errors.Is(e, cause) || !errors.Is(e, ErrModel) {
		t.Fatal("lost cause")
	}
}
