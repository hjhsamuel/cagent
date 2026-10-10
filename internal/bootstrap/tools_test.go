package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hjhsamuel/cagent/internal/adapter/mongodb"
	"github.com/hjhsamuel/cagent/internal/apperrors"
	"github.com/hjhsamuel/cagent/internal/config"
	"github.com/hjhsamuel/cagent/internal/domain"
	"github.com/hjhsamuel/cagent/internal/storage/schema"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

var exampleBuildOnce sync.Once
var exampleToolsDir string
var exampleBuildErr error

func TestMain(m *testing.M) {
	code := m.Run()
	if exampleToolsDir != "" {
		_ = os.RemoveAll(exampleToolsDir)
	}
	os.Exit(code)
}

// 构建并调用真实业务程序；测试无需事先在工作区生成 bin 文件。
func exampleToolDirectory(t *testing.T) string {
	t.Helper()
	exampleBuildOnce.Do(func() {
		exampleToolsDir, exampleBuildErr = os.MkdirTemp("", "cagent-local-tools-")
		if exampleBuildErr != nil {
			return
		}
		for _, name := range []string{"echo", "read_skill"} {
			dir := filepath.Join(exampleToolsDir, name)
			if exampleBuildErr = os.MkdirAll(filepath.Join(dir, "bin"), 0700); exampleBuildErr != nil {
				return
			}
			var data []byte
			data, exampleBuildErr = os.ReadFile(filepath.Join("..", "..", "local-tools", name, "tool.json"))
			if exampleBuildErr != nil {
				return
			}
			if exampleBuildErr = os.WriteFile(filepath.Join(dir, "tool.json"), data, 0600); exampleBuildErr != nil {
				return
			}
			binary := name
			if runtime.GOOS == "windows" {
				binary += ".exe"
			}
			cmd := exec.Command("go", "build", "-buildvcs=false", "-o", filepath.Join(dir, "bin", binary), "./local-tools/"+name)
			cmd.Dir = filepath.Join("..", "..")
			if output, err := cmd.CombinedOutput(); err != nil {
				exampleBuildErr = errors.New("cannot build example executable: " + string(output))
				return
			}
		}
		skillDir := filepath.Join(exampleToolsDir, "read_skill", "skills", "code-review")
		if exampleBuildErr = os.MkdirAll(skillDir, 0700); exampleBuildErr != nil {
			return
		}
		var data []byte
		data, exampleBuildErr = os.ReadFile(filepath.Join("..", "..", "local-tools", "read_skill", "skills", "code-review", "SKILL.md"))
		if exampleBuildErr == nil {
			exampleBuildErr = os.WriteFile(filepath.Join(skillDir, "SKILL.md"), data, 0600)
		}
	})
	if exampleBuildErr != nil {
		t.Fatal(exampleBuildErr)
	}
	return exampleToolsDir
}

type toolReaderStub struct {
	connections []schema.ToolConnection
	err         error
}

func (s toolReaderStub) ListToolConnections(context.Context) ([]schema.ToolConnection, error) {
	return s.connections, s.err
}

func toolConfig(t *testing.T) config.Tools {
	t.Helper()
	cfg := config.DefaultTools()
	cfg.LocalDir = exampleToolDirectory(t)
	return cfg
}

func toolCall(scope domain.Scope, protocol domain.ToolProtocol, name, arguments string) domain.ToolCall {
	return domain.ToolCall{Scope: scope, ID: "c", SessionID: "s", RunID: "r", Caller: domain.AgentExecution{AgentID: "a", InvocationID: "i"}, Protocol: protocol, Name: name, Arguments: []byte(arguments)}
}

func TestAutomaticLocalToolsAndManifestConfiguration(t *testing.T) {
	ctx := context.Background()
	catalog, maxCalls, closeAll, err := loadTools(ctx, toolReaderStub{}, toolConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer closeAll(ctx)
	if maxCalls != 16 {
		t.Fatal(maxCalls)
	}
	wantSkill, err := os.ReadFile(filepath.Join("..", "..", "local-tools", "read_skill", "skills", "code-review", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, scope := range []domain.Scope{{TenantID: "t", UserID: "alice"}, {TenantID: "other", UserID: "bob"}} {
		list, err := catalog.List(ctx, scope)
		if err != nil || len(list) != 2 || list[0].Name != "echo" || list[1].Name != "read_skill" {
			t.Fatal(list, err)
		}
		for _, name := range []string{"echo", "read_skill"} {
			executor, err := catalog.Resolve(ctx, scope, domain.ToolLocal, name)
			if err != nil {
				t.Fatal(err)
			}
			arguments, want := `{"text":"中文"}`, "中文"
			if name == "read_skill" {
				arguments, want = `{"name":"code-review"}`, string(wantSkill)
			}
			call := toolCall(scope, domain.ToolLocal, name, arguments)
			out, err := executor.Execute(ctx, call)
			if err != nil || out.Result == nil || out.Result.Error != "" || out.Result.Parts[0].Text != want {
				t.Fatal("automatic tool execution failed", out, err)
			}
			call.Scope.UserID = "other"
			if _, err := executor.Execute(ctx, call); !errors.Is(err, apperrors.ErrNotFound) {
				t.Fatal("captured executor bypassed scope", err)
			}
			call.Scope = scope
			call.Arguments = []byte(`{"name":"code-review","skills_dir":"other"}`)
			if _, err := executor.Execute(ctx, call); !errors.Is(err, apperrors.ErrInvalidArgument) {
				t.Fatal("model supplied unauthorized configuration", err)
			}
		}
	}
	if _, err := catalog.List(ctx, domain.Scope{}); !errors.Is(err, apperrors.ErrInvalidArgument) {
		t.Fatal("invalid identity accepted", err)
	}
}

func TestToolSourcesFailClosed(t *testing.T) {
	cfg := config.DefaultTools()
	cfg.LocalDir = t.TempDir()
	dbErr := errors.New("database unavailable")
	if _, _, _, err := loadTools(context.Background(), toolReaderStub{err: dbErr}, cfg); !errors.Is(err, dbErr) {
		t.Fatal("database failure ignored", err)
	}
	valid := schema.ToolConnection{TenantID: "t", UserID: "u", ID: "remote", Protocol: "mcp", URL: "http://127.0.0.1"}
	for _, change := range []func(*schema.ToolConnection){
		func(c *schema.ToolConnection) { c.UserID = "" },
		func(c *schema.ToolConnection) { c.ID = " " },
		func(c *schema.ToolConnection) { c.URL = "" },
		func(c *schema.ToolConnection) { c.Protocol = "local" },
		func(c *schema.ToolConnection) { c.Protocol = "unknown" },
	} {
		bad := valid
		change(&bad)
		if _, _, _, err := loadTools(context.Background(), toolReaderStub{connections: []schema.ToolConnection{bad}}, cfg); err == nil {
			t.Fatal("invalid database connection accepted")
		}
	}
	if _, _, _, err := loadTools(context.Background(), toolReaderStub{connections: []schema.ToolConnection{valid, valid}}, cfg); !errors.Is(err, apperrors.ErrInvalidArgument) {
		t.Fatal("duplicate connection accepted", err)
	}
	// 空目录允许仅部署远端工具；损坏或缺失的本地目录不能静默跳过。
	catalog, _, closeAll, err := loadTools(context.Background(), toolReaderStub{}, cfg)
	if err != nil || catalog == nil {
		t.Fatal(err)
	}
	closeAll(context.Background())
	if err := os.Mkdir(filepath.Join(cfg.LocalDir, "broken"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := loadTools(context.Background(), toolReaderStub{}, cfg); !errors.Is(err, apperrors.ErrInvalidArgument) {
		t.Fatal("broken tool directory skipped", err)
	}
	cfg.LocalDir = filepath.Join(t.TempDir(), "missing")
	if _, _, _, err := loadTools(context.Background(), toolReaderStub{}, cfg); !errors.Is(err, apperrors.ErrInvalidArgument) {
		t.Fatal("missing directory skipped", err)
	}
}

func TestMongoConnectionsDiscoverMCPAndA2AWithScopedCredentials(t *testing.T) {
	ctx := context.Background()
	provider := sdk.NewServer(&sdk.Implementation{Name: "test", Version: "1"}, nil)
	for _, name := range []string{"remote_echo", "filtered"} {
		provider.AddTool(&sdk.Tool{Name: name, InputSchema: json.RawMessage(`{"type":"object"}`)}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "mcp result"}}}, nil
		})
	}
	var aliceRequests, bobRequests atomic.Int32
	handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return provider }, nil)
	mcpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Authorization") {
		case "Bearer alice":
			aliceRequests.Add(1)
		case "Bearer bob":
			bobRequests.Add(1)
		default:
			t.Error("incorrect credential resolution")
		}
		handler.ServeHTTP(w, r)
	}))
	defer mcpServer.Close()
	var cardReads atomic.Int32
	var a2aServer *httptest.Server
	a2aServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "Bearer alice" {
			t.Error("A2A credential missing")
		}
		if r.Method == http.MethodGet {
			if r.URL.Path != "/custom-card" {
				t.Error("configured card path ignored")
			}
			cardReads.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "remote_agent", "url": a2aServer.URL, "protocolVersion": "0.3.0", "capabilities": map[string]any{}})
			return
		}
		var req struct {
			ID json.RawMessage `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"kind": "message", "messageId": "reply", "role": "agent", "parts": []any{map[string]any{"kind": "text", "text": "a2a result"}}}})
	}))
	defer a2aServer.Close()
	t.Setenv("CAGENT_TEST_ALICE_TOOL_AUTH", "Bearer alice")
	t.Setenv("CAGENT_TEST_BOB_TOOL_AUTH", "Bearer bob")
	reader := toolReaderStub{connections: []schema.ToolConnection{
		{TenantID: "t", UserID: "alice", ID: "mcp", Protocol: "mcp", URL: mcpServer.URL, Tools: []string{"remote_echo"}, CredentialRef: "primary", Credentials: map[string]string{"primary": "CAGENT_TEST_ALICE_TOOL_AUTH"}},
		{TenantID: "t", UserID: "bob", ID: "mcp", Protocol: "mcp", URL: mcpServer.URL, Tools: []string{"remote_echo"}, CredentialRef: "primary", Credentials: map[string]string{"primary": "CAGENT_TEST_BOB_TOOL_AUTH"}},
		{TenantID: "t", UserID: "alice", ID: "a2a", Protocol: "a2a", URL: a2aServer.URL, CardPath: "/custom-card", CredentialRef: "primary", Credentials: map[string]string{"primary": "CAGENT_TEST_ALICE_TOOL_AUTH"}},
	}}
	catalog, _, closeAll, err := loadTools(ctx, reader, toolConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer closeAll(ctx)
	for _, user := range []string{"alice", "bob", "other"} {
		scope := domain.Scope{TenantID: "t", UserID: user}
		list, err := catalog.List(ctx, scope)
		wantCount := map[string]int{"alice": 4, "bob": 3, "other": 2}[user]
		if err != nil || len(list) != wantCount {
			t.Fatal("incorrect scoped tool list", user, list, err)
		}
		if _, err := catalog.Resolve(ctx, scope, domain.ToolMCP, "filtered"); !errors.Is(err, apperrors.ErrNotFound) {
			t.Fatal("remote allowlist ignored", err)
		}
		if user == "other" {
			if _, err := catalog.Resolve(ctx, scope, domain.ToolMCP, "remote_echo"); !errors.Is(err, apperrors.ErrNotFound) {
				t.Fatal("remote scope leaked", err)
			}
			continue
		}
		call := toolCall(scope, domain.ToolMCP, "remote_echo", "{}")
		executor, err := catalog.Resolve(ctx, scope, call.Protocol, call.Name)
		if err != nil {
			t.Fatal(err)
		}
		out, err := executor.Execute(ctx, call)
		if err != nil || out.Result == nil || out.Result.Parts[0].Text != "mcp result" {
			t.Fatal(out, err)
		}
		if user == "bob" {
			if _, err := catalog.Resolve(ctx, scope, domain.ToolA2A, "remote_agent"); !errors.Is(err, apperrors.ErrNotFound) {
				t.Fatal("A2A scope leaked", err)
			}
		}
	}
	call := toolCall(domain.Scope{TenantID: "t", UserID: "alice"}, domain.ToolA2A, "remote_agent", `{"text":"hello"}`)
	executor, err := catalog.Resolve(ctx, call.Scope, call.Protocol, call.Name)
	if err != nil {
		t.Fatal(err)
	}
	out, err := executor.Execute(ctx, call)
	if err != nil || out.Result == nil || out.Result.Parts[0].Text != "a2a result" || cardReads.Load() != 1 || aliceRequests.Load() == 0 || bobRequests.Load() == 0 {
		t.Fatal("remote discovery or execution failed", out, err)
	}
}

func TestToolStartupFailureClosesOpenedMCPSession(t *testing.T) {
	var deleted atomic.Int32
	provider := sdk.NewServer(&sdk.Implementation{Name: "test", Version: "1"}, nil)
	handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return provider }, nil)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deleted.Add(1)
		}
		if r.Method == http.MethodGet {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	cfg := config.DefaultTools()
	cfg.LocalDir = t.TempDir()
	reader := toolReaderStub{connections: []schema.ToolConnection{
		{TenantID: "t", UserID: "u", ID: "first", Protocol: "mcp", URL: server.URL},
		{TenantID: "t", UserID: "u", ID: "second", Protocol: "a2a", URL: server.URL},
	}}
	if _, _, _, err := loadTools(context.Background(), reader, cfg); err == nil || deleted.Load() != 1 {
		t.Fatal("failed startup did not close MCP session", err, deleted.Load())
	}
}

// TestExternalToolSmoke 只有部署者显式启用才真实执行一次指定工具。
func TestExternalToolSmoke(t *testing.T) {
	if os.Getenv("CAGENT_TEST_TOOLS") != "1" {
		t.Skip("external tool provider not configured")
	}
	uri, database := os.Getenv("CAGENT_TEST_TOOLS_MONGODB_URI"), os.Getenv("CAGENT_TEST_TOOLS_MONGODB_DATABASE")
	if uri == "" || database == "" {
		t.Fatal("CAGENT_TEST_TOOLS_MONGODB_URI and CAGENT_TEST_TOOLS_MONGODB_DATABASE are required")
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
	db, err := mongodb.Open(ctx, mongodb.Options{URI: uri, Database: database, Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal("database setup failed")
	}
	defer db.Close(context.Background())
	catalog, _, closeAll, err := loadTools(ctx, db, toolConfig(t))
	if err != nil {
		t.Fatal("provider setup failed")
	}
	defer closeAll(context.Background())
	call := toolCall(domain.Scope{TenantID: in.TenantID, UserID: in.UserID}, in.Protocol, in.Name, string(in.Arguments))
	executor, err := catalog.Resolve(ctx, call.Scope, call.Protocol, call.Name)
	if err != nil {
		t.Fatal("tool resolution failed")
	}
	out, err := executor.Execute(ctx, call)
	if err != nil || out.ValidateForCall(call) != nil {
		t.Fatal("provider call failed")
	}
	if out.Task != nil {
		t.Log("task handle returned; task lifecycle is not part of this smoke")
	}
}
