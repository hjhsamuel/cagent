// Package app 编排作用域内会话、运行生命周期及持久事件流，HTTP 层不得绕过本边界。
package app

import (
	"context"

	"github.com/hjhsamuel/cagent/internal/domain"
)

// StartRun 的 Scope 必须来自认证结果；客户端不指定资源 ID、版本或序号。
// 非空幂等键的范围为 Scope+SessionID，同键不同输入必须冲突。
type StartRun struct {
	Scope          domain.Scope
	SessionID      string
	Input          []domain.Part
	IdempotencyKey string
}

// Service 是 HTTP 等传输适配器的应用边界；取消订阅不等于 CancelRun。
// 生命周期管理由具体 Application 的 Close/RecoverRun 暴露给可信启动装配。
type Service interface {
	CreateSession(context.Context, domain.Scope, string) (domain.Session, error)
	CreateSessionWithModel(context.Context, domain.Scope, string, string) (domain.Session, error)
	GetSession(context.Context, domain.Scope, string) (domain.Session, error)
	StartRun(context.Context, StartRun) (domain.Run, error)
	GetRun(context.Context, domain.Scope, string) (domain.Run, error)
	CancelRun(context.Context, domain.Scope, string) error
}
