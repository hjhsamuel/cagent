package observability

import (
	"context"
	"errors"
	"github.com/hjhsamuel/cagent/internal/apperrors"
	"github.com/sirupsen/logrus"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// 同时进入临界区的操作数不能超过容量；取消和重复释放不能偷走其他持有者的槽位。
func TestGateOverloadCancellationAndRelease(t *testing.T) {
	g := NewGate(2)
	a, _ := g.Try(context.Background())
	b, _ := g.Try(context.Background())
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := g.Try(context.Background()); !errors.Is(err, apperrors.ErrOverloaded) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	a()
	a()
	c, err := g.Try(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = g.Try(context.Background()); !errors.Is(err, apperrors.ErrOverloaded) {
		t.Fatal("double release expanded capacity", err)
	}
	b()
	c()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = g.Try(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	d, err := g.Try(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	d()
}

func TestTraceCorrelationBoundedRetentionAndMetrics(t *testing.T) {
	r := NewRecorder()
	request, cancel := context.WithCancel(context.Background())
	parent, end := r.Start(request, "request")
	child := Link(context.Background(), parent)
	cancel()
	if child.Err() != nil {
		t.Fatal("request cancellation reached background operation")
	}
	child, done := r.Start(child, "model")
	done(errors.New("secret response"))
	done(nil)
	end(nil)
	spans := r.Spans()
	if len(spans) != 2 || spans[0].ParentID != spans[1].ID || spans[0].TraceID != TraceID(child) || !spans[0].Failed {
		t.Fatal(spans)
	}
	spans[0].ID = "mutated"
	if r.Spans()[0].ID == "mutated" {
		t.Fatal("mutable trace alias")
	}
	for i := 0; i < 300; i++ {
		_, done := r.Start(context.Background(), "secret-user-tool-name")
		done(nil)
	}
	if len(r.Spans()) != 256 {
		t.Fatal("unbounded traces")
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	out := w.Body.String()
	for _, s := range []string{`cagent_operations_total{operation="model"} 1`, `cagent_failures_total{operation="model"} 1`, `cagent_active{operation="model"} 0`, `cagent_operations_total{operation="other"} 300`} {
		if !strings.Contains(out, s) {
			t.Fatal(out)
		}
	}
	if strings.Contains(out, "secret") {
		t.Fatal("sensitive label")
	}
}

func TestRedactionDoesNotFormatSecretsOrMutateEntry(t *testing.T) {
	e := logrus.NewEntry(logrus.New())
	e.Message = "tool.failed"
	e.Data = logrus.Fields{"api_key": "secret-key", "authorization": "secret-bearer", "prompt": "secret-prompt", "tenant_id": "secret-tenant", "nested": map[string]string{"token": "secret-nested"}, "error": errors.New("secret-error"), "run_id": "safe-run"}
	f := safeFormatter{&logrus.TextFormatter{DisableColors: true}}
	data, err := f.Format(e)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "secret") || !strings.Contains(string(data), "safe-run") || !strings.Contains(string(data), "internal_error") {
		t.Fatal(string(data))
	}
	if e.Data["api_key"] != "secret-key" {
		t.Fatal("mutated caller fields")
	}
}
