package observability

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/hjhsamuel/cagent/internal/apperrors"
)

// Gate 是进程内的无队列容量闸门。调用方在创建 goroutine/外部副作用前领取，
// 在完整操作（含流式读取）结束后释放；它不替代数据库租约或跨实例配额。
type Gate struct {
	slots chan struct{}
	name  string
}

// NewGate 的可选名称仅用于固定类别的拒绝计数；测试/无观测的闸门可省略。
func NewGate(n int, names ...string) *Gate {
	if n < 1 {
		panic("capacity must be positive")
	}
	name := ""
	if len(names) > 0 {
		name = operation(names[0])
	}
	return &Gate{slots: make(chan struct{}, n), name: name}
}

// Try 不创建等待者，过载立即返回稳定类别。释放函数幂等，便于多个退出路径共用。
func (g *Gate) Try(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case g.slots <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-g.slots }) }, nil
	default:
		if g.name != "" {
			Default.reject(g.name)
		}
		return nil, apperrors.ErrOverloaded
	}
}

// Span 只包含服务生成的关联标识、固定操作名及数值，不保存请求、错误文本或凭据。
// 环形缓存仅用于本实例排障；重启丢失，不承担审计和恢复事实存储职责。
type Span struct {
	TraceID, ID, ParentID, Operation string
	Started                          time.Time
	Seconds                          float64
	Failed                           bool
}
type correlation struct{ trace, span string }
type traceKey struct{}

// Link 仅复制观测关联，生命周期仍取自 parent。HTTP 断开不能取消后台运行；
// 不使用 WithoutCancel 保留整个请求上下文，避免意外延长认证信息的内存寿命。
func Link(parent, source context.Context) context.Context {
	if c, ok := source.Value(traceKey{}).(correlation); ok {
		return context.WithValue(parent, traceKey{}, c)
	}
	return parent
}

// TraceID 供响应头和固定日志字段关联；值只由服务生成，不信任客户端传入的标识。
func TraceID(ctx context.Context) string { c, _ := ctx.Value(traceKey{}).(correlation); return c.trace }

type measurement struct {
	count, failures, active, rejected uint64
	seconds                           float64
}
type Recorder struct {
	mu         sync.Mutex
	metrics    map[string]*measurement
	spans      [256]Span
	next, size int
}

// reject 与操作失败分开统计：恢复候选跳过并不是远端执行失败。
func (r *Recorder) reject(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.metrics[name]
	if m == nil {
		m = &measurement{}
		r.metrics[name] = m
	}
	m.rejected++
}

func NewRecorder() *Recorder { return &Recorder{metrics: make(map[string]*measurement)} }

// Default 供同一进程的适配边界共享；测试可独立创建 Recorder，禁止运行中替换它。
var Default = NewRecorder()

func identifier() string { var b [16]byte; _, _ = rand.Read(b[:]); return hex.EncodeToString(b[:]) }
func operation(s string) string {
	switch s {
	case "request", "model", "tool", "storage", "sse", "recovery", "observation", "run":
		return s
	default:
		return "other"
	}
}

// Start 沿 context 传播父子关联；不接受远端任意标签。结束函数幂等，即使 panic
// 清理与正常返回共用 defer，也不会重复扣减活动数或累计耗时。
func (r *Recorder) Start(ctx context.Context, name string) (context.Context, func(error)) {
	name = operation(name)
	parent, _ := ctx.Value(traceKey{}).(correlation)
	if parent.trace == "" {
		parent.trace = identifier()
	}
	span := Span{TraceID: parent.trace, ID: identifier(), ParentID: parent.span, Operation: name, Started: time.Now()}
	r.mu.Lock()
	m := r.metrics[name]
	if m == nil {
		m = &measurement{}
		r.metrics[name] = m
	}
	m.active++
	r.mu.Unlock()
	var once sync.Once
	return context.WithValue(ctx, traceKey{}, correlation{span.TraceID, span.ID}), func(err error) {
		once.Do(func() {
			span.Seconds = time.Since(span.Started).Seconds()
			span.Failed = err != nil
			r.mu.Lock()
			defer r.mu.Unlock()
			m.active--
			m.count++
			m.seconds += span.Seconds
			if span.Failed {
				m.failures++
			}
			r.spans[r.next] = span
			r.next = (r.next + 1) % len(r.spans)
			if r.size < len(r.spans) {
				r.size++
			}
		})
	}
}

// Spans 返回独立副本，最多 256 条；不因客户端读取速度积累后台 goroutine。
func (r *Recorder) Spans() []Span {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Span, 0, r.size)
	for i := 0; i < r.size; i++ {
		out = append(out, r.spans[(r.next-r.size+i+len(r.spans))%len(r.spans)])
	}
	return out
}

// ServeHTTP 输出 Prometheus 文本格式，标签只有固定的 operation，绝不使用用户、
// Run、工具名、URL 或错误文本。路由的认证与网络隔离由 HTTP 装配负责。
func (r *Recorder) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	r.mu.Lock()
	copy := make(map[string]measurement, len(r.metrics))
	for k, v := range r.metrics {
		copy[k] = *v
	}
	r.mu.Unlock()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	for k, v := range copy {
		fmt.Fprintf(w, "cagent_operations_total{operation=%q} %d\ncagent_failures_total{operation=%q} %d\ncagent_active{operation=%q} %d\ncagent_duration_seconds_sum{operation=%q} %g\n", k, v.count, k, v.failures, k, v.active, k, v.seconds)
		fmt.Fprintf(w, "cagent_capacity_rejections_total{operation=%q} %d\n", k, v.rejected)
	}
}
