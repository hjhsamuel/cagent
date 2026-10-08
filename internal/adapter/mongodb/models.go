package mongodb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/hjhsamuel/cagent/internal/storage/schema"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// GetModel 读取管理员维护的全局配置，不暴露于租户 HTTP API。
func (b *Database) GetModel(ctx context.Context, id string) (schema.Model, error) {
	if strings.TrimSpace(id) == "" {
		return schema.Model{}, invalid("model.id")
	}
	ctx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()
	raw, err := b.collection(ModelCollection).FindOne(ctx, bson.M{"_id": id}).Raw()
	if err != nil {
		return schema.Model{}, safeError(err)
	}
	return decodeModel(raw)
}

// ListModels 加载模型目录，不选择默认模型；排序只保证快照顺序稳定。
func (b *Database) ListModels(ctx context.Context) ([]schema.Model, error) {
	ctx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()
	cur, err := b.collection(ModelCollection).Find(ctx, bson.M{}, options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}))
	if err != nil {
		return nil, safeError(err)
	}
	defer cur.Close(ctx)
	var docs []schema.Model
	for cur.Next(ctx) {
		doc, err := decodeModel(cur.Current)
		if err != nil {
			return nil, err
		}
		docs = append(docs, doc)
	}
	if err := cur.Err(); err != nil {
		return nil, safeError(err)
	}
	return docs, nil
}

// ReplaceModelKeys 原子替换密文，拒绝覆盖并发修改。
func (b *Database) ReplaceModelKeys(ctx context.Context, old, updated []schema.Model) error {
	if len(old) != len(updated) {
		return invalid("model.keys")
	}
	return b.withTransaction(ctx, "model.keys.rotate", func(tx context.Context) error {
		for i, doc := range old {
			result, err := b.collection(ModelCollection).UpdateOne(tx, bson.M{"_id": doc.ID, "api_keys": doc.APIKeys}, bson.M{"$set": bson.M{"api_keys": updated[i].APIKeys}})
			if err != nil {
				return err
			}
			if result.MatchedCount != 1 {
				return errors.New("model keys changed during rotation")
			}
		}
		return nil
	})
}

func decodeModel(raw bson.Raw) (schema.Model, error) {
	var doc schema.Model
	decoder := bson.NewDecoder(bson.NewDocumentReader(bytes.NewReader(raw)))
	// thinking.value 的嵌套对象必须保持 JSON object，而不是 BSON D 的元素数组。
	decoder.DefaultDocumentM()
	if err := decoder.Decode(&doc); err != nil {
		return schema.Model{}, safeError(err)
	}
	return doc, nil
}

// WriteModel applies a catalog snapshot precondition to avoid silently
// overwriting another instance's changes. Nil old creates; nil updated deletes.
func (b *Database) WriteModel(ctx context.Context, old, updated *schema.Model) error {
	if old == nil && updated == nil {
		return invalid("model")
	}
	ctx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()
	if old == nil {
		if strings.TrimSpace(updated.ID) == "" {
			return invalid("model.id")
		}
		_, err := b.collection(ModelCollection).InsertOne(ctx, updated)
		return safeError(err)
	}
	if strings.TrimSpace(old.ID) == "" || (updated != nil && updated.ID != old.ID) {
		return invalid("model.id")
	}
	return b.withTransaction(ctx, "model.configure", func(tx context.Context) error {
		filter := bson.M{"_id": old.ID}
		raw, err := b.collection(ModelCollection).FindOne(tx, filter).Raw()
		if err != nil {
			return err
		}
		current, err := decodeModel(raw)
		if err != nil {
			return err
		}
		// Canonical JSON comparison ignores BSON object field order, including
		// nested thinking values, while detecting changes in all known fields.
		expected, err := json.Marshal(old)
		if err != nil {
			return invalid("model")
		}
		actual, err := json.Marshal(current)
		if err != nil {
			return invalid("model")
		}
		if !bytes.Equal(expected, actual) {
			return conflict("model")
		}
		if updated == nil {
			_, err = b.collection(ModelCollection).DeleteOne(tx, filter)
		} else {
			_, err = b.collection(ModelCollection).ReplaceOne(tx, filter, updated)
		}
		return err
	})
}
