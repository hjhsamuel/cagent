package domain

import (
	"reflect"
	"strings"
	"testing"
)

// fixtures 每次返回独立的值，避免负例修改共享对象，污染后续子测试。
func fixtures() (Session, Run, ToolCall, TaskHandle) {
	scope := Scope{TenantID: "tenant", UserID: "user"}
	s := Session{Scope: scope, ID: "session", AgentID: "agent"}
	r := Run{Scope: scope, ID: "run", SessionID: s.ID}
	c := ToolCall{Scope: scope, ID: "call", SessionID: s.ID, RunID: r.ID,
		Caller: AgentExecution{AgentID: "agent", InvocationID: "invocation"}, Protocol: ToolA2A, Name: "search"}
	h := TaskHandle{Protocol: ToolA2A, ConnectionID: "connection", RemoteID: "remote"}
	return s, r, c, h
}

func TestScopeValidation(t *testing.T) {
	valid := Scope{TenantID: "tenant", UserID: "user"}
	for _, scope := range []Scope{{}, {TenantID: "tenant"}, {UserID: "user"}, {TenantID: " \t", UserID: "user"}, {TenantID: "tenant", UserID: "\u3000"}} {
		if err := scope.Validate(); err == nil {
			t.Errorf("accepted missing scope: %#v", scope)
		}
		if err := valid.ValidateAgainst(scope); err == nil {
			t.Errorf("accepted invalid expected scope: %#v", scope)
		}
	}
	if err := valid.ValidateAgainst(valid); err != nil {
		t.Fatal(err)
	}
	for _, scope := range []Scope{{TenantID: "other", UserID: "user"}, {TenantID: "tenant", UserID: "other"}} {
		if err := scope.ValidateAgainst(valid); err == nil {
			t.Fatal("accepted foreign scope")
		}
	}
	// ID 不做隐式清洗；前后空格属于原值，不能据此匹配另一个租户。
	padded := Scope{TenantID: " tenant ", UserID: "user"}
	before := padded
	if err := padded.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := padded.ValidateAgainst(valid); err == nil {
		t.Fatal("silently normalized tenant ID")
	}
	if padded != before {
		t.Fatal("validation mutated scope")
	}
}

func TestResourceAssociations(t *testing.T) {
	s, r, c, h := fixtures()
	m := Message{Scope: s.Scope, ID: "message", SessionID: s.ID, RunID: r.ID}
	e := Event{Scope: s.Scope, RunID: r.ID}
	snapshot := ContextSnapshot{Scope: s.Scope, ID: "snapshot", SessionID: s.ID}
	task := Task{Scope: s.Scope, ID: "task", Call: c, Handle: h}
	for name, check := range map[string]func() error{
		"run":             func() error { return r.ValidateForSession(s) },
		"message session": func() error { return m.ValidateForSession(s) },
		"message run":     func() error { return m.ValidateForRun(r) },
		"event":           func() error { return e.ValidateForRun(r) },
		"snapshot":        func() error { return snapshot.ValidateForSession(s) },
		"call":            func() error { return c.ValidateForRun(r) },
		"task":            func() error { return task.ValidateForRun(r) },
	} {
		t.Run(name, func(t *testing.T) {
			if err := check(); err != nil {
				t.Fatal(err)
			}
		})
	}

	// 对所属资源逐字段破坏，覆盖同租户不同用户、同用户不同租户和错误资源 ID。
	for _, field := range []string{"tenant", "user", "session", "run", "missing"} {
		t.Run(field, func(t *testing.T) {
			badSession, badRun := s, r
			switch field {
			case "tenant":
				badSession.Scope.TenantID = "other"
				badRun.Scope.TenantID = "other"
			case "user":
				badSession.Scope.UserID = "other"
				badRun.Scope.UserID = "other"
			case "session":
				badSession.ID = "other"
				badRun.SessionID = "other"
			case "run":
				badRun.ID = "other"
			case "missing":
				badSession = Session{}
				badRun = Run{}
			}
			checks := []func() error{
				func() error { return m.ValidateForRun(badRun) },
				func() error { return c.ValidateForRun(badRun) },
				func() error { return task.ValidateForRun(badRun) },
			}
			if field != "run" {
				checks = append(checks,
					func() error { return r.ValidateForSession(badSession) },
					func() error { return m.ValidateForSession(badSession) },
					func() error { return snapshot.ValidateForSession(badSession) })
			}
			// Event 不携带 SessionID，只能检查作用域和 RunID。
			if field != "session" {
				checks = append(checks, func() error { return e.ValidateForRun(badRun) })
			}
			for i, check := range checks {
				if err := check(); err == nil {
					t.Errorf("check %d accepted foreign resource", i)
				}
			}
		})
	}
	m.RunID = ""
	if err := m.ValidateForSession(s); err != nil {
		t.Fatal(err)
	}
	if err := m.ValidateForRun(r); err == nil {
		t.Fatal("accepted unbound message as run output")
	}
}

func TestRequiredIdentityFields(t *testing.T) {
	s, r, c, h := fixtures()
	// 反射仅用于逐个清空必填字段，不复用生产代码的校验规则。
	cases := []struct {
		value  any
		fields []string
	}{
		{s, []string{"ID", "AgentID"}}, {r, []string{"ID", "SessionID"}},
		{Message{Scope: s.Scope, ID: "m", SessionID: s.ID}, []string{"ID", "SessionID"}},
		{Event{Scope: s.Scope, RunID: r.ID}, []string{"RunID"}},
		{ContextSnapshot{Scope: s.Scope, ID: "s", SessionID: s.ID}, []string{"ID", "SessionID"}},
		{c, []string{"ID", "SessionID", "RunID", "Name"}},
		{h, []string{"ConnectionID", "RemoteID"}},
		{c.Caller, []string{"AgentID", "InvocationID"}},
		{ToolResult{CallID: c.ID}, []string{"CallID"}},
		{Task{Scope: s.Scope, ID: "t", Call: c, Handle: h}, []string{"ID"}},
	}
	for _, tc := range cases {
		for _, field := range tc.fields {
			for _, blank := range []string{"", " \t\n"} {
				copy := reflect.New(reflect.TypeOf(tc.value)).Elem()
				copy.Set(reflect.ValueOf(tc.value))
				copy.FieldByName(field).SetString(blank)
				if err := copy.Interface().(interface{ Validate() error }).Validate(); err == nil {
					t.Errorf("%T accepted blank %s", tc.value, field)
				}
			}
		}
	}
}

func TestToolOutcomes(t *testing.T) {
	_, _, c, h := fixtures()
	result := ToolResult{CallID: c.ID}
	for _, outcome := range []ToolOutcome{{Result: &result}, {Task: &h}} {
		if err := outcome.ValidateForCall(c); err != nil {
			t.Fatal(err)
		}
	}
	badResult := ToolResult{CallID: "different"}
	badHandle := h
	badHandle.Protocol = ToolMCP
	for _, outcome := range []ToolOutcome{{}, {Result: &result, Task: &h}, {Result: &ToolResult{}}, {Task: &TaskHandle{}}, {Result: &badResult}, {Task: &badHandle}} {
		if err := outcome.ValidateForCall(c); err == nil {
			t.Errorf("accepted invalid outcome: %#v", outcome)
		}
	}
	for _, protocol := range []ToolProtocol{ToolLocal, ToolMCP, ToolA2A} {
		call, handle := c, h
		call.Protocol = protocol
		handle.Protocol = protocol
		if err := handle.ValidateForCall(call); err != nil {
			t.Fatal(err)
		}
	}
	for _, protocol := range []ToolProtocol{"", "unknown"} {
		call, handle := c, h
		call.Protocol = protocol
		handle.Protocol = protocol
		if err := call.Validate(); err == nil {
			t.Fatal("accepted unknown call protocol")
		}
		if err := handle.Validate(); err == nil {
			t.Fatal("accepted unknown handle protocol")
		}
	}
}

func TestTaskNestedAssociations(t *testing.T) {
	s, _, c, h := fixtures()
	valid := Task{Scope: s.Scope, ID: "task", Call: c, Handle: h, Result: &ToolResult{CallID: c.ID}}
	for name, mutate := range map[string]func(*Task){
		"tenant":             func(v *Task) { v.Call.Scope.TenantID = "other" },
		"user":               func(v *Task) { v.Call.Scope.UserID = "other" },
		"empty scope":        func(v *Task) { v.Scope = Scope{} },
		"empty nested scope": func(v *Task) { v.Call.Scope = Scope{} },
		"protocol":           func(v *Task) { v.Handle.Protocol = ToolMCP },
		"result":             func(v *Task) { v.Result = &ToolResult{CallID: "other"} },
		"self parent":        func(v *Task) { v.Call.Caller.ParentInvocationID = v.Call.Caller.InvocationID },
		"blank parent":       func(v *Task) { v.Call.Caller.ParentInvocationID = " " },
	} {
		t.Run(name, func(t *testing.T) {
			value := valid
			mutate(&value)
			if err := value.Validate(); err == nil {
				t.Fatal("accepted invalid nested association")
			}
		})
	}
	valid.Call.Caller.ParentInvocationID = "parent"
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	// 错误报告应提供字段位置，而非暴露用户输入或远端标识。
	secret := "private-call-id"
	valid.Result = &ToolResult{CallID: secret}
	err := valid.Validate()
	if err == nil || !strings.Contains(err.Error(), "result.call_id") || strings.Contains(err.Error(), secret) {
		t.Fatalf("unexpected diagnostic: %v", err)
	}
}
