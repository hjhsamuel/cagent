package schema

// ToolConnection 是管理员维护的远端连接文档；凭据只保存环境变量引用。
// ID 在完整 tenant/user Scope 内唯一，供持久任务恢复使用。
type ToolConnection struct {
	TenantID      string            `bson:"tenant_id" json:"tenant_id"`
	UserID        string            `bson:"user_id" json:"user_id"`
	ID            string            `bson:"id" json:"id"`
	Protocol      string            `bson:"protocol" json:"protocol"`
	URL           string            `bson:"url" json:"url"`
	CardPath      string            `bson:"card_path,omitempty" json:"card_path,omitempty"`
	CredentialRef string            `bson:"credential_ref,omitempty" json:"credential_ref,omitempty"`
	Credentials   map[string]string `bson:"credentials,omitempty" json:"credentials,omitempty"`
	Tools         []string          `bson:"tools,omitempty" json:"tools,omitempty"`
}
