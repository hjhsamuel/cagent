package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/hjhsamuel/cagent/internal/apperrors"
	"github.com/hjhsamuel/cagent/internal/domain"
	"github.com/hjhsamuel/cagent/internal/observability"
)

// Entry 由可信装配代码创建，默认只授予一个完整 Scope；SharedLocal 仅用于共享本地工具。
// Executor 不得忽略 context；Catalog 不用遗留后台 goroutine 伪装超时，
// 也不会自动重试有副作用的调用。
type Entry struct {
	SharedLocal  bool
	Scope        domain.Scope
	Descriptor   Descriptor
	ConnectionID string
	Executor     Executor
	Tasks        TaskClient
}

// ArtifactWriter 保存完整大结果，返回授权作用域内的不透明引用。未配置时明确拒绝
// 超大结果，不截断 JSON、不把内容伪装成成功。实现负责持久性及下载授权。
type ArtifactWriter interface {
	Put(context.Context, domain.Scope, []byte) (string, error)
}

// Limits 同时约束本地与协议工具；超时依赖工具响应 context，输入/结果按 JSON 字节计。
type Limits struct {
	Timeout                       time.Duration
	MaxInputBytes, MaxOutputBytes int
	Artifacts                     ArtifactWriter
}

// Catalog 构造后只读，可并发共享。执行器/凭据解析器的并发安全由各自实现保证；
// 本结构不保存用户请求的可变状态，不按进程内锁推断 Run 的跨实例所有权。
type Catalog struct {
	entries []Entry
	schemas []*jsonschema.Resolved
	limits  Limits
}

// NewCatalog 创建不可变注册快照；同作用域工具名全局唯一，避免模型无法区分协议。
// JSON Schema 在启动时解析，本地/远端工具经过同一个参数、授权与返回值边界。
func NewCatalog(entries []Entry, limits Limits) (*Catalog, error) {
	if limits.Timeout <= 0 || limits.MaxInputBytes <= 0 || limits.MaxOutputBytes <= 0 {
		return nil, bad("tool.limits")
	}
	c := &Catalog{limits: limits}
	seen := map[string]bool{}
	sharedNames := map[string]bool{}
	scopedNames := map[string]bool{}
	for _, e := range entries {
		validScope := e.Scope.Validate() == nil
		if e.SharedLocal {
			validScope = e.Scope == (domain.Scope{}) && e.Descriptor.Protocol == domain.ToolLocal && e.Tasks == nil
		}
		if !validScope || e.Descriptor.Protocol.Validate() != nil || strings.TrimSpace(e.Descriptor.Name) == "" || e.Executor == nil || strings.TrimSpace(e.ConnectionID) == "" {
			return nil, bad("tool.entry")
		}
		if e.SharedLocal {
			if sharedNames[e.Descriptor.Name] || scopedNames[e.Descriptor.Name] {
				return nil, apperrors.ErrConflict
			}
			sharedNames[e.Descriptor.Name] = true
		} else {
			if sharedNames[e.Descriptor.Name] {
				return nil, apperrors.ErrConflict
			}
			scopedNames[e.Descriptor.Name] = true
		}
		keyData, _ := json.Marshal([]string{e.Scope.TenantID, e.Scope.UserID, e.Descriptor.Name})
		key := string(keyData)
		if seen[key] {
			return nil, apperrors.ErrConflict
		}
		seen[key] = true
		var s jsonschema.Schema
		if json.Unmarshal(e.Descriptor.InputSchema, &s) != nil || s.Type != "object" {
			return nil, bad("tool.schema")
		}
		resolved, err := s.Resolve(nil)
		if err != nil {
			return nil, bad("tool.schema")
		}
		e.Descriptor.InputSchema = append([]byte(nil), e.Descriptor.InputSchema...)
		c.entries = append(c.entries, e)
		c.schemas = append(c.schemas, resolved)
	}
	return c, nil
}
func bad(field string) error {
	return apperrors.New(apperrors.ErrInvalidArgument, field, "invalid tool request")
}

// List 返回当前用户可见的独立描述副本，稳定排序方便模型请求与测试复现。
func (c *Catalog) List(ctx context.Context, s domain.Scope) ([]Descriptor, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := s.Validate(); err != nil {
		return nil, err
	}
	out := []Descriptor{}
	for _, e := range c.entries {
		if e.SharedLocal || e.Scope == s {
			d := e.Descriptor
			d.InputSchema = append([]byte(nil), d.InputSchema...)
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Resolve 对不存在和无权限的名称统一返回 NotFound，不暴露其他作用域的工具清单。
func (c *Catalog) Resolve(ctx context.Context, s domain.Scope, p domain.ToolProtocol, name string) (Executor, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := s.Validate(); err != nil {
		return nil, err
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	for i, e := range c.entries {
		if (e.SharedLocal || e.Scope == s) && e.Descriptor.Protocol == p && e.Descriptor.Name == name {
			return &guarded{c: c, index: i, scope: s}, nil
		}
	}
	return nil, apperrors.ErrNotFound
}

// ResolveTask 只解析可信连接引用；句柄必须来自调用方先按 Scope 读取的持久任务。
// 包装器对后续每次操作重验完整调用和句柄，不能用已解析客户端跨用户查询。
func (c *Catalog) ResolveTask(ctx context.Context, s domain.Scope, h domain.TaskHandle) (TaskClient, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := s.Validate(); err != nil {
		return nil, err
	}
	if err := h.Validate(); err != nil {
		return nil, err
	}
	for _, e := range c.entries {
		if e.Scope == s && e.ConnectionID == h.ConnectionID && e.Descriptor.Protocol == h.Protocol {
			if e.Tasks == nil {
				return nil, apperrors.ErrUnsupported
			}
			return &taskGuard{entry: e, handle: h, limits: c.limits, catalog: c}, nil
		}
	}
	return nil, apperrors.ErrNotFound
}

type guarded struct {
	c     *Catalog
	index int
	scope domain.Scope
}

func (g *guarded) Execute(ctx context.Context, call domain.ToolCall) (result domain.ToolOutcome, resultErr error) {
	ctx, finish := observability.Default.Start(ctx, "tool")
	defer func() { finish(resultErr) }()
	e := g.c.entries[g.index]
	if err := call.Validate(); err != nil {
		return domain.ToolOutcome{}, err
	}
	if call.Scope != g.scope || call.Protocol != e.Descriptor.Protocol || call.Name != e.Descriptor.Name {
		return domain.ToolOutcome{}, apperrors.ErrNotFound
	}
	if len(call.Arguments) > g.c.limits.MaxInputBytes {
		return domain.ToolOutcome{}, bad("tool.input_size")
	}
	var args map[string]any
	if json.Unmarshal(call.Arguments, &args) != nil || args == nil || g.c.schemas[g.index].Validate(args) != nil {
		return domain.ToolOutcome{}, bad("tool.arguments")
	}
	ctx, cancel := context.WithTimeout(ctx, g.c.limits.Timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return domain.ToolOutcome{}, err
	}
	call.Arguments = append([]byte(nil), call.Arguments...)
	out, err := e.Executor.Execute(ctx, call)
	if err != nil {
		return domain.ToolOutcome{}, SafeError(err)
	}
	if err = ctx.Err(); err != nil {
		return domain.ToolOutcome{}, err
	}
	if err = out.ValidateForCall(call); err != nil {
		return domain.ToolOutcome{}, err
	}
	if out.Task != nil && out.Task.ConnectionID != e.ConnectionID {
		return domain.ToolOutcome{}, bad("tool.connection")
	}
	if out.Result != nil {
		out.Result, err = LimitResult(ctx, call.Scope, out.Result, g.c.limits)
		if err != nil {
			return domain.ToolOutcome{}, err
		}
	}
	// 深拷贝切断本地执行器复用缓冲区和后续调用之间的共享关系。
	data, _ := json.Marshal(out)
	var copy domain.ToolOutcome
	_ = json.Unmarshal(data, &copy)
	return copy, nil
}

// LimitResult 保留工具业务错误和完整内容。超限时只接受 ArtifactWriter 的小引用，
// 没有持久 artifact 实现或引用自身仍超限则失败，不生成被截断却看似成功的结果。
func LimitResult(ctx context.Context, s domain.Scope, r *domain.ToolResult, l Limits) (*domain.ToolResult, error) {
	data, err := json.Marshal(r)
	if err != nil {
		return nil, bad("tool.result")
	}
	if len(data) <= l.MaxOutputBytes {
		return r, nil
	}
	if l.Artifacts == nil {
		return nil, bad("tool.output_size")
	}
	uri, err := l.Artifacts.Put(ctx, s, data)
	if err != nil {
		return nil, SafeError(err)
	}
	if strings.TrimSpace(uri) == "" {
		return nil, bad("tool.artifact")
	}
	result := &domain.ToolResult{CallID: r.CallID, Parts: []domain.Part{{Kind: "resource", URI: uri, MIMEType: "application/json"}}}
	if r.Error != "" {
		result.Error = "tool reported an error; see artifact"
	}
	encoded, _ := json.Marshal(result)
	if len(encoded) > l.MaxOutputBytes {
		return nil, bad("tool.artifact_size")
	}
	return result, nil
}

// ExecutorFunc 便于注册本地工具；同样经过 Catalog 的调用前后校验。
type ExecutorFunc func(context.Context, domain.ToolCall) (domain.ToolOutcome, error)

func (f ExecutorFunc) Execute(ctx context.Context, c domain.ToolCall) (domain.ToolOutcome, error) {
	return f(ctx, c)
}

var ErrProvider = errors.New("tool provider operation failed")

type providerError struct{ cause error }

func (e *providerError) Error() string              { return ErrProvider.Error() }
func (e *providerError) Unwrap() error              { return e.cause }
func (e *providerError) Format(s fmt.State, _ rune) { fmt.Fprint(s, e.Error()) }

// SafeError 不回显远端 URL/错误体；取消与超时保留标准 errors.Is 语义。
func SafeError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return &providerError{err}
}

type taskGuard struct {
	catalog *Catalog
	entry   Entry
	handle  domain.TaskHandle
	limits  Limits
}

func (g *taskGuard) check(c domain.ToolCall, h domain.TaskHandle) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if c.Scope != g.entry.Scope || c.Protocol != g.entry.Descriptor.Protocol || h != g.handle {
		return apperrors.ErrNotFound
	}
	allowed := false
	for _, e := range g.catalog.entries {
		if e.Scope == c.Scope && e.ConnectionID == h.ConnectionID && e.Descriptor.Protocol == c.Protocol && e.Descriptor.Name == c.Name {
			allowed = true
			break
		}
	}
	if !allowed {
		return apperrors.ErrNotFound
	}
	return h.ValidateForCall(c)
}
func (g *taskGuard) Get(ctx context.Context, c domain.ToolCall, h domain.TaskHandle) (domain.TaskUpdate, error) {
	if err := g.check(c, h); err != nil {
		return domain.TaskUpdate{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, g.limits.Timeout)
	defer cancel()
	u, err := g.entry.Tasks.Get(ctx, c, h)
	if err != nil {
		return domain.TaskUpdate{}, SafeError(err)
	}
	if err = ctx.Err(); err != nil {
		return domain.TaskUpdate{}, err
	}
	if err = u.Status.Validate(); err != nil {
		return domain.TaskUpdate{}, err
	}
	if u.Result != nil {
		if !u.Status.IsTerminal() {
			return domain.TaskUpdate{}, bad("task.result")
		}
		if err = u.Result.ValidateForCall(c); err != nil {
			return domain.TaskUpdate{}, err
		}
		u.Result, err = LimitResult(ctx, c.Scope, u.Result, g.limits)
	}
	if err != nil {
		return domain.TaskUpdate{}, err
	}
	progress, _ := json.Marshal(u.Progress)
	if len(progress) > g.limits.MaxOutputBytes {
		return domain.TaskUpdate{}, bad("task.progress_size")
	}
	data, _ := json.Marshal(u)
	var copy domain.TaskUpdate
	_ = json.Unmarshal(data, &copy)
	return copy, nil
}
func (g *taskGuard) Follow(ctx context.Context, c domain.ToolCall, h domain.TaskHandle, cursor string, emit func(domain.TaskUpdate) error) error {
	if err := g.check(c, h); err != nil {
		return err
	}
	if emit == nil {
		return bad("task.emit")
	}
	// 通知只是唤醒信号。重新读取完整快照，避免重复或乱序增量覆盖已确认状态。
	return g.entry.Tasks.Follow(ctx, c, h, cursor, func(notification domain.TaskUpdate) error {
		u, err := g.Get(ctx, c, h)
		if err != nil {
			return err
		}
		u.Cursor = notification.Cursor
		return emit(u)
	})
}
func (g *taskGuard) Cancel(ctx context.Context, c domain.ToolCall, h domain.TaskHandle) error {
	if err := g.check(c, h); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, g.limits.Timeout)
	defer cancel()
	return SafeError(g.entry.Tasks.Cancel(ctx, c, h))
}
func (g *taskGuard) Continue(ctx context.Context, c domain.ToolCall, h domain.TaskHandle, in TaskInput) error {
	if err := g.check(c, h); err != nil {
		return err
	}
	client, ok := g.entry.Tasks.(TaskInteractor)
	if !ok {
		return apperrors.ErrUnsupported
	}
	ctx, cancel := context.WithTimeout(ctx, g.limits.Timeout)
	defer cancel()
	return SafeError(client.Continue(ctx, c, h, in))
}
