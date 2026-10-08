// Package httpapi 仅负责可信身份、DTO 和 HTTP 编码，不持有运行生命周期。
package httpapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/hjhsamuel/cagent/internal/app"
	"github.com/hjhsamuel/cagent/internal/apperrors"
	"github.com/hjhsamuel/cagent/internal/config"
	"github.com/hjhsamuel/cagent/internal/domain"
	"github.com/hjhsamuel/cagent/internal/observability"
	"github.com/hjhsamuel/cagent/internal/store"
	"github.com/hjhsamuel/cagent/internal/tool"
	"github.com/sirupsen/logrus"
)

// Events 是只读订阅边界，HTTP 无权直接发布事件或更改持久序号。
type Events interface {
	CheckCursor(context.Context, domain.Scope, string, int64) error
	Follow(context.Context, domain.Scope, string, int64, func(domain.Event) error) error
}

// Claims 的 sub 是用户身份，tenant_id 是签发方确认的租户。任何普通头或请求体
// 中的同名字段均不参与授权；必须完成签名与标准声明验证后才能转换为 Scope。
type Claims struct {
	TenantID string `json:"tenant_id"`
	jwt.RegisteredClaims
}

// New 构造 Gin 路由。ready 必须检查当前依赖可用性；服务关闭时由装配层返回 false。
func New(service app.Service, events Events, cfg config.HTTP, agentName string, ready func(context.Context) bool, taskInputs ...TaskInputs) (http.Handler, error) {
	if err := cfg.ValidateServer(); err != nil {
		return nil, err
	}
	if service == nil || events == nil || ready == nil {
		return nil, errors.New("HTTP dependencies are required")
	}
	subscriptions := observability.NewGate(cfg.MaxSubscriptions, "sse")
	r := gin.New()
	r.Use(func(c *gin.Context) {
		ctx, finish := observability.Default.Start(c.Request.Context(), "request")
		c.Request = c.Request.WithContext(ctx)
		defer func() {
			var err error
			if c.Writer.Status() >= 400 {
				err = errors.New("request failed")
			}
			finish(err)
		}()
		id := uuid.NewString()
		c.Set("request_id", id)
		c.Header("X-Request-ID", id)
		c.Header("X-Trace-ID", observability.TraceID(ctx))
		defer func() {
			if recover() != nil {
				if !c.Writer.Written() {
					failure(c, 500, "internal_error")
				}
				c.Abort()
			}
			logrus.WithFields(logrus.Fields{"request_id": id, "method": c.Request.Method, "route": c.FullPath(), "status": c.Writer.Status(), "session_id": c.Param("sessionID"), "run_id": c.Param("runID")}).Info("http.request")
		}()
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, int64(cfg.MaxBodyBytes))
		c.Next()
	})
	r.GET("/healthz", func(c *gin.Context) { c.JSON(200, gin.H{"status": "ok"}) })
	r.GET("/readyz", func(c *gin.Context) {
		if !ready(c.Request.Context()) {
			failure(c, 503, "not_ready")
			return
		}
		c.JSON(200, gin.H{"status": "ready"})
	})
	// 运维凭据与用户 JWT 分离，不允许普通租户读取实例级观测信息。
	if cfg.DiagnosticsToken != "" {
		admin := r.Group("/debug", func(c *gin.Context) {
			if subtle.ConstantTimeCompare([]byte(c.GetHeader("Authorization")), []byte("Bearer "+cfg.DiagnosticsToken)) != 1 {
				unauthorized(c)
				return
			}
			c.Next()
		})
		admin.GET("/metrics", gin.WrapH(observability.Default))
		admin.GET("/traces", func(c *gin.Context) { c.JSON(200, observability.Default.Spans()) })
	}
	api := r.Group("/api/v1", func(c *gin.Context) {
		parts := strings.Fields(c.GetHeader("Authorization"))
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			unauthorized(c)
			return
		}
		claims := new(Claims)
		token, err := jwt.ParseWithClaims(parts[1], claims, func(t *jwt.Token) (any, error) {
			return []byte(cfg.JWT.Secret), nil
		}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
			jwt.WithExpirationRequired(),
		)
		scope := domain.Scope{TenantID: claims.TenantID, UserID: claims.Subject}
		if err != nil || token == nil || !token.Valid || scope.Validate() != nil {
			unauthorized(c)
			return
		}
		c.Set("scope", scope)
		c.Next()
	})
	api.POST("/sessions", func(c *gin.Context) {
		s, err := service.CreateSession(c.Request.Context(), scopeOf(c), agentName)
		if err != nil {
			respondError(c, err)
			return
		}
		c.JSON(201, sessionDTO(s))
	})
	api.GET("/sessions/:sessionID", func(c *gin.Context) {
		s, err := service.GetSession(c.Request.Context(), scopeOf(c), c.Param("sessionID"))
		if err != nil {
			respondError(c, err)
			return
		}
		c.JSON(200, sessionDTO(s))
	})
	api.POST("/sessions/:sessionID/runs", func(c *gin.Context) {
		// P7 最小输入只接受文本，不允许客户端伪造 system/tool 消息或内部调用关联。
		var body struct {
			Text string `json:"text"`
		}
		decoder := json.NewDecoder(c.Request.Body)
		err := decoder.Decode(&body)
		if err == nil {
			var extra any
			if trailingErr := decoder.Decode(&extra); trailingErr != io.EOF {
				err = trailingErr
				if err == nil {
					err = apperrors.ErrInvalidArgument
				}
			}
		}
		if err != nil {
			var large *http.MaxBytesError
			if errors.As(err, &large) {
				failure(c, 413, "body_too_large")
			} else {
				failure(c, 400, "invalid_argument")
			}
			return
		}
		run, err := service.StartRun(c.Request.Context(), app.StartRun{Scope: scopeOf(c), SessionID: c.Param("sessionID"), Input: []domain.Part{{Kind: domain.PartText, Text: body.Text}}, IdempotencyKey: c.GetHeader("Idempotency-Key")})
		if err != nil {
			respondError(c, err)
			return
		}
		c.JSON(202, runDTO(run))
	})
	api.GET("/runs/:runID", func(c *gin.Context) {
		run, err := service.GetRun(c.Request.Context(), scopeOf(c), c.Param("runID"))
		if err != nil {
			respondError(c, err)
			return
		}
		c.JSON(200, runDTO(run))
	})
	api.POST("/runs/:runID/cancel", func(c *gin.Context) {
		if err := service.CancelRun(c.Request.Context(), scopeOf(c), c.Param("runID")); err != nil {
			respondError(c, err)
			return
		}
		c.Status(204)
	})
	api.GET("/runs/:runID/events", func(c *gin.Context) {
		release, err := subscriptions.Try(c.Request.Context())
		if err != nil {
			respondError(c, err)
			return
		}
		defer release()
		serveEvents(c, events, cfg)
	})
	api.GET("/tasks/:taskID", func(c *gin.Context) {
		tasks, ok := service.(Tasks)
		if !ok {
			respondError(c, apperrors.ErrUnsupported)
			return
		}
		t, e := tasks.GetTask(c.Request.Context(), scopeOf(c), c.Param("taskID"))
		if e != nil {
			respondError(c, e)
			return
		}
		// 显式 DTO 不暴露 Scope、远端句柄、连接引用、工具参数或检查点。
		c.JSON(200, taskDTO(t))
	})
	api.POST("/tasks/:taskID/cancel", func(c *gin.Context) {
		tasks, ok := service.(Tasks)
		if !ok {
			respondError(c, apperrors.ErrUnsupported)
			return
		}
		if e := tasks.CancelTask(c.Request.Context(), scopeOf(c), c.Param("taskID")); e != nil {
			respondError(c, e)
			return
		}
		c.Status(202)
	})
	if len(taskInputs) > 1 {
		return nil, apperrors.ErrInvalidArgument
	}
	for _, route := range []string{"input", "authorization"} {
		authorization := route == "authorization"
		api.POST("/tasks/:taskID/"+route, func(c *gin.Context) {
			if len(taskInputs) == 0 || taskInputs[0] == nil {
				respondError(c, apperrors.ErrUnsupported)
				return
			}
			var body struct {
				Text          string `json:"text"`
				CredentialRef string `json:"credential_ref"`
			}
			dec := json.NewDecoder(c.Request.Body)
			dec.DisallowUnknownFields()
			err := dec.Decode(&body)
			var extra any
			if err != nil || dec.Decode(&extra) != io.EOF || strings.TrimSpace(body.Text) == "" || authorization != (body.CredentialRef != "") {
				respondError(c, apperrors.ErrInvalidArgument)
				return
			}
			if err = taskInputs[0].Submit(c.Request.Context(), scopeOf(c), c.Param("taskID"), tool.TaskInput{Text: body.Text, CredentialRef: body.CredentialRef}, authorization); err != nil {
				respondError(c, err)
				return
			}
			c.Status(202)
		})
	}
	return r, nil
}

// TaskInputs 将已认证 Scope 和本地任务 ID 交给应用边界，HTTP 不解析远端连接配置。
type TaskInputs interface {
	Submit(context.Context, domain.Scope, string, tool.TaskInput, bool) error
}

// Tasks 是已有远端任务的查询/取消命令边界；取消仅表示意图已持久化。
type Tasks interface {
	GetTask(context.Context, domain.Scope, string) (domain.Task, error)
	CancelTask(context.Context, domain.Scope, string) error
}

func scopeOf(c *gin.Context) domain.Scope { return c.MustGet("scope").(domain.Scope) }
func unauthorized(c *gin.Context) {
	c.Header("WWW-Authenticate", "Bearer")
	failure(c, 401, "unauthorized")
}

// failure 只输出稳定类别和服务端关联 ID，不暴露 JWT、驱动原因链或模型输入。
func failure(c *gin.Context, status int, code string) {
	c.AbortWithStatusJSON(status, gin.H{"error": gin.H{"code": code, "request_id": c.GetString("request_id")}})
}
func respondError(c *gin.Context, err error) {
	status, code := 500, "internal_error"
	switch {
	case errors.Is(err, apperrors.ErrOverloaded):
		status, code = 503, "overloaded"
		c.Header("Retry-After", "1")
	case errors.Is(err, store.ErrCursorExpired):
		status, code = 410, "cursor_expired"
	case errors.Is(err, apperrors.ErrInvalidArgument):
		status, code = 400, "invalid_argument"
	case errors.Is(err, apperrors.ErrNotFound):
		status, code = 404, "not_found"
	case errors.Is(err, apperrors.ErrConflict):
		status, code = 409, "conflict"
	case errors.Is(err, apperrors.ErrUnsupported):
		status, code = 501, "unsupported"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		status, code = 503, "unavailable"
	}
	failure(c, status, code)
}

// 显式 DTO 白名单隔离领域模型；不序列化 Scope、幂等键、租约或 SDK 检查点。
func sessionDTO(s domain.Session) any {
	return struct {
		ID          string    `json:"id"`
		AgentID     string    `json:"agent_id"`
		ActiveRunID string    `json:"active_run_id,omitempty"`
		Version     int64     `json:"version"`
		CreatedAt   time.Time `json:"created_at"`
		UpdatedAt   time.Time `json:"updated_at"`
	}{s.ID, s.AgentID, s.ActiveRunID, s.Version, s.CreatedAt, s.UpdatedAt}
}
func runDTO(r domain.Run) any {
	return struct {
		ID        string           `json:"id"`
		SessionID string           `json:"session_id"`
		Status    domain.RunStatus `json:"status"`
		Version   int64            `json:"version"`
		CreatedAt time.Time        `json:"created_at"`
		UpdatedAt time.Time        `json:"updated_at"`
	}{r.ID, r.SessionID, r.Status, r.Version, r.CreatedAt, r.UpdatedAt}
}

// serveEvents 只有当前 handler 写 socket。Follow 经无缓冲通道交付事件，至多有一条
// 待交付事件和应用层一页缓存；心跳不会另开并发 writer。退出先取消再等待订阅结束，
// 客户端断开仅结束读取，绝不调用 CancelRun。每次写入/flush 都刷新写截止时间。
func serveEvents(c *gin.Context, events Events, cfg config.HTTP) {
	traceCtx, finish := observability.Default.Start(c.Request.Context(), "sse")
	c.Request = c.Request.WithContext(traceCtx)
	var streamErr error
	defer func() { finish(streamErr) }()
	after := int64(0)
	if raw := c.GetHeader("Last-Event-ID"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 0 {
			failure(c, 400, "invalid_cursor")
			return
		}
		after = n
	}
	scope, id := scopeOf(c), c.Param("runID")
	if err := events.CheckCursor(c.Request.Context(), scope, id, after); err != nil {
		respondError(c, err)
		return
	}
	// Gin 的包装器不保证暴露 SetWriteDeadline，取底层 writer 交给标准控制器。
	controller := http.NewResponseController(c.Writer.(interface{ Unwrap() http.ResponseWriter }).Unwrap())
	if err := controller.SetWriteDeadline(time.Now().Add(cfg.WriteTimeout)); err != nil {
		respondError(c, err)
		return
	}
	defer controller.SetWriteDeadline(time.Time{})
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("X-Accel-Buffering", "no")
	write := func(s string) (err error) {
		defer func() {
			if err != nil {
				streamErr = err
			}
		}()
		if err := controller.SetWriteDeadline(time.Now().Add(cfg.WriteTimeout)); err != nil {
			return err
		}
		if _, err := io.WriteString(c.Writer, s); err != nil {
			return err
		}
		return controller.Flush()
	}
	if err := write(": connected\n\n"); err != nil {
		return
	}
	ctx, cancel := context.WithCancel(c.Request.Context())
	deliveries := make(chan domain.Event)
	done := make(chan error, 1)
	go func() {
		// 订阅边界 panic 不得导致整个进程退出，也不能伪造成功终态。
		defer func() {
			if recover() != nil {
				done <- errors.New("event subscription failed")
			}
		}()
		done <- events.Follow(ctx, scope, id, after, func(e domain.Event) error {
			select {
			case deliveries <- e:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	finished := false
	defer func() {
		cancel()
		if !finished {
			<-done
		}
	}()
	ticker := time.NewTicker(cfg.SSEHeartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			streamErr = ctx.Err()
			return
		case streamErr = <-done:
			finished = true
			return
		case <-ticker.C:
			if write(": heartbeat\n\n") != nil {
				return
			}
		case e := <-deliveries:
			// data 使用 JSON 信封；原始 Data 保持字节（JSON base64），不假设一定是 JSON。
			data, err := json.Marshal(struct {
				RunID     string    `json:"run_id"`
				Sequence  int64     `json:"sequence"`
				Data      []byte    `json:"data,omitempty"`
				CreatedAt time.Time `json:"created_at"`
			}{e.RunID, e.Sequence, e.Data, e.CreatedAt})
			if err != nil {
				return
			}
			if write("id: "+strconv.FormatInt(e.Sequence, 10)+"\nevent: "+string(e.Kind)+"\ndata: "+string(data)+"\n\n") != nil {
				return
			}
		}
	}
}
