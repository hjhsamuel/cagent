package app

import (
	"context"
	"strings"

	"github.com/hjhsamuel/cagent/internal/adapter/mongodb"
	"github.com/hjhsamuel/cagent/internal/apperrors"
	"github.com/hjhsamuel/cagent/internal/domain"
	"github.com/hjhsamuel/cagent/internal/tool"
)

// TaskInputs 使用本地 Task ID 加载完整原调用，客户端不能提交远端句柄、RunID 或 Scope。
// 只转交暂停交互；P9 观察器负责确认终态并原子消费，成功响应不表示任务完成。
type TaskInputs struct {
	DB       *mongodb.Database
	Registry tool.Registry
}

func (s TaskInputs) Submit(ctx context.Context, scope domain.Scope, id string, in tool.TaskInput, authorization bool) error {
	if s.DB == nil || s.Registry == nil {
		return apperrors.ErrUnsupported
	}
	if strings.TrimSpace(in.Text) == "" || authorization != (in.CredentialRef != "") {
		return apperrors.ErrInvalidArgument
	}
	task, e := s.DB.GetTask(ctx, scope, id)
	if e != nil {
		return e
	}
	run, e := s.DB.GetRun(ctx, scope, task.Call.RunID)
	if e != nil {
		return e
	}
	if run.Status.IsTerminal() {
		return apperrors.ErrConflict
	}
	client, e := s.Registry.ResolveTask(ctx, scope, task.Handle)
	if e != nil {
		return e
	}
	u, e := client.Get(ctx, task.Call, task.Handle)
	if e != nil {
		return e
	}
	expected := domain.TaskInputRequired
	if authorization {
		expected = domain.TaskAuthRequired
	}
	if u.Status != expected {
		return apperrors.ErrConflict
	}
	interactor, ok := client.(tool.TaskInteractor)
	if !ok {
		return apperrors.ErrUnsupported
	}
	return interactor.Continue(ctx, task.Call, task.Handle, in)
}
