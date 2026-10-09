package config

import (
	"os"
	"strconv"
	"time"
)

// Load 从进程环境加载一次配置：默认值 < 已设置的 CAGENT_* 环境变量。
// 未设置保留默认值；显式空值参与覆盖并按规则报错，不静默回退。
// 不自动加载 .env、不展开变量引用、不裁剪字符串；装配层负责调用及处理错误。
// 应在启动时调用，运行期间不要一边修改进程环境一边加载配置。
func Load() (Config, error) { return LoadFromEnv(os.LookupEnv, os.Environ()...) }

// LoadFromEnv 与 Load 使用相同流程，但由调用方提供环境查找函数。
// 它支持不修改全局环境的确定性测试；函数应在本次调用中返回稳定的快照。
// names 提供版本密钥的环境名称或 name=value 项；其他配置只读取已知键。
// 每个已知键至多读取一次，不拒绝宿主环境中的其他变量。
// 解析失败或校验失败均返回零值 Config，防止调用方误用部分配置。
// nil 查找函数返回参数错误；本函数不捕获调用方函数的 panic。
func LoadFromEnv(lookup func(string) (string, bool), names ...string) (Config, error) {
	if lookup == nil {
		return Config{}, invalid("environment", "environment lookup is required")
	}
	c := Defaults()
	for _, setting := range []struct {
		key, field string
		dst        *bool
	}{
		{"CAGENT_CONTEXT_ARCHIVE_COMPLETED", "context.archive_completed", &c.Context.ArchiveCompleted},
		{"CAGENT_CONTEXT_COMPRESSION_ENABLED", "context.compression_enabled", &c.Context.CompressionEnabled},
	} {
		if value, present := lookup(setting.key); present {
			parsed, err := strconv.ParseBool(value)
			if err != nil {
				return Config{}, invalid(setting.field, "must be a boolean")
			}
			*setting.dst = parsed
		}
	}
	logging, err := LoadLoggingFromEnv(lookup)
	if err != nil {
		return Config{}, err
	}
	c.Logging = logging
	c.ModelEncryption, err = LoadModelEncryptionFromEnv(lookup, names)
	if err != nil {
		return Config{}, err
	}
	// 显式绑定避免反射和隐式命名规则，新增配置时必须同时决定其环境变量名称。
	for _, item := range []struct {
		key string
		dst *string
	}{
		{"CAGENT_TOOLS_FILE", &c.ToolsFile},
		{"CAGENT_HTTP_ADDRESS", &c.HTTP.Address},
		{"CAGENT_HTTP_JWT_SECRET", &c.HTTP.JWT.Secret},
		{"CAGENT_MONGODB_URI", &c.MongoDB.URI},
		{"CAGENT_MONGODB_DATABASE", &c.MongoDB.Database},
		{"CAGENT_AGENT_NAME", &c.Agent.Name},
		{"CAGENT_CONTEXT_POLICY_VERSION", &c.Context.PolicyVersion},
	} {
		if value, present := lookup(item.key); present {
			*item.dst = value
		}
	}
	for _, item := range []struct {
		key, field string
		dst        *time.Duration
	}{
		{"CAGENT_HTTP_SSE_HEARTBEAT", "http.sse_heartbeat", &c.HTTP.SSEHeartbeat},
		{"CAGENT_HTTP_LOGIN_TOKEN_TTL", "http.login.token_ttl", &c.HTTP.Login.TokenTTL},
		{"CAGENT_HTTP_WRITE_TIMEOUT", "http.write_timeout", &c.HTTP.WriteTimeout},
		{"CAGENT_HTTP_SHUTDOWN_GRACE", "http.shutdown_grace", &c.HTTP.ShutdownGrace},
		{"CAGENT_TASKS_POLL_INTERVAL", "tasks.poll_interval", &c.Tasks.PollInterval},
		{"CAGENT_TASKS_OBSERVATION_TIMEOUT", "tasks.observation_timeout", &c.Tasks.ObservationTimeout},
		{"CAGENT_TASKS_RECONNECT_BACKOFF", "tasks.reconnect_backoff", &c.Tasks.ReconnectBackoff},
		{"CAGENT_MAINTENANCE_CANCEL_GRACE", "maintenance.cancel_grace", &c.Maintenance.CancelGrace},
		{"CAGENT_MAINTENANCE_DETACHED_GRACE", "maintenance.detached_grace", &c.Maintenance.DetachedGrace},
		{"CAGENT_MAINTENANCE_OUTAGE_GRACE", "maintenance.outage_grace", &c.Maintenance.OutageGrace},
		{"CAGENT_MAINTENANCE_INTERACTION_GRACE", "maintenance.interaction_grace", &c.Maintenance.InteractionGrace},
		{"CAGENT_MAINTENANCE_MAX_BACKOFF", "maintenance.max_backoff", &c.Maintenance.MaxBackoff},
	} {
		if value, present := lookup(item.key); present {
			parsed, err := time.ParseDuration(value)
			if err != nil {
				// 标准解析错误会回显原始输入，因此故意不包装、不保留该原因。
				return Config{}, invalid(item.field, "must be a duration with units, such as 500ms or 30s")
			}
			*item.dst = parsed
		}
	}
	for _, item := range []struct {
		key, field string
		dst        *int
	}{
		{"CAGENT_CAPACITY_RUNS", "capacity.runs", &c.Capacity.Runs},
		{"CAGENT_MAINTENANCE_WORKERS", "maintenance.workers", &c.Maintenance.Workers},
		{"CAGENT_CAPACITY_MODELS", "capacity.models", &c.Capacity.Models},
		{"CAGENT_CAPACITY_OBSERVATIONS", "capacity.observations", &c.Capacity.Observations},
		{"CAGENT_HTTP_MAX_SUBSCRIPTIONS", "http.max_subscriptions", &c.HTTP.MaxSubscriptions},
		{"CAGENT_CONTEXT_KEEP_RECENT_ROUNDS", "context.keep_recent_rounds", &c.Context.KeepRecentRounds},
		{"CAGENT_CONTEXT_COMPRESSION_THRESHOLD_PERCENT", "context.compression_threshold_percent", &c.Context.CompressionThresholdPercent},
		{"CAGENT_HTTP_MAX_BODY_BYTES", "http.max_body_bytes", &c.HTTP.MaxBodyBytes},
	} {
		if value, present := lookup(item.key); present {
			parsed, err := strconv.Atoi(value)
			if err != nil {
				return Config{}, invalid(item.field, "must be a decimal integer within the platform int range")
			}
			*item.dst = parsed
		}
	}
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}
