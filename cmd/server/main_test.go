package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
)

func TestMissingBusinessConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.log")
	if code := run(func(key string) (string, bool) { return path, key == "CAGENT_LOG_PATH" }); code != 1 {
		t.Fatalf("exit code %d", code)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("invalid config must fail before opening log: %v", err)
	}
}

func TestMissingModelEncryptionFailsBeforeLogInitialization(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.log")
	values := map[string]string{"CAGENT_LOG_PATH": path, "CAGENT_MONGODB_URI": "mongodb://localhost:27017", "CAGENT_HTTP_JWT_SECRET": strings.Repeat("a", 32)}
	// 其他进程参数使用合法默认值，密钥缺失时不能创建日志或连接数据库。
	if code := run(func(key string) (string, bool) { value, ok := values[key]; return value, ok }); code != 1 {
		t.Fatal("startup accepted missing encryption")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("invalid encryption initialized logging", err)
	}
}
func TestInvalidLoggingConfiguration(t *testing.T) {
	// 文件配置无效时使用 stderr，不在默认路径创建文件，不重配全局 logger。
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	oldStderr := os.Stderr
	os.Stderr = writer
	defer func() { os.Stderr = oldStderr }()
	oldOutput := logrus.StandardLogger().Out
	code := run(func(key string) (string, bool) { return "-1", key == "CAGENT_LOG_SIZE" })
	writer.Close()
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if code != 1 || !strings.Contains(string(data), "process.start_failed") || !strings.Contains(string(data), "logging.size") {
		t.Fatalf("missing failure: %s", data)
	}
	if logrus.StandardLogger().Out != oldOutput {
		t.Fatal("invalid config replaced output")
	}
}
