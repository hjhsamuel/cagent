package bootstrap

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"time"

	"github.com/hjhsamuel/cagent/internal/adapter/a2a"
	"github.com/hjhsamuel/cagent/internal/adapter/mcp"
	"github.com/hjhsamuel/cagent/internal/adapter/toolhttp"
	"github.com/hjhsamuel/cagent/internal/apperrors"
	"github.com/hjhsamuel/cagent/internal/domain"
	"github.com/hjhsamuel/cagent/internal/tool"
)

// toolFile 仅从管理文件读取。凭据是环境变量引用，解析器按连接固定白名单，
// 所以 HTTP 的 credential_ref 无法读取模型密钥或其他用户连接的环境变量。
type toolFile struct {
	Timeout        string `json:"timeout"`
	MaxInputBytes  int    `json:"max_input_bytes"`
	MaxOutputBytes int    `json:"max_output_bytes"`
	MaxModelCalls  int    `json:"max_model_calls"`
	Connections    []struct {
		TenantID      string              `json:"tenant_id"`
		UserID        string              `json:"user_id"`
		ID            string              `json:"id"`
		Protocol      domain.ToolProtocol `json:"protocol"`
		URL           string              `json:"url"`
		CardPath      string              `json:"card_path"`
		CredentialRef string              `json:"credential_ref"`
		Credentials   map[string]string   `json:"credentials"`
		Tools         []string            `json:"tools"`
	} `json:"connections"`
}

func loadTools(ctx context.Context, path string) (*tool.Catalog, int, func(context.Context), error) {
	closeAll := func(context.Context) {}
	if path == "" {
		return nil, 0, closeAll, nil
	}
	f, e := os.Open(path)
	if e != nil {
		return nil, 0, closeAll, tool.SafeError(e)
	}
	defer f.Close()
	cfg := toolFile{Timeout: "30s", MaxInputBytes: 1 << 20, MaxOutputBytes: 1 << 20, MaxModelCalls: 16}
	dec := json.NewDecoder(io.LimitReader(f, 4<<20))
	dec.DisallowUnknownFields()
	if dec.Decode(&cfg) != nil {
		return nil, 0, closeAll, apperrors.ErrInvalidArgument
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		return nil, 0, closeAll, apperrors.ErrInvalidArgument
	}
	timeout, e := time.ParseDuration(cfg.Timeout)
	if e != nil || timeout <= 0 || cfg.MaxModelCalls < 1 || cfg.MaxModelCalls > 128 || cfg.MaxInputBytes <= 0 || cfg.MaxInputBytes > 4<<20 || cfg.MaxOutputBytes < 256 || cfg.MaxOutputBytes > 8<<20 {
		return nil, 0, closeAll, apperrors.ErrInvalidArgument
	}
	var closers []func(context.Context) error
	closeAll = func(ctx context.Context) {
		for i := len(closers) - 1; i >= 0; i-- {
			_ = closers[i](ctx)
		}
	}
	success := false
	defer func() {
		if !success {
			cleanup, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			closeAll(cleanup)
		}
	}()
	var entries []tool.Entry
	seen := map[string]bool{}
	for _, conn := range cfg.Connections {
		scope := domain.Scope{TenantID: conn.TenantID, UserID: conn.UserID}
		key, _ := json.Marshal([]string{scope.TenantID, scope.UserID, conn.ID})
		if seen[string(key)] || scope.Validate() != nil || strings.TrimSpace(conn.ID) == "" {
			return nil, 0, closeAll, apperrors.ErrInvalidArgument
		}
		seen[string(key)] = true
		credentialMap := conn.Credentials
		resolve := func(_ context.Context, s domain.Scope, ref string) (string, error) {
			env, ok := credentialMap[ref]
			if s != scope || !ok {
				return "", apperrors.ErrNotFound
			}
			v, ok := os.LookupEnv(env)
			if !ok || v == "" {
				return "", apperrors.ErrNotFound
			}
			return v, nil
		}
		remote := toolhttp.Config{Scope: scope, ID: conn.ID, URL: conn.URL, CredentialRef: conn.CredentialRef, Credentials: resolve, Timeout: timeout, MaxBytes: 16 << 20}
		var descriptors []tool.Descriptor
		var executor tool.Executor
		var tasks tool.TaskClient
		switch conn.Protocol {
		case domain.ToolLocal:
			descriptors = []tool.Descriptor{{Name: "echo", Protocol: domain.ToolLocal, Description: "返回输入文本，用于验证本地工具链路", InputSchema: []byte(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"],"additionalProperties":false}`)}}
			executor = tool.ExecutorFunc(func(_ context.Context, c domain.ToolCall) (domain.ToolOutcome, error) {
				var args struct {
					Text string `json:"text"`
				}
				if json.Unmarshal(c.Arguments, &args) != nil {
					return domain.ToolOutcome{}, apperrors.ErrInvalidArgument
				}
				return domain.ToolOutcome{Result: &domain.ToolResult{CallID: c.ID, Parts: []domain.Part{{Kind: domain.PartText, Text: args.Text}}}}, nil
			})
		case domain.ToolMCP:
			client, err := mcp.New(ctx, remote)
			if err != nil {
				return nil, 0, closeAll, err
			}
			closers = append(closers, client.Close)
			descriptors, err = client.Discover(ctx)
			if err != nil {
				return nil, 0, closeAll, err
			}
			executor = client
			tasks = client
		case domain.ToolA2A:
			path := conn.CardPath
			if path == "" {
				path = "/.well-known/agent-card.json"
			}
			client, err := a2a.New(ctx, remote, path)
			if err != nil {
				return nil, 0, closeAll, err
			}
			descriptors = []tool.Descriptor{client.Discover()}
			executor = client
			tasks = client
		default:
			return nil, 0, closeAll, apperrors.ErrUnsupported
		}
		allowed := map[string]bool{}
		for _, name := range conn.Tools {
			allowed[name] = true
		}
		for _, d := range descriptors {
			if len(allowed) > 0 && !allowed[d.Name] {
				continue
			}
			entries = append(entries, tool.Entry{Scope: scope, Descriptor: d, ConnectionID: conn.ID, Executor: executor, Tasks: tasks})
		}
	}
	catalog, e := tool.NewCatalog(entries, tool.Limits{Timeout: timeout, MaxInputBytes: cfg.MaxInputBytes, MaxOutputBytes: cfg.MaxOutputBytes})
	if e != nil {
		return nil, 0, closeAll, e
	}
	success = true
	return catalog, cfg.MaxModelCalls, closeAll, nil
}
