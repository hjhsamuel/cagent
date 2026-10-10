package httpapi_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/hjhsamuel/cagent/internal/adapter/mongodb"
	"github.com/hjhsamuel/cagent/internal/bootstrap"
	"github.com/hjhsamuel/cagent/internal/config"
	"github.com/hjhsamuel/cagent/internal/storage/schema"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// 从生产装配入口启动真实监听、MongoDB 探针和 ADK 工厂，并验证停止后的端口释放。
func TestProductionBootstrapStartStopAndOccupiedPort(t *testing.T) {
	_, dbCfg := testDatabase(t)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := l.Addr().String()
	l.Close()
	cfg := config.Defaults()
	// 此启动夹具不部署本地工具；空目录仍经过生产扫描路径。
	cfg.Tools.LocalDir = t.TempDir()
	cfg.HTTP = settings()
	cfg.HTTP.Address = address
	cfg.MongoDB = config.MongoDB{URI: dbCfg.URI, Database: dbCfg.Database}
	cfg.ModelEncryption = config.ModelEncryption{Keys: map[string]string{"v1": "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="}}
	ring, err := config.NewKeyring(cfg.ModelEncryption)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ring.Encrypt("test-key", 1)
	if err != nil {
		t.Fatal(err)
	}
	provision, err := mongo.Connect(options.Client().ApplyURI(dbCfg.URI))
	if err != nil {
		t.Fatal("could not connect model fixture")
	}
	defer provision.Disconnect(context.Background())
	doc := schema.Model{ID: "test-model", Model: "test-model", Provider: "GLM", BaseURL: "http://127.0.0.1:1/v1", APIKeys: []schema.EncryptedKey{key}, Options: schema.ModelConfig{WindowTokens: 32768, RequestTimeout: "2m"}}
	if _, err := provision.Database(dbCfg.Database).Collection(mongodb.ModelCollection).InsertOne(context.Background(), doc); err != nil {
		t.Fatal("could not provision model fixture")
	}
	parent, stop := context.WithCancel(context.Background())
	defer stop()
	result := make(chan error, 1)
	go func() { result <- bootstrap.Run(parent, cfg) }()
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(10 * time.Second)
	for {
		r, e := client.Get("http://" + address + "/readyz")
		if e == nil {
			io.Copy(io.Discard, r.Body)
			r.Body.Close()
			if r.StatusCode == 200 {
				break
			}
		}
		select {
		case err := <-result:
			t.Fatalf("startup failed: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("server did not become ready")
		}
		time.Sleep(20 * time.Millisecond)
	}
	stop()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("production shutdown blocked")
	}
	l, err = net.Listen("tcp", address)
	if err != nil {
		t.Fatal("port was not released", err)
	}
	defer l.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err = bootstrap.Run(ctx, cfg); err == nil {
		t.Fatal("occupied port accepted")
	}
	// 配置失败必须在数据库连接之前返回，不依赖可达外部服务。
	cfg.HTTP.JWT.Secret = ""
	if err = bootstrap.Run(ctx, cfg); err == nil {
		t.Fatal("missing JWT config accepted")
	}
}
