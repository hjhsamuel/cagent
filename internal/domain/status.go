package domain

import "github.com/hjhsamuel/cagent/internal/apperrors"

// Validate 只接受本服务定义的 Run 状态；零值不是 queued 的别名。
// 身份校验 Run.Validate 与状态校验独立，创建和持久化边界应同时调用二者。
func (s RunStatus) Validate() error {
	switch s {
	case RunQueued, RunRunning, RunWaiting, RunCompleted, RunFailed, RunCancelled:
		return nil
	default:
		return apperrors.New(apperrors.ErrInvalidArgument, "run.status", "unknown run status")
	}
}

// IsTerminal 表示本地运行已结束；未知值返回 false，但不因此成为合法活动状态。
func (s RunStatus) IsTerminal() bool {
	return s == RunCompleted || s == RunFailed || s == RunCancelled
}

// ValidateTransition 校验单次状态迁移，不修改资源、版本或时间。
// 相同合法状态是幂等的无变化操作，包括终态；不同终态之间不能互相覆盖。
// queued 必须先进入 running 才能完成或等待，但允许启动失败和执行前取消。
// waiting_tool 仅由运行时确认所有分支不可执行后设置；此处不代替分支聚合。
// RunCancelled 表示本地停止生成，远端 Task 可以继续留存实际完成结果。
// 调用方必须以存储版本比较提交，单独调用本方法不能解决多实例并发竞争。
func (s RunStatus) ValidateTransition(next RunStatus) error {
	if err := s.Validate(); err != nil {
		return err
	}
	if err := next.Validate(); err != nil {
		return err
	}
	if s == next {
		return nil
	}
	allowed := false
	switch s {
	case RunQueued:
		allowed = next == RunRunning || next == RunFailed || next == RunCancelled
	case RunRunning:
		allowed = next == RunWaiting || next.IsTerminal()
	case RunWaiting:
		allowed = next == RunRunning || next.IsTerminal()
	}
	if !allowed {
		return stateConflict("run.status")
	}
	return nil
}

// Validate 验证标准化任务状态。适配器遇到未知提供方状态应保留诊断并重新查询，
// 不得映射成 succeeded，也不得将未知值直接写入领域状态。
func (s TaskStatus) Validate() error {
	switch s {
	case TaskSubmitted, TaskRunning, TaskInputRequired, TaskAuthRequired,
		TaskSucceeded, TaskFailed, TaskCancelled, TaskRejected:
		return nil
	default:
		return apperrors.New(apperrors.ErrInvalidArgument, "task.status", "unknown task status")
	}
}

// IsTerminal 仅识别提供方确认的四种终态；取消请求、观察错误和暂停均不是终态。
func (s TaskStatus) IsTerminal() bool {
	return s == TaskSucceeded || s == TaskFailed || s == TaskCancelled || s == TaskRejected
}

// IsPaused 表示需要补充输入或授权；恢复时继续同一任务，不重新执行工具。
func (s TaskStatus) IsPaused() bool {
	return s == TaskInputRequired || s == TaskAuthRequired
}

// ValidateTransition 允许跳过未观察到的中间状态：submitted 可直接到达终态，
// running 与两种暂停状态可互转；离开 submitted 后不能退回 submitted。
// 已确认终态只能接受相同状态。相同状态不代表进度或游标相同，完整更新由
// Task.ApplyUpdate 判断。游标是不透明值，乱序活动状态须由适配层先协调。
func (s TaskStatus) ValidateTransition(next TaskStatus) error {
	if err := s.Validate(); err != nil {
		return err
	}
	if err := next.Validate(); err != nil {
		return err
	}
	if s == next {
		return nil
	}
	if s.IsTerminal() || next == TaskSubmitted {
		return stateConflict("task.status")
	}
	return nil
}

// stateConflict 的说明是静态文本，不把提供方原始状态或业务内容拼入错误。
// 两个合法状态之间不允许的迁移属于冲突；未知状态属于参数无效。
func stateConflict(field string) error {
	return apperrors.New(apperrors.ErrConflict, field, "state update conflicts with current state")
}
