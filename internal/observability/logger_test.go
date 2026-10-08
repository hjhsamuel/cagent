package observability_test

import (
	"compress/gzip"
	"errors"
	"fmt"
	"github.com/hjhsamuel/cagent/internal/apperrors"
	"github.com/hjhsamuel/cagent/internal/config"
	"github.com/hjhsamuel/cagent/internal/observability"
	"github.com/sirupsen/logrus"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// 全局配置测试串行执行。只写 TempDir，关闭文件后恢复全局配置。
func initLogging(t *testing.T, level string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cagent.log")
	std := logrus.StandardLogger()
	oldLevel, oldOutput, oldFormatter := std.GetLevel(), std.Out, std.Formatter
	t.Cleanup(func() {
		if err := observability.Close(); err != nil {
			t.Error(err)
		}
		logrus.SetLevel(oldLevel)
		logrus.SetOutput(oldOutput)
		logrus.SetFormatter(oldFormatter)
	})
	if err := observability.Init(config.Logging{Level: level, Path: path, Size: 1, Rolls: 2}); err != nil {
		t.Fatal(err)
	}
	if logrus.StandardLogger() != std {
		t.Fatal("standard logger replaced")
	}
	return path
}
func readLog(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
func TestFileOutputAndLevels(t *testing.T) {
	levels := []string{"trace", "debug", "info", "warn", "error"}
	for minimum, level := range levels {
		t.Run(level, func(t *testing.T) {
			path := initLogging(t, level)
			logrus.Trace("trace.event")
			logrus.Debug("debug.event")
			logrus.Info("info.event")
			logrus.Warn("warn.event")
			logrus.Error("error.event")
			output := readLog(t, path)
			for i, event := range levels {
				if strings.Contains(output, event+".event") != (i >= minimum) {
					t.Fatalf("incorrect filtering: %s", output)
				}
			}
			if strings.Count(output, "\n") != len(levels)-minimum || !strings.Contains(output, "time=") || strings.Contains(output, "\x1b[") {
				t.Fatal("invalid text output")
			}
		})
	}
}
func TestInvalidConfigurationLeavesLoggerUnchanged(t *testing.T) {
	path := initLogging(t, "info")
	std := logrus.StandardLogger()
	output, formatter, level := std.Out, std.Formatter, std.GetLevel()
	for _, options := range []config.Logging{{}, {Level: "info", Path: path, Size: -1}, {Level: "info", Path: path, Size: 1, Rolls: -1}} {
		if err := observability.Init(options); !errors.Is(err, apperrors.ErrInvalidArgument) {
			t.Fatal("invalid configuration accepted")
		}
		if std.Out != output || std.Formatter != formatter || std.GetLevel() != level {
			t.Fatal("invalid init changed globals")
		}
	}
	logrus.Info("still.usable")
	if !strings.Contains(readLog(t, path), "still.usable") {
		t.Fatal("logger lost")
	}
}
func TestExplicitFieldsAcrossGoroutines(t *testing.T) {
	path := initLogging(t, "info")
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("worker-%d", i)
			// 关联 ID 在输出位置显式提供，不保存到全局 logger 或共享映射。
			logrus.WithFields(logrus.Fields{"request_id": id, "session_id": id, "run_id": id, "task_id": id, "invocation_id": id}).Info("worker.done")
		}(i)
	}
	wg.Wait()
	logrus.Info("unrelated")
	lines := strings.Split(strings.TrimSpace(readLog(t, path)), "\n")
	if len(lines) != 65 {
		t.Fatalf("lost/duplicate logs: %d", len(lines))
	}
	seen := map[string]bool{}
	for _, line := range lines[:64] {
		fields := map[string]string{}
		for _, token := range strings.Fields(line) {
			key, value, ok := strings.Cut(token, "=")
			if ok {
				fields[key] = value
			}
		}
		id := fields["request_id"]
		if id == "" || seen[id] {
			t.Fatal("missing/duplicate worker")
		}
		seen[id] = true
		for _, key := range []string{"session_id", "run_id", "task_id", "invocation_id"} {
			if fields[key] != id {
				t.Fatalf("mixed correlation: %s", line)
			}
		}
	}
	if strings.Contains(lines[64], "_id=") {
		t.Fatal("global logger retained IDs")
	}
}

// 实际写入超过 1 MiB，验证轮转后的活动文件及压缩备份内容。
func TestFileRotationAndCompression(t *testing.T) {
	path := initLogging(t, "info")
	logrus.Info("first-record-" + strings.Repeat("a", 600*1024))
	logrus.Info("second-record-" + strings.Repeat("b", 600*1024))
	if err := observability.Close(); err != nil {
		t.Fatal(err)
	}
	current := readLog(t, path)
	if strings.Contains(current, "first-record-") || !strings.Contains(current, "second-record-") {
		t.Fatal("file did not rotate")
	}
	// 压缩异步执行，等完整 gzip 可读且原备份移除后再清理临时目录。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		files, err := filepath.Glob(filepath.Join(filepath.Dir(path), "cagent-*.log.gz"))
		if err != nil {
			t.Fatal(err)
		}
		if len(files) == 1 {
			file, err := os.Open(files[0])
			if err == nil {
				z, err := gzip.NewReader(file)
				if err == nil {
					data, readErr := io.ReadAll(z)
					z.Close()
					file.Close()
					_, statErr := os.Stat(strings.TrimSuffix(files[0], ".gz"))
					if readErr == nil && os.IsNotExist(statErr) {
						if !strings.Contains(string(data), "first-record-") {
							t.Fatal("backup lost data")
						}
						return
					}
				} else {
					file.Close()
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("compressed backup did not complete")
}
