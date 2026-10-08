package domain

import (
	"fmt"
	"strings"

	"github.com/hjhsamuel/cagent/internal/apperrors"
)

// required 检查标识或名称是否为空白，但不裁剪、不改写原值。
// 标识是调用方提供的不透明字符串；自动规范化可能把不同资源错误地合并。
// 错误只包含字段名，不回显标识、工具参数或凭据。
// 所有身份与关联校验失败均返回 apperrors.ErrInvalidArgument；调用方可通过
// errors.As 提取 *apperrors.Error 的字段路径，不需要解析展示文本。
func required(field, value string) error {
	if strings.TrimSpace(value) == "" {
		return apperrors.New(apperrors.ErrInvalidArgument, field, "must not be blank")
	}
	return nil
}

// equal 检查关联字段完全相等。调用前应先验证两侧对象，避免两个空值通过比较。
func equal(field, actual, expected string) error {
	if actual != expected {
		return apperrors.New(apperrors.ErrInvalidArgument, field, "does not match referenced resource")
	}
	return nil
}

// Validate 要求租户与用户同时存在；空 Scope 不能用于表示全局访问。
// 本方法只校验结构，不验证身份真实性；Scope 必须由可信认证边界构造。
func (s Scope) Validate() error {
	if err := required("scope.tenant_id", s.TenantID); err != nil {
		return err
	}
	return required("scope.user_id", s.UserID)
}

// ValidateAgainst 校验双方作用域并逐字段匹配，防止仅比较用户 ID 导致跨租户访问。
// expected 应来自可信调用上下文或已加载的所属资源，不能直接相信请求体。
func (s Scope) ValidateAgainst(expected Scope) error {
	if err := s.Validate(); err != nil {
		return err
	}
	if err := expected.Validate(); err != nil {
		return fmt.Errorf("expected: %w", err)
	}
	if err := equal("scope.tenant_id", s.TenantID, expected.TenantID); err != nil {
		return err
	}
	return equal("scope.user_id", s.UserID, expected.UserID)
}

// Validate 校验会话的身份字段。版本和时间由后续持久化流程管理。
func (s Session) Validate() error {
	if err := s.Scope.Validate(); err != nil {
		return err
	}
	if err := required("session.id", s.ID); err != nil {
		return err
	}
	return required("session.agent_id", s.AgentID)
}

// Validate 校验运行的身份及所属会话引用。幂等键允许省略，状态迁移另行处理。
func (r Run) Validate() error {
	if err := r.Scope.Validate(); err != nil {
		return err
	}
	if err := required("run.id", r.ID); err != nil {
		return err
	}
	return required("run.session_id", r.SessionID)
}

// ValidateForSession 同时验证对象和关联会话，拒绝跨用户或跨会话挂接。
// 这不替代存储查询的 Scope 条件，也不能证明数据库中的引用确实存在。
func (r Run) ValidateForSession(s Session) error {
	if err := r.Validate(); err != nil {
		return err
	}
	if err := s.Validate(); err != nil {
		return fmt.Errorf("session: %w", err)
	}
	if err := r.Scope.ValidateAgainst(s.Scope); err != nil {
		return err
	}
	return equal("run.session_id", r.SessionID, s.ID)
}

// Validate 校验消息身份。RunID 允许为空，以容纳不属于某次运行的系统或历史消息。
// 消息内容和工具调用配对由上下文模块处理；此处不猜测 Part.Kind 的协议语义。
func (m Message) Validate() error {
	if err := m.Scope.Validate(); err != nil {
		return err
	}
	if err := required("message.id", m.ID); err != nil {
		return err
	}
	return required("message.session_id", m.SessionID)
}

// ValidateForSession 验证消息属于指定会话，适用于未绑定 Run 的历史消息。
func (m Message) ValidateForSession(s Session) error {
	if err := m.Validate(); err != nil {
		return err
	}
	if err := s.Validate(); err != nil {
		return fmt.Errorf("session: %w", err)
	}
	if err := m.Scope.ValidateAgainst(s.Scope); err != nil {
		return err
	}
	return equal("message.session_id", m.SessionID, s.ID)
}

// ValidateForRun 验证运行消息的完整关联；在此入口下 RunID 必须匹配非空的运行 ID。
func (m Message) ValidateForRun(r Run) error {
	if err := m.Validate(); err != nil {
		return err
	}
	if err := r.Validate(); err != nil {
		return fmt.Errorf("run: %w", err)
	}
	if err := m.Scope.ValidateAgainst(r.Scope); err != nil {
		return err
	}
	if err := equal("message.session_id", m.SessionID, r.SessionID); err != nil {
		return err
	}
	return equal("message.run_id", m.RunID, r.ID)
}

// Validate 校验事件所属运行；序号允许尚未分配，序号分配由持久化事件层负责。
func (e Event) Validate() error {
	if err := e.Scope.Validate(); err != nil {
		return err
	}
	return required("event.run_id", e.RunID)
}

// ValidateForRun 防止事件写入其他用户或其他运行的事件流。
func (e Event) ValidateForRun(r Run) error {
	if err := e.Validate(); err != nil {
		return err
	}
	if err := r.Validate(); err != nil {
		return fmt.Errorf("run: %w", err)
	}
	if err := e.Scope.ValidateAgainst(r.Scope); err != nil {
		return err
	}
	return equal("event.run_id", e.RunID, r.ID)
}

// Validate 校验摘要快照身份；内容质量、覆盖范围和并发版本由上下文与存储层检查。
func (s ContextSnapshot) Validate() error {
	if err := s.Scope.Validate(); err != nil {
		return err
	}
	if err := required("snapshot.id", s.ID); err != nil {
		return err
	}
	return required("snapshot.session_id", s.SessionID)
}

// ValidateForSession 防止其他会话的摘要被用于当前会话。
func (s ContextSnapshot) ValidateForSession(session Session) error {
	if err := s.Validate(); err != nil {
		return err
	}
	if err := session.Validate(); err != nil {
		return fmt.Errorf("session: %w", err)
	}
	if err := s.Scope.ValidateAgainst(session.Scope); err != nil {
		return err
	}
	return equal("snapshot.session_id", s.SessionID, session.ID)
}
