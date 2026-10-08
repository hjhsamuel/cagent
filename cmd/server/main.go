// Command server 是 HTTP/SSE 与 Agent 运行时的唯一进程入口。
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/gin-gonic/gin"
	"github.com/hjhsamuel/cagent/internal/apperrors"
	"github.com/hjhsamuel/cagent/internal/bootstrap"
	"github.com/hjhsamuel/cagent/internal/config"
	"github.com/hjhsamuel/cagent/internal/observability"
	_ "github.com/joho/godotenv/autoload" // 启动时加载工作目录的 .env，已有环境变量优先。
	"github.com/sirupsen/logrus"
)

func main() { os.Exit(run(os.LookupEnv)) }

// run 先校验完整配置，再初始化日志和业务依赖。使用返回码而非 Fatal，保证
// defer 能释放应用、数据库与日志；SIGINT/SIGTERM 触发优雅关闭。
func run(lookup func(string) (string, bool)) int {
	cfg, err := config.LoadFromEnv(lookup, os.Environ()...)
	if err == nil {
		err = cfg.HTTP.ValidateServer()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "process.start_failed:", err)
		return 1
	}
	if err = observability.Init(cfg.Logging); err != nil {
		fmt.Fprintln(os.Stderr, "process.start_failed")
		return 1
	}
	defer observability.Close()
	gin.SetMode(gin.ReleaseMode)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logrus.WithField("process_id", os.Getpid()).Info("process.starting")
	if err = bootstrap.Run(ctx, cfg); err != nil {
		logrus.WithField("process_id", os.Getpid()).WithError(err).Error("process.failed")
		fmt.Fprintln(os.Stderr, "process.start_failed:", startupError(err))
		return 1
	}
	logrus.WithField("process_id", os.Getpid()).Info("process.stopped")
	return 0
}

// startupError 只显示边界提供的安全说明，不回显驱动错误或凭据。
func startupError(err error) string {
	var detail *apperrors.Error
	if errors.As(err, &detail) {
		return detail.Error()
	}
	for _, kind := range []apperrors.Kind{apperrors.ErrInvalidArgument, apperrors.ErrNotFound, apperrors.ErrConflict, apperrors.ErrUnsupported, apperrors.ErrOverloaded} {
		if errors.Is(err, kind) {
			return kind.Error()
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "startup timed out"
	}
	if errors.Is(err, context.Canceled) {
		return "startup cancelled"
	}
	return "startup failed; check the configured log file"
}
