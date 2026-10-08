package adk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/hjhsamuel/cagent/internal/contextengine"
)

// 使用真实 OpenAI SDK 和 BPE 验证摘要模型选择、非流式无工具请求、输出限额，
// 同时证明输入超限不发网络请求，截断和服务错误不会伪装为完整摘要或自动重试。
func TestSummaryModelHTTPBudgetAndFailure(t *testing.T) {
	for _, kind := range []string{"ok", "oversize", "truncated", "server-error", "empty", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if body["model"] != "summary-only" || body["tools"] != nil || body["stream"] == true || body["max_tokens"] != float64(100) {
					t.Errorf("wrong request %+v", body)
				}
				messages := body["messages"].([]any)
				if len(messages) != 2 || messages[1].(map[string]any)["role"] != "user" {
					t.Error("history promoted")
				}
				if kind == "server-error" {
					w.WriteHeader(500)
					return
				}
				finish, text := "stop", "摘要事实"
				if kind == "truncated" {
					finish = "length"
				}
				if kind == "empty" {
					text = ""
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"choices":[{"index":0,"finish_reason":%q,"message":{"role":"assistant","content":%q}}]}`, finish, text)
			}))
			defer srv.Close()
			cfg := openAIConfig(srv.URL)
			cfg.Model = "summary-only"
			llm, e := NewOpenAI(cfg, nil)
			if e != nil {
				t.Fatal(e)
			}
			b := contextengine.Budget{WindowTokens: 4096, OutputTokens: 100, SafetyTokens: 64}
			if kind == "oversize" {
				b.WindowTokens = 165
			}
			s, e := NewSummaryModel(llm, llm, b)
			if e != nil {
				t.Fatal(e)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if kind == "cancel" {
				cancel()
			}
			out, e := s.Summarize(ctx, request("original user requirement").Messages)
			if kind == "ok" {
				if e != nil || out != "摘要事实" {
					t.Fatal(out, e)
				}
			} else if e == nil || out != "" {
				t.Fatal("unsafe summary", out, e)
			}
			want := int32(1)
			if kind == "oversize" || kind == "cancel" {
				want = 0
			}
			if calls.Load() != want {
				t.Fatal("unexpected requests", calls.Load())
			}
			if kind == "oversize" && !errors.Is(e, contextengine.ErrBudgetExceeded) {
				t.Fatal(e)
			}
		})
	}
}
