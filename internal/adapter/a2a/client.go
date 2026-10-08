// Package a2a 将官方 A2A Go SDK 适配到工具及已有任务接口。
package a2a

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"

	sdk "github.com/a2aproject/a2a-go/a2a"
	"github.com/a2aproject/a2a-go/a2aclient"
	"github.com/a2aproject/a2a-go/a2aclient/agentcard"
	"github.com/hjhsamuel/cagent/internal/adapter/toolhttp"
	"github.com/hjhsamuel/cagent/internal/apperrors"
	"github.com/hjhsamuel/cagent/internal/domain"
	"github.com/hjhsamuel/cagent/internal/tool"
)

type Client struct {
	connection *toolhttp.Connection
	client     *a2aclient.Client
	card       *sdk.AgentCard
}

func New(ctx context.Context, cfg toolhttp.Config, cardPath string) (*Client, error) {
	if !strings.HasPrefix(cardPath, "/") || strings.HasPrefix(cardPath, "//") || strings.ContainsAny(cardPath, "?#") {
		return nil, apperrors.ErrInvalidArgument
	}
	conn, err := toolhttp.New(cfg)
	if err != nil {
		return nil, err
	}
	card, err := agentcard.NewResolver(conn.HTTPClient()).Resolve(ctx, cfg.URL, agentcard.WithPath(cardPath))
	if err != nil {
		return nil, protocolError(err)
	}
	if card.ProtocolVersion != string(sdk.Version) || (card.PreferredTransport != "" && card.PreferredTransport != sdk.TransportProtocolJSONRPC) || card.URL != cfg.URL || card.Name == "" {
		return nil, apperrors.ErrUnsupported
	}
	// 只使用已验证的管理端点，不允许 SDK 选择 Card 中的其他地址。
	card.AdditionalInterfaces = nil
	card.PreferredTransport = sdk.TransportProtocolJSONRPC
	client, err := a2aclient.NewFromCard(ctx, card, a2aclient.WithDefaultsDisabled(), a2aclient.WithJSONRPCTransport(conn.HTTPClient()), a2aclient.WithConfig(a2aclient.Config{Polling: true}))
	if err != nil {
		return nil, protocolError(err)
	}
	return &Client{connection: conn, client: client, card: card}, nil
}
func (c *Client) Discover() tool.Descriptor {
	return tool.Descriptor{Name: c.card.Name, Description: c.card.Description, Protocol: domain.ToolA2A, InputSchema: []byte(`{"type":"object","properties":{"text":{"type":"string","minLength":1}},"required":["text"],"additionalProperties":false}`)}
}
func parts(in sdk.ContentParts) ([]domain.Part, error) {
	out := []domain.Part{}
	for _, p := range in {
		switch v := p.(type) {
		case sdk.TextPart:
			out = append(out, domain.Part{Kind: domain.PartText, Text: v.Text})
		case sdk.DataPart:
			raw, err := json.Marshal(v.Data)
			if err != nil {
				return nil, apperrors.ErrInvalidArgument
			}
			out = append(out, domain.Part{Kind: "data", MIMEType: "application/json", Data: raw})
		case sdk.FilePart:
			raw, err := json.Marshal(v.File)
			if err != nil {
				return nil, apperrors.ErrInvalidArgument
			}
			out = append(out, domain.Part{Kind: "data", MIMEType: "application/json", Data: raw})
		default:
			return nil, apperrors.ErrUnsupported
		}
	}
	return out, nil
}
func (c *Client) send(ctx context.Context, h *domain.TaskHandle, text, credential string) (sdk.SendMessageResult, error) {
	m := sdk.NewMessage(sdk.MessageRoleUser, sdk.TextPart{Text: text})
	if h != nil {
		m.TaskID = sdk.TaskID(h.RemoteID)
		m.ContextID = h.ContextID
	}
	response, err := c.client.SendMessage(toolhttp.WithCredentialRef(ctx, credential), &sdk.MessageSendParams{Message: m})
	return response, protocolError(err)
}
func (c *Client) Execute(ctx context.Context, call domain.ToolCall) (domain.ToolOutcome, error) {
	if err := c.connection.Check(call, nil, domain.ToolA2A); err != nil {
		return domain.ToolOutcome{}, err
	}
	if call.Name != c.card.Name {
		return domain.ToolOutcome{}, apperrors.ErrNotFound
	}
	var args struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(call.Arguments, &args) != nil || strings.TrimSpace(args.Text) == "" {
		return domain.ToolOutcome{}, apperrors.ErrInvalidArgument
	}
	response, err := c.send(ctx, nil, args.Text, "")
	if err != nil {
		return domain.ToolOutcome{}, err
	}
	switch v := response.(type) {
	case *sdk.Task:
		if v == nil || v.ID == "" || v.ContextID == "" {
			return domain.ToolOutcome{}, apperrors.ErrInvalidArgument
		}
		return domain.ToolOutcome{Task: &domain.TaskHandle{Protocol: domain.ToolA2A, ConnectionID: c.connection.ID(), RemoteID: string(v.ID), ContextID: v.ContextID}}, nil
	case *sdk.Message:
		if v == nil || v.Role != sdk.MessageRoleAgent || v.ID == "" {
			return domain.ToolOutcome{}, apperrors.ErrInvalidArgument
		}
		p, err := parts(v.Parts)
		return domain.ToolOutcome{Result: &domain.ToolResult{CallID: call.ID, Parts: p}}, err
	default:
		return domain.ToolOutcome{}, apperrors.ErrInvalidArgument
	}
}
func mapState(s sdk.TaskState) (domain.TaskStatus, error) {
	switch s {
	case sdk.TaskStateSubmitted:
		return domain.TaskSubmitted, nil
	case sdk.TaskStateWorking:
		return domain.TaskRunning, nil
	case sdk.TaskStateInputRequired:
		return domain.TaskInputRequired, nil
	case sdk.TaskStateAuthRequired:
		return domain.TaskAuthRequired, nil
	case sdk.TaskStateCompleted:
		return domain.TaskSucceeded, nil
	case sdk.TaskStateFailed:
		return domain.TaskFailed, nil
	case sdk.TaskStateCanceled:
		return domain.TaskCancelled, nil
	case sdk.TaskStateRejected:
		return domain.TaskRejected, nil
	default:
		return "", &toolhttp.UnknownState{Value: string(s)}
	}
}
func matches(t *sdk.Task, h domain.TaskHandle) bool {
	return t != nil && string(t.ID) == h.RemoteID && t.ContextID == h.ContextID
}
func (c *Client) Get(ctx context.Context, call domain.ToolCall, h domain.TaskHandle) (domain.TaskUpdate, error) {
	if err := c.connection.Check(call, &h, domain.ToolA2A); err != nil {
		return domain.TaskUpdate{}, err
	}
	t, err := c.client.GetTask(ctx, &sdk.TaskQueryParams{ID: sdk.TaskID(h.RemoteID)})
	if err != nil {
		return domain.TaskUpdate{}, protocolError(err)
	}
	if !matches(t, h) {
		return domain.TaskUpdate{}, apperrors.ErrInvalidArgument
	}
	status, err := mapState(t.Status.State)
	if err != nil {
		return domain.TaskUpdate{}, err
	}
	u := domain.TaskUpdate{Status: status}
	if t.Status.Message != nil {
		u.Progress, err = parts(t.Status.Message.Parts)
		if err != nil {
			return domain.TaskUpdate{}, err
		}
	}
	if status.IsTerminal() {
		r := &domain.ToolResult{CallID: call.ID}
		for _, a := range t.Artifacts {
			if a == nil {
				return domain.TaskUpdate{}, apperrors.ErrInvalidArgument
			}
			p, err := parts(a.Parts)
			if err != nil {
				return domain.TaskUpdate{}, err
			}
			r.Parts = append(r.Parts, p...)
		}
		if len(r.Parts) == 0 {
			r.Parts = append(r.Parts, u.Progress...)
		}
		if status != domain.TaskSucceeded {
			r.Error = "A2A task ended without success"
		}
		u.Result = r
	}
	return u, nil
}

// SDK 未暴露 SSE ID；非空旧游标明确拒绝。订阅通知只触发完整查询，避免乱序增量。
func (c *Client) Follow(ctx context.Context, call domain.ToolCall, h domain.TaskHandle, cursor string, emit func(domain.TaskUpdate) error) error {
	if err := c.connection.Check(call, &h, domain.ToolA2A); err != nil {
		return err
	}
	if !c.card.Capabilities.Streaming || cursor != "" {
		return apperrors.ErrUnsupported
	}
	if emit == nil {
		return apperrors.ErrInvalidArgument
	}
	for event, err := range c.client.ResubscribeToTask(ctx, &sdk.TaskIDParams{ID: sdk.TaskID(h.RemoteID)}) {
		if err != nil {
			return protocolError(err)
		}
		if event == nil {
			return apperrors.ErrInvalidArgument
		}
		info := event.TaskInfo()
		if string(info.TaskID) != h.RemoteID || info.ContextID != h.ContextID {
			return apperrors.ErrInvalidArgument
		}
		u, err := c.Get(ctx, call, h)
		if err != nil {
			return err
		}
		if err = emit(u); err != nil {
			return err
		}
		if u.Status.IsTerminal() {
			return nil
		}
	}
	// 连接结束不等于任务完成。
	return tool.SafeError(io.ErrUnexpectedEOF)
}
func (c *Client) Cancel(ctx context.Context, call domain.ToolCall, h domain.TaskHandle) error {
	if err := c.connection.Check(call, &h, domain.ToolA2A); err != nil {
		return err
	}
	t, err := c.client.CancelTask(ctx, &sdk.TaskIDParams{ID: sdk.TaskID(h.RemoteID)})
	if err != nil {
		return protocolError(err)
	}
	if !matches(t, h) {
		return apperrors.ErrInvalidArgument
	}
	return nil
}
func protocolError(err error) error {
	if err == nil {
		return nil
	}
	var kind apperrors.Kind
	switch {
	case errors.Is(err, sdk.ErrTaskNotFound):
		kind = apperrors.ErrNotFound
	case errors.Is(err, sdk.ErrTaskNotCancelable):
		kind = apperrors.ErrConflict
	case errors.Is(err, sdk.ErrMethodNotFound), errors.Is(err, sdk.ErrUnsupportedOperation), errors.Is(err, sdk.ErrUnsupportedContentType), errors.Is(err, sdk.ErrPushNotificationNotSupported):
		kind = apperrors.ErrUnsupported
	default:
		return tool.SafeError(err)
	}
	return apperrors.Wrap(kind, "a2a.operation", "A2A operation was rejected", tool.SafeError(err))
}
func (c *Client) Continue(ctx context.Context, call domain.ToolCall, h domain.TaskHandle, in tool.TaskInput) error {
	u, err := c.Get(ctx, call, h)
	if err != nil {
		return err
	}
	if !u.Status.IsPaused() {
		return apperrors.ErrConflict
	}
	if strings.TrimSpace(in.Text) == "" {
		return apperrors.ErrInvalidArgument
	}
	if u.Status == domain.TaskAuthRequired && in.CredentialRef == "" {
		return apperrors.ErrUnsupported
	}
	if u.Status == domain.TaskInputRequired && in.CredentialRef != "" {
		return apperrors.ErrInvalidArgument
	}
	response, err := c.send(ctx, &h, in.Text, in.CredentialRef)
	if err != nil {
		return err
	}
	t, ok := response.(*sdk.Task)
	if !ok || !matches(t, h) {
		return apperrors.ErrInvalidArgument
	}
	return nil
}

var _ tool.Executor = (*Client)(nil)
var _ tool.TaskClient = (*Client)(nil)
var _ tool.TaskInteractor = (*Client)(nil)
