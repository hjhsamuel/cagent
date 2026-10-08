package httpapi_test

import (
	"context"
	"github.com/hjhsamuel/cagent/internal/apperrors"
	"github.com/hjhsamuel/cagent/internal/domain"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSSECapacityDisconnectAndReconnect(t *testing.T) {
	cfg := settings()
	cfg.MaxSubscriptions = 1
	exited := make(chan struct{}, 4)
	e := stubEvents{follow: func(ctx context.Context, _ int64, _ func(domain.Event) error) error {
		<-ctx.Done()
		exited <- struct{}{}
		return ctx.Err()
	}}
	service := new(stubService)
	server := httptest.NewServer(handler(t, service, e, cfg))
	defer server.Close()
	first := openStream(t, server.URL+"/api/v1/runs/run/events", token(t), "0")
	if first.StatusCode != 200 {
		t.Fatal(first.StatusCode)
	}
	second := openStream(t, server.URL+"/api/v1/runs/run/events", token(t), "0")
	data, _ := io.ReadAll(second.Body)
	second.Body.Close()
	if second.StatusCode != 503 || second.Header.Get("Retry-After") != "1" || !strings.Contains(string(data), "overloaded") {
		t.Fatal(second.StatusCode, string(data))
	}
	first.Body.Close()
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("subscription did not stop")
	}
	deadline := time.Now().Add(time.Second)
	for {
		next := openStream(t, server.URL+"/api/v1/runs/run/events", token(t), "1")
		next.Body.Close()
		if next.StatusCode == 200 {
			break
		}
		if next.StatusCode != 503 || time.Now().After(deadline) {
			t.Fatal("slot leaked", next.StatusCode)
		}
		time.Sleep(time.Millisecond)
	}
	if service.cancelled.Load() != 0 {
		t.Fatal("disconnect cancelled Run")
	}
}

func TestDiagnosticsWithoutAuthenticationAndOverloadMapping(t *testing.T) {
	cfg := settings()
	h := handler(t, &stubService{err: apperrors.ErrOverloaded}, stubEvents{}, cfg)
	for _, path := range []string{"/debug/metrics", "/debug/traces"} {
		for _, credential := range []string{"", token(t), "wrong"} {
			if w := request(h, "GET", path, "", credential); w.Code != 200 {
				t.Fatal(path, w.Code)
			}
		}
	}
	w := request(h, "POST", "/api/v1/sessions", "", token(t))
	if w.Code != 503 || w.Header().Get("Retry-After") != "1" || len(w.Header().Get("X-Trace-ID")) != 32 {
		t.Fatal(w.Code, w.Header())
	}
}
