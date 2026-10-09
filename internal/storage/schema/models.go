package schema

// Model 是管理员维护的 models 集合结构，ID 对应 MongoDB _id。
// 模型统一使用 OpenAI 兼容接口；Provider 只用于供应商分类展示。
type Model struct {
	ID       string         `bson:"_id" json:"id"`
	Model    string         `bson:"model" json:"model"`
	Provider string         `bson:"provider" json:"provider"`
	BaseURL  string         `bson:"base_url" json:"base_url"`
	APIKeys  []EncryptedKey `bson:"api_keys" json:"api_keys"`
	Options  ModelConfig    `bson:"config" json:"config"`
}

// EncryptedKey 保存稳定凭据标识、AES-GCM 密文、nonce、密钥版本标识与权重。
// Ciphertext 和 Nonce 分别使用 Base64 编码，AES 密钥材料只由环境变量注入。
type EncryptedKey struct {
	ID         string `bson:"id,omitempty" json:"id,omitempty"`
	Version    string `bson:"version" json:"version"`
	Ciphertext string `bson:"ciphertext" json:"ciphertext"`
	Nonce      string `bson:"nonce" json:"nonce"`
	Weight     int64  `bson:"weight" json:"weight"`
}

type ModelConfig struct {
	// WindowTokens 是供应商模型的上下文上限，仅用于自动摘要比例触发。
	WindowTokens   int64    `bson:"window_tokens" json:"window_tokens"`
	RequestTimeout string   `bson:"request_timeout" json:"request_timeout"`
	Thinking       Thinking `bson:"thinking" json:"thinking"`
}

type Thinking struct {
	Enabled bool   `bson:"enabled" json:"enabled"`
	Key     string `bson:"key" json:"key"`
	Value   any    `bson:"value" json:"value"`
}
