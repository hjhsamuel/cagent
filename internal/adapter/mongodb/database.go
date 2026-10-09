// Package mongodb 实现 P2 存储契约。事务采用 snapshot 读与 majority 写；所有
// 作用域过滤在适配器统一构造，领域层不引用 BSON 或驱动类型。
package mongodb

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/hjhsamuel/cagent/internal/apperrors"
	"github.com/hjhsamuel/cagent/internal/domain"
	"github.com/hjhsamuel/cagent/internal/storage/schema"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readconcern"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"
	"go.mongodb.org/mongo-driver/v2/mongo/writeconcern"
)

// Options 不自动读取环境或记录 URI。Timeout 同时约束连接探测及每个完整事务，
// Context 中更早的截止时间优先；必须显式指定，避免不可控的后台重试。
type Options struct {
	URI, Database string
	Timeout       time.Duration
}

// Database 仅暴露普通作用域内能力，不包含跨租户恢复扫描或事件清理。
// 调用方停止业务后 Close；本对象不会自行启动后台运行、工具或恢复循环。
type Database struct {
	client    *mongo.Client
	db        *mongo.Database
	timeout   time.Duration
	eventOnce sync.Once
	eventHub  *eventHub
	// beforeCommit 仅由同包集成测试安装，用于在真实事务提交前注入故障。
	// 生产构造函数始终为 nil；测试不得在请求并发运行时修改它。
	beforeCommit func(string) error
}

// Open 验证 URI、连接/认证、事务拓扑，并幂等创建索引；任何失败关闭已建立客户端。
// 不支持 standalone，不静默降级非事务写入。支持 replica set 和 mongos。
func Open(ctx context.Context, cfg Options) (*Database, error) {
	b, err := connect(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err = b.ensureIndexes(ctx); err != nil {
		_ = b.Close(context.Background())
		return nil, err
	}
	if err = b.migrateRecovery(ctx); err != nil {
		_ = b.Close(context.Background())
		return nil, err
	}
	if err = b.migrateContextMetadata(ctx); err != nil {
		_ = b.Close(context.Background())
		return nil, err
	}
	return b, nil
}

func (b *Database) Close(ctx context.Context) error {
	b.notifications().stop()
	ctx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()
	return safeError(b.client.Disconnect(ctx))
}

// Ping 供就绪探针使用，不读取跨租户资源，且遵守调用方的更短超时。
func (b *Database) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()
	return safeError(b.client.Ping(ctx, readpref.Primary()))
}

func connect(ctx context.Context, cfg Options) (*Database, error) {
	if strings.TrimSpace(cfg.URI) == "" || strings.TrimSpace(cfg.Database) == "" || cfg.Timeout <= 0 {
		return nil, invalid("mongodb.options")
	}
	if strings.ContainsAny(cfg.Database, "/\\.\"$\x00 ") || len(cfg.Database) > 63 {
		return nil, invalid("mongodb.database")
	}
	opts := options.Client().ApplyURI(cfg.URI).
		SetAppName("cagent-storage").
		SetMonitor(commandMonitor()).
		SetTimeout(cfg.Timeout).
		SetReadPreference(readpref.Primary()).
		SetReadConcern(readconcern.Majority()).
		SetWriteConcern(writeconcern.Majority())
	if err := opts.Validate(); err != nil {
		return nil, apperrors.New(apperrors.ErrInvalidArgument, "mongodb.uri", "invalid MongoDB connection options")
	}
	client, err := mongo.Connect(opts)
	if err != nil {
		return nil, safeError(err)
	}
	b := &Database{client: client, db: client.Database(cfg.Database), timeout: cfg.Timeout}
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	if err = client.Ping(ctx, readpref.Primary()); err == nil {
		var hello schema.Hello
		err = client.Database("admin").RunCommand(ctx, bson.D{{Key: "hello", Value: 1}}).Decode(&hello)
		if err == nil && (hello.Sessions == nil || (hello.SetName == "" && hello.Msg != "isdbgrid")) {
			err = apperrors.New(apperrors.ErrUnsupported, "mongodb.topology", "transactions require a replica set or mongos")
		}
	}
	if err != nil {
		_ = b.Close(context.Background())
		return nil, safeError(err)
	}
	if err = b.validateCollections(ctx); err != nil {
		_ = b.Close(context.Background())
		return nil, safeError(err)
	}
	return b, nil
}

func invalid(field string) error {
	return apperrors.New(apperrors.ErrInvalidArgument, field, "invalid storage argument")
}
func conflict(field string) error {
	return apperrors.New(apperrors.ErrConflict, field, "storage precondition changed")
}
func invariant() error { return &storageError{message: "stored document invariant violated"} }

// 未分类驱动错误保持原因链，但展示内容固定，不回显 URI、凭据或被拒绝的文档。
type storageError struct {
	message string
	cause   error
}

func (e *storageError) Error() string              { return e.message }
func (e *storageError) Unwrap() error              { return e.cause }
func (e *storageError) Format(s fmt.State, v rune) { fmt.Fprintf(s, fmt.FormatString(s, v), e.message) }
func safeError(err error) error {
	if err == nil {
		return nil
	}
	var public *apperrors.Error
	var safe *storageError
	if errors.As(err, &public) || errors.As(err, &safe) {
		return err
	}
	if errors.Is(err, mongo.ErrNoDocuments) {
		return apperrors.Wrap(apperrors.ErrNotFound, "resource", "resource not found in scope", err)
	}
	if mongo.IsDuplicateKeyError(err) {
		return apperrors.Wrap(apperrors.ErrConflict, "resource", "unique constraint conflict", err)
	}
	return &storageError{message: "MongoDB operation failed", cause: err}
}

func scoped(scope domain.Scope) bson.M {
	return bson.M{"tenant_id": scope.TenantID, "user_id": scope.UserID}
}
func key(scope domain.Scope, id string) bson.M { f := scoped(scope); f["id"] = id; return f }
func validateKey(scope domain.Scope, id string) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(id) == "" {
		return invalid("resource.id")
	}
	return nil
}

// withTransaction 仅管理会话、超时和提交，不封装任何 collection 读写。
// 回调可能被驱动重放，必须重新构造局部状态，且不能触发模型、工具或事件发布。
// 回调中的驱动错误保留原类型，让 WithTransaction 识别瞬态标签；完成后才安全包装。
func (b *Database) withTransaction(ctx context.Context, name string, fn func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()
	s, err := b.client.StartSession()
	if err != nil {
		return safeError(err)
	}
	defer s.EndSession(ctx)
	_, err = s.WithTransaction(ctx, func(tx context.Context) (any, error) {
		if err := fn(tx); err != nil {
			return nil, err
		}
		if b.beforeCommit != nil {
			if err := b.beforeCommit(name); err != nil {
				return nil, err
			}
		}
		return nil, nil
	}, options.Transaction().SetReadConcern(readconcern.Snapshot()).SetWriteConcern(writeconcern.Majority()).SetReadPreference(readpref.Primary()))
	return safeError(err)
}

// now 使用服务端 $$NOW。必须在当前事务内调用，不能用进程时间判断租约所有权。
func (b *Database) now(ctx context.Context) (time.Time, error) {
	cur, err := b.collection(ClockCollection).Aggregate(ctx, mongo.Pipeline{{{Key: "$limit", Value: 1}}, {{Key: "$project", Value: bson.M{"_id": 0, "now": "$$NOW"}}}})
	if err != nil {
		return time.Time{}, err
	}
	defer cur.Close(ctx)
	if !cur.Next(ctx) {
		if err = cur.Err(); err != nil {
			return time.Time{}, err
		}
		return time.Time{}, invariant()
	}
	var row schema.ClockTime
	err = cur.Decode(&row)
	return row.Now, err
}

func (d *Database) collection(name string) *mongo.Collection { return d.db.Collection(name) }
