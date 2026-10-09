package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hjhsamuel/cagent/internal/agent"
	"github.com/hjhsamuel/cagent/internal/apperrors"
	"github.com/hjhsamuel/cagent/internal/contextengine"
	"github.com/hjhsamuel/cagent/internal/domain"
	"github.com/hjhsamuel/cagent/internal/store"
)

// 真实装配链路：生成长历史 -> 独立摘要模型 -> CAS 快照 -> ADK 主模型 -> 持久终态。
// 第三轮会重建更长前缀并递增版本；主模型请求到来时摘要必须已经可读。
func TestCompressionOpenAIServicePersistsBeforeGeneration(t *testing.T) {
	db, _ := testDatabase(t)
	ctx := context.Background()
	long := strings.Repeat("recorded fact ", 500)
	seed := service(t, db, func(c context.Context, r agent.Request, emit agent.Emit) error {
		return emit(c, agent.Update{Kind: domain.EventMessageCompleted, Message: []domain.Part{{Kind: domain.PartText, Text: long}}, PromptTokens: 8192})
	})
	s := session(t, seed)
	first := start(t, seed, s, "")
	awaitStatus(t, db, first, domain.RunCompleted)
	var summaries, generations atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model    string
			Stream   bool
			Messages []struct{ Role, Content string }
		}
		if e := json.NewDecoder(r.Body).Decode(&body); e != nil {
			t.Error(e)
		}
		if body.Model == "summary-model" {
			number := summaries.Add(1)
			if body.Stream || len(body.Messages) != 2 || (number == 1 && !strings.Contains(body.Messages[1].Content, long)) || (number == 2 && (!strings.Contains(body.Messages[1].Content, "recorded facts") || strings.Contains(body.Messages[1].Content, long))) {
				t.Error("summary did not use the expected original or incremental source")
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"recorded facts"}}]}`)
			return
		}
		generations.Add(1)
		snap, e := db.LatestSnapshot(ctx, scope, s.ID)
		if e != nil || snap.Version != int64(min(generations.Load(), 2)) {
			t.Error("generation preceded snapshot", e, snap.Version)
		}
		hellos := 0
		for _, m := range body.Messages {
			if strings.Contains(m.Content, long) {
				t.Error("uncompressed answer sent")
			}
			if m.Content == "hello" {
				hellos++
			}
		}
		if hellos != int(generations.Load())+1 {
			t.Error("user original lost", hellos)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"answer\"},\"finish_reason\":\"stop\"}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":8192,\"completion_tokens\":1,\"total_tokens\":8193}}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	cfg := adkConfig(server.URL)
	cfg.Context.CompressionEnabled = true
	cfg.Context.KeepRecentRounds = 1
	cfg.Context.SummaryModel = "summary-model"
	a, e := NewOpenAIService(ctx, db, cfg, []domain.Part{{Kind: domain.PartText, Text: "fixed rules"}}, Options{LeaseDuration: 3 * time.Second, PollInterval: 30 * time.Millisecond})
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close(ctx)
	for i := 0; i < 2; i++ {
		r := start(t, a, s, "")
		awaitStatus(t, db, r, domain.RunCompleted)
	}
	if summaries.Load() != 2 || generations.Load() != 2 {
		t.Fatal(summaries.Load(), generations.Load())
	}
	snap, e := db.LatestSnapshot(ctx, scope, s.ID)
	if e != nil || snap.ThroughSequence != 4 || snap.Version != 2 || snap.PolicyVersion != cfg.Context.PolicyVersion {
		t.Fatal(snap, e)
	}
	page, e := db.ListMessages(ctx, scope, s.ID, store.SequencePage{Limit: 10})
	if e != nil || len(page.Items) != 6 || page.Items[1].Parts[0].Text != long || page.Items[3].PromptTokens != 8192 || page.Items[5].PromptTokens != 8192 {
		t.Fatal("source history lost", e)
	}
	// 关闭新摘要生成后仍可复用旧快照，但全部历史用户原文必须继续保留。
	cfg.Context.CompressionEnabled = false
	disabled, e := NewOpenAIService(ctx, db, cfg, nil, Options{LeaseDuration: 3 * time.Second, PollInterval: 30 * time.Millisecond})
	if e != nil {
		t.Fatal(e)
	}
	defer disabled.Close(ctx)
	r := start(t, disabled, s, "")
	awaitStatus(t, db, r, domain.RunCompleted)
	if summaries.Load() != 2 || generations.Load() != 3 {
		t.Fatal("disabled compression called summary")
	}
}

type appSummaryFunc func(context.Context, []domain.Message) (string, error)

// 主模型实际 HTTP usage 决定触发；跨服务重建保留用量，摘要模型用量不影响主对话。
func TestCompressionUsesPersistedLatestLLMUsage(t *testing.T) {
	db, _ := testDatabase(t)
	ctx := context.Background()
	var summaries, generations atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Model string }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.Model == "summary-model" {
			summaries.Add(1)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"facts"}}],"usage":{"prompt_tokens":100000,"completion_tokens":1}}`)
			return
		}
		n := int(generations.Add(1))
		wantSummaries := int32(0)
		if n >= 4 {
			wantSummaries = 1
		}
		if summaries.Load() != wantSummaries {
			t.Error("summary was not triggered by the latest response", n, summaries.Load())
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"answer\"},\"finish_reason\":\"stop\"}]}\n\n")
		tokens := []int{0, 8191, 8192, 0, 0}[n-1]
		if tokens != 0 {
			fmt.Fprintf(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":%d,\"completion_tokens\":1}}\n\n", tokens)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()
	cfg := adkConfig(server.URL)
	cfg.Context.KeepRecentRounds = 1
	cfg.Context.SummaryModel = "summary-model"
	newService := func() *Application {
		a, err := NewOpenAIService(ctx, db, cfg, nil, Options{LeaseDuration: 3 * time.Second, PollInterval: 30 * time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = a.Close(ctx) })
		return a
	}
	a := newService()
	s := session(t, a)
	for i := 0; i < 5; i++ {
		if i == 3 {
			if err := a.Close(ctx); err != nil {
				t.Fatal(err)
			}
			a = newService()
		}
		run := start(t, a, s, "")
		awaitStatus(t, db, run, domain.RunCompleted)
	}
	if summaries.Load() != 1 || generations.Load() != 5 {
		t.Fatal("unexpected summary calls", summaries.Load(), generations.Load())
	}
}

func (f appSummaryFunc) Summarize(c context.Context, m []domain.Message) (string, error) {
	return f(c, m)
}

// 用户取消撤销生成权限，即使摘要提供方迟到返回成功，也不得保存候选或调用运行时。
func TestCompressionCancellationRejectsLateSummary(t *testing.T) {
	db, _ := testDatabase(t)
	ctx := context.Background()
	seed := service(t, db, func(c context.Context, r agent.Request, emit agent.Emit) error {
		return emit(c, agent.Update{Kind: domain.EventMessageCompleted, Message: []domain.Part{{Kind: domain.PartText, Text: strings.Repeat("x", 2000)}}, PromptTokens: 8192})
	})
	s := session(t, seed)
	r := start(t, seed, s, "")
	awaitStatus(t, db, r, domain.RunCompleted)
	entered, release := make(chan struct{}), make(chan struct{})
	engine, e := contextengine.NewCompressing(appSummaryFunc(func(context.Context, []domain.Message) (string, error) {
		close(entered)
		<-release
		return "late summary", nil
	}), contextengine.CompressionPolicy{WindowTokens: 10240, ThresholdPercent: 80, KeepRecentRounds: 1})
	if e != nil {
		t.Fatal(e)
	}
	var executed atomic.Int32
	a := service(t, db, func(context.Context, agent.Request, agent.Emit) error { executed.Add(1); return nil })
	a.opts.Prepare, e = NewContextPreparer(db, engine, ContextOptions{PolicyVersion: "v1"})
	if e != nil {
		t.Fatal(e)
	}
	r = start(t, a, s, "")
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("summary not called")
	}
	e = a.CancelRun(ctx, scope, r.ID)
	close(release)
	if e != nil {
		t.Fatal(e)
	}
	if e = a.Close(ctx); e != nil {
		t.Fatal(e)
	}
	awaitStatus(t, db, r, domain.RunCancelled)
	_, e = db.LatestSnapshot(ctx, scope, s.ID)
	if !errors.Is(e, apperrors.ErrNotFound) || executed.Load() != 0 {
		t.Fatal("late summary accepted", e)
	}
}
