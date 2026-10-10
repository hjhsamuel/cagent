package schema

// ToolConnection 是管理员维护的远端连接文档；凭据只保存环境变量引用。
// ID 在完整 tenant/user Scope 内唯一，供持久任务恢复使用。
type ToolConnection struct {
	// TenantID 是连接所属租户标识，与 UserID 一起限定连接的可见范围。
	TenantID string `bson:"tenant_id" json:"tenant_id"`
	// UserID 是连接所属用户标识，不同用户可以使用相同的连接 ID。
	UserID string `bson:"user_id" json:"user_id"`
	// ID 是作用域内唯一的连接标识，供任务句柄的 ConnectionID 引用。
	ID string `bson:"id" json:"id"`
	// Protocol 是远端工具协议，当前支持 mcp、a2a，用于选择相应客户端。
	Protocol string `bson:"protocol" json:"protocol"`
	// URL 是远端工具服务地址，客户端从该地址建立连接或查询任务。
	URL string `bson:"url" json:"url"`
	// CardPath 是 A2A Agent Card 的发现路径；为空时使用 /.well-known/agent-card.json，MCP 不使用。
	CardPath string `bson:"card_path,omitempty" json:"card_path,omitempty"`
	// CredentialRef 是连接默认使用的凭据引用名，实际值通过 Credentials 映射和环境变量解析。
	CredentialRef string `bson:"credential_ref,omitempty" json:"credential_ref,omitempty"`
	// Credentials 将凭据引用名映射到环境变量名；数据库只保存变量名，运行时读取凭据内容。
	Credentials map[string]string `bson:"credentials,omitempty" json:"credentials,omitempty"`
	// Tools 是允许注册的工具名称列表；非空时作为白名单，空列表允许所有已发现工具。
	Tools []string `bson:"tools,omitempty" json:"tools,omitempty"`
}
