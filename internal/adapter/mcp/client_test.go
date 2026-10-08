package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hjhsamuel/cagent/internal/adapter/toolhttp"
	"github.com/hjhsamuel/cagent/internal/apperrors"
	"github.com/hjhsamuel/cagent/internal/domain"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func original() domain.ToolCall {
	return domain.ToolCall{Scope: domain.Scope{TenantID: "t", UserID: "u"}, SessionID: "s", RunID: "r", ID: "call", Caller: domain.AgentExecution{AgentID: "a", InvocationID: "i"}, Protocol: domain.ToolMCP, Name: "sync", Arguments: []byte(`{"x":1}`)}
}
func TestOfficialSDKDiscoveryCallAndUnsupportedTasks(t *testing.T) {
	var starts, deleted atomic.Int32
	provider := sdk.NewServer(&sdk.Implementation{Name: "test", Version: "1"}, nil)
	provider.AddTool(&sdk.Tool{Name: "sync", InputSchema: json.RawMessage(`{"type":"object"}`)}, func(_ context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		starts.Add(1)
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "taskId=not-a-handle"}, &sdk.ResourceLink{URI: "https://example.com/result", Name: "result"}}, StructuredContent: map[string]any{"taskId": "still-not-a-handle"}, IsError: true}, nil
	})
	handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return provider }, nil)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer scoped-secret" {
			t.Error("lost credentials")
		}
		if r.Method == "DELETE" {
			deleted.Add(1)
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	c, err := New(context.Background(), toolhttp.Config{Scope: original().Scope, ID: "conn", URL: server.URL, Timeout: time.Second, MaxBytes: 1 << 20, CredentialRef: "ref", Credentials: func(_ context.Context, s domain.Scope, ref string) (string, error) {
		if s != original().Scope || ref != "ref" {
			t.Error("wrong lookup")
		}
		return "Bearer scoped-secret", nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(context.Background())
	list, err := c.Discover(context.Background())
	if err != nil || len(list) != 1 {
		t.Fatal(list, err)
	}
	out, err := c.Execute(context.Background(), original())
	if err != nil || out.Task != nil || out.Result == nil || out.Result.Error == "" || len(out.Result.Parts) != 3 {
		t.Fatal(out, err)
	}
	h := domain.TaskHandle{Protocol: domain.ToolMCP, ConnectionID: "conn", RemoteID: "old-task", ContextID: "old-session"}
	if _, err = c.Get(context.Background(), original(), h); !errors.Is(err, apperrors.ErrUnsupported) {
		t.Fatal(err)
	}
	if err = c.Cancel(context.Background(), original(), h); !errors.Is(err, apperrors.ErrUnsupported) {
		t.Fatal(err)
	}
	if err = c.Follow(context.Background(), original(), h, "", func(domain.TaskUpdate) error { return nil }); !errors.Is(err, apperrors.ErrUnsupported) {
		t.Fatal(err)
	}
	wrong := original()
	wrong.Scope.UserID = "other"
	if _, err = c.Execute(context.Background(), wrong); !errors.Is(err, apperrors.ErrNotFound) {
		t.Fatal(err)
	}
	if _, err = c.Get(context.Background(), wrong, h); !errors.Is(err, apperrors.ErrNotFound) {
		t.Fatal(err)
	}
	if starts.Load() != 1 {
		t.Fatal("reexecuted task")
	}
	if err = c.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = c.Close(context.Background())
	if deleted.Load() != 1 {
		t.Fatal("session not closed exactly once", deleted.Load())
	}
	if _, err = c.Discover(context.Background()); !errors.Is(err, apperrors.ErrConflict) {
		t.Fatal(err)
	}
}

// SDK 不提供 tasks；即便远端声明 tasks，也不发送 task 参数、不把 task-only 响应当作成功。
func TestTaskOnlyResultIsRejected(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "DELETE" {
			w.WriteHeader(204)
			return
		}
		var req struct {
			ID     json.RawMessage            `json:"id"`
			Method string                     `json:"method"`
			Params map[string]json.RawMessage `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		var result any
		switch req.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": Version, "capabilities": map[string]any{"tools": map[string]any{}, "tasks": map[string]any{}}, "serverInfo": map[string]any{"name": "test", "version": "1"}}
		case "notifications/initialized":
			w.WriteHeader(202)
			return
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "sync", "inputSchema": map[string]any{"type": "object"}}}}
		case "tools/call":
			calls.Add(1)
			if req.Params["task"] != nil {
				t.Error("sent task request")
			}
			result = map[string]any{"task": map[string]any{"taskId": "remote", "status": "working"}}
		default:
			t.Error("unexpected RPC", req.Method)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}))
	defer server.Close()
	c, err := New(context.Background(), toolhttp.Config{Scope: original().Scope, ID: "conn", URL: server.URL, Timeout: time.Second, MaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(context.Background())
	if _, err = c.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Execute(context.Background(), original()); err == nil {
		t.Fatal("accepted task-only result")
	}
	if calls.Load() != 1 {
		t.Fatal("retried execution")
	}
}
