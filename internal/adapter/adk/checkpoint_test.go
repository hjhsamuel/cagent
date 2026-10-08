package adk

import (
	"context"
	"encoding/json"
	"iter"
	"sync/atomic"
	"testing"

	sdkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"
)

// 这不是自写的恢复模拟：调用真实 SDK 的长工具暂停、事件 JSON 往返、新 SessionService
// 与新 Runner 的续接。只有模型返回内容和远端工具提供方用替身，不假装验证 P9 事务。
func TestSDKPauseSerializeRestoreAndResume(t *testing.T) {
	ctx := context.Background()
	svc := session.InMemoryService()
	_, err := svc.Create(ctx, &session.CreateRequest{AppName: "probe", UserID: "u", SessionID: "s"})
	if err != nil {
		t.Fatal(err)
	}
	var modelCalls, toolCalls atomic.Int32
	m := modelFunc(func(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
		return func(yield func(*model.LLMResponse, error) bool) {
			switch modelCalls.Add(1) {
			case 1:
				yield(&model.LLMResponse{Content: &genai.Content{Role: "model", Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: "original", Name: "remote_task", Args: map[string]any{}}}}}}, nil)
			case 2:
				yield(finalResponse("waiting", 1, 1), nil)
			default:
				yield(finalResponse("resumed", 1, 1), nil)
			}
		}
	})
	remote, err := functiontool.New(functiontool.Config{Name: "remote_task", Description: "already running remote task", IsLongRunning: true}, func(sdkagent.Context, struct{}) (map[string]string, error) {
		toolCalls.Add(1)
		return map[string]string{"status": "pending"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	newRunner := func(svc session.Service) *runner.Runner {
		t.Helper()
		a, e := llmagent.New(llmagent.Config{Name: "agent", Model: m, Tools: []tool.Tool{remote}})
		if e != nil {
			t.Fatal(e)
		}
		r, e := runner.New(runner.Config{AppName: "probe", Agent: a, SessionService: svc})
		if e != nil {
			t.Fatal(e)
		}
		return r
	}
	first := newRunner(svc)
	var callID string
	for event, e := range first.Run(ctx, "u", "s", genai.NewContentFromText("start", "user"), sdkagent.RunConfig{}) {
		if e != nil {
			t.Fatal(e)
		}
		if event != nil && len(event.LongRunningToolIDs) > 0 {
			callID = event.LongRunningToolIDs[0]
		}
	}
	if callID == "" || toolCalls.Load() != 1 || modelCalls.Load() != 2 {
		t.Fatalf("did not pause: %q %d %d", callID, toolCalls.Load(), modelCalls.Load())
	}
	saved, e := svc.Get(ctx, &session.GetRequest{AppName: "probe", UserID: "u", SessionID: "s"})
	if e != nil {
		t.Fatal(e)
	}
	snap, e := snapshotSDK(saved.Session)
	if e != nil {
		t.Fatal(e)
	}
	encoded, e := json.Marshal(snap)
	if e != nil {
		t.Fatal(e)
	}
	var decoded sdkSnapshot
	if e = json.Unmarshal(encoded, &decoded); e != nil {
		t.Fatal(e)
	}
	restored, _, e := restoreSDK(ctx, decoded, "probe", "u", "s")
	if e != nil {
		t.Fatal(e)
	}
	second := newRunner(restored)
	reply := &genai.Content{Role: "user", Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{ID: callID, Name: "remote_task", Response: map[string]any{"output": "done"}}}}}
	found := false
	for event, e := range second.Run(ctx, "u", "s", reply, sdkagent.RunConfig{}) {
		if e != nil {
			t.Fatal(e)
		}
		if event != nil && event.Content != nil {
			for _, part := range event.Content.Parts {
				if part.Text == "resumed" {
					found = true
				}
			}
		}
	}
	if !found || toolCalls.Load() != 1 || modelCalls.Load() != 3 {
		t.Fatalf("bad resume: final=%v model=%d tool=%d", found, modelCalls.Load(), toolCalls.Load())
	}
}
