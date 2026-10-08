package config

import (
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/hjhsamuel/cagent/internal/apperrors"
)

// invalid 统一使用 P1.2 的参数错误契约。字段为稳定配置路径，不含环境变量值。
// 所有说明由代码提供，特别不能拼接 URI 或保留会回显输入的解析错误原因。
func invalid(field, message string) error {
	return apperrors.New(apperrors.ErrInvalidArgument, field, message)
}

// Validate 按固定顺序返回首个错误，不修改配置，不解析 DNS，也不检查端口是否可绑定。
// 加载与直接构造的配置共用此入口，错误均可用 errors.Is/As 判断及提取字段路径。
// 这里只验证进程配置的结构和组合；外部连接、授权与 SDK 能力由后续装配/适配器验证。
func (c Config) Validate() error {
	if err := c.HTTP.Login.Validate(); err != nil {
		return err
	}
	if err := c.Capacity.Validate(); err != nil {
		return err
	}
	if c.HTTP.MaxSubscriptions < 1 || c.HTTP.MaxSubscriptions > 100000 {
		return invalid("http.max_subscriptions", "must be between 1 and 100000")
	}
	if c.Agent.Keys != nil || c.Agent.APIKey != "" || c.Agent.BaseURL != "" {
		if err := c.Agent.ValidateOpenAI(); err != nil {
			return err
		}
	}
	if err := c.Logging.Validate(); err != nil {
		return err
	}
	if !validAddress(c.HTTP.Address) {
		return invalid("http.address", "must be host:port with a numeric port from 1 to 65535")
	}
	for _, item := range []struct{ field, value string }{
		{"mongodb.uri", c.MongoDB.URI},
		{"mongodb.database", c.MongoDB.Database},
		{"agent.name", c.Agent.Name},
		{"context.policy_version", c.Context.PolicyVersion},
	} {
		if strings.TrimSpace(item.value) == "" {
			return invalid(item.field, "must not be blank")
		}
	}
	for _, item := range []struct {
		field string
		value time.Duration
	}{
		{"http.sse_heartbeat", c.HTTP.SSEHeartbeat},
		{"http.shutdown_grace", c.HTTP.ShutdownGrace},
		{"tasks.poll_interval", c.Tasks.PollInterval},
		{"tasks.observation_timeout", c.Tasks.ObservationTimeout},
		{"tasks.reconnect_backoff", c.Tasks.ReconnectBackoff},
	} {
		if item.value <= 0 {
			return invalid(item.field, "must be greater than zero")
		}
	}
	if c.Context.WindowTokens <= 0 {
		return invalid("context.window_tokens", "must be greater than zero")
	}
	if c.Context.OutputTokens <= 0 {
		return invalid("context.output_tokens", "must be greater than zero")
	}
	if c.Context.ToolTokens < 0 {
		return invalid("context.tool_tokens", "must not be negative")
	}
	if c.Context.SafetyTokens < 0 {
		return invalid("context.safety_tokens", "must not be negative")
	}
	// 不先相加：多个接近 MaxInt 的合法整数相加可能溢出，绕过总预算检查。
	// 每次先与正的剩余额度比较再扣减，确保全程无溢出且输入至少剩余一个 Token。
	remaining := c.Context.WindowTokens
	for _, reserve := range []int{c.Context.OutputTokens, c.Context.ToolTokens, c.Context.SafetyTokens} {
		if reserve >= remaining {
			return invalid("context.window_tokens", "must exceed the total output, tool and safety token reserves")
		}
		remaining -= reserve
	}
	return c.Context.ValidateCompression()
}

// validAddress 只检查 TCP 监听地址的形状。空 host、IP（含 IPv6 zone）及 ASCII
// 主机名均可用，端口必须为十进制数字；不接受 URL、服务名或自动分配端口 0。
// 主机名是否可解析、地址是否属于本机，应由真正的监听操作判定。
func validAddress(address string) bool {
	host, port, err := net.SplitHostPort(address)
	if err != nil || port == "" {
		return false
	}
	for _, digit := range port {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	n, err := strconv.ParseUint(port, 10, 16)
	if err != nil || n == 0 {
		return false
	}
	// 方括号专用于 IPv6，不能将 [hostname] 或 [IPv4] 当作合法监听地址。
	if strings.HasPrefix(address, "[") {
		ip, err := netip.ParseAddr(host)
		return err == nil && ip.Is6()
	}
	if host == "" {
		return true
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return true
	}
	// 接受尾部根域点；IPv6 解析失败后不能作为主机名混入冒号或 zone 字符。
	name := strings.TrimSuffix(host, ".")
	if len(name) == 0 || len(name) > 253 {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, ch := range label {
			if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-') {
				return false
			}
		}
	}
	return true
}
