package agent

import (
	"context"

	"github.com/hjhsamuel/cagent/internal/domain"
)

// TaskTracker 管理已经启动的工具任务，不负责调度或重试工具执行。
// 实现由运行执行器绑定当前租约和暂停检查点；不能通过公共 HTTP 构造 Track 凭证。
type TaskTracker interface {
	// Track 将原调用、句柄、检查点和等待事件原子保存，按完整调用身份去重。
	Track(context.Context, domain.ToolCall, domain.TaskHandle) (domain.Task, error)
	Get(context.Context, domain.Scope, string) (domain.Task, error)
	// Follow 只通知已经持久化的任务版本，回调同步施加背压；取消订阅不会取消任务。
	Follow(context.Context, domain.Scope, string, func(domain.Task) error) error
	Cancel(context.Context, domain.Scope, string) error
}
