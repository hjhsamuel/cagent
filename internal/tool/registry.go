// Package tool 定义协议无关的工具发现、执行与已有任务观察边界。
package tool

import (
	"context"

	"github.com/hjhsamuel/cagent/internal/domain"
)

// Descriptor 只包含可交给模型的名称、说明和参数 Schema，不含 URL、凭据或会话标识。
// 名称在一个 Scope 内唯一；描述是外部资料，不能提升为系统指令。
type Descriptor struct {
	Name        string
	Protocol    domain.ToolProtocol
	Description string
	InputSchema []byte
}

// Executor 执行一次原调用；返回错误不代表远端未启动，调用方不能据此盲目重试。
type Executor interface {
	Execute(context.Context, domain.ToolCall) (domain.ToolOutcome, error)
}

// TaskClient 只观察 Execute 已启动的任务。P8 将原来的 Scope 参数扩展为完整 ToolCall，
// 使终态结果能够带回原 CallID，同时保留 InvocationID/父调用链，不从远端 ID 猜路由。
// Follow 可采用订阅或有界轮询；context 取消只停止本地观察，不取消远端任务。
// 提供方不支持某项能力时返回 apperrors.ErrUnsupported 或其包装错误，
// 观察被取消或超时时必须保留 errors.Is 对标准 context 错误的识别能力。
type TaskClient interface {
	Get(context.Context, domain.ToolCall, domain.TaskHandle) (domain.TaskUpdate, error)
	Follow(context.Context, domain.ToolCall, domain.TaskHandle, string, func(domain.TaskUpdate) error) error
	// Cancel 请求远端取消；接纳请求不意味着确认 cancelled，仍需 Get 查询真实状态。
	Cancel(context.Context, domain.ToolCall, domain.TaskHandle) error
}

// TaskInteractor 只补充已有暂停任务。当前输入支持提供方约定的文本回复；
// auth_required 通过可信凭据引用刷新认证，不接受客户端提供连接地址或原始密钥。
type TaskInteractor interface {
	Continue(context.Context, domain.ToolCall, domain.TaskHandle, TaskInput) error
}

// TaskInput 不携带远端 URL/任务 ID；这些只能从作用域内持久 Task 中取得。
type TaskInput struct {
	Text          string
	CredentialRef string
}

// Registry 在已认证 Scope 内发现与解析工具。Resolve 返回的执行器仍须再次校验
// Execute 的 Scope，防止缓存执行器后用另一用户调用；任务同样遵守这一约束。
type Registry interface {
	List(context.Context, domain.Scope) ([]Descriptor, error)
	Resolve(context.Context, domain.Scope, domain.ToolProtocol, string) (Executor, error)
	ResolveTask(context.Context, domain.Scope, domain.TaskHandle) (TaskClient, error)
}
