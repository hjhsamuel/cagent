package mongodb

import (
	"context"

	"github.com/hjhsamuel/cagent/internal/apperrors"
	"github.com/hjhsamuel/cagent/internal/storage/schema"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// 作用域标识必须按原字节比较。拒绝已有的非 simple 默认排序规则和 view，防止
// 大小写不敏感的集合把两个用户合并；部署账号不得在服务运行期间更改集合模式。
func (b *Database) validateCollections(ctx context.Context) error {
	specs, err := b.db.ListCollectionSpecifications(ctx, bson.M{"name": bson.M{"$in": []string{ModelCollection, ToolConnectionCollection, SessionCollection, MessageCollection, RunCollection, EventCollection, TaskCollection, SnapshotCollection, CheckpointCollection, TaskDeliveryCollection, LeaseCollection, MutationReceiptCollection, ClockCollection, PayloadCollection}}})
	if err != nil {
		return err
	}
	for _, spec := range specs {
		if spec.Type != "collection" {
			return apperrors.New(apperrors.ErrUnsupported, "mongodb.collection", "ordinary collections are required")
		}
		value := spec.Options.Lookup("collation")
		if value.Type != 0 {
			if value.Type != bson.TypeEmbeddedDocument {
				return invariant()
			}
			locale := value.Document().Lookup("locale")
			if locale.Type != bson.TypeString || locale.StringValue() != "simple" {
				return apperrors.New(apperrors.ErrUnsupported, "mongodb.collation", "binary identifier comparison is required")
			}
		}
	}
	return nil
}

func index(name string, unique bool, fields ...string) mongo.IndexModel {
	keys := bson.D{{Key: "tenant_id", Value: 1}, {Key: "user_id", Value: 1}}
	for _, f := range fields {
		keys = append(keys, bson.E{Key: f, Value: 1})
	}
	return mongo.IndexModel{Keys: keys, Options: options.Index().SetName(name).SetUnique(unique).SetCollation(&options.Collation{Locale: "simple"})}
}

// ensureIndexes 可重复执行。固定名称/参数使不兼容旧索引显式报错，不偷偷删索引。
// 不对事件设置 TTL；清理必须经过管理入口并原子推进连续前缀水位。
func (b *Database) ensureIndexes(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()
	collections := []string{ToolConnectionCollection, SessionCollection, MessageCollection, RunCollection, EventCollection, TaskCollection, SnapshotCollection, CheckpointCollection, TaskDeliveryCollection, LeaseCollection, MutationReceiptCollection, PayloadCollection}
	for _, name := range collections {
		models := []mongo.IndexModel{index("scope_id", true, "id")}
		switch name {
		case MessageCollection:
			models = append(models, index("context_protected", false, "session_id", "context_protected", "sequence"))
			models = append(models, index("session_sequence", true, "session_id", "sequence"))
		case EventCollection:
			models = append(models, index("run_sequence", true, "run_id", "sequence"))
		case RunCollection:
			due := mongo.IndexModel{Keys: bson.D{{Key: "next_action_at", Value: 1}, {Key: "tenant_id", Value: 1}, {Key: "user_id", Value: 1}, {Key: "id", Value: 1}}, Options: options.Index().SetName("recovery_due").SetPartialFilterExpression(bson.M{"recovery": true}).SetCollation(&options.Collation{Locale: "simple"})}
			models = append(models, due)
			idem := index("session_idempotency", true, "session_id", "idempotency_key")
			idem.Options.SetPartialFilterExpression(bson.M{"idempotency_key": bson.M{"$type": "string", "$gt": ""}})
			models = append(models, idem, index("run_state", false, "status", "id"))
		case TaskCollection:
			models = append(models, index("maintenance_due", false, "run_id", "unsettled", "next_action_at", "id"))
			models = append(models, index("original_call", true, "run_id", "invocation_id", "call_id"), index("remote_handle", false, "protocol", "connection_id", "remote_id"), index("unsettled", false, "run_id", "unsettled", "id"))
		case TaskDeliveryCollection:
			models = append(models, index("delivery_state", false, "run_id", "status", "id"))
		case CheckpointCollection:
			models = append(models, index("run_invocation", true, "run_id", "invocation_id"))
		case SnapshotCollection:
			models = append(models, index("session_version", true, "session_id", "version"))
		case MutationReceiptCollection:
			models = append(models, index("run_operation", true, "run_id", "call_id"))
		}
		if _, err := b.collection(name).Indexes().CreateMany(ctx, models); err != nil {
			return safeError(err)
		}
	}
	_, err := b.collection(ClockCollection).UpdateOne(ctx, bson.M{"_id": "clock"}, bson.M{"$setOnInsert": schema.Clock{ID: "clock", Schema: schema.DocumentVersion}}, options.UpdateOne().SetUpsert(true))
	return safeError(err)
}
