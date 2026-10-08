package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hjhsamuel/cagent/internal/adapter/adk"
	"github.com/hjhsamuel/cagent/internal/domain"
	"github.com/hjhsamuel/cagent/internal/store"
	"github.com/hjhsamuel/cagent/internal/tool"
)

// 真实 MongoDB 副本集 + OpenAI HTTP SDK + ADK Runner：不是运行时替身。
// 即时结果验证跨轮历史；双任务验证数据库 TrackTask 检查点集合约束和会话占用。
func TestToolsThroughModelSDKAndDurableTransactions(t *testing.T) {
	for _, pending := range []bool{false, true} {
		t.Run(fmt.Sprint("pending=", pending), func(t *testing.T) {
			db, _ := testDatabase(t)
			var calls, executions atomic.Int32
			model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Tools    []json.RawMessage `json:"tools"`
					Messages []struct {
						Role       string `json:"role"`
						ToolCallID string `json:"tool_call_id"`
					} `json:"messages"`
				}
				if e := json.NewDecoder(r.Body).Decode(&body); e != nil {
					t.Error(e)
				}
				if len(body.Tools) != 1 {
					t.Error("missing tool schema")
				}
				w.Header().Set("Content-Type", "application/json")
				if calls.Add(1) == 1 {
					count := 1
					if pending {
						count = 2
					}
					var toolCalls []any
					for i := 0; i < count; i++ {
						toolCalls = append(toolCalls, map[string]any{"id": fmt.Sprint("call", i), "type": "function", "function": map[string]any{"name": "echo", "arguments": "{}"}})
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"index": 0, "finish_reason": "tool_calls", "message": map[string]any{"role": "assistant", "tool_calls": toolCalls}}}})
				} else {
					if body.Messages[len(body.Messages)-1].Role != "tool" || body.Messages[len(body.Messages)-1].ToolCallID != "call0" {
						t.Error("lost tool wire history")
					}
					fmt.Fprint(w, `{"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"done"}}]}`)
				}
			}))
			defer model.Close()
			catalog, e := tool.NewCatalog([]tool.Entry{{Scope: scope, ConnectionID: "local", Descriptor: tool.Descriptor{Name: "echo", Protocol: domain.ToolLocal, InputSchema: []byte(`{"type":"object"}`)}, Executor: tool.ExecutorFunc(func(_ context.Context, c domain.ToolCall) (domain.ToolOutcome, error) {
				executions.Add(1)
				if pending {
					return domain.ToolOutcome{Task: &domain.TaskHandle{Protocol: domain.ToolLocal, ConnectionID: "local", RemoteID: "remote-" + c.ID}}, nil
				}
				return domain.ToolOutcome{Result: &domain.ToolResult{CallID: c.ID, Parts: []domain.Part{{Kind: domain.PartText, Text: "echo"}}}}, nil
			})}}, tool.Limits{Timeout: time.Second, MaxInputBytes: 1024, MaxOutputBytes: 1024})
			if e != nil {
				t.Fatal(e)
			}
			a, e := NewOpenAIService(context.Background(), db, adkConfig(model.URL), nil, Options{LeaseDuration: 3 * time.Second, PollInterval: 30 * time.Millisecond}, adk.ToolOptions{Registry: catalog, MaxModelCalls: 4})
			if e != nil {
				t.Fatal(e)
			}
			t.Cleanup(func() { a.Close(context.Background()) })
			s := session(t, a)
			run := start(t, a, s, "tool-key")
			want := domain.RunCompleted
			if pending {
				want = domain.RunWaiting
			}
			awaitStatus(t, db, run, want)
			messages, e := db.ListMessages(context.Background(), scope, s.ID, store.SequencePage{Limit: 20})
			if e != nil {
				t.Fatal(e)
			}
			cp, e := db.GetCheckpoint(context.Background(), scope, run.ID, run.ID)
			if e != nil {
				t.Fatal(e)
			}
			if pending {
				if calls.Load() != 1 || executions.Load() != 2 || len(messages.Items) != 2 || len(cp.PendingCallIDs) != 2 || cp.Version != 2 || cp.Format != adk.PendingCheckpointFormat {
					t.Fatalf("pending mismatch: calls=%d executions=%d messages=%d cp=%+v", calls.Load(), executions.Load(), len(messages.Items), cp)
				}
				events, e := db.ListEvents(context.Background(), scope, run.ID, store.SequencePage{Limit: 20})
				if e != nil {
					t.Fatal(e)
				}
				tracked := 0
				for _, ev := range events.Items {
					if ev.Kind == domain.EventRunCompleted || ev.Kind == domain.EventRunFailed {
						t.Fatal("terminal while pending")
					}
					if ev.Kind == domain.EventToolWaiting {
						var data map[string]string
						_ = json.Unmarshal(ev.Data, &data)
						task, e := db.GetTask(context.Background(), scope, data["task_id"])
						if e != nil || task.Call.ID != data["tool_call_id"] || task.Handle.RemoteID != "remote-"+task.Call.ID {
							t.Fatal(task, e)
						}
						tracked++
					}
				}
				if tracked != 2 {
					t.Fatal("tasks not tracked")
				}
				saved, _ := db.GetSession(context.Background(), scope, s.ID)
				if saved.ActiveRunID != run.ID {
					t.Fatal("released active session")
				}
			} else {
				if calls.Load() != 2 || executions.Load() != 1 || len(messages.Items) != 4 || messages.Items[1].Parts[0].Kind != domain.PartToolCall || messages.Items[2].Role != domain.RoleTool || messages.Items[2].Parts[0].ToolCallID != "call0" || messages.Items[3].Parts[0].Text != "done" {
					t.Fatal("broken pair", messages, calls.Load(), executions.Load())
				}
			}
		})
	}
}
