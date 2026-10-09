package adk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hjhsamuel/cagent/internal/adapter/mongodb"
	"github.com/hjhsamuel/cagent/internal/agent"
	"github.com/hjhsamuel/cagent/internal/config"
)

func openAIConfig(url string) config.Agent {
	return config.Agent{Provider: "openai", Model: "dynamic/vendor-model", BaseURL: url, APIKey: "test-secret", RequestTimeout: 2 * time.Second}
}
func sse(w http.ResponseWriter, text, finish string) {
	fmt.Fprintf(w, "data: {\"id\":\"id\",\"choices\":[{\"index\":0,\"delta\":{\"content\":%q},\"finish_reason\":%q}]}\n\n", text, finish)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// 真实 HTTP 边界验证动态 URL/模型/密钥、不发送输出限额字段、长输入及流式聚合。
func TestOpenAIWireStreamingWithoutTokenLimits(t *testing.T) {
	for _, field := range []string{"plain", "long input"} {
		t.Run(field, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path != "/custom/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer test-secret" {
					t.Error("wrong endpoint/auth")
				}
				var body map[string]any
				if e := json.NewDecoder(r.Body).Decode(&body); e != nil {
					t.Error(e)
				}
				if body["model"] != "dynamic/vendor-model" || body["max_tokens"] != nil || body["max_completion_tokens"] != nil || body["stream"] != true {
					t.Error("dynamic parameters not sent")
				}
				messages := body["messages"].([]any)
				if len(messages) != 3 || messages[0].(map[string]any)["role"] != "system" {
					t.Error("request mapping mismatch")
				}
				if field == "long input" && messages[2].(map[string]any)["content"] != strings.Repeat("history ", 10000) {
					t.Error("long input was trimmed")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				sse(w, "hello ", "")
				sse(w, "world", "stop")
				fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":20,\"completion_tokens\":2,\"total_tokens\":22}}\n\ndata: [DONE]\n\n")
			}))
			defer server.Close()
			cfg := openAIConfig(server.URL + "/custom/v1")
			m, e := NewOpenAI(cfg, nil)
			if e != nil {
				t.Fatal(e)
			}
			runtime := runtimeFor(t, m)
			req := request("user")
			if field == "long input" {
				req.Messages[len(req.Messages)-1].Parts[0].Text = strings.Repeat("history ", 10000)
			}
			var updates []agent.Update
			if e = runtime.Execute(context.Background(), req, func(_ context.Context, u agent.Update) error { updates = append(updates, u); return nil }); e != nil {
				t.Fatal(e)
			}
			if len(updates) != 3 || updates[2].Message[0].Text != "hello world" || calls.Load() != 1 {
				t.Fatal("bad streamed completion")
			}
			var data OutputData
			if e = json.Unmarshal(updates[2].Data, &data); e != nil || data.PromptTokens != 20 || updates[2].PromptTokens != 20 {
				t.Fatal("lost usage", e)
			}
		})
	}
}

func TestOpenAIHTTPFailuresAreBoundedAndSafe(t *testing.T) {
	for _, mode := range []string{"status", "error_frame", "truncated", "length", "tool", "refusal", "timeout", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if mode == "status" {
					w.WriteHeader(429)
					fmt.Fprint(w, `{"error":{"message":"test-secret prompt contents"}}`)
					return
				}
				if mode == "timeout" || mode == "cancel" {
					_, _ = io.Copy(io.Discard, r.Body)
					select {
					case <-r.Context().Done():
					case <-time.After(time.Second):
					}
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				switch mode {
				case "error_frame":
					fmt.Fprint(w, "data: {\"error\":{\"message\":\"test-secret prompt contents\"}}\n\n")
				case "truncated":
					sse(w, "partial", "")
				case "length":
					sse(w, "partial", "length")
				case "tool":
					fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c\",\"function\":{\"name\":\"unsafe\"}}]}}]}\n\n")
				case "refusal":
					fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"refusal\":\"no\"}}]}\n\n")
				}
			}))
			defer server.Close()
			cfg := openAIConfig(server.URL)
			cfg.RequestTimeout = 100 * time.Millisecond
			m, e := NewOpenAI(cfg, nil)
			if e != nil {
				t.Fatal(e)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "cancel" {
				time.AfterFunc(20*time.Millisecond, cancel)
			}
			mapped, e := mapMessages(request("u").Messages, m.Name())
			if e != nil {
				t.Fatal(e)
			}
			var failure error
			final := false
			for response, e := range m.GenerateContent(ctx, mapped, true) {
				if e != nil {
					failure = e
					break
				}
				if response != nil && !response.Partial {
					final = true
				}
			}
			if failure == nil || final || calls.Load() != 1 {
				t.Fatalf("false success/retry: %v %v %d", failure, final, calls.Load())
			}
			if strings.Contains(fmt.Sprintf("%+v", failure), "test-secret") {
				t.Fatal("secret in error")
			}
			if mode == "cancel" && !errors.Is(failure, context.Canceled) {
				t.Fatal(failure)
			}
			if mode == "timeout" && !errors.Is(failure, context.DeadlineExceeded) {
				t.Fatal(failure)
			}
		})
	}
}

func TestOpenAIRedirectAndNonStreaming(t *testing.T) {
	var destinationCalls atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { destinationCalls.Add(1) }))
	defer destination.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, destination.URL, 307) }))
	defer redirect.Close()
	m, e := NewOpenAI(openAIConfig(redirect.URL), nil)
	if e != nil {
		t.Fatal(e)
	}
	mapped, _ := mapMessages(request("u").Messages, m.Name())
	for _, e := range m.GenerateContent(context.Background(), mapped, true) {
		if e == nil {
			t.Fatal("redirect followed")
		}
	}
	if destinationCalls.Load() != 0 {
		t.Fatal("credential redirected")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"id","choices":[{"index":0,"message":{"role":"assistant","content":"answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`)
	}))
	defer server.Close()
	m, e = NewOpenAI(openAIConfig(server.URL), nil)
	if e != nil {
		t.Fatal(e)
	}
	count := 0
	for out, e := range m.GenerateContent(context.Background(), mapped, false) {
		if e != nil {
			t.Fatal(e)
		}
		count++
		if out.Content.Parts[0].Text != "answer" || !out.TurnComplete || out.Partial {
			t.Fatal("missing answer")
		}
	}
	if count != 1 {
		t.Fatal(count)
	}
}

// 真实模型仅在显式设置 CAGENT_TEST_MODEL=1 后运行，不从开发者环境猜测调用目标。
// 使用实际配置；失败不打印密钥、URL、提示词或模型回复，只验证非空最终输出及 usage。
func TestRealOpenAISmoke(t *testing.T) {
	if os.Getenv("CAGENT_TEST_MODEL") != "1" {
		t.Skip("real model not configured: set CAGENT_TEST_MODEL=1, MongoDB model selector and encryption keyring")
	}
	cfg, e := config.Load()
	if e != nil {
		t.Fatal(e)
	}
	ring, e := config.NewKeyring(cfg.ModelEncryption)
	if e != nil {
		t.Fatal(e)
	}
	db, e := mongodb.Open(context.Background(), mongodb.Options{URI: cfg.MongoDB.URI, Database: cfg.MongoDB.Database, Timeout: 10 * time.Second})
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close(context.Background())
	docs, e := db.ListModels(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	catalog, e := config.NewModelCatalog(docs, cfg.Agent.Name, ring)
	if e != nil {
		t.Fatal(e)
	}
	selected, e := catalog.Select("")
	if e != nil {
		t.Fatal(e)
	}
	cfg.Agent = selected.Agent
	m, e := NewOpenAI(cfg.Agent, nil)
	if e != nil {
		t.Fatal(e)
	}
	runtime, e := New(m)
	if e != nil {
		t.Fatal(e)
	}
	req := request("smoke")
	req.Messages = req.Messages[len(req.Messages)-1:]
	req.Messages[0].Parts[0].Text = "Reply with the single word OK."
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Agent.RequestTimeout)
	defer cancel()
	completed := false
	e = runtime.Execute(ctx, req, func(_ context.Context, u agent.Update) error {
		if u.Kind == "message.completed" {
			completed = true
			var data OutputData
			if e := json.Unmarshal(u.Data, &data); e != nil {
				return e
			}
			if data.PromptTokens <= 0 {
				return fmt.Errorf("provider usage is missing")
			}
		}
		return nil
	})
	if e != nil {
		t.Fatal(safeError(e))
	}
	if !completed {
		t.Fatal("missing final model output")
	}
}
