package domain

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/hjhsamuel/cagent/internal/apperrors"
)

// 在公开校验入口验证错误契约，覆盖必填、关联、自引用、协议、互斥及嵌套错误。
// 字段路径保持 P1.1 的命名，引用对象的外层说明不改变结构化路径。
func TestValidationErrorContract(t *testing.T) {
	s, r, c, h := fixtures()
	secret := "private-credential-prompt-connection"
	badCall := c
	badCall.Protocol = ToolProtocol(secret)
	for _, tc := range []struct {
		name, field string
		check       func() error
	}{
		{"required", "scope.tenant_id", func() error { return (Scope{}).Validate() }},
		{"association", "scope.user_id", func() error { return (Scope{TenantID: s.Scope.TenantID, UserID: secret}).ValidateAgainst(s.Scope) }},
		{"expected scope", "scope.tenant_id", func() error { return s.Scope.ValidateAgainst(Scope{}) }},
		{"referenced session", "scope.tenant_id", func() error { return r.ValidateForSession(Session{}) }},
		{"referenced run", "scope.tenant_id", func() error { return c.ValidateForRun(Run{}) }},
		{"protocol", "protocol", func() error { return ToolProtocol(secret).Validate() }},
		{"self reference", "caller.parent_invocation_id", func() error {
			return (AgentExecution{AgentID: "agent", InvocationID: secret, ParentInvocationID: secret}).Validate()
		}},
		{"neither outcome", "outcome", func() error { return (ToolOutcome{}).Validate() }},
		{"both outcomes", "outcome", func() error { return (ToolOutcome{Result: &ToolResult{CallID: c.ID}, Task: &h}).Validate() }},
		{"nested protocol", "protocol", func() error { return (Task{Scope: s.Scope, ID: "task", Call: badCall, Handle: h}).Validate() }},
		{"nested result", "result.call_id", func() error {
			return (Task{Scope: s.Scope, ID: "task", Call: c, Handle: h, Result: &ToolResult{CallID: secret}}).Validate()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.check()
			if err == nil {
				t.Fatal("invalid input accepted")
			}
			err = fmt.Errorf("boundary: %w", err)
			var detail *apperrors.Error
			if !errors.Is(err, apperrors.ErrInvalidArgument) || !errors.As(err, &detail) {
				t.Fatalf("lost public error contract: %v", err)
			}
			if detail.Kind() != apperrors.ErrInvalidArgument || detail.Field() != tc.field || detail.Message() == "" {
				t.Fatalf("incorrect structured details: %v", detail)
			}
			if errors.Is(err, apperrors.ErrUnsupported) || strings.Contains(err.Error(), secret) || strings.Contains(detail.Message(), secret) {
				t.Fatal("wrong category or sensitive input exposed")
			}
		})
	}
}
