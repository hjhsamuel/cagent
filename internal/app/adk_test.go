package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hjhsamuel/cagent/internal/adapter/adk"
	"github.com/hjhsamuel/cagent/internal/agent"
	"github.com/hjhsamuel/cagent/internal/apperrors"
	"github.com/hjhsamuel/cagent/internal/config"
	"github.com/hjhsamuel/cagent/internal/domain"
	"github.com/hjhsamuel/cagent/internal/store"
)

func adkConfig(url string) config.Config {
	cfg := config.Defaults()
	cfg.MongoDB.URI = "mongodb://localhost:27017"
	cfg.Agent.Provider = "openai"
	cfg.Agent.Model = "configured-model"
	cfg.Agent.BaseURL = url
	cfg.Agent.APIKey = "test-key"
	cfg.Agent.TokenEncoding = "o200k_base"
	return cfg
}

// 覆盖真实 HTTP SDK -> ADK Runner -> P4 事务 -> MongoDB 输出/检查点/终态的完整链路。
func TestADKOpenAIToDurableOutput(t *testing.T) {
	db, _ := testDatabase(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body struct {
			Messages []struct{ Role, Content string }
		}
		if e := json.NewDecoder(r.Body).Decode(&body); e != nil {
			t.Error(e)
		}
		if len(body.Messages) != 2 || body.Messages[0].Role != "system" || body.Messages[1].Content != "hello" {
			t.Error("lost or duplicated input")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"answer\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	a, e := NewOpenAIService(context.Background(), db, adkConfig(server.URL), []domain.Part{{Kind: domain.PartText, Text: "rules"}}, Options{LeaseDuration: 3 * time.Second, PollInterval: 50 * time.Millisecond})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { a.Close(context.Background()) })
	s := session(t, a)
	r := start(t, a, s, "stable")
	awaitStatus(t, db, r, domain.RunCompleted)
	messages, e := db.ListMessages(context.Background(), scope, s.ID, store.SequencePage{Limit: 10})
	if e != nil || len(messages.Items) != 2 || messages.Items[1].Parts[0].Text != "answer" {
		t.Fatal("durable messages", e)
	}
	cp, e := db.GetCheckpoint(context.Background(), scope, r.ID, r.ID)
	if e != nil || cp.Version != 1 || cp.Format != adk.CheckpointFormat {
		t.Fatal("durable checkpoint", e)
	}
	if strings.Contains(string(cp.Data), "test-key") {
		t.Fatal("credential in checkpoint")
	}
	events, e := db.ListEvents(context.Background(), scope, r.ID, store.SequencePage{Limit: 10})
	if e != nil {
		t.Fatal(e)
	}
	kinds := []domain.EventKind{domain.EventRunStarted, domain.EventTextDelta, domain.EventMessageCompleted, domain.EventRunCompleted}
	if len(events.Items) != len(kinds) {
		t.Fatalf("events: %+v", events.Items)
	}
	for i, ev := range events.Items {
		if ev.Kind != kinds[i] || ev.Sequence != int64(i+1) {
			t.Fatal("event order")
		}
	}
	replay := start(t, a, s, "stable")
	if replay.ID != r.ID || calls.Load() != 1 {
		t.Fatal("replayed model")
	}
}

type crashAfterOutput struct {
	inner agent.Runtime
	crash func()
}

func (r crashAfterOutput) Execute(ctx context.Context, req agent.Request, emit agent.Emit) error {
	return r.inner.Execute(ctx, req, func(c context.Context, u agent.Update) error {
		if e := emit(c, u); e != nil {
			return e
		}
		if u.Checkpoint != nil {
			r.crash()
		}
		return nil
	})
}
func (r crashAfterOutput) Resume(ctx context.Context, req agent.Request, c agent.Continuation, emit agent.Emit) error {
	return r.inner.Resume(ctx, req, c, emit)
}

// 在最终消息事务已提交、Run 终态尚未提交的准确窗口停止服务，新实例用持久完成
// 检查点结算 Run；禁止重新调用模型或重复追加已经提交的最终消息。
func TestADKRecoverCommittedOutputWithoutRegeneration(t *testing.T) {
	db, _ := testDatabase(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"durable\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	opts := Options{LeaseDuration: 3 * time.Second, PollInterval: 50 * time.Millisecond}
	cfg := adkConfig(server.URL)
	a, e := NewOpenAIService(context.Background(), db, cfg, nil, opts)
	if e != nil {
		t.Fatal(e)
	}
	a.runtime = crashAfterOutput{inner: a.runtime, crash: a.stop}
	t.Cleanup(func() { a.Close(context.Background()) })
	s := session(t, a)
	r := start(t, a, s, "")
	deadline := time.Now().Add(5 * time.Second)
	for a.ctx.Err() == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if a.ctx.Err() == nil {
		t.Fatal("crash point not reached")
	}
	if e = a.Close(context.Background()); e != nil {
		t.Fatal(e)
	}
	current, e := db.GetRun(context.Background(), scope, r.ID)
	if e != nil || current.Status != domain.RunRunning {
		t.Fatal("wrong crash window", e)
	}
	before, e := db.ListMessages(context.Background(), scope, s.ID, store.SequencePage{Limit: 10})
	if e != nil || len(before.Items) != 2 {
		t.Fatal("final output not committed", e)
	}
	b, e := NewOpenAIService(context.Background(), db, cfg, nil, opts)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { b.Close(context.Background()) })
	if e = b.RecoverRun(context.Background(), scope, r.ID); e != nil {
		t.Fatal(e)
	}
	awaitStatus(t, db, r, domain.RunCompleted)
	after, e := db.ListMessages(context.Background(), scope, s.ID, store.SequencePage{Limit: 10})
	if e != nil || len(after.Items) != 2 || calls.Load() != 1 {
		t.Fatal("duplicate generation or output", e)
	}
}

func TestADKFailedModelHasNoMessageOrCheckpoint(t *testing.T) {
	db, _ := testDatabase(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n")
	}))
	defer server.Close()
	a, e := NewOpenAIService(context.Background(), db, adkConfig(server.URL), nil, Options{LeaseDuration: 3 * time.Second, PollInterval: 50 * time.Millisecond})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { a.Close(context.Background()) })
	s := session(t, a)
	r := start(t, a, s, "")
	awaitStatus(t, db, r, domain.RunFailed)
	page, e := db.ListMessages(context.Background(), scope, s.ID, store.SequencePage{Limit: 10})
	if e != nil || len(page.Items) != 1 {
		t.Fatal("partial persisted as final", e)
	}
	if _, e = db.GetCheckpoint(context.Background(), scope, r.ID, r.ID); !errors.Is(e, apperrors.ErrNotFound) {
		t.Fatal("false checkpoint", e)
	}
	if calls.Load() != 1 {
		t.Fatal("retried uncertain model request")
	}
}
