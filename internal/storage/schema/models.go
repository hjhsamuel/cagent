package schema

// Model 是管理员维护的 models 集合结构，ID 对应 MongoDB _id。
// 模型统一使用 OpenAI 兼容接口；Provider 只用于供应商分类展示。
type Model struct {
	// ID 是模型配置标识，对应 MongoDB 的 _id，供会话和检查点的 ModelID 引用。
	ID string `bson:"_id" json:"id"`
	// Model 是发送给供应商接口的实际模型名称，可与本地配置 ID 不同。
	Model string `bson:"model" json:"model"`
	// Provider 是供应商名称或分类，仅用于展示；模型调用统一使用 OpenAI 兼容接口。
	Provider string `bson:"provider" json:"provider"`
	// BaseURL 是供应商 OpenAI 兼容接口的基础地址。
	BaseURL string `bson:"base_url" json:"base_url"`
	// APIKeys 是此模型的加密凭据列表；启动或配置解析时解密，按权重选择或按稳定 ID 绑定。
	APIKeys []EncryptedKey `bson:"api_keys" json:"api_keys"`
	// Options 是模型运行参数，对应 BSON 的 config 子文档，包含上下文上限、请求超时和深度思考设置。
	Options ModelConfig `bson:"config" json:"config"`
}

// EncryptedKey 保存稳定凭据标识、AES-GCM 密文、nonce、密钥版本标识与权重。
// Ciphertext 和 Nonce 分别使用 Base64 编码，AES 密钥材料只由环境变量注入。
type EncryptedKey struct {
	// ID 是模型内稳定的凭据标识，供 APIKeyID 引用；轮换加密密钥时保留该标识。
	ID string `bson:"id,omitempty" json:"id,omitempty"`
	// Version 是环境密钥环中的 AES 加密密钥版本，用于选择解密密钥，与业务记录版本不同。
	Version string `bson:"version" json:"version"`
	// Ciphertext 是 Base64 编码的 AES-GCM 密文，包含认证标签，不保存明文 API 密钥。
	Ciphertext string `bson:"ciphertext" json:"ciphertext"`
	// Nonce 是本次 AES-GCM 加密使用的随机数，以独立的 Base64 字符串保存。
	Nonce string `bson:"nonce" json:"nonce"`
	// Weight 是凭据随机选择的非负权重，值越大被选中的概率越高；0 表示不参与随机选择。
	Weight int64 `bson:"weight" json:"weight"`
}

// ModelConfig 映射模型文档中的 config 运行配置。
type ModelConfig struct {
	// WindowTokens 是供应商模型的上下文 token 上限，仅用于自动摘要比例触发。
	WindowTokens int64 `bson:"window_tokens" json:"window_tokens"`
	// RequestTimeout 是模型请求超时的正时长字符串，如 30s、2m，解析为 Go time.Duration。
	RequestTimeout string `bson:"request_timeout" json:"request_timeout"`
	// Thinking 是供应商深度思考参数配置，决定是否向请求中注入指定 JSON 参数。
	Thinking Thinking `bson:"thinking" json:"thinking"`
}

// Thinking 映射供应商深度思考请求参数。
type Thinking struct {
	// Enabled 表示是否启用深度思考参数注入；关闭时不注入 Key、Value。
	Enabled bool `bson:"enabled" json:"enabled"`
	// Key 是要注入的 JSON 参数路径，支持点号分隔的嵌套路径，如 thinking.type。
	Key string `bson:"key" json:"key"`
	// Value 是注入到 Key 路径的 JSON 值，可以是字符串、数值、布尔值或对象等。
	Value any `bson:"value" json:"value"`
}
