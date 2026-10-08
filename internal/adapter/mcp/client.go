// Package mcp 将官方 MCP Go SDK 的工具能力适配到领域接口。
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/hjhsamuel/cagent/internal/adapter/toolhttp"
	"github.com/hjhsamuel/cagent/internal/apperrors"
	"github.com/hjhsamuel/cagent/internal/domain"
	"github.com/hjhsamuel/cagent/internal/tool"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const Version = "2025-11-25"

type Client struct {
	connection *toolhttp.Connection
	session    *sdk.ClientSession
	mu         sync.RWMutex
	names      map[string]bool
	closed     bool
}

func New(ctx context.Context, cfg toolhttp.Config) (*Client, error) {
	conn, err := toolhttp.New(cfg)
	if err != nil {
		return nil, err
	}
	client := sdk.NewClient(&sdk.Implementation{Name: "cagent", Version: "1"}, &sdk.ClientOptions{
		Capabilities: &sdk.ClientCapabilities{}, MultiRoundTrip: &sdk.MultiRoundTripOptions{Disabled: true},
	})
	session, err := client.Connect(ctx, &sdk.StreamableClientTransport{
		Endpoint: cfg.URL, HTTPClient: conn.HTTPClient(), DisableStandaloneSSE: true, MaxRetries: -1, MaxEventSize: int(cfg.MaxBytes),
	}, &sdk.ClientSessionOptions{ProtocolVersion: Version})
	if err != nil {
		return nil, protocolError(err)
	}
	init := session.InitializeResult()
	if init.ProtocolVersion != Version || init.Capabilities == nil || init.Capabilities.Tools == nil {
		_ = session.Close()
		return nil, apperrors.ErrUnsupported
	}
	return &Client{connection: conn, session: session, names: map[string]bool{}}, nil
}
func (c *Client) ready() error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.closed {
		return apperrors.ErrConflict
	}
	return nil
}

// SDK 负责结束本地连接及有界 DELETE；本适配器不创建 MCP 任务。
func (c *Client) Close(context.Context) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()
	return protocolError(c.session.Close())
}
func (c *Client) Discover(ctx context.Context) ([]tool.Descriptor, error) {
	if err := c.ready(); err != nil {
		return nil, err
	}
	out := []tool.Descriptor{}
	names, cursors := map[string]bool{}, map[string]bool{}
	cursor := ""
	for range 100 {
		page, err := c.session.ListTools(ctx, &sdk.ListToolsParams{Cursor: cursor})
		if err != nil {
			return nil, protocolError(err)
		}
		for _, t := range page.Tools {
			if t == nil || t.Name == "" || names[t.Name] {
				return nil, apperrors.ErrInvalidArgument
			}
			schema, err := json.Marshal(t.InputSchema)
			if err != nil {
				return nil, apperrors.ErrInvalidArgument
			}
			names[t.Name] = true
			out = append(out, tool.Descriptor{Name: t.Name, Description: t.Description, Protocol: domain.ToolMCP, InputSchema: schema})
		}
		if page.NextCursor == "" {
			c.mu.Lock()
			c.names = names
			c.mu.Unlock()
			return out, nil
		}
		if cursors[page.NextCursor] {
			return nil, apperrors.ErrInvalidArgument
		}
		cursors[page.NextCursor] = true
		cursor = page.NextCursor
	}
	return nil, apperrors.ErrInvalidArgument
}
func (c *Client) Execute(ctx context.Context, call domain.ToolCall) (domain.ToolOutcome, error) {
	if err := c.ready(); err != nil {
		return domain.ToolOutcome{}, err
	}
	if err := c.connection.Check(call, nil, domain.ToolMCP); err != nil {
		return domain.ToolOutcome{}, err
	}
	c.mu.RLock()
	known := c.names[call.Name]
	c.mu.RUnlock()
	if !known {
		return domain.ToolOutcome{}, apperrors.ErrNotFound
	}
	var args map[string]any
	if json.Unmarshal(call.Arguments, &args) != nil || args == nil {
		return domain.ToolOutcome{}, apperrors.ErrInvalidArgument
	}
	result, err := c.session.CallTool(ctx, &sdk.CallToolParams{Name: call.Name, Arguments: json.RawMessage(call.Arguments)})
	if err != nil {
		return domain.ToolOutcome{}, protocolError(err)
	}
	r, err := convertResult(call.ID, result)
	return domain.ToolOutcome{Result: r}, err
}
func convertResult(id string, v *sdk.CallToolResult) (*domain.ToolResult, error) {
	if v == nil || (len(v.Content) == 0 && v.StructuredContent == nil) {
		return nil, apperrors.ErrInvalidArgument
	}
	r := &domain.ToolResult{CallID: id}
	for _, content := range v.Content {
		switch p := content.(type) {
		case *sdk.TextContent:
			r.Parts = append(r.Parts, domain.Part{Kind: domain.PartText, Text: p.Text})
		case *sdk.ResourceLink:
			r.Parts = append(r.Parts, domain.Part{Kind: "resource", URI: p.URI, MIMEType: p.MIMEType})
		default:
			raw, err := json.Marshal(content)
			if err != nil {
				return nil, apperrors.ErrInvalidArgument
			}
			r.Parts = append(r.Parts, domain.Part{Kind: "data", MIMEType: "application/json", Data: raw})
		}
	}
	if v.StructuredContent != nil {
		raw, err := json.Marshal(v.StructuredContent)
		if err != nil {
			return nil, apperrors.ErrInvalidArgument
		}
		r.Parts = append(r.Parts, domain.Part{Kind: "data", MIMEType: "application/json", Data: raw})
	}
	if v.IsError {
		r.Error = "MCP tool reported an error"
	}
	return r, nil
}

// SDK v1.8.0 未提供 tasks API。旧持久句柄不能在新会话重新执行或假装成功。
func (c *Client) taskUnsupported(call domain.ToolCall, h domain.TaskHandle) error {
	if err := c.ready(); err != nil {
		return err
	}
	if err := c.connection.Check(call, &h, domain.ToolMCP); err != nil {
		return err
	}
	return apperrors.ErrUnsupported
}
func (c *Client) Get(_ context.Context, call domain.ToolCall, h domain.TaskHandle) (domain.TaskUpdate, error) {
	return domain.TaskUpdate{}, c.taskUnsupported(call, h)
}
func (c *Client) Follow(_ context.Context, call domain.ToolCall, h domain.TaskHandle, _ string, _ func(domain.TaskUpdate) error) error {
	return c.taskUnsupported(call, h)
}
func (c *Client) Cancel(_ context.Context, call domain.ToolCall, h domain.TaskHandle) error {
	return c.taskUnsupported(call, h)
}
func protocolError(err error) error {
	var rpc *jsonrpc.Error
	if errors.As(err, &rpc) && rpc.Code == -32601 {
		return apperrors.Wrap(apperrors.ErrUnsupported, "mcp.operation", "MCP operation was rejected", tool.SafeError(err))
	}
	return tool.SafeError(err)
}

var _ tool.Executor = (*Client)(nil)
var _ tool.TaskClient = (*Client)(nil)
