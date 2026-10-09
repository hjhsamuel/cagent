package adk

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	openaimodel "github.com/hjhsamuel/cagent/pkg/llm/openai"
	"github.com/openai/openai-go/v3/option"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// wireMessages 验证实际 pkg/llm/openai 请求，避免测试保留一份独立协议转换。
func wireMessages(req *model.LLMRequest) ([]any, error) {
	type captured struct {
		messages []any
		err      error
	}
	result := make(chan captured, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var wire struct {
			Messages []any `json:"messages"`
		}
		err := json.NewDecoder(r.Body).Decode(&wire)
		result <- captured{wire.Messages, err}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer server.Close()
	llm, err := openaimodel.NewModel(context.Background(), "test-model", &openaimodel.Config{
		ApiKey: "test-secret", Url: server.URL, API: openaimodel.APIChatCompletions,
		Options: []option.RequestOption{option.WithMaxRetries(0)},
	})
	if err != nil {
		return nil, err
	}
	for _, err := range llm.GenerateContent(context.Background(), req, false) {
		if err != nil {
			return nil, err
		}
	}
	wire := <-result
	return wire.messages, wire.err
}

func finalResponse(text string, prompt, completion int64) *model.LLMResponse {
	return &model.LLMResponse{
		Content: genai.NewContentFromText(text, "model"), TurnComplete: true,
		FinishReason:  genai.FinishReasonStop,
		UsageMetadata: &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: int32(prompt), CandidatesTokenCount: int32(completion)},
	}
}

func TestManagedModelRejectsRefusalWithStop(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"refusal\":\"no\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
				} else {
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprint(w, `{"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","refusal":"no"}}]}`)
				}
			}))
			defer server.Close()
			llm, err := NewOpenAI(openAIConfig(server.URL), nil)
			if err != nil {
				t.Fatal(err)
			}
			req, _ := mapMessages(request("user").Messages, llm.Name())
			failed := false
			for response, err := range llm.GenerateContent(context.Background(), req, stream) {
				if err != nil {
					failed = true
				}
				if response != nil && !response.Partial {
					t.Fatal("refusal yielded a completed response")
				}
			}
			if !failed {
				t.Fatal("refusal accepted as success")
			}
		})
	}
}

func TestManagedModelRejectsInvalidToolBatch(t *testing.T) {
	for _, kind := range []string{"unknown", "duplicate", "missing-id"} {
		t.Run(kind, func(t *testing.T) {
			secondID, secondName := "second", "lookup"
			switch kind {
			case "unknown":
				secondName = "unknown"
			case "duplicate":
				secondID = "first"
			case "missing-id":
				secondID = ""
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if body["stream"] == true {
					t.Error("tools requested a streaming response")
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","tool_calls":[{"id":"first","type":"function","function":{"name":"lookup","arguments":"{}"}},{"id":%q,"type":"function","function":{"name":%q,"arguments":"{}"}}]}}]}`, secondID, secondName)
			}))
			defer server.Close()
			llm, err := NewOpenAI(openAIConfig(server.URL), nil)
			if err != nil {
				t.Fatal(err)
			}
			req, _ := mapMessages(request("user").Messages, llm.Name())
			req.Config.Tools = []*genai.Tool{{FunctionDeclarations: []*genai.FunctionDeclaration{{Name: "lookup"}}}}
			failed := false
			for response, err := range llm.GenerateContent(context.Background(), req, true) {
				if response != nil {
					t.Fatal("invalid batch reached ADK")
				}
				if err != nil {
					failed = true
				}
			}
			if !failed {
				t.Fatal("invalid batch accepted")
			}
		})
	}
}
