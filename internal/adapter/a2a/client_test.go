package a2a

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/a2aproject/a2a-go/a2a"
	"github.com/hjhsamuel/cagent/internal/adapter/toolhttp"
	"github.com/hjhsamuel/cagent/internal/apperrors"
	"github.com/hjhsamuel/cagent/internal/domain"
	"github.com/hjhsamuel/cagent/internal/tool"
)

func original() domain.ToolCall {
	return domain.ToolCall{Scope: domain.Scope{TenantID: "t", UserID: "u"}, SessionID: "s", RunID: "r", ID: "call", Caller: domain.AgentExecution{AgentID: "a", InvocationID: "i"}, Protocol: domain.ToolA2A, Name: "remote_agent", Arguments: []byte(`{"text":"hello"}`)}
}

func TestProtocolErrorsRetainCauseAndMeaning(t *testing.T) {
	for code, want := range map[error]error{sdk.ErrTaskNotFound: apperrors.ErrNotFound, sdk.ErrTaskNotCancelable: apperrors.ErrConflict, sdk.ErrUnsupportedOperation: apperrors.ErrUnsupported, sdk.ErrMethodNotFound: apperrors.ErrUnsupported} {
		cause := sdk.NewError(code, "SECRET")
		err := protocolError(cause)
		if !errors.Is(err, want) || !errors.Is(err, cause) {
			t.Fatal(code, err)
		}
	}
}
func TestProviderMessageTaskStateStreamAndPausedInteraction(t *testing.T) {
	var state atomic.Value
	state.Store("working")
	var sends atomic.Int32
	var instant atomic.Bool
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "GET" {
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "remote_agent", "url": server.URL, "protocolVersion": "0.3.0", "capabilities": map[string]any{"streaming": true}})
			return
		}
		var req struct {
			ID     string `json:"id"`
			Method string `json:"method"`
			Params struct {
				ID      string      `json:"id"`
				Message sdk.Message `json:"message"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		task := map[string]any{"kind": "task", "id": "remote", "contextId": "context", "status": map[string]any{"state": state.Load()}, "artifacts": []any{map[string]any{"parts": []any{map[string]any{"kind": "text", "text": "done"}}}}}
		var result any = task
		switch req.Method {
		case "message/send":
			sends.Add(1)
			if req.Params.Message.TaskID != "" {
				if req.Params.Message.TaskID != "remote" || req.Params.Message.ContextID != "context" {
					t.Error("restarted paused task")
				}
				if state.Load() == "auth-required" && r.Header.Get("Authorization") != "Bearer secondary" {
					t.Error("wrong auth ref")
				}
			} else if instant.Load() {
				result = map[string]any{"kind": "message", "messageId": "m", "role": "agent", "parts": []any{map[string]any{"kind": "text", "text": "instant"}}}
			}
		case "tasks/get", "tasks/cancel":
			if req.Params.ID != "remote" {
				t.Error("wrong task")
			}
		case "tasks/resubscribe":
			if r.Header.Get("Last-Event-ID") != "" {
				t.Error("lost cursor")
			}
			w.Header().Set("Content-Type", "text/event-stream")
			state.Store("completed")
			b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"kind": "status-update", "taskId": "remote", "contextId": "context"}})
			fmt.Fprintf(w, "id: next\ndata: %s\n\n", b)
			return
		default:
			t.Error(req.Method)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}))
	defer server.Close()
	c, e := New(context.Background(), toolhttp.Config{Scope: original().Scope, ID: "conn", URL: server.URL, Timeout: time.Second, MaxBytes: 1 << 20, Credentials: func(_ context.Context, s domain.Scope, ref string) (string, error) {
		if s != original().Scope || ref != "secondary" {
			return "", apperrors.ErrNotFound
		}
		return "Bearer secondary", nil
	}}, "/.well-known/agent-card.json")
	if e != nil {
		t.Fatal(e)
	}
	instant.Store(true)
	out, e := c.Execute(context.Background(), original())
	if e != nil || out.Result.Parts[0].Text != "instant" {
		t.Fatal(out, e)
	}
	instant.Store(false)
	out, e = c.Execute(context.Background(), original())
	if e != nil || out.Task == nil {
		t.Fatal(out, e)
	}
	h := *out.Task
	for wire, want := range map[string]domain.TaskStatus{"submitted": domain.TaskSubmitted, "working": domain.TaskRunning, "input-required": domain.TaskInputRequired, "auth-required": domain.TaskAuthRequired, "completed": domain.TaskSucceeded, "failed": domain.TaskFailed, "canceled": domain.TaskCancelled, "rejected": domain.TaskRejected} {
		state.Store(wire)
		u, e := c.Get(context.Background(), original(), h)
		if e != nil || u.Status != want || (u.Result != nil) != want.IsTerminal() {
			t.Fatal(wire, u, e)
		}
		if u.Result != nil && u.Result.CallID != "call" {
			t.Fatal("lost call ID")
		}
	}
	state.Store("unknown-new")
	_, e = c.Get(context.Background(), original(), h)
	var unknown *toolhttp.UnknownState
	if !errors.As(e, &unknown) {
		t.Fatal(e)
	}
	state.Store("input-required")
	if e = c.Continue(context.Background(), original(), h, tool.TaskInput{Text: "answer"}); e != nil {
		t.Fatal(e)
	}
	state.Store("auth-required")
	if e = c.Continue(context.Background(), original(), h, tool.TaskInput{Text: "continue"}); !errors.Is(e, apperrors.ErrUnsupported) {
		t.Fatal(e)
	}
	if e = c.Continue(context.Background(), original(), h, tool.TaskInput{Text: "continue", CredentialRef: "secondary"}); e != nil {
		t.Fatal(e)
	}
	state.Store("working")
	if e = c.Cancel(context.Background(), original(), h); e != nil {
		t.Fatal(e)
	}
	if state.Load() != "working" {
		t.Fatal("cancel acceptance confused with cancellation")
	}
	if e = c.Continue(context.Background(), original(), h, tool.TaskInput{Text: "again"}); !errors.Is(e, apperrors.ErrConflict) {
		t.Fatal(e)
	}
	if e = c.Follow(context.Background(), original(), h, "old", func(domain.TaskUpdate) error { return nil }); !errors.Is(e, apperrors.ErrUnsupported) {
		t.Fatal(e)
	}
	var observed domain.TaskUpdate
	if e = c.Follow(context.Background(), original(), h, "", func(u domain.TaskUpdate) error { observed = u; return nil }); e != nil || observed.Cursor != "" || observed.Status != domain.TaskSucceeded {
		t.Fatal(observed, e)
	}
	if sends.Load() != 4 {
		t.Fatal("unexpected reexecution", sends.Load())
	}
	wrong := h
	wrong.ConnectionID = "other"
	if _, e = c.Get(context.Background(), original(), wrong); !errors.Is(e, apperrors.ErrNotFound) {
		t.Fatal(e)
	}
	c.card.Capabilities.Streaming = false
	if e = c.Follow(context.Background(), original(), h, "", func(domain.TaskUpdate) error { return nil }); !errors.Is(e, apperrors.ErrUnsupported) {
		t.Fatal(e)
	}
}

func TestSDKProtocolErrorsAndTaskAssociation(t *testing.T) {
	for _, mode := range []string{"not-found", "conflict", "unsupported", "wrong-context", "disconnect"} {
		t.Run(mode, func(t *testing.T) {
			var requests atomic.Int32
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == "GET" {
					_ = json.NewEncoder(w).Encode(map[string]any{"name": "remote_agent", "url": server.URL, "protocolVersion": "0.3.0", "capabilities": map[string]any{}})
					return
				}
				requests.Add(1)
				var req struct {
					ID string `json:"id"`
				}
				_ = json.NewDecoder(r.Body).Decode(&req)
				if mode == "disconnect" {
					conn, _, _ := w.(http.Hijacker).Hijack()
					conn.Close()
					return
				}
				if mode == "wrong-context" {
					_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"kind": "task", "id": "remote", "contextId": "other", "status": map[string]any{"state": "completed"}}})
					return
				}
				code := map[string]int{"not-found": -32001, "conflict": -32002, "unsupported": -32601}[mode]
				_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": code, "message": "SECRET", "data": map[string]any{"secret": "SECRET"}}})
			}))
			defer server.Close()
			c, err := New(context.Background(), toolhttp.Config{Scope: original().Scope, ID: "conn", URL: server.URL, Timeout: time.Second, MaxBytes: 1 << 20}, "/.well-known/agent-card.json")
			if err != nil {
				t.Fatal(err)
			}
			h := domain.TaskHandle{Protocol: domain.ToolA2A, ConnectionID: "conn", RemoteID: "remote", ContextID: "context"}
			err = c.Cancel(context.Background(), original(), h)
			want := map[string]error{"not-found": apperrors.ErrNotFound, "conflict": apperrors.ErrConflict, "unsupported": apperrors.ErrUnsupported, "wrong-context": apperrors.ErrInvalidArgument}[mode]
			if err == nil || (want != nil && !errors.Is(err, want)) {
				t.Fatal(mode, err)
			}
			if strings.Contains(fmt.Sprintf("%v %+v %#v", err, err, err), "SECRET") {
				t.Fatal("provider error leaked")
			}
			if requests.Load() != 1 {
				t.Fatal("replayed request", requests.Load())
			}
		})
	}
}
