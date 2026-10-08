// Package bootstrap 装配唯一服务进程，负责依赖的反向释放和有界关闭。
package bootstrap

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/hjhsamuel/cagent/internal/adapter/adk"
	"github.com/hjhsamuel/cagent/internal/adapter/mongodb"
	"github.com/hjhsamuel/cagent/internal/app"
	"github.com/hjhsamuel/cagent/internal/config"
	"github.com/hjhsamuel/cagent/internal/tool"
	"github.com/hjhsamuel/cagent/internal/transport/httpapi"
	"github.com/sirupsen/logrus"
)

// Run 在所有配置与数据库初始化成功后才监听。parent 只提供停止信号，
// 应用和 HTTP 各有进程上下文，确保关闭顺序不受请求取消影响。
func Run(parent context.Context, cfg config.Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := cfg.HTTP.ValidateServer(); err != nil {
		return err
	}
	ring, err := config.NewKeyring(cfg.ModelEncryption)
	if err != nil {
		return err
	}
	db, err := mongodb.Open(parent, mongodb.Options{URI: cfg.MongoDB.URI, Database: cfg.MongoDB.Database, Timeout: 10 * time.Second})
	if err != nil {
		return err
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), cfg.HTTP.ShutdownGrace)
		defer cancel()
		_ = db.Close(ctx)
	}()
	cfg, err = loadModels(parent, db, cfg, ring)
	if err != nil {
		return err
	}
	catalog, maxCalls, closeTools, err := loadTools(parent, cfg.ToolsFile)
	if err != nil {
		return err
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), cfg.HTTP.ShutdownGrace)
		defer cancel()
		closeTools(ctx)
	}()
	var toolOptions []adk.ToolOptions
	var registry tool.Registry
	if catalog != nil {
		registry = catalog
		toolOptions = append(toolOptions, adk.ToolOptions{Registry: catalog, MaxModelCalls: maxCalls})
	}
	application, err := app.NewOpenAIService(context.Background(), db, cfg, nil, app.Options{LeaseDuration: 30 * time.Second, PollInterval: time.Second, Registry: registry, Tasks: cfg.Tasks}, toolOptions...)
	if err != nil {
		return err
	}
	applicationClosed := false
	var recovery *mongodb.Recovery
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), cfg.HTTP.ShutdownGrace)
		defer cancel()
		if !applicationClosed {
			_ = application.Close(ctx)
		}
		if recovery != nil {
			_ = recovery.Close(ctx)
		}
	}()
	events, err := app.NewEventStream(db, 250*time.Millisecond, 128)
	if err != nil {
		return err
	}
	// 管理扫描连接独立装配；退出时先停止应用扫描，再关闭管理连接和普通数据库。
	recovery, err = mongodb.OpenRecovery(parent, mongodb.Options{URI: cfg.MongoDB.URI, Database: cfg.MongoDB.Database, Timeout: 10 * time.Second})
	if err != nil {
		return err
	}
	if err = application.StartRecovery(recovery, cfg.Tasks.ReconnectBackoff); err != nil {
		return err
	}
	var stopping atomic.Bool
	handler, err := httpapi.New(application, events, cfg.HTTP, cfg.Agent.Name, func(ctx context.Context) bool {
		if stopping.Load() {
			return false
		}
		ctx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		return db.Ping(ctx) == nil && !stopping.Load()
	}, app.TaskInputs{DB: db, Registry: registry})
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", cfg.HTTP.Address)
	if err != nil {
		return errors.New("HTTP listener could not start")
	}
	logrus.WithField("address", listener.Addr().String()).Info("http.listening")
	return serve(parent, listener, handler, cfg.HTTP.ShutdownGrace, func() { stopping.Store(true) }, func(ctx context.Context) error {
		// serve 已消耗关闭宽限期时不在 defer 中重新等待一个完整宽限期。
		applicationClosed = true
		return application.Close(ctx)
	})
}

// serve 先撤销就绪，再取消请求上下文使 SSE 退出，然后停止接收请求并等待 handler。
// 随后取消并等待运行，最后由 Run 关闭数据库。超过宽限期强制关闭 socket，返回错误
// 使进程以失败退出；不把进程关闭持久化为用户取消，重启后扫描原 Run 恢复。
func serve(parent context.Context, listener net.Listener, handler http.Handler, grace time.Duration, notReady func(), closeApp func(context.Context) error) error {
	requests, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10, BaseContext: func(net.Listener) context.Context { return requests }}
	result := make(chan error, 1)
	go func() { result <- server.Serve(listener) }()
	var serveErr error
	select {
	case <-parent.Done():
	case serveErr = <-result:
	}
	notReady()
	cancel()
	ctx, stop := context.WithTimeout(context.Background(), grace)
	defer stop()
	shutdownErr := server.Shutdown(ctx)
	if shutdownErr != nil {
		_ = server.Close()
	}
	appErr := closeApp(ctx)
	if errors.Is(serveErr, http.ErrServerClosed) {
		serveErr = nil
	}
	return errors.Join(serveErr, shutdownErr, appErr)
}
