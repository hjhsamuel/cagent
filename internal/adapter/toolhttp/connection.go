// Package toolhttp 只提供官方 SDK 共用的 HTTP 安全策略和作用域校验。
// 不实现 JSON-RPC、SSE、协议握手或协议客户端。
package toolhttp

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/hjhsamuel/cagent/internal/apperrors"
	"github.com/hjhsamuel/cagent/internal/domain"
	"github.com/hjhsamuel/cagent/internal/tool"
)

type Credentials func(context.Context, domain.Scope, string) (string, error)

// MaxBytes 限制单次响应总字节数（包括订阅流）；协议解析由官方 SDK 完成。
type Config struct {
	Scope                  domain.Scope
	ID, URL, CredentialRef string
	Credentials            Credentials
	Timeout                time.Duration
	MaxBytes               int64
	HTTPClient             *http.Client
}
type Connection struct {
	cfg  Config
	http *http.Client
}

func New(cfg Config) (*Connection, error) {
	u, err := url.Parse(cfg.URL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || cfg.Scope.Validate() != nil || strings.TrimSpace(cfg.ID) == "" || cfg.Timeout <= 0 || cfg.MaxBytes <= 0 || cfg.MaxBytes > 64<<20 || cfg.CredentialRef != "" && cfg.Credentials == nil {
		return nil, apperrors.ErrInvalidArgument
	}
	h := &http.Client{}
	if cfg.HTTPClient != nil {
		*h = *cfg.HTTPClient
	}
	base := h.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	h.Transport = &transport{base: base, cfg: cfg, endpoint: u}
	h.Timeout = cfg.Timeout
	h.Jar = nil
	h.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Connection{cfg: cfg, http: h}, nil
}
func (c *Connection) HTTPClient() *http.Client { return c.http }
func (c *Connection) ID() string               { return c.cfg.ID }
func (c *Connection) Check(call domain.ToolCall, h *domain.TaskHandle, p domain.ToolProtocol) error {
	if err := call.Validate(); err != nil {
		return err
	}
	if call.Scope != c.cfg.Scope || call.Protocol != p {
		return apperrors.ErrNotFound
	}
	if h != nil {
		if err := h.ValidateForCall(call); err != nil {
			return err
		}
		if h.ConnectionID != c.cfg.ID {
			return apperrors.ErrNotFound
		}
	}
	return nil
}

type credentialKey struct{}

// WithCredentialRef 仅传递引用；每次请求重新解析，不修改连接共享状态。
func WithCredentialRef(ctx context.Context, ref string) context.Context {
	return context.WithValue(ctx, credentialKey{}, ref)
}

type transport struct {
	base     http.RoundTripper
	cfg      Config
	endpoint *url.URL
}

func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != t.endpoint.Scheme || req.URL.Host != t.endpoint.Host || req.URL.User != nil {
		return nil, apperrors.ErrInvalidArgument
	}
	r := req.Clone(req.Context())
	r.Header.Del("Cookie")
	r.Header.Del("Authorization")
	ref := t.cfg.CredentialRef
	if override, _ := r.Context().Value(credentialKey{}).(string); override != "" {
		ref = override
	}
	if ref != "" {
		if t.cfg.Credentials == nil {
			return nil, apperrors.ErrUnsupported
		}
		secret, err := t.cfg.Credentials(r.Context(), t.cfg.Scope, ref)
		if err != nil {
			return nil, tool.SafeError(err)
		}
		if secret == "" || strings.ContainsAny(secret, "\r\n") {
			return nil, apperrors.ErrInvalidArgument
		}
		r.Header.Set("Authorization", secret)
	}
	resp, err := t.base.RoundTrip(r)
	if err != nil {
		return nil, tool.SafeError(err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		resp.Body.Close()
		return nil, tool.SafeError(fmt.Errorf("remote HTTP status %d", resp.StatusCode))
	}
	resp.Body = &boundedBody{ReadCloser: resp.Body, remaining: t.cfg.MaxBytes}
	return resp, nil
}

type boundedBody struct {
	io.ReadCloser
	remaining int64
}

func (b *boundedBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if b.remaining == 0 {
		var probe [1]byte
		n, err := b.ReadCloser.Read(probe[:])
		if n > 0 {
			return 0, apperrors.ErrInvalidArgument
		}
		return 0, err
	}
	if int64(len(p)) > b.remaining {
		p = p[:b.remaining]
	}
	n, err := b.ReadCloser.Read(p)
	b.remaining -= int64(n)
	return n, err
}

// UnknownState 保留内部诊断值，对外格式化不包含提供方文本。
type UnknownState struct{ Value string }

func (e *UnknownState) Error() string              { return "unknown provider task state" }
func (e *UnknownState) Format(s fmt.State, _ rune) { fmt.Fprint(s, e.Error()) }
