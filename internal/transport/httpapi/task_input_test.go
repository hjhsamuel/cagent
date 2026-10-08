package httpapi_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hjhsamuel/cagent/internal/domain"
	"github.com/hjhsamuel/cagent/internal/tool"
	"github.com/hjhsamuel/cagent/internal/transport/httpapi"
)

type inputStub struct {
	called int
	scope  domain.Scope
	id     string
	input  tool.TaskInput
	auth   bool
}

func (s *inputStub) Submit(_ context.Context, scope domain.Scope, id string, in tool.TaskInput, auth bool) error {
	s.called++
	s.scope = scope
	s.id = id
	s.input = in
	s.auth = auth
	return nil
}
func TestTaskInputRoutesTrustJWTAndRejectHandleInjection(t *testing.T) {
	stub := &inputStub{}
	h, e := httpapi.New(&stubService{}, stubEvents{}, settings(), "a", func(context.Context) bool { return true }, stub)
	if e != nil {
		t.Fatal(e)
	}
	for _, tc := range []struct {
		route, body   string
		authenticated bool
		want          int
	}{{"input", `{"text":"reply"}`, false, 401}, {"input", `{"text":"reply","scope":"other"}`, true, 400}, {"input", `{"text":"reply","remote_id":"evil"}`, true, 400}, {"input", `{"text":"reply","credential_ref":"secret"}`, true, 400}, {"authorization", `{"text":"continue"}`, true, 400}, {"input", `{"text":"reply"}`, true, 202}, {"authorization", `{"text":"continue","credential_ref":"secondary"}`, true, 202}} {
		req := httptest.NewRequest("POST", "/api/v1/tasks/local-id/"+tc.route, strings.NewReader(tc.body))
		if tc.authenticated {
			req.Header.Set("Authorization", "Bearer "+token(t))
		}
		req.Header.Set("X-Tenant-ID", "forged")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Fatal(tc, rec.Code, rec.Body.String())
		}
	}
	if stub.called != 2 || stub.scope != (domain.Scope{TenantID: "tenant", UserID: "user"}) || stub.id != "local-id" || !stub.auth || stub.input.CredentialRef != "secondary" {
		t.Fatal(stub)
	}
}
