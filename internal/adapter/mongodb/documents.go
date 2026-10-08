package mongodb

import (
	"math"
	"time"

	"github.com/hjhsamuel/cagent/internal/domain"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// document 是适配层唯一的持久化信封。查询/索引字段显式映射，领域结构不带标签。
// Data 是 schema=1 的 BSON 子文档（Go 字段名的小写名称），仅供本适配器解码；
// 不直接暴露给 HTTP。保留 nil/空切片区别，不使用 omitempty 编码业务载荷。
// 未来修改领域字段编码需升级 Schema 并迁移，禁止悄悄改变既有持久格式。
type document struct {
	Tenant         string   `bson:"tenant_id"`
	User           string   `bson:"user_id"`
	ID             string   `bson:"id"`
	Schema         int      `bson:"schema"`
	Data           bson.Raw `bson:"data"`
	Version        int64    `bson:"version"`
	SessionID      string   `bson:"session_id,omitempty"`
	RunID          string   `bson:"run_id,omitempty"`
	InvocationID   string   `bson:"invocation_id,omitempty"`
	CallID         string   `bson:"call_id,omitempty"`
	Protocol       string   `bson:"protocol,omitempty"`
	ConnectionID   string   `bson:"connection_id,omitempty"`
	RemoteID       string   `bson:"remote_id,omitempty"`
	Status         string   `bson:"status,omitempty"`
	IdempotencyKey string   `bson:"idempotency_key,omitempty"`
	Sequence       int64    `bson:"sequence,omitempty"`
	LastSequence   int64    `bson:"last_sequence"`
	PrunedThrough  int64    `bson:"pruned_through"`
	Unsettled      int64    `bson:"unsettled"`
	Start          bson.Raw `bson:"start,omitempty"`
	Fingerprint    []byte   `bson:"fingerprint,omitempty"`
}

func pack(scope domain.Scope, id string, value any, version int64) (document, error) {
	raw, err := bson.Marshal(value)
	return document{Tenant: scope.TenantID, User: scope.UserID, ID: id, Schema: 1, Data: raw, Version: version}, err
}

// decode 只负责 BSON 数据转换及格式版本校验，不执行数据库操作。
func (d document) decode(value any) error {
	if d.Schema != 1 {
		return invariant()
	}
	return bson.Unmarshal(d.Data, value)
}

// versionKey 将读取时的版本加入更新条件，避免旧快照覆盖并发提交。
func (d document) versionKey() bson.M {
	f := key(domain.Scope{TenantID: d.Tenant, UserID: d.User}, d.ID)
	f["version"] = d.Version
	return f
}
func repack(old document, value any, version int64) (document, error) {
	raw, err := bson.Marshal(value)
	old.Data = raw
	old.Version = version
	return old, err
}
func increment(value int64) (int64, error) {
	if value < 0 || value == math.MaxInt64 {
		return 0, conflict("counter")
	}
	return value + 1, nil
}

type leaseDocument struct {
	Tenant   string    `bson:"tenant_id"`
	User     string    `bson:"user_id"`
	ID       string    `bson:"id"`
	Owner    string    `bson:"owner"`
	Fence    int64     `bson:"fence"`
	Expires  time.Time `bson:"expires_at"`
	Revision int64     `bson:"revision"`
}
