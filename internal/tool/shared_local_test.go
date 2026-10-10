package tool

import (
	"context"
	"errors"
	"testing"

	"github.com/hjhsamuel/cagent/internal/apperrors"
	"github.com/hjhsamuel/cagent/internal/domain"
)

func TestSharedLocalToolsRetainActualIdentityAndCapturedScope(t *testing.T) {
	shared := entry(func(_ context.Context, c domain.ToolCall) (domain.ToolOutcome, error) {
		return domain.ToolOutcome{Result: &domain.ToolResult{CallID: c.ID, Parts: []domain.Part{{Kind: domain.PartText, Text: c.Scope.TenantID + "/" + c.Scope.UserID}}}}, nil
	})
	shared.Scope, shared.SharedLocal = domain.Scope{}, true
	catalog, err := NewCatalog([]Entry{shared}, limits())
	if err != nil {
		t.Fatal(err)
	}
	for _, scope := range []domain.Scope{{TenantID: "t", UserID: "alice"}, {TenantID: "other", UserID: "bob"}} {
		c := call()
		c.Scope = scope
		executor, err := catalog.Resolve(context.Background(), scope, c.Protocol, c.Name)
		if err != nil {
			t.Fatal(err)
		}
		out, err := executor.Execute(context.Background(), c)
		if err != nil || out.Result.Parts[0].Text != scope.TenantID+"/"+scope.UserID {
			t.Fatal("actual scope not forwarded", out, err)
		}
		c.Scope.UserID = "different"
		if _, err := executor.Execute(context.Background(), c); !errors.Is(err, apperrors.ErrNotFound) {
			t.Fatal("captured executor changed scope", err)
		}
	}
	for _, change := range []func(*Entry){
		func(e *Entry) { e.Descriptor.Protocol = domain.ToolMCP },
		func(e *Entry) { e.Scope = call().Scope },
	} {
		bad := shared
		change(&bad)
		if _, err := NewCatalog([]Entry{bad}, limits()); !errors.Is(err, apperrors.ErrInvalidArgument) {
			t.Fatal("invalid shared registration accepted", err)
		}
	}
	scoped := shared
	scoped.SharedLocal, scoped.Scope = false, call().Scope
	for _, entries := range [][]Entry{{shared, scoped}, {scoped, shared}, {shared, shared}} {
		if _, err := NewCatalog(entries, limits()); !errors.Is(err, apperrors.ErrConflict) {
			t.Fatal("shared name collision accepted", err)
		}
	}
}
