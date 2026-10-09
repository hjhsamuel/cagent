package mongodb

import (
	"math"

	"github.com/hjhsamuel/cagent/internal/domain"
	"github.com/hjhsamuel/cagent/internal/storage/schema"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// document 的存储字段统一在 schema.Document 中维护；适配层仅保留转换方法。
type document schema.Document

func pack(scope domain.Scope, id string, value any, version int64) (document, error) {
	raw, err := bson.Marshal(value)
	d := document{Tenant: scope.TenantID, User: scope.UserID, ID: id, Schema: schema.DocumentVersion, Data: raw, Version: version}
	setModelBinding(&d, value)
	if m, ok := value.(domain.Message); ok {
		d.ContextProtected = m.Role == domain.RoleUser
		for _, part := range m.Parts {
			if part.Kind == domain.PartToolCall || part.Kind == domain.PartToolResult {
				d.ContextProtected = true
			}
		}
	}
	return d, err
}

func setModelBinding(d *document, value any) {
	switch v := value.(type) {
	case domain.Session:
		d.ModelID, d.APIKeyID = v.ModelID, v.APIKeyID
	case domain.Checkpoint:
		d.ModelID, d.APIKeyID = v.ModelID, v.APIKeyID
	}
}

// decode 只负责 BSON 数据转换及格式版本校验，不执行数据库操作。
func (d document) decode(value any) error {
	if d.Schema != schema.DocumentVersion {
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
	setModelBinding(&old, value)
	return old, err
}
func increment(value int64) (int64, error) {
	if value < 0 || value == math.MaxInt64 {
		return 0, conflict("counter")
	}
	return value + 1, nil
}

type leaseDocument = schema.Lease
