package bootstrap

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hjhsamuel/cagent/internal/domain"
)

func TestTrustedToolFileAndLocalCall(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tools.json")
	if e := os.WriteFile(path, []byte(`{"connections":[{"tenant_id":"t","user_id":"u","id":"local","protocol":"local","tools":["echo"]}]}`), 0600); e != nil {
		t.Fatal(e)
	}
	catalog, maxCalls, close, e := loadTools(context.Background(), path)
	if e != nil {
		t.Fatal(e)
	}
	defer close(context.Background())
	if maxCalls != 16 {
		t.Fatal(maxCalls)
	}
	scope := domain.Scope{TenantID: "t", UserID: "u"}
	list, e := catalog.List(context.Background(), scope)
	if e != nil || len(list) != 1 {
		t.Fatal(list, e)
	}
	exec, e := catalog.Resolve(context.Background(), scope, domain.ToolLocal, "echo")
	if e != nil {
		t.Fatal(e)
	}
	out, e := exec.Execute(context.Background(), domain.ToolCall{Scope: scope, ID: "c", SessionID: "s", RunID: "r", Caller: domain.AgentExecution{AgentID: "a", InvocationID: "i"}, Protocol: domain.ToolLocal, Name: "echo", Arguments: []byte(`{"text":"中文"}`)})
	if e != nil || out.Result.Parts[0].Text != "中文" {
		t.Fatal(out, e)
	}
	for _, bad := range []string{`{"connections":[{"tenant_id":"t","user_id":"","id":"local","protocol":"local"}]}`, `{"timeout":"0s"}`, `{"unknown":1}`, `{} {}`, `{"max_output_bytes":999999999}`} {
		if e = os.WriteFile(path, []byte(bad), 0600); e != nil {
			t.Fatal(e)
		}
		if _, _, _, e = loadTools(context.Background(), path); e == nil {
			t.Fatal("bad config accepted", bad)
		}
	}
}

// TestExternalToolSmoke 只有部署者显式启用才真实执行一次指定工具。
// 它不重试、不输出参数/结果/凭据，也不把任务句柄伪装成已完成的任务验收。
func TestExternalToolSmoke(t *testing.T) {
	if os.Getenv("CAGENT_TEST_TOOLS") != "1" {
		t.Skip("external tool provider not configured")
	}
	path := os.Getenv("CAGENT_TEST_TOOLS_FILE")
	if path == "" {
		t.Fatal("CAGENT_TEST_TOOLS_FILE is required")
	}
	var in struct {
		TenantID  string              `json:"tenant_id"`
		UserID    string              `json:"user_id"`
		Protocol  domain.ToolProtocol `json:"protocol"`
		Name      string              `json:"name"`
		Arguments json.RawMessage     `json:"arguments"`
	}
	if json.Unmarshal([]byte(os.Getenv("CAGENT_TEST_TOOL_CALL")), &in) != nil {
		t.Fatal("CAGENT_TEST_TOOL_CALL must be JSON")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	catalog, _, close, e := loadTools(ctx, path)
	if e != nil {
		t.Fatal("provider setup failed")
	}
	defer close(context.Background())
	call := domain.ToolCall{Scope: domain.Scope{TenantID: in.TenantID, UserID: in.UserID}, ID: "smoke-call", SessionID: "smoke-session", RunID: "smoke-run", Caller: domain.AgentExecution{AgentID: "smoke-agent", InvocationID: "smoke-invocation"}, Protocol: in.Protocol, Name: in.Name, Arguments: in.Arguments}
	exec, e := catalog.Resolve(ctx, call.Scope, call.Protocol, call.Name)
	if e != nil {
		t.Fatal("tool resolution failed")
	}
	out, e := exec.Execute(ctx, call)
	if e != nil || out.ValidateForCall(call) != nil {
		t.Fatal("provider call failed")
	}
	if out.Task != nil {
		t.Log("task handle returned; task lifecycle is not part of this smoke")
	}
}
