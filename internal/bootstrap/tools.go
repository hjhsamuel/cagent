package bootstrap

import (
	"context"
	"encoding/json"
	"os"
	"strings"

	"github.com/hjhsamuel/cagent/internal/adapter/a2a"
	"github.com/hjhsamuel/cagent/internal/adapter/local"
	"github.com/hjhsamuel/cagent/internal/adapter/mcp"
	"github.com/hjhsamuel/cagent/internal/adapter/toolhttp"
	"github.com/hjhsamuel/cagent/internal/apperrors"
	"github.com/hjhsamuel/cagent/internal/config"
	"github.com/hjhsamuel/cagent/internal/domain"
	"github.com/hjhsamuel/cagent/internal/storage/schema"
	"github.com/hjhsamuel/cagent/internal/tool"
)

type toolConnectionReader interface {
	ListToolConnections(context.Context) ([]schema.ToolConnection, error)
}

// loadTools 从 MongoDB 读取远端连接，独立全量扫描本地目录。
// 本地工具对所有有效 Scope 可见；远端连接仍只授权文档指定的完整 Scope。
func loadTools(ctx context.Context, db toolConnectionReader, cfg config.Tools) (*tool.Catalog, int, func(context.Context), error) {
	closeAll := func(context.Context) {}
	if err := cfg.Validate(); err != nil {
		return nil, 0, closeAll, err
	}
	connections, err := db.ListToolConnections(ctx)
	if err != nil {
		return nil, 0, closeAll, err
	}
	seen := map[string]bool{}
	for _, conn := range connections {
		scope := domain.Scope{TenantID: conn.TenantID, UserID: conn.UserID}
		key, _ := json.Marshal([]string{scope.TenantID, scope.UserID, conn.ID})
		if seen[string(key)] || scope.Validate() != nil || strings.TrimSpace(conn.ID) == "" || strings.TrimSpace(conn.URL) == "" {
			return nil, 0, closeAll, apperrors.ErrInvalidArgument
		}
		if conn.Protocol != string(domain.ToolMCP) && conn.Protocol != string(domain.ToolA2A) {
			return nil, 0, closeAll, apperrors.ErrUnsupported
		}
		seen[string(key)] = true
	}
	definitions, err := local.Load(ctx, local.Config{ToolsDir: cfg.LocalDir}, nil, local.Limits{Timeout: cfg.Timeout, MaxOutputBytes: cfg.MaxOutputBytes})
	if err != nil {
		return nil, 0, closeAll, err
	}
	var entries []tool.Entry
	for _, d := range definitions {
		entries = append(entries, tool.Entry{SharedLocal: true, ConnectionID: "local", Descriptor: d.Descriptor, Executor: d.Executor})
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
			cleanup, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
			defer cancel()
			closeAll(cleanup)
		}
	}()
	for _, conn := range connections {
		scope := domain.Scope{TenantID: conn.TenantID, UserID: conn.UserID}
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
		remote := toolhttp.Config{Scope: scope, ID: conn.ID, URL: conn.URL, CredentialRef: conn.CredentialRef, Credentials: resolve, Timeout: cfg.Timeout, MaxBytes: 16 << 20}
		var descriptors []tool.Descriptor
		var executor tool.Executor
		var tasks tool.TaskClient
		switch domain.ToolProtocol(conn.Protocol) {
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
			executor, tasks = client, client
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
			executor, tasks = client, client
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
	catalog, err := tool.NewCatalog(entries, tool.Limits{Timeout: cfg.Timeout, MaxInputBytes: cfg.MaxInputBytes, MaxOutputBytes: cfg.MaxOutputBytes})
	if err != nil {
		return nil, 0, closeAll, err
	}
	success = true
	return catalog, cfg.MaxModelCalls, closeAll, nil
}
