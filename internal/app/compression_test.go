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
		return emit(c, agent.Update{Kind: domain.EventMessageCompleted, Message: []domain.Part{{Kind: domain.PartText, Text: long}}})
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
			summaries.Add(1)
			if body.Stream || len(body.Messages) != 2 || !strings.Contains(body.Messages[1].Content, long) {
				t.Error("summary did not read original history")
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
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"answer\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	cfg := adkConfig(server.URL)
	cfg.Context.CompressionThresholdPercent = 1
	cfg.Context.KeepRecentRounds = 1
	cfg.Context.SummaryModel = "summary-model"
	cfg.Context.SummaryWindowTokens = 32000
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
	if e != nil || len(page.Items) != 6 || page.Items[1].Parts[0].Text != long {
		t.Fatal("source history lost", e)
	}
	// 关闭新摘要生成后仍可复用旧快照，但全部历史用户原文必须继续保留。
	cfg.Context.CompressionThresholdPercent = 0
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

func (f appSummaryFunc) Summarize(c context.Context, m []domain.Message) (string, error) {
	return f(c, m)
}

// 用户取消撤销生成权限，即使摘要提供方迟到返回成功，也不得保存候选或调用运行时。
func TestCompressionCancellationRejectsLateSummary(t *testing.T) {
	db, _ := testDatabase(t)
	ctx := context.Background()
	seed := service(t, db, func(c context.Context, r agent.Request, emit agent.Emit) error {
		return emit(c, agent.Update{Kind: domain.EventMessageCompleted, Message: []domain.Part{{Kind: domain.PartText, Text: strings.Repeat("x", 2000)}}})
	})
	s := session(t, seed)
	r := start(t, seed, s, "")
	awaitStatus(t, db, r, domain.RunCompleted)
	entered, release := make(chan struct{}), make(chan struct{})
	engine, e := contextengine.NewCompressing(contextCounter(func(_ context.Context, m []domain.Message) (int, error) {
		n := 0
		for _, v := range m {
			for _, p := range v.Parts {
				n += len(p.Text) + 1
			}
		}
		return n, nil
	}), appSummaryFunc(func(context.Context, []domain.Message) (string, error) {
		close(entered)
		<-release
		return "late summary", nil
	}), contextengine.CompressionPolicy{ThresholdPercent: 1, KeepRecentRounds: 1})
	if e != nil {
		t.Fatal(e)
	}
	var executed atomic.Int32
	a := service(t, db, func(context.Context, agent.Request, agent.Emit) error { executed.Add(1); return nil })
	a.opts.Prepare, e = NewContextPreparer(db, engine, ContextOptions{PolicyVersion: "v1", Budget: contextengine.Budget{WindowTokens: 4000, OutputTokens: 100}})
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
