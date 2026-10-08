package httpapi_test

import (
	"context"
	"github.com/hjhsamuel/cagent/internal/apperrors"
	"github.com/hjhsamuel/cagent/internal/domain"
	"github.com/hjhsamuel/cagent/internal/transport/httpapi"
	"net/http/httptest"
	"strings"
	"testing"
)

type tasksStub struct {
	stubService
	gets, cancels int
	scope         domain.Scope
}

func (s *tasksStub) GetTask(_ context.Context, scope domain.Scope, id string) (domain.Task, error) {
	s.gets++
	s.scope = scope
	if id == "missing" {
		return domain.Task{}, apperrors.ErrNotFound
	}
	return domain.Task{Scope: scope, ID: id, Status: domain.TaskSucceeded, Call: domain.ToolCall{ID: "call", RunID: "run", Arguments: []byte("secret-arguments"), Caller: domain.AgentExecution{AgentID: "agent", InvocationID: "branch", ParentInvocationID: "parent"}}, Handle: domain.TaskHandle{ConnectionID: "private-connection", RemoteID: "private-remote"}, Result: &domain.ToolResult{CallID: "call", Parts: []domain.Part{{Kind: domain.PartText, Text: "result"}}}}, nil
}
func (s *tasksStub) CancelTask(_ context.Context, scope domain.Scope, id string) error {
	s.cancels++
	s.scope = scope
	if id == "missing" {
		return apperrors.ErrNotFound
	}
	return nil
}

func TestTaskReadCancelRoutes(t *testing.T) {
	stub := &tasksStub{}
	h, e := httpapi.New(stub, stubEvents{}, settings(), "agent", func(context.Context) bool { return true })
	if e != nil {
		t.Fatal(e)
	}
	for _, tc := range []struct {
		method, path string
		auth         bool
		want         int
	}{{"GET", "/api/v1/tasks/id", false, 401}, {"POST", "/api/v1/tasks/id/cancel", false, 401}, {"GET", "/api/v1/tasks/missing", true, 404}, {"POST", "/api/v1/tasks/missing/cancel", true, 404}, {"GET", "/api/v1/tasks/id", true, 200}, {"POST", "/api/v1/tasks/id/cancel", true, 202}} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{"remote_id":"forged","scope":"forged"}`))
		if tc.auth {
			req.Header.Set("Authorization", "Bearer "+token(t))
		}
		req.Header.Set("X-Tenant-ID", "forged")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != tc.want {
			t.Fatal(tc, w.Code, w.Body.String())
		}
		if tc.want == 200 {
			body := w.Body.String()
			for _, private := range []string{"secret-arguments", "private-connection", "private-remote", "tenant", "user", "Scope", "Handle"} {
				if strings.Contains(body, private) {
					t.Fatalf("private field leaked: %s", private)
				}
			}
			for _, public := range []string{`"invocation_id":"branch"`, `"tool_call_id":"call"`, `"text":"result"`} {
				if !strings.Contains(body, public) {
					t.Fatal("missing DTO field", body)
				}
			}
		}
	}
	if stub.gets != 2 || stub.cancels != 2 || stub.scope != (domain.Scope{TenantID: "tenant", UserID: "user"}) {
		t.Fatal(stub)
	}
}
