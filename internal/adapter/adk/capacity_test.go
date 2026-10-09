package adk

import (
	"context"
	"errors"
	"github.com/hjhsamuel/cagent/internal/apperrors"
	"github.com/hjhsamuel/cagent/internal/observability"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"google.golang.org/adk/v2/model"
)

// 模型闸门覆盖整个流消费过程；超载不得触发 HTTP，消费者提前停止后必须关闭流并归还槽位。
func TestSharedModelCapacityAndEarlyConsumerRelease(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		sse(w, "hello", "stop")
	}))
	defer server.Close()
	gate := observability.NewGate(1)
	m, err := NewOpenAI(openAIConfig(server.URL), nil, gate)
	if err != nil {
		t.Fatal(err)
	}
	summary, err := NewOpenAI(openAIConfig(server.URL), nil, gate)
	if err != nil {
		t.Fatal(err)
	}
	mapped, _ := mapMessages(request("u").Messages, m.Name())
	hold, _ := gate.Try(context.Background())
	for _, client := range []model.LLM{m, summary} {
		var got error
		for _, e := range client.GenerateContent(context.Background(), mapped, true) {
			got = e
		}
		if !errors.Is(got, apperrors.ErrOverloaded) {
			t.Fatal(got)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("overload performed network call")
	}
	hold()
	for _, e := range m.GenerateContent(context.Background(), mapped, true) {
		if e != nil {
			t.Fatal(e)
		}
		break
	}
	release, err := gate.Try(context.Background())
	if err != nil {
		t.Fatal("early stop leaked model slot", err)
	}
	release()
	if calls.Load() != 1 {
		t.Fatal(calls.Load())
	}
}
