package bootstrap

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// 使用真实 listener 和持续请求验证关闭顺序，避免只检查 mock 调用次数。
func TestGracefulShutdownReleasesStreamingRequestsBeforeApplication(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	parent, stop := context.WithCancel(context.Background())
	defer stop()
	ended := make(chan struct{})
	var unavailable atomic.Bool
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(ended)
		w.Write([]byte("connected\n"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	result := make(chan error, 1)
	go func() {
		result <- serve(parent, l, h, time.Second, func() { unavailable.Store(true) }, func(ctx context.Context) error {
			if !unavailable.Load() {
				return errors.New("readiness still enabled")
			}
			select {
			case <-ended:
				return nil
			default:
				return errors.New("handler still active")
			}
		})
	}()
	client := &http.Client{Timeout: 3 * time.Second}
	response, err := client.Get("http://" + l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	stop()
	if _, err = io.ReadAll(response.Body); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown blocked")
	}
	if conn, err := net.DialTimeout("tcp", l.Addr().String(), time.Second); err == nil {
		conn.Close()
		t.Fatal("listener still accepts requests")
	}
}

func TestShutdownDeadlineForUncooperativeHandler(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	parent, stop := context.WithCancel(context.Background())
	defer stop()
	release := make(chan struct{})
	defer close(release)
	result := make(chan error, 1)
	go func() {
		result <- serve(parent, l, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("connected"))
			w.(http.Flusher).Flush()
			<-release
		}), 30*time.Millisecond, func() {}, func(ctx context.Context) error { return ctx.Err() })
	}()
	client := &http.Client{Timeout: 3 * time.Second}
	r, err := client.Get("http://" + l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	stop()
	select {
	case err = <-result:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("want timeout: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown not bounded")
	}
}
