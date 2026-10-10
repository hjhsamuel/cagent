package mongodb

import (
	"context"

	"github.com/hjhsamuel/cagent/internal/storage/schema"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// ListToolConnections 仅供启动装配读取管理员维护的远端连接目录。
// 不按协议过滤，避免错误配置被静默忽略；bootstrap 负责完整校验和授权。
func (b *Database) ListToolConnections(ctx context.Context) ([]schema.ToolConnection, error) {
	ctx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()
	cur, err := b.collection(ToolConnectionCollection).Find(ctx, bson.M{}, options.Find().SetSort(bson.D{{Key: "tenant_id", Value: 1}, {Key: "user_id", Value: 1}, {Key: "id", Value: 1}}))
	if err != nil {
		return nil, safeError(err)
	}
	defer cur.Close(ctx)
	var docs []schema.ToolConnection
	if err := cur.All(ctx, &docs); err != nil {
		return nil, safeError(err)
	}
	return docs, nil
}
