package mongodb

import (
	"bytes"
	"context"
	"strings"

	"github.com/hjhsamuel/cagent/internal/storage/schema"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// GetModel 只供启动读取管理员维护的全局配置，不暴露于租户 HTTP API。
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
