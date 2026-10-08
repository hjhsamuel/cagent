// Package config 提供进程级配置的默认值、环境变量加载与无副作用校验。
// 本包不读取配置文件、不连接外部服务、不初始化 SDK，也不输出配置值或日志。
package config

import "time"

// Config 是一次加载得到的独立配置快照，不保存用户身份或请求级状态。
// 装配完成后应按只读值使用；直接构造或修改配置后必须再次调用 Validate。
// MongoDB.URI 可能含凭据，禁止将整个配置格式化、序列化或写入日志。
type Config struct {
	// ToolsFile 是可选可信工具清单路径；空值禁用工具。文件不接受 HTTP 请求覆盖。
	ToolsFile string
	// Capacity 限制单实例资源；多实例总容量为各实例之和。
	Capacity Capacity
	Logging  Logging
	HTTP     HTTP
	MongoDB  MongoDB
	Agent    Agent
	Tasks    Tasks
	Context  Context
	// ModelEncryption 只保存版本化 AES 密钥的环境配置，禁止整体打印。
	ModelEncryption ModelEncryption
	// Models 是 MongoDB 模型目录，不包含默认模型。
	Models *ModelCatalog
	// SummaryAgent 在装配时从 MongoDB 加载；nil 表示沿用主模型。
	SummaryAgent *Agent
}

// HTTP 定义服务监听与 SSE 生命周期参数，不控制后台 Run 的生命周期。
type HTTP struct {
	// JWT 只验证外部签发的令牌；服务不提供登录或令牌签发接口。
	JWT JWT
	// MaxSubscriptions 限制本实例 SSE 连接，超限在发送流头前返回 503。
	MaxSubscriptions int
	// DiagnosticsToken 是独立运维 Bearer 凭据；空值关闭指标和追踪接口。
	DiagnosticsToken string
	// WriteTimeout 限制单次 SSE 写入，避免慢客户端永久占用订阅。
	WriteTimeout time.Duration
	MaxBodyBytes int
	// Address 使用 host:port；空 host 表示所有网卡，IPv6 必须加方括号。
	Address string
	// SSEHeartbeat 是订阅心跳间隔，必须大于零，不占用持久化事件序号。
	SSEHeartbeat time.Duration
	// ShutdownGrace 是优雅关闭等待时间，必须大于零。
	ShutdownGrace time.Duration
}

// MongoDB 保存连接目标。URI 必须显式设置，避免缺少部署配置时意外连接默认实例。
// 此处只检查 URI 和 Database 非空；URI 语法、驱动选项、认证和事务拓扑由适配器验证。
type MongoDB struct {
	URI      string
	Database string
}

// Agent 保存模型客户端的启动快照，禁止整体打印。
// 环境只选择 Model 文档 ID；装配后 Model 为供应商模型名，Provider 为展示分类。
type Agent struct {
	Name     string
	Provider string
	// Keys 是解密后的只读加权密钥池，仅用于模型请求。
	Keys     *KeyPool
	Thinking *Thinking
	Model    string
	// BaseURL 为完整 API 前缀（如 https://host/v1），不从模型名推断地址。
	BaseURL string
	// APIKey 仅交给模型客户端，禁止日志打印整个 Agent/Config 或写入检查点。
	APIKey string
	// TokenEncoding 必须显式匹配实际模型，当前支持 cl100k_base/o200k_base。
	TokenEncoding string
	// MaxTokensField 适配兼容端的输出限额字段，不按任意模型名称猜测。
	MaxTokensField string
	RequestTimeout time.Duration
}

// Tasks 控制已启动远端任务的观察节奏，不定义任务执行队列或远端任务寿命。
type Tasks struct {
	// PollInterval 是轮询观察之间的间隔，必须大于零。
	PollInterval time.Duration
	// ObservationTimeout 仅限制单次观察；超时不等于任务失败，也不能触发重新执行。
	ObservationTimeout time.Duration
	// ReconnectBackoff 是观察失败后的重连等待，必须大于零以避免忙循环。
	ReconnectBackoff time.Duration
}

// Context 定义模型窗口与预留预算；窗口和输出从 MongoDB 模型加载，工具与安全预留可为零。
// 扣除全部预留后至少留一个输入 Token；实际计数与模型窗口匹配由模型适配器负责。
type Context struct {
	// CompressionThresholdPercent 为 0 时禁用压缩，1–100 表示输入预算触发百分比。
	CompressionThresholdPercent int
	// KeepRecentRounds 包含当前用户轮次；用户原文、工具消息和当前 Run 永远保留。
	KeepRecentRounds int
	// SummaryModel 加载前为 MongoDB 文档 ID，空值沿用主模型；加载后为供应商模型名。
	SummaryModel         string
	SummaryTokenEncoding string
	SummaryWindowTokens  int
	SummaryOutputTokens  int
	// WindowTokens 是模型上下文总窗口；默认值不代表所选模型的真实能力。
	WindowTokens int
	// OutputTokens 是本次生成输出预留，必须为正。
	OutputTokens int
	// ToolTokens 为工具定义等额外输入预留，可显式设为零。
	ToolTokens int
	// SafetyTokens 是计数偏差的安全余量，可显式设为零。
	SafetyTokens int
	// PolicyVersion 标识上下文策略；压缩策略升级时从原始历史重建不兼容摘要。
	// 修改模型、提示词或保留规则时应同时变更版本，避免继续使用旧策略内容。
	PolicyVersion string
}

// Defaults 返回全新默认值，不访问进程环境，也不保证配置已经可以使用。
// MongoDB URI 必须显式设置；生产启动加载模型目录，创建会话时确定模型。
// Token 默认值只是初始预算，部署时必须根据所选模型调整，不代表任何模型的能力。
func Defaults() Config {
	return Config{
		Capacity: Capacity{Runs: 64, Models: 16, Observations: 32},
		Logging:  Logging{Level: "info", Path: "/app/logs/cagent.log", Size: 50, Rolls: 3},
		HTTP:     HTTP{MaxSubscriptions: 256, Address: "127.0.0.1:8080", SSEHeartbeat: 15 * time.Second, ShutdownGrace: 30 * time.Second, WriteTimeout: 10 * time.Second, MaxBodyBytes: 1 << 20},
		MongoDB:  MongoDB{Database: "cagent"},
		Agent:    Agent{Name: "cagent", MaxTokensField: "max_tokens", RequestTimeout: 2 * time.Minute},
		Tasks:    Tasks{PollInterval: 2 * time.Second, ObservationTimeout: 30 * time.Second, ReconnectBackoff: time.Second},
		Context:  Context{WindowTokens: 8192, OutputTokens: 2048, ToolTokens: 1024, SafetyTokens: 512, PolicyVersion: "v1", CompressionThresholdPercent: 80, KeepRecentRounds: 2, SummaryWindowTokens: 8192, SummaryOutputTokens: 512},
	}
}
