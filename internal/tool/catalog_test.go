package tool

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hjhsamuel/cagent/internal/apperrors"
	"github.com/hjhsamuel/cagent/internal/domain"
)

func call() domain.ToolCall {
	return domain.ToolCall{Scope: domain.Scope{TenantID: "t", UserID: "u"}, SessionID: "s", RunID: "r", ID: "c", Caller: domain.AgentExecution{AgentID: "a", InvocationID: "i"}, Protocol: domain.ToolLocal, Name: "echo", Arguments: []byte(`{"text":"hello"}`)}
}
func entry(f ExecutorFunc) Entry {
	return Entry{Scope: call().Scope, ConnectionID: "local", Descriptor: Descriptor{Name: "echo", Protocol: domain.ToolLocal, InputSchema: []byte(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"],"additionalProperties":false}`)}, Executor: f}
}
func limits() Limits { return Limits{Timeout: time.Second, MaxInputBytes: 1024, MaxOutputBytes: 1024} }
func TestScopeSchemaOutcomeAndCapturedExecutor(t *testing.T) {
	var invoked atomic.Int32
	c, e := NewCatalog([]Entry{entry(func(_ context.Context, c domain.ToolCall) (domain.ToolOutcome, error) {
		invoked.Add(1)
		return domain.ToolOutcome{Result: &domain.ToolResult{CallID: c.ID, Error: "business failure"}}, nil
	})}, limits())
	if e != nil {
		t.Fatal(e)
	}
	ctx := context.Background()
	exec, e := c.Resolve(ctx, call().Scope, domain.ToolLocal, "echo")
	if e != nil {
		t.Fatal(e)
	}
	for _, mutate := range []func(*domain.ToolCall){func(c *domain.ToolCall) { c.Scope.UserID = "other" }, func(c *domain.ToolCall) { c.Scope.TenantID = "other" }, func(c *domain.ToolCall) { c.Name = "unknown" }, func(c *domain.ToolCall) { c.Arguments = []byte(`{"text":3}`) }} {
		v := call()
		mutate(&v)
		if _, e := exec.Execute(ctx, v); e == nil {
			t.Fatal("invalid call accepted")
		}
	}
	if invoked.Load() != 0 {
		t.Fatal("unauthorized side effect")
	}
	out, e := exec.Execute(ctx, call())
	if e != nil || out.Result.Error != "business failure" || invoked.Load() != 1 {
		t.Fatal(out, e)
	}
	list, e := c.List(ctx, domain.Scope{TenantID: "t", UserID: "other"})
	if e != nil || len(list) != 0 {
		t.Fatal(list, e)
	}
	list, _ = c.List(ctx, call().Scope)
	list[0].InputSchema[0] = 'x'
	list, _ = c.List(ctx, call().Scope)
	if list[0].InputSchema[0] != '{' {
		t.Fatal("schema alias")
	}
	if _, e = c.Resolve(ctx, call().Scope, domain.ToolLocal, "missing"); !errors.Is(e, apperrors.ErrNotFound) {
		t.Fatal(e)
	}
}
func TestInvalidOutcomesAndTimeout(t *testing.T) {
	for _, out := range []domain.ToolOutcome{{}, {Result: &domain.ToolResult{CallID: "wrong"}}, {Result: &domain.ToolResult{CallID: "c"}, Task: &domain.TaskHandle{}}, {Task: &domain.TaskHandle{Protocol: domain.ToolLocal, ConnectionID: "wrong", RemoteID: "id"}}} {
		c, e := NewCatalog([]Entry{entry(func(context.Context, domain.ToolCall) (domain.ToolOutcome, error) { return out, nil })}, limits())
		if e != nil {
			t.Fatal(e)
		}
		exec, _ := c.Resolve(context.Background(), call().Scope, domain.ToolLocal, "echo")
		if _, e = exec.Execute(context.Background(), call()); e == nil {
			t.Fatal("invalid outcome accepted")
		}
	}
	l := limits()
	l.Timeout = time.Millisecond
	c, _ := NewCatalog([]Entry{entry(func(ctx context.Context, _ domain.ToolCall) (domain.ToolOutcome, error) {
		<-ctx.Done()
		return domain.ToolOutcome{}, ctx.Err()
	})}, l)
	exec, _ := c.Resolve(context.Background(), call().Scope, domain.ToolLocal, "echo")
	if _, e := exec.Execute(context.Background(), call()); !errors.Is(e, context.DeadlineExceeded) {
		t.Fatal(e)
	}
	secret := SafeError(errors.New("SECRET"))
	for _, format := range []string{"%v", "%+v", "%#v"} {
		if strings.Contains(fmt.Sprintf(format, secret), "SECRET") {
			t.Fatal("leak")
		}
	}
}

type artifactFunc func(context.Context, domain.Scope, []byte) (string, error)

func (f artifactFunc) Put(ctx context.Context, s domain.Scope, b []byte) (string, error) {
	return f(ctx, s, b)
}
func TestLargeResultAndConcurrentIsolation(t *testing.T) {
	f := func(_ context.Context, c domain.ToolCall) (domain.ToolOutcome, error) {
		return domain.ToolOutcome{Result: &domain.ToolResult{CallID: c.ID, Parts: []domain.Part{{Kind: domain.PartText, Text: strings.Repeat("x", 2048)}}}}, nil
	}
	l := limits()
	c, _ := NewCatalog([]Entry{entry(f)}, l)
	exec, _ := c.Resolve(context.Background(), call().Scope, domain.ToolLocal, "echo")
	if _, e := exec.Execute(context.Background(), call()); !errors.Is(e, apperrors.ErrInvalidArgument) {
		t.Fatal(e)
	}
	l.Artifacts = artifactFunc(func(_ context.Context, s domain.Scope, b []byte) (string, error) {
		if s != call().Scope || len(b) < 2048 {
			t.Error("artifact lost scope/content")
		}
		return "artifact:scoped-id", nil
	})
	c, _ = NewCatalog([]Entry{entry(f)}, l)
	exec, _ = c.Resolve(context.Background(), call().Scope, domain.ToolLocal, "echo")
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			v := call()
			v.ID = fmt.Sprint(i)
			out, e := exec.Execute(context.Background(), v)
			if e != nil || out.Result.CallID != v.ID || out.Result.Parts[0].URI != "artifact:scoped-id" {
				t.Error(out, e)
			}
		}(i)
	}
	wg.Wait()
}
