package mongodb

import (
	"math"

	"github.com/hjhsamuel/cagent/internal/domain"
	"github.com/hjhsamuel/cagent/internal/storage/schema"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// 集合映射彼此独立，转换方法只接受该集合对应的领域载荷。

type sessionDocument schema.Session

func packSession(scope domain.Scope, id string, value domain.Session, version int64) (sessionDocument, error) {
	raw, err := bson.Marshal(value)
	d := sessionDocument{Tenant: scope.TenantID, User: scope.UserID, ID: id, Schema: schema.DocumentVersion, Data: raw, Version: version}
	d.ModelID, d.APIKeyID = value.ModelID, value.APIKeyID
	return d, err
}

func (d sessionDocument) decode(value *domain.Session) error {
	return decodeData(d.Schema, d.Data, value)
}

func (d sessionDocument) versionKey() bson.M {
	return documentVersionKey(domain.Scope{TenantID: d.Tenant, UserID: d.User}, d.ID, d.Version)
}

type runDocument schema.Run

func packRun(scope domain.Scope, id string, value domain.Run, version int64) (runDocument, error) {
	raw, err := bson.Marshal(value)
	d := runDocument{Tenant: scope.TenantID, User: scope.UserID, ID: id, Schema: schema.DocumentVersion, Data: raw, Version: version}
	return d, err
}

func (d runDocument) decode(value *domain.Run) error {
	return decodeData(d.Schema, d.Data, value)
}

func (d runDocument) versionKey() bson.M {
	return documentVersionKey(domain.Scope{TenantID: d.Tenant, UserID: d.User}, d.ID, d.Version)
}

type messageDocument schema.Message

func packMessage(scope domain.Scope, id string, value domain.Message, version int64) (messageDocument, error) {
	raw, err := bson.Marshal(value)
	d := messageDocument{Tenant: scope.TenantID, User: scope.UserID, ID: id, Schema: schema.DocumentVersion, Data: raw, Version: version}
	d.ContextProtected = value.Role == domain.RoleUser
	for _, part := range value.Parts {
		if part.Kind == domain.PartToolCall || part.Kind == domain.PartToolResult {
			d.ContextProtected = true
		}
	}
	return d, err
}

func (d messageDocument) decode(value *domain.Message) error {
	return decodeData(d.Schema, d.Data, value)
}

type eventDocument schema.Event

func packEvent(scope domain.Scope, id string, value domain.Event, version int64) (eventDocument, error) {
	raw, err := bson.Marshal(value)
	d := eventDocument{Tenant: scope.TenantID, User: scope.UserID, ID: id, Schema: schema.DocumentVersion, Data: raw, Version: version}
	return d, err
}

func (d eventDocument) decode(value *domain.Event) error {
	return decodeData(d.Schema, d.Data, value)
}

type taskRecord schema.Task

func packTask(scope domain.Scope, id string, value domain.Task, version int64) (taskRecord, error) {
	raw, err := bson.Marshal(value)
	d := taskRecord{Tenant: scope.TenantID, User: scope.UserID, ID: id, Schema: schema.DocumentVersion, Data: raw, Version: version}
	return d, err
}

func (d taskRecord) decode(value *domain.Task) error {
	return decodeData(d.Schema, d.Data, value)
}

func (d taskRecord) versionKey() bson.M {
	return documentVersionKey(domain.Scope{TenantID: d.Tenant, UserID: d.User}, d.ID, d.Version)
}

type deliveryDocument schema.TaskDelivery

func packDelivery(scope domain.Scope, id string, value domain.TaskDelivery, version int64) (deliveryDocument, error) {
	raw, err := bson.Marshal(value)
	d := deliveryDocument{Tenant: scope.TenantID, User: scope.UserID, ID: id, Schema: schema.DocumentVersion, Data: raw, Version: version}
	return d, err
}

func (d deliveryDocument) decode(value *domain.TaskDelivery) error {
	return decodeData(d.Schema, d.Data, value)
}

func (d deliveryDocument) versionKey() bson.M {
	return documentVersionKey(domain.Scope{TenantID: d.Tenant, UserID: d.User}, d.ID, d.Version)
}

type checkpointDocument schema.Checkpoint

func packCheckpointRecord(scope domain.Scope, id string, value domain.Checkpoint, version int64) (checkpointDocument, error) {
	raw, err := bson.Marshal(value)
	d := checkpointDocument{Tenant: scope.TenantID, User: scope.UserID, ID: id, Schema: schema.DocumentVersion, Data: raw, Version: version}
	d.ModelID, d.APIKeyID = value.ModelID, value.APIKeyID
	return d, err
}

func (d checkpointDocument) decode(value *domain.Checkpoint) error {
	return decodeData(d.Schema, d.Data, value)
}

func (d checkpointDocument) versionKey() bson.M {
	return documentVersionKey(domain.Scope{TenantID: d.Tenant, UserID: d.User}, d.ID, d.Version)
}

type snapshotDocument schema.ContextSnapshot

func packSnapshot(scope domain.Scope, id string, value domain.ContextSnapshot, version int64) (snapshotDocument, error) {
	raw, err := bson.Marshal(value)
	d := snapshotDocument{Tenant: scope.TenantID, User: scope.UserID, ID: id, Schema: schema.DocumentVersion, Data: raw, Version: version}
	return d, err
}

func (d snapshotDocument) decode(value *domain.ContextSnapshot) error {
	return decodeData(d.Schema, d.Data, value)
}

type receiptDocument schema.MutationReceipt

func packReceipt(scope domain.Scope, id string, value persistedReceipt, version int64) (receiptDocument, error) {
	raw, err := bson.Marshal(value)
	d := receiptDocument{Tenant: scope.TenantID, User: scope.UserID, ID: id, Schema: schema.DocumentVersion, Data: raw, Version: version}
	return d, err
}

func (d receiptDocument) decode(value *persistedReceipt) error {
	return decodeData(d.Schema, d.Data, value)
}

func repackSession(old sessionDocument, value domain.Session, version int64) (sessionDocument, error) {
	raw, err := bson.Marshal(value)
	old.Data = raw
	old.Version = version
	old.ModelID, old.APIKeyID = value.ModelID, value.APIKeyID
	return old, err
}

func repackRun(old runDocument, value domain.Run, version int64) (runDocument, error) {
	raw, err := bson.Marshal(value)
	old.Data = raw
	old.Version = version
	return old, err
}

func repackDelivery(old deliveryDocument, value domain.TaskDelivery, version int64) (deliveryDocument, error) {
	raw, err := bson.Marshal(value)
	old.Data = raw
	old.Version = version
	return old, err
}

// decodeData 只负责 BSON 数据转换及格式版本校验，不执行数据库操作。
func decodeData(format int, raw bson.Raw, value any) error {
	if format != schema.DocumentVersion {
		return invariant()
	}
	return bson.Unmarshal(raw, value)
}

// documentVersionKey 将读取时的版本加入更新条件，避免旧快照覆盖并发提交。
func documentVersionKey(scope domain.Scope, id string, version int64) bson.M {
	f := key(scope, id)
	f["version"] = version
	return f
}

func increment(value int64) (int64, error) {
	if value < 0 || value == math.MaxInt64 {
		return 0, conflict("counter")
	}
	return value + 1, nil
}

type leaseDocument = schema.Lease
