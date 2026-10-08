package config

import "strings"

// JWT 的签名密钥仅供可信签发方与本服务持有，不得输出到日志或响应。
// 算法固定 HS256，不允许令牌头选择算法。
type JWT struct{ Secret string }

// ValidateServer 是实际 HTTP 启动的附加约束；离线模型/存储组件无需认证配置。
func (h HTTP) ValidateServer() error {
	if err := h.Login.Validate(); err != nil {
		return err
	}
	if h.MaxSubscriptions < 1 || h.MaxSubscriptions > 100000 {
		return invalid("http.max_subscriptions", "must be between 1 and 100000")
	}
	if len(h.JWT.Secret) < 32 || strings.TrimSpace(h.JWT.Secret) == "" {
		return invalid("http.jwt.secret", "must contain at least 32 bytes")
	}
	if h.WriteTimeout <= 0 || h.MaxBodyBytes <= 0 {
		return invalid("http.limits", "write timeout and body limit must be positive")
	}
	if h.SSEHeartbeat <= 0 || h.ShutdownGrace <= 0 {
		return invalid("http.lifecycle", "heartbeat and shutdown grace must be positive")
	}
	return nil
}
