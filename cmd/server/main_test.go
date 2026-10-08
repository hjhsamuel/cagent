package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"testing"

	"github.com/hjhsamuel/cagent/internal/apperrors"
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

func TestStartupErrorShowsSafeDetails(t *testing.T) {
	err := apperrors.Wrap(apperrors.ErrUnsupported, "mongodb.topology", "transactions require a replica set or mongos", errors.New("private-credentials"))
	text := startupError(err)
	if !strings.Contains(text, "mongodb.topology") || !strings.Contains(text, "replica set") || strings.Contains(text, "private-credentials") {
		t.Fatal("unsafe or missing startup diagnosis")
	}
	if text := startupError(errors.New("private-credentials")); strings.Contains(text, "private-credentials") {
		t.Fatal("raw error leaked")
	}
	if text := startupError(context.DeadlineExceeded); !strings.Contains(text, "timed out") {
		t.Fatal("timeout diagnosis missing")
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
