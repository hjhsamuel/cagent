package mongodb

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

var integrationURI string
var databaseCounter atomic.Int64

// CAGENT_TEST_MONGOD 启动临时真实副本集；CAGENT_TEST_MONGODB_URI 可使用已有测试集群。
// 二者都不设置时明确跳过集成测试，绝不把跳过当成通过。所有数据库名自动生成，
// 不接受用户提供数据库名称；清理仅删除本次创建的 cagent_p3_test_* 数据库。
func TestMain(m *testing.M) {
	integrationURI = os.Getenv("CAGENT_TEST_MONGODB_URI")
	cleanup := func() {}
	if executable := os.Getenv("CAGENT_TEST_MONGOD"); executable != "" {
		uri, stop, err := startMongo(executable, true)
		if err != nil {
			fmt.Fprintln(os.Stderr, "MongoDB fixture failed:", safeError(err))
			os.Exit(1)
		}
		integrationURI = uri
		cleanup = stop
	}
	code := m.Run()
	cleanup()
	os.Exit(code)
}

func startMongo(executable string, replica bool) (string, func(), error) {
	dir, err := os.MkdirTemp("", "cagent-mongodb-test-")
	if err != nil {
		return "", nil, err
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", nil, err
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	args := []string{"--bind_ip", "127.0.0.1", "--port", strconv.Itoa(port), "--dbpath", dir, "--logpath", filepath.Join(dir, "mongod.log"), "--setParameter", "enableTestCommands=1"}
	if replica {
		args = append(args, "--replSet", "cagent_test")
	}
	cmd := exec.Command(executable, args...)
	hideTestProcess(cmd)
	if err = cmd.Start(); err != nil {
		_ = os.RemoveAll(dir)
		return "", nil, err
	}
	stop := func() { _ = cmd.Process.Kill(); _ = cmd.Wait(); _ = os.RemoveAll(dir) }
	uri := fmt.Sprintf("mongodb://127.0.0.1:%d/?directConnection=true", port)
	client, err := mongo.Connect(options.Client().ApplyURI(uri).SetServerSelectionTimeout(300 * time.Millisecond).SetHeartbeatInterval(500 * time.Millisecond))
	if err != nil {
		stop()
		return "", nil, err
	}
	defer client.Disconnect(context.Background())
	deadline := time.Now().Add(40 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		err = client.Database("admin").RunCommand(ctx, bson.D{{Key: "ping", Value: 1}}).Err()
		cancel()
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			stop()
			return "", nil, err
		}
		time.Sleep(100 * time.Millisecond)
	}
	if replica {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err = client.Database("admin").RunCommand(ctx, bson.D{{Key: "replSetInitiate", Value: bson.M{"_id": "cagent_test", "members": bson.A{bson.M{"_id": 0, "host": fmt.Sprintf("127.0.0.1:%d", port)}}}}}).Err()
		cancel()
		if err != nil {
			stop()
			return "", nil, err
		}
		for {
			var hello struct {
				Primary bool `bson:"isWritablePrimary"`
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			err = client.Database("admin").RunCommand(ctx, bson.D{{Key: "hello", Value: 1}}).Decode(&hello)
			cancel()
			if err == nil && hello.Primary {
				break
			}
			if time.Now().After(deadline) {
				stop()
				return "", nil, fmt.Errorf("replica primary election timed out")
			}
			time.Sleep(100 * time.Millisecond)
		}
		uri += "&replicaSet=cagent_test"
	}
	return uri, stop, nil
}

func testDatabase(t *testing.T) (*Database, Options) {
	t.Helper()
	if integrationURI == "" {
		t.Skip("real MongoDB integration skipped: set CAGENT_TEST_MONGOD or CAGENT_TEST_MONGODB_URI")
	}
	name := fmt.Sprintf("cagent_p3_test_%d_%d", time.Now().UnixNano(), databaseCounter.Add(1))
	cfg := Options{URI: integrationURI, Database: name, Timeout: 15 * time.Second}
	db, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if !strings.HasPrefix(name, "cagent_p3_test_") {
			t.Error("unsafe cleanup name")
			return
		}
		if err := db.db.Drop(ctx); err != nil {
			t.Error(safeError(err))
		}
		if err := db.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	return db, cfg
}
