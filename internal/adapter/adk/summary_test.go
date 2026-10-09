package adk

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// 使用真实 OpenAI SDK 验证摘要模型选择、无 Token 限额及失败不重试。
func TestSummaryModelHTTPWithoutLimitsAndFailure(t *testing.T) {
	for _, kind := range []string{"ok", "long input", "truncated", "server-error", "empty", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if body["model"] != "summary-only" || body["tools"] != nil || body["stream"] == true || body["max_tokens"] != nil || body["max_completion_tokens"] != nil {
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
			s, e := NewSummaryModel(llm)
			if e != nil {
				t.Fatal(e)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if kind == "cancel" {
				cancel()
			}
			source := request("original user requirement").Messages
			if kind == "long input" {
				source[len(source)-1].Parts[0].Text = strings.Repeat("history ", 10000)
			}
			out, e := s.Summarize(ctx, source)
			if kind == "ok" || kind == "long input" {
				if e != nil || out != "摘要事实" {
					t.Fatal(out, e)
				}
			} else if e == nil || out != "" {
				t.Fatal("unsafe summary", out, e)
			}
			want := int32(1)
			if kind == "cancel" {
				want = 0
			}
			if calls.Load() != want {
				t.Fatal("unexpected requests", calls.Load())
			}
		})
	}
}
