// Package schema 集中定义 MongoDB 集合名、文档和命令返回值的 BSON 映射。
// 此包只维护存储结构，不加载环境、连接数据库或执行加解密。
package schema

import (
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

const DocumentVersion = 1

// Document 为业务集合共用的持久化信封；Data 是领域载荷的 BSON 子文档。
// Data 的 schema=1 编码保持既有 Go 字段小写映射和 nil/空切片区别。
// 修改既有载荷编码须升级 Schema 并迁移，不能隐式改变持久格式。
type Document struct {
	Tenant           string      `bson:"tenant_id"`
	User             string      `bson:"user_id"`
	ID               string      `bson:"id"`
	ModelID          string      `bson:"model_id,omitempty"`
	APIKeyID         string      `bson:"api_key_id,omitempty"`
	Schema           int         `bson:"schema"`
	Data             bson.Raw    `bson:"data,omitempty"`
	Payload          *PayloadRef `bson:"payload,omitempty"`
	Version          int64       `bson:"version"`
	SessionID        string      `bson:"session_id,omitempty"`
	RunID            string      `bson:"run_id,omitempty"`
	InvocationID     string      `bson:"invocation_id,omitempty"`
	CallID           string      `bson:"call_id,omitempty"`
	Protocol         string      `bson:"protocol,omitempty"`
	ConnectionID     string      `bson:"connection_id,omitempty"`
	RemoteID         string      `bson:"remote_id,omitempty"`
	Status           string      `bson:"status,omitempty"`
	IdempotencyKey   string      `bson:"idempotency_key,omitempty"`
	Sequence         int64       `bson:"sequence,omitempty"`
	LastSequence     int64       `bson:"last_sequence"`
	PrunedThrough    int64       `bson:"pruned_through"`
	Unsettled        int64       `bson:"unsettled"`
	Recovery         bool        `bson:"recovery"`
	ContextProtected bool        `bson:"context_protected"`
	NextActionAt     time.Time   `bson:"next_action_at"`
	Start            bson.Raw    `bson:"start,omitempty"`
	Fingerprint      []byte      `bson:"fingerprint,omitempty"`
}

// PayloadRef is a versioned immutable, scope-local BSON payload split into small
// chunks. Inline legacy documents remain readable; references are never TTL'd.
type PayloadRef struct {
	Format int    `bson:"format"`
	Hash   string `bson:"hash"`
	Bytes  int    `bson:"bytes"`
	Chunks int    `bson:"chunks"`
}

type Lease struct {
	Tenant   string    `bson:"tenant_id"`
	User     string    `bson:"user_id"`
	ID       string    `bson:"id"`
	Owner    string    `bson:"owner"`
	Fence    int64     `bson:"fence"`
	Expires  time.Time `bson:"expires_at"`
	Revision int64     `bson:"revision"`
}

type Clock struct {
	ID     string `bson:"_id"`
	Schema int    `bson:"schema"`
}
