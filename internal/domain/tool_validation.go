package domain

import (
	"fmt"

	"github.com/hjhsamuel/cagent/internal/apperrors"
)

// Validate 只接受领域层支持的协议。新增协议必须显式扩展契约，不能静默回退。
// 未知协议属于参数无效；合法协议的提供方缺少某项能力时才使用 ErrUnsupported。
func (p ToolProtocol) Validate() error {
	switch p {
	case ToolLocal, ToolMCP, ToolA2A:
		return nil
	default:
		return apperrors.New(apperrors.ErrInvalidArgument, "protocol", "unsupported tool protocol")
	}
}

// Validate 校验 Agent 调用身份。根调用的父 ID 为空；子调用不能引用自身。
// 是否存在该父调用、是否形成跨层循环，需要运行时加载调用图后验证。
func (a AgentExecution) Validate() error {
	if err := required("caller.agent_id", a.AgentID); err != nil {
		return err
	}
	if err := required("caller.invocation_id", a.InvocationID); err != nil {
		return err
	}
	if a.ParentInvocationID != "" {
		if err := required("caller.parent_invocation_id", a.ParentInvocationID); err != nil {
			return err
		}
		if a.ParentInvocationID == a.InvocationID {
			return apperrors.New(apperrors.ErrInvalidArgument, "caller.parent_invocation_id", "must not reference itself")
		}
	}
	return nil
}

// Validate 校验工具调用的完整路由信息，确保结果能定位到具体 Agent/Subagent 调用。
// Arguments 的编码、Schema 与工具权限属于适配器或工具执行边界；本方法不解析它们。
// 幂等键可选；缺少幂等键不代表调用失败后可以安全重试。
func (c ToolCall) Validate() error {
	if err := c.Scope.Validate(); err != nil {
		return err
	}
	if err := required("call.id", c.ID); err != nil {
		return err
	}
	if err := required("call.session_id", c.SessionID); err != nil {
		return err
	}
	if err := required("call.run_id", c.RunID); err != nil {
		return err
	}
	if err := c.Caller.Validate(); err != nil {
		return err
	}
	if err := c.Protocol.Validate(); err != nil {
		return err
	}
	return required("call.name", c.Name)
}

// ValidateForRun 验证工具调用的作用域、会话及运行三者一致。
func (c ToolCall) ValidateForRun(r Run) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if err := r.Validate(); err != nil {
		return fmt.Errorf("run: %w", err)
	}
	if err := c.Scope.ValidateAgainst(r.Scope); err != nil {
		return err
	}
	if err := equal("call.session_id", c.SessionID, r.SessionID); err != nil {
		return err
	}
	return equal("call.run_id", c.RunID, r.ID)
}

// Validate 要求结果携带原工具调用 ID。空 Parts 可以表示无输出的成功执行，
// Error 与 Parts 可同时存在，以保留失败时产生的诊断内容。
func (r ToolResult) Validate() error { return required("result.call_id", r.CallID) }

// ValidateForCall 验证结果属于原调用；调用者仍需保留作用域和 InvocationID，
// 不能仅凭全局检索 CallID 来定位用户或 Agent 分支。
func (r ToolResult) ValidateForCall(c ToolCall) error {
	if err := r.Validate(); err != nil {
		return err
	}
	if err := c.Validate(); err != nil {
		return err
	}
	return equal("result.call_id", r.CallID, c.ID)
}

// Validate 校验已有任务的定位信息。ConnectionID 是配置引用，不是连接 URL 或凭据。
// 所有协议均要求该引用，以便恢复时解析对应提供方；ContextID 为提供方可选字段。
// 本地工具同样可实现长任务，但必须提供可重新解析的连接引用和任务 ID。
func (h TaskHandle) Validate() error {
	if err := h.Protocol.Validate(); err != nil {
		return err
	}
	if err := required("handle.connection_id", h.ConnectionID); err != nil {
		return err
	}
	return required("handle.remote_id", h.RemoteID)
}

// ValidateForCall 要求句柄协议与发起调用的协议一致，避免把句柄交给错误的 TaskClient。
func (h TaskHandle) ValidateForCall(c ToolCall) error {
	if err := h.Validate(); err != nil {
		return err
	}
	if err := c.Validate(); err != nil {
		return err
	}
	return equal("handle.protocol", string(h.Protocol), string(c.Protocol))
}

// Validate 强制即时结果与长任务句柄恰好存在一个，并验证选中分支的必要字段。
// 返回任务句柄仅表示已有任务需要跟踪，即便远端已结束，也不能当作最终工具结果。
func (o ToolOutcome) Validate() error {
	if (o.Result == nil) == (o.Task == nil) {
		return apperrors.New(apperrors.ErrInvalidArgument, "outcome", "exactly one of result or task is required")
	}
	if o.Result != nil {
		return o.Result.Validate()
	}
	return o.Task.Validate()
}

// ValidateForCall 在结构检查之后验证原调用关联，供工具执行边界接收返回值时使用。
func (o ToolOutcome) ValidateForCall(c ToolCall) error {
	if err := o.Validate(); err != nil {
		return err
	}
	if o.Result != nil {
		return o.Result.ValidateForCall(c)
	}
	return o.Task.ValidateForCall(c)
}

// Validate 校验任务及嵌套调用的作用域、句柄和可选结果关联。
// 此方法不推断任务状态，不把观察错误或取消请求转换成远端终态。
func (t Task) Validate() error {
	if err := t.Scope.Validate(); err != nil {
		return err
	}
	if err := required("task.id", t.ID); err != nil {
		return err
	}
	if err := t.Call.Validate(); err != nil {
		return err
	}
	if err := t.Scope.ValidateAgainst(t.Call.Scope); err != nil {
		return err
	}
	if err := t.Handle.ValidateForCall(t.Call); err != nil {
		return err
	}
	if t.Result != nil {
		return t.Result.ValidateForCall(t.Call)
	}
	return nil
}

// ValidateForRun 沿 Task → ToolCall → Run 验证关联链，保留具体调用分支的身份。
func (t Task) ValidateForRun(r Run) error {
	if err := t.Validate(); err != nil {
		return err
	}
	return t.Call.ValidateForRun(r)
}
