package adk

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/hjhsamuel/cagent/internal/config"
	"github.com/hjhsamuel/cagent/internal/storage/schema"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

func TestWeightedKeysAndThinkingOnBothWirePaths(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			var mu sync.Mutex
			seen := map[string]int{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				auth := r.Header.Get("Authorization")
				if auth != "Bearer first-secret" && auth != "Bearer second-secret" {
					t.Error("unrecognized authorization")
				}
				mu.Lock()
				seen[auth]++
				mu.Unlock()
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					return
				}
				thinking, ok := body["thinking"].(map[string]any)
				if !ok || thinking["type"] != "enabled" || body["max_tokens"] != float64(16) || body["model"] != "dynamic/vendor-model" {
					t.Error("configured thinking or model limits not sent")
				}
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					sse(w, "hello", "stop")
					fmt.Fprint(w, "data: [DONE]\n\n")
				} else {
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprint(w, `{"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hello"}}]}`)
				}
			}))
			defer server.Close()
			ring, err := config.NewKeyring(config.ModelEncryption{Keys: map[string]string{"v1": "MDEyMzQ1Njc4OWFiY2RlZg=="}})
			if err != nil {
				t.Fatal(err)
			}
			first, _ := ring.Encrypt("first-secret", 1)
			second, _ := ring.Encrypt("second-secret", 1)
			disabled, _ := ring.Encrypt("disabled-secret", 0)
			pool, err := config.NewKeyPool([]schema.EncryptedKey{first, second, disabled}, ring)
			if err != nil {
				t.Fatal(err)
			}
			cfg := openAIConfig(server.URL)
			cfg.APIKey = ""
			cfg.Provider = "GLM"

			cfg.Keys = pool
			cfg.Thinking = &config.Thinking{Enabled: true, Key: "thinking.type", Value: "enabled"}
			llm, err := NewOpenAI(cfg, nil)
			if err != nil {
				t.Fatal(err)
			}
			cfg.Thinking.Value = "caller-mutation"
			for range 80 {
				req := &model.LLMRequest{Model: llm.Name(), Contents: []*genai.Content{genai.NewContentFromText("hello", "user")}, Config: &genai.GenerateContentConfig{MaxOutputTokens: 16}}
				for _, err := range llm.GenerateContent(context.Background(), req, stream) {
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if seen["Bearer first-secret"] == 0 || seen["Bearer second-secret"] == 0 || len(seen) != 2 {
				t.Fatal("keys were not independently selected per request")
			}
		})
	}
}
